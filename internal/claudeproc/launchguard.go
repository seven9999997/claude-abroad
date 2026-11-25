package claudeproc

import (
	"context"
	"sync"
	"syscall"
	"time"

	"claude-abroad/internal/procinfo"
)

// 启动管控的第三层“发现即结束”（设计 10.2，M6-4b，m6-plan 的 10.2 口径 A–D）：不允许启动时每 200 毫秒扫一次进程表，
// 只结束 AutoJudge 判定“可以自动结束”的进程（只认签名）；先 TERM，宽限期后还在的 KILL（不阻塞扫描：下一轮补发）。
// 结束后不自动补开任何东西。每次发信号前用“进程号加启动时间”复核，不向自己、0、1 号进程发信号。

// GuardPolicy 是当前的启动管控口径（由 abroadd 按状态表算出）。
type GuardPolicy struct {
	Allowed  bool      // 允许启动（不结束任何进程）
	Since    time.Time // 这一段“不允许”开始的时刻：只结束在它之后启动的进程（口径 B、C）
	CoverAll bool      // 不看启动时间，所有确认过的都结束（口径 D：回国中、已回国；开机后第一次启动）
}

// 默认参数。
const (
	DefaultGuardInterval = 200 * time.Millisecond
	DefaultGuardGrace    = time.Second // 自动结束时 TERM 之后等多久再 KILL（没有人在等，短一些）
	MaxGuardRecords      = 20
)

// GuardRecord 是一次拦截（不含命令行和路径）。
type GuardRecord struct {
	Time time.Time `json:"time"`
	PID  int       `json:"pid"`
	Kind Kind      `json:"kind"`
	Name string    `json:"name"` // 安装方式的名字（来自覆盖表）
}

// LaunchGuard 是第三层。零值不可用：Procs、Info、Sig、Judge、Policy 必填。
type LaunchGuard struct {
	Procs    procinfo.Lister
	Info     func(pid int) (procinfo.Proc, error)
	Sig      Signaler
	Judge    *AutoJudge
	Policy   func() GuardPolicy
	Self     int // abroadd 自己的进程号
	Clock    Clock
	Interval time.Duration
	Grace    time.Duration
	OnBlock  func(GuardRecord) // 每拦下一个进程调用一次（记事件、发通知）；可为 nil

	mu      sync.Mutex
	pending map[procKey]time.Time // 已发 TERM、到点还在就 KILL
	records []GuardRecord
	blocked map[procKey]bool // 同一个进程只记一次
}

func (g *LaunchGuard) clock() Clock {
	if g.Clock != nil {
		return g.Clock
	}
	return RealClock
}

// Records 是最近的拦截记录（新的在后）。
func (g *LaunchGuard) Records() []GuardRecord {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]GuardRecord(nil), g.records...)
}

func (g *LaunchGuard) same(pid int, start time.Time) bool {
	p, err := g.Info(pid)
	return err == nil && p.PID == pid && p.Start.Equal(start)
}

func (g *LaunchGuard) signal(pid int, start time.Time, sig syscall.Signal) {
	if pid <= 1 || pid == g.Self || !g.same(pid, start) {
		return
	}
	_ = g.Sig.Signal(pid, sig)
}

// Tick 扫一轮。返回这一轮新拦下的进程（测试用）。
func (g *LaunchGuard) Tick() []GuardRecord {
	now := g.clock().Now()
	procs, err := g.Procs.List()
	if err != nil {
		return nil // 扫描失败：这一轮不结束任何进程
	}
	g.Judge.Prune(procs)
	g.mu.Lock()
	if g.pending == nil {
		g.pending, g.blocked = map[procKey]time.Time{}, map[procKey]bool{}
	}
	// 宽限期到了还在的：KILL
	var kill []procKey
	for k, dl := range g.pending {
		if !now.Before(dl) {
			kill = append(kill, k)
			delete(g.pending, k)
		}
	}
	g.mu.Unlock()
	for _, k := range kill {
		g.signal(k.pid, k.start, syscall.SIGKILL)
	}
	pol := g.Policy()
	if pol.Allowed {
		return nil
	}
	grace := dur(g.Grace, DefaultGuardGrace)
	var out []GuardRecord
	for _, p := range procs {
		if p.PID <= 1 || p.PID == g.Self {
			continue
		}
		if !pol.CoverAll && !p.Start.After(pol.Since) {
			continue // 口径 B、C：“不允许”开始之前已经在运行的不结束（它们没有网络）
		}
		r := g.Judge.Judge(p)
		if !r.Kill {
			continue
		}
		k := procKey{pid: p.PID, start: p.Start}
		g.mu.Lock()
		_, waiting := g.pending[k]
		first := !g.blocked[k]
		if !waiting {
			g.pending[k] = now.Add(grace)
		}
		var rec GuardRecord
		if first {
			g.blocked[k] = true
			rec = GuardRecord{Time: now, PID: p.PID, Kind: r.Kind, Name: kindName(r.Kind)}
			g.records = append(g.records, rec)
			if len(g.records) > MaxGuardRecords {
				g.records = g.records[len(g.records)-MaxGuardRecords:]
			}
		}
		g.mu.Unlock()
		if !waiting {
			g.signal(p.PID, p.Start, syscall.SIGTERM)
		}
		if first {
			out = append(out, rec)
			if g.OnBlock != nil {
				g.OnBlock(rec)
			}
		}
	}
	// 已经不在进程表里的拦截记号清掉（进程号被复用时不会被当成“记过了”）
	alive := make(map[procKey]bool, len(procs))
	for _, p := range procs {
		alive[procKey{pid: p.PID, start: p.Start}] = true
	}
	g.mu.Lock()
	for k := range g.blocked {
		if !alive[k] {
			delete(g.blocked, k)
		}
	}
	g.mu.Unlock()
	return out
}

// Run 每 Interval 扫一轮，直到 ctx 结束。
func (g *LaunchGuard) Run(ctx context.Context) {
	iv := dur(g.Interval, DefaultGuardInterval)
	for {
		g.Tick()
		if err := g.clock().Sleep(ctx, iv); err != nil {
			return
		}
	}
}

func kindName(k Kind) string {
	for _, r := range coverage {
		if r.Kind == k {
			return r.Name
		}
	}
	return string(k)
}
