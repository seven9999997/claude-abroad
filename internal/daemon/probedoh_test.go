package daemon

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"claude-abroad/internal/fakedns"
	"claude-abroad/internal/gatesm"
	"claude-abroad/internal/wire"

	"github.com/miekg/dns"
)

type countOpener struct{ n atomic.Int32 }

func (o *countOpener) Open(context.Context, wire.Header) (net.Conn, error) {
	o.n.Add(1)
	return nil, errors.New("测试：不真的开流")
}

func (o *countOpener) Close() error { return nil }

// 2026-11-27 真机：启用第 ⑦ 步闸门还关着，07 的回显、08 的 STUN 解析经系统解析器都被答 SERVFAIL。
// 只给环境检测用的 ProbeLookup 在探测窗口开着时经主代理的 DoH 查；窗口和闸门都关着时不开流；普通的 DoH 上游照旧按闸门。
func TestProbeLookupFollowsProbeWindow(t *testing.T) {
	op := &countOpener{}
	dp := NewDataPlane(op, nil)
	g := newGate(gatesm.RealClock)
	g.AddReason(gatesm.ReasonState, "测试")
	dp.bind(g.View(), func() {})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if _, err := dp.ProbeTXT(ctx, "o-o.myaddr.l.google.com"); !errors.Is(err, fakedns.ErrGateClosed) || op.n.Load() != 0 {
		t.Fatalf("窗口和闸门都关着：应不开流、返回闸门关闭：%v（开流 %d 次）", err, op.n.Load())
	}
	if !g.OpenProbe(time.Minute) {
		t.Fatal("开探测窗口失败")
	}
	defer g.CloseProbe()
	if _, err := dp.ProbeA(ctx, "stun.l.google.com"); err == nil || errors.Is(err, fakedns.ErrGateClosed) || op.n.Load() == 0 {
		t.Fatalf("探测窗口开着：应经主代理开流去查：%v（开流 %d 次）", err, op.n.Load())
	}
	before := op.n.Load()
	if _, err := dp.doh.Exchange(ctx, new(dns.Msg)); !errors.Is(err, fakedns.ErrGateClosed) || op.n.Load() != before {
		t.Fatalf("普通的 DoH 上游应照旧按闸门：%v", err)
	}
}
