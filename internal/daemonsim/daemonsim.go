// Package daemonsim 是 abroad-dev daemon-sim 的逻辑（M2-9b-2 决定 9）：在进程内启动 abroadd 的 daemon，
// 系统层用假的（FakePF、sysnet.Fake + 命令模拟器），数据面用真的（网络栈、fake-IP DNS、代理、拨号器），
// utun 用 tun.Pipe，管道另一头接 tunsim 的客户端；数据通道连真实虚拟机的 data.sock。
// 经本地接口发启用、停用、再启用、恢复，在各阶段跑客户端探测。不碰宿主机网络（只连 unix socket）。
package daemonsim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"claude-abroad/internal/daemon"
	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/redact"
	"claude-abroad/internal/sysnet"
	"claude-abroad/internal/tun"
	"claude-abroad/internal/tunsim"
	"claude-abroad/internal/wire"
)

// Config 是一次运行的参数。
type Config struct {
	VMDir     string         // 真实虚拟机的状态目录（ctl.sock、host.sock、data.sock）
	StateDir  string         // daemon 的状态目录（短路径，unix socket 不能太长）；运行前应不存在
	ExitHost  string         // 出口检查的主机
	UDPTarget netip.AddrPort // UDP 探测的目标（真实 IP；主代理是开发上游时 mihomo 解析不了 UDP 的域名目标）
	Name      string         // 探测用的域名（默认 dsim.test）
	RealIP    netip.AddrPort // 真实 IP 的 TCP 探测（默认 203.0.113.9:80）
	Log       io.Writer      // daemon 的日志（经 redact）；nil 丢弃
	Timeout   time.Duration  // 单步时限（默认 15 秒）
	// DevExits 是经开发接口（agent 的 /v1/dev-exits）建的出口：daemon 按开发用的出口处理（M4-8，m4-plan v2.12 决定 4）。
	DevExits []string
	// ExitVM、CheckExit、RecheckInterval：分流出口的阶段（--exits）用；为空时不下发、不检查、复查用默认间隔。
	ExitVM          daemon.ExitVM
	CheckExit       func(ctx context.Context, tag string) exitcheck.Report
	RecheckInterval time.Duration
}

// Phase 是一个阶段的结果。
type Phase struct {
	Name   string        `json:"name"`
	HTTP   int           `json:"http"`
	State  string        `json:"state"`
	Gate   bool          `json:"gate_open"`
	Steps  []tunsim.Step `json:"steps,omitempty"`
	Opens  int64         `json:"opens"`        // 阶段结束时数据面的开流次数
	Opens0 int64         `json:"opens_before"` // 阶段开始时（请求生效后）的开流次数
	Expect string        `json:"expect"`
	OK     bool          `json:"ok"`
}

// Segment 是一段时间里的开流种类（“种类/出口/协议” → 次数，不含目的）。
type Segment struct {
	Name  string         `json:"name"`
	Kinds map[string]int `json:"kinds"`
}

// Report 是一次运行的报告（不含地址）。
type Report struct {
	OK       bool      `json:"ok"`
	Phases   []Phase   `json:"phases"`
	Segments []Segment `json:"segments"` // 按时间段列出每次开流的种类（M2-9c 第 0 步）
	LogLeak  bool      `json:"log_leak"` // daemon 日志里出现了探测用的假地址
	Err      string    `json:"err,omitempty"`
}

type namedDev struct {
	tun.Device
	name string
}

func (d namedDev) Name() string { return d.name }

func unixHTTP(sock string) *http.Client {
	return &http.Client{Timeout: 120 * time.Second, Transport: &http.Transport{DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}}}
}

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
	w  io.Writer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	l.b.Write(p)
	l.mu.Unlock()
	if l.w != nil {
		return l.w.Write(p)
	}
	return len(p), nil
}

func (l *logBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// Run 跑一遍。
func Run(ctx context.Context, cfg Config) (rep Report) {
	if cfg.Name == "" {
		cfg.Name = "dsim.test"
	}
	if !cfg.RealIP.IsValid() {
		cfg.RealIP = netip.MustParseAddrPort("203.0.113.9:80")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	fail := func(err error) Report { rep.Err = redact.String(err.Error()); return rep }

	hs, err := newHarness(ctx, cfg, nil)
	if err != nil {
		return fail(err)
	}
	defer hs.close()
	dp, fs, sim, lb, op, client := hs.dp, hs.fs, hs.sim, hs.lb, hs.op, hs.client
	pcfg := tunsim.Config{Name: cfg.Name, RealIP: cfg.RealIP, UDPTarget: cfg.UDPTarget, Timeout: cfg.Timeout}
	add := func(p Phase) { rep.Phases = append(rep.Phases, p) }
	mark := 0
	seg := func(name string) {
		log := dp.OpenLog()
		k := map[string]int{}
		for _, e := range log[mark:] {
			k[e]++
		}
		mark = len(log)
		rep.Segments = append(rep.Segments, Segment{Name: name, Kinds: k})
	}

	// 1. 启用：探测全部通过。
	code, st := op("/v1/enable")
	seg("启用请求（含出口检查）")
	p := Phase{Name: "enable", HTTP: code, State: st.State, Gate: st.GateOpen, Expect: "全部通过"}
	var fake netip.Addr
	if code == 200 {
		cl, err := client()
		if err != nil {
			return fail(err)
		}
		r := tunsim.Probe(ctx, cl, pcfg, netip.Addr{})
		p.Steps, p.OK, fake = r.Steps, r.OK(), r.Fake
	}
	p.Opens = dp.Opens()
	seg("启用后的探测")
	add(p)
	time.Sleep(500 * time.Millisecond)
	seg("探测结束后、停用请求之前（等 0.5 秒）")

	// 2. 停用：同样的探测（沿用启用时的假地址，不再查 DNS：假地址是本地分配的）全部失败，开流次数不增长。
	code, st = op("/v1/disable")
	seg("停用请求")
	p = Phase{Name: "disable", HTTP: code, State: st.State, Gate: st.GateOpen, Expect: "全部失败、不开流"}
	if code == 200 && fake.IsValid() {
		before := dp.Opens()
		p.Opens0 = before
		cl, err := client()
		if err != nil {
			return fail(err)
		}
		short := pcfg
		short.Timeout = 3 * time.Second
		r := tunsim.Probe(ctx, cl, short, fake)
		p.Steps = r.Steps
		p.Opens = dp.Opens()
		p.OK = r.Failed() && p.Opens == before && !st.GateOpen
	}
	seg("停用后的探测")
	add(p)

	// 3. 再启用：恢复。
	code, st = op("/v1/enable")
	seg("再启用请求（含出口检查）")
	p = Phase{Name: "re-enable", HTTP: code, State: st.State, Gate: st.GateOpen, Expect: "全部通过"}
	if code == 200 {
		cl, err := client()
		if err != nil {
			return fail(err)
		}
		r := tunsim.Probe(ctx, cl, pcfg, netip.Addr{})
		p.Steps, p.OK = r.Steps, r.OK() && r.Fake == fake
	}
	p.Opens = dp.Opens()
	seg("再启用后的探测")
	add(p)

	// 4. 恢复：回到 RESTORED，假系统与初始值相等。
	code, st = op("/v1/restore")
	p = Phase{Name: "restore", HTTP: code, State: st.State, Gate: st.GateOpen, Expect: "RESTORED、系统还原"}
	p.OK = code == 200 && st.State == "RESTORED" && !st.GateOpen && fs.Net.Snapshot().Equal(daemon.FakeInitialNet())
	p.Opens = dp.Opens()
	add(p)

	if u := sim.Unknown(); len(u) > 0 {
		rep.Err = fmt.Sprintf("模拟器收到计划外的命令 %d 条", len(u))
	}
	if fake.IsValid() && strings.Contains(lb.String(), fake.String()) {
		rep.LogLeak = true
	}
	rep.OK = rep.Err == "" && !rep.LogLeak
	for _, ph := range rep.Phases {
		rep.OK = rep.OK && ph.OK
	}
	return rep
}

// EnsureFresh 确认状态目录不存在（每次运行都从 RESTORED 开始）。
func EnsureFresh(dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("状态目录已存在：%s", dir)
	}
	return nil
}

// harness 是进程内的 daemon：系统层假的（FakePF、sysnet.Fake + 命令模拟器），数据面真的，数据通道接真实虚拟机。
type harness struct {
	dp     *daemon.DataPlane
	d      *daemon.Daemon
	fs     *daemon.FakeSystem
	sim    *sysnet.SimRunner
	lb     *logBuf
	logger *log.Logger
	op     func(path string) (int, daemon.Status)
	client func() (*tunsim.Client, error)
	close  func()
}

// newHarness 起进程内的 daemon。configure 在 daemon 启动前修改数据面（接直连拨号模块、识别器）。
func newHarness(ctx context.Context, cfg Config, configure func(*daemon.DataPlane)) (*harness, error) {
	// 系统层：FakePF + sysnet.Fake（经命令模拟器，用真实的 Mac* 实现）。
	fs := daemon.NewFakeSystem()
	sim := sysnet.NewSimRunner(fs.Net)
	dataSock := filepath.Join(cfg.VMDir, "data.sock")
	dialer := &wire.Dialer{Config: wire.DefaultSessionConfig(), Dial: func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", dataSock)
	}}
	dp := daemon.NewDataPlane(dialer, nil)
	if configure != nil {
		configure(dp)
	}
	var mu sync.Mutex
	var peers []tun.Device
	sys, err := daemon.NewRealSystem(daemon.RealParams{
		PFRunner: fs.PF, Net: sim,
		OpenTun: func() (tun.Device, error) {
			a, b := tun.Pipe(1500)
			mu.Lock()
			peers = append(peers, b)
			n := len(peers)
			mu.Unlock()
			return namedDev{a, fmt.Sprintf("utun%d", 40+n)}, nil
		},
		LookupUID: func(u string) (string, error) {
			if u == daemon.DefaultAllowUser {
				return "250", nil
			}
			return "", errors.New("没有这个用户")
		},
		Ifaces: func() ([]sysnet.IfaceInfo, error) {
			return []sysnet.IfaceInfo{{Name: "en0", Up: true, Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.1.23/24")}}}, nil
		},
		FirstHop: func() (string, error) { return "203.0.113.5", nil },
		// 进程退出或恢复时设备关闭，经它的路由随之消失（操作系统的行为）。
		OnTunClose: func(d tun.Device) { fs.Net.RemoveRoutesVia(d.Name()) },
		Data:       dp, VM: daemon.ExternalVM{Dir: cfg.VMDir, Timeout: 30 * time.Second},
		Exit:   daemon.ExitCheck{Opener: dp.Streams(), Host: cfg.ExitHost},
		Killer: fs.Kill,
		ExitVM: cfg.ExitVM, CheckExit: cfg.CheckExit,
	})
	if err != nil {
		return nil, err
	}
	lb := &logBuf{w: cfg.Log}
	logger := log.New(&redact.Writer{W: lb}, "daemon-sim ", log.LstdFlags)
	d, err := daemon.Start(ctx, daemon.Options{StateDir: cfg.StateDir, Sys: sys, Log: logger, SuperviseInterval: time.Second, DevExits: cfg.DevExits,
		RecheckInterval: cfg.RecheckInterval})
	if err != nil {
		return nil, err
	}
	hc := unixHTTP(d.SocketPath())
	// send 发一次；恢复要两步（m4-plan v2.3 决定 5）：拿到带确认码的 409 后，带确认码再调一次。
	send := func(path string) (*http.Response, error) {
		resp, err := hc.Post("http://abroadd"+path, "application/json", nil)
		if err != nil || path != "/v1/restore" || resp.StatusCode != http.StatusConflict {
			return resp, err
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var cn daemon.ConfirmNeeded
		if json.Unmarshal(b, &cn) != nil || cn.Confirm == "" {
			resp.Body = io.NopCloser(bytes.NewReader(b)) // 别的 409（忙）：照原样交给重试
			return resp, nil
		}
		logger.Printf("%s：预览列出 %d 个要结束的 Claude 进程，确认后执行", path, len(cn.Preview.Processes))
		body, _ := json.Marshal(daemon.ConfirmRequest{Confirm: cn.Confirm})
		return hc.Post("http://abroadd"+path, "application/json", bytes.NewReader(body))
	}
	op := func(path string) (int, daemon.Status) {
		var st daemon.Status
		resp, err := send(path)
		// 监护每轮会短暂占用状态机的忙标记，这时得到 409 是设计的行为：稍后重试（最多 10 秒）。
		for i := 0; err == nil && resp.StatusCode == http.StatusConflict && i < 100; i++ {
			resp.Body.Close()
			time.Sleep(100 * time.Millisecond)
			resp, err = send(path)
		}
		if err != nil {
			return 0, st
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(b, &st)
		if resp.StatusCode != 200 {
			logger.Printf("%s → %d：%s", path, resp.StatusCode, redact.String(string(b)))
		}
		return resp.StatusCode, st
	}
	// 每个管道只建一个客户端，各阶段复用：客户端 Close 会关掉它拿到的设备（管道另一头），管道一端关了两端都失效，
	// daemon 会判 utun 没了、关闸门重建（M2-9c 第 0 步查出的“多出一条流”就是这样来的：重开闸门前的出口检查）。
	clients := map[tun.Device]*tunsim.Client{}
	client := func() (*tunsim.Client, error) {
		mu.Lock()
		p := peers[len(peers)-1]
		mu.Unlock()
		if c, ok := clients[p]; ok {
			return c, nil
		}
		c, err := tunsim.NewClient(p, netip.Addr{})
		if err == nil {
			clients[p] = c
		}
		return c, err
	}
	return &harness{dp: dp, d: d, fs: fs, sim: sim, lb: lb, logger: logger, op: op, client: client, close: func() {
		for _, c := range clients {
			c.Close()
		}
		d.Shutdown()
		dialer.Close()
	}}, nil
}
