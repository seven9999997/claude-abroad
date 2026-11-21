// Package agent 是虚拟机里的 abroad-agent（设计文档 7.1）：作为 PID 1 完成启动、
// 加载模块、配置网卡，提供控制面 API，管理引擎子进程。
//
// 这个包除 sys_linux.go 外都可以在 macOS 上编译和测试：系统调用经 System 接口注入。
package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"claude-abroad/internal/kmod"
)

// Version 由构建时 -ldflags 注入；没注入时是 dev。
var Version = "dev"

// NetStatus 是网卡配置的结果。
type NetStatus struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Health 是 GET /v1/health 的应答。
type Health struct {
	OK bool `json:"ok"`
	// BootID 是这次开机的随机标识（serve 启动时生成，一次开机内不变）。
	// 宿主据此发现虚拟机原地重启过（内核复位时 VZ 不会通知宿主，设计文档 7.1）。
	BootID    string                  `json:"boot_id"`
	Version   string                  `json:"version"`
	Kernel    string                  `json:"kernel"`
	UptimeS   int64                   `json:"uptime_s"`
	Modules   []kmod.Result           `json:"modules"`
	Net       NetStatus               `json:"net"`
	Engines   map[string]EngineStatus `json:"engines"`
	MainProxy MainProxyStatus         `json:"main_proxy"`
	Data      *DataStats              `json:"data,omitempty"`
	Clock     *ClockStatus            `json:"clock,omitempty"`
	// Exits 是分流出口的状态（M4-7）：只有出口名、种类、可用与否和原因，没有端点和配置。
	Exits    []ExitStatus `json:"exits,omitempty"`
	Problems []string     `json:"problems,omitempty"`
}

// MainProxyStatus 是主代理的状态。
type MainProxyStatus struct {
	Configured bool `json:"configured"`
}

// State 汇总 agent 的运行状态，供 health 读取。并发安全。
type State struct {
	mu       sync.Mutex
	bootID   string
	start    time.Time
	kernel   string
	modules  []kmod.Result
	net      NetStatus
	problems []string
	now      func() time.Time
	engines  func() map[string]EngineStatus
	mainCfg  func() bool
	data     func() DataStats
	clock    func() ClockStatus
	exits    func() []ExitStatus
}

// SetExits 登记分流出口状态的来源（不影响 ok：某个分流出口不可用只影响走它的流量）。
func (s *State) SetExits(f func() []ExitStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exits = f
}

// SetClock 登记对时状态的来源（clock 段不影响 ok）。
func (s *State) SetClock(f func() ClockStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock = f
}

// SetData 登记数据面计数的来源。
func (s *State) SetData(f func() DataStats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = f
}

// SetEngines 登记引擎状态的来源；登记后 health 的 ok 还要求 sing-box 在运行。
func (s *State) SetEngines(engines func() map[string]EngineStatus, mainConfigured func() bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.engines, s.mainCfg = engines, mainConfigured
}

// NewState 记录启动时间。
func NewState(kernel string) *State {
	return &State{bootID: newBootID(), start: time.Now(), kernel: kernel, now: time.Now}
}

func newBootID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 在 Linux 上不会失败（getrandom）；万一失败，用时间兜底，仍保证每次开机不同。
		return fmt.Sprintf("t%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// AddModules 追加模块加载结果。
func (s *State) AddModules(rs []kmod.Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modules = append(s.modules, rs...)
}

// SetNet 记录网卡配置结果。
func (s *State) SetNet(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.net = NetStatus{OK: err == nil}
	if err != nil {
		s.net.Error = err.Error()
	}
}

// AddProblem 记一条不致命但需要报告的问题。
func (s *State) AddProblem(p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.problems = append(s.problems, p)
}

// Snapshot 返回当前状态。ok 表示模块全部加载成功、网卡已配置、没有问题。
func (s *State) Snapshot() Health {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := Health{
		BootID:   s.bootID,
		Version:  Version,
		Kernel:   s.kernel,
		UptimeS:  int64(s.now().Sub(s.start) / time.Second),
		Modules:  append([]kmod.Result{}, s.modules...),
		Net:      s.net,
		Engines:  map[string]EngineStatus{},
		Problems: append([]string(nil), s.problems...),
	}
	h.OK = h.Net.OK && len(kmod.Failed(h.Modules)) == 0 && len(h.Problems) == 0
	if s.engines != nil {
		h.Engines = s.engines()
		h.OK = h.OK && h.Engines["sing-box"].State == EngineRunning // 主代理没配置不算不正常
	}
	if s.mainCfg != nil {
		h.MainProxy.Configured = s.mainCfg()
	}
	if s.data != nil {
		d := s.data()
		h.Data = &d
	}
	if s.clock != nil {
		c := s.clock()
		h.Clock = &c
	}
	if s.exits != nil {
		h.Exits = s.exits()
	}
	return h
}

// Handler 是只有 health 和 shutdown 的控制面（测试和早期用法）。
func Handler(s *State, shutdown func()) http.Handler {
	return (&API{State: s, Shutdown: shutdown}).Handler()
}

// MaxLogTail 是 GET /v1/logs 一次最多返回的行数。
const MaxLogTail = 5000

// API 是控制面（vsock 1081）：
//   - GET  /v1/health
//   - POST /v1/shutdown：应答后调用 Shutdown，serve 随即退出、PID 1 关机
//     （vmhost 停虚拟机时先走这里，超时再强停）；
//   - PUT  /v1/main-proxy：见 MainProxy；
//   - GET  /v1/logs?engine=<名>&tail=N：引擎日志（已脱敏）。
type API struct {
	State     *State
	Shutdown  func()
	Logs      map[string]*RingLog
	MainProxy http.Handler
	Clock     http.Handler
	DevExits  http.Handler // 只有开发标记打开时非 nil（m3-plan v2.8 决定 7）
	// NetnsProbe 是 M4-1 的探查（POST /v1/dev/netns-probe），只有开发标记打开时非 nil。
	NetnsProbe http.Handler
	// Region 是 PUT /v1/region（M4-5）。
	Region http.Handler
	// Exits 是 GET、PUT /v1/exits（M4-7）。
	Exits http.Handler
	// DevPeer 是 M4-7 端到端用的同机对端（/v1/dev/exit-peer），只有开发标记打开时非 nil。
	DevPeer http.Handler
}

// Handler 返回路由。
func (a *API) Handler() http.Handler {
	s, shutdown := a.State, a.Shutdown
	mux := http.NewServeMux()
	if a.MainProxy != nil {
		mux.Handle("/v1/main-proxy", a.MainProxy)
	}
	if a.Clock != nil {
		mux.Handle("/v1/clock", a.Clock)
	}
	if a.DevExits != nil {
		mux.Handle("/v1/dev-exits", a.DevExits)
	}
	if a.NetnsProbe != nil {
		mux.Handle("/v1/dev/netns-probe", a.NetnsProbe)
	}
	if a.Region != nil {
		mux.Handle("/v1/region", a.Region)
	}
	if a.Exits != nil {
		mux.Handle("/v1/exits", a.Exits)
	}
	if a.DevPeer != nil {
		mux.Handle("/v1/dev/exit-peer", a.DevPeer)
	}
	mux.HandleFunc("/v1/logs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		name := r.URL.Query().Get("engine")
		l, ok := a.Logs[name]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "没有这个引擎"})
			return
		}
		n := 200
		if v := r.URL.Query().Get("tail"); v != "" {
			if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tail 应为正整数"})
				return
			}
		}
		n = min(n, MaxLogTail)
		lines, dropped := l.Tail(n)
		if lines == nil {
			lines = []string{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"engine": name, "lines": lines, "dropped": dropped})
	})
	mux.HandleFunc("/v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		shutdown()
	})
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		writeJSON(w, http.StatusOK, s.Snapshot())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// NewServer 返回控制面的 http.Server（带超时）。调用方传入监听器后调用 Serve。
func NewServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}
