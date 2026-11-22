package daemon

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"time"

	"claude-abroad/internal/agent"
	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/exitpolicy"
	"claude-abroad/internal/usernet"
)

// 开发模式里主代理一侧的假实现（M5-8：应用的集成测试要走完“应用主代理 → 完成引导 → 启用”）。
// 只在 abroadd --fake-system --fake-main-proxy 时接上；不碰网络，出口是文档用的地址（203.0.113.0/24）。

// FakeProber 是假的出口检查：总是通过，出口在 US。
type FakeProber struct{}

// Check 实现 Prober。
func (FakeProber) Check(_ context.Context, exit string, _ exitcheck.Options) exitcheck.Report {
	g := &exitpolicy.Geo{IP: netip.MustParseAddr("203.0.113.7"), Country: "US", Region: "Example State", City: "Exampleville", Timezone: "America/Los_Angeles"}
	return exitcheck.Report{Exit: exit, At: time.Now(), Verdict: exitcheck.Unknown, IP: g.IP, Country: "US", Geo: g, Timezone: g.Timezone,
		Items: []exitcheck.Item{
			{Name: "https", Result: exitcheck.Pass}, {Name: "ip", Result: exitcheck.Pass}, {Name: "mainland", Result: exitcheck.Pass},
			{Name: "geo", Result: exitcheck.Pass}, {Name: "rule", Result: exitcheck.Unknown, Cause: exitcheck.CauseNoBaseline},
			{Name: "doh", Result: exitcheck.Pass},
		}}
}

// FakeMainVM 是假的虚拟机一侧（主代理和分流出口的下发都记下摘要、总是成功）。
type FakeMainVM struct {
	mu     sync.Mutex
	digest string
}

func (*FakeMainVM) BootID(context.Context) (string, error)               { return "fake-boot", nil }
func (*FakeMainVM) PushMainHops(context.Context, usernet.MainHops) error { return nil }
func (*FakeMainVM) HopPrivate(context.Context) (int64, error)            { return 0, nil }
func (*FakeMainVM) PushEndpoints(context.Context, []ExitEndpoint) error  { return nil }
func (*FakeMainVM) AgentExits(context.Context) ([]AgentExit, error)      { return nil, nil }
func (f *FakeMainVM) PushMainProxy(_ context.Context, yaml []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.digest = agent.ConfigDigest(yaml)
	return nil
}
func (f *FakeMainVM) MainProxyDigest(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.digest, nil
}
func (*FakeMainVM) PushExits(_ context.Context, xs []StoredExit) ([]AgentExit, error) {
	out := make([]AgentExit, 0, len(xs))
	for _, x := range xs {
		out = append(out, AgentExit{Tag: x.Tag, Up: true})
	}
	return out, nil
}
func (f *FakeMainVM) Health(context.Context) (VMHealth, error) {
	return VMHealth{BootID: "fake-boot", Version: "fake", Engines: map[string]EngineView{"mihomo": {State: "running"}}}, nil
}

// ErrFakeNoFetch 是假系统里抓取订阅的结果（不碰网络）。
var ErrFakeNoFetch = errors.New("开发模式的假系统不抓取订阅")

// WithFakeMainProxy 给假系统接上主代理一侧：出口基准（假检查）、假虚拟机；抓取订阅一律失败。
func WithFakeMainProxy(s System, stateDir string) System {
	vm := &FakeMainVM{}
	s.Baseline = NewBaselineService(FakeProber{}, stateDir)
	s.MainVM, s.ExitVM = vm, vm
	s.FetchSource = func(context.Context, string, bool) ([]byte, error) { return nil, ErrFakeNoFetch }
	return s
}
