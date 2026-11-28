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

// blockOpener 的 Open 一直等到 release 关闭（复现真机：链式主代理对 STUN 的 UDP 迟迟不回开流结果）。
type blockOpener struct{ release chan struct{} }

func (o blockOpener) Open(ctx context.Context, _ wire.Header) (net.Conn, error) {
	select {
	case <-o.release:
	case <-ctx.Done():
	}
	return nil, errors.New("测试：开流失败")
}

func (blockOpener) Close() error { return nil }

// 2026-11-28 真机：08 判“请求确实经主代理发出”看的是 UDP 开流的发起次数，开流还没返回时就要计上。
func TestUDPTriedCountedBeforeOpenReturns(t *testing.T) {
	op := blockOpener{release: make(chan struct{})}
	dp := NewDataPlane(op, nil)
	co := countingOpener{dp, "proxy"}
	done := make(chan struct{})
	go func() {
		_, _ = co.Open(context.Background(), wire.Header{Exit: "main", Net: wire.UDP})
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for dp.OpenCounts()["main"].UDPTried != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("开流没返回时 UDPTried 应已是 1：%+v", dp.OpenCounts()["main"])
		}
		time.Sleep(5 * time.Millisecond)
	}
	if c := dp.OpenCounts()["main"]; c.UDP != 0 || c.Failed != 0 {
		t.Fatalf("开流还没返回：%+v", c)
	}
	close(op.release)
	<-done
	if c := dp.OpenCounts()["main"]; c.UDPTried != 1 || c.Failed != 1 {
		t.Fatalf("返回之后：%+v", c)
	}
	_, _ = co.Open(context.Background(), wire.Header{Exit: "main", Net: wire.TCP})
	if c := dp.OpenCounts()["main"]; c.UDPTried != 1 {
		t.Fatalf("TCP 不计进 UDPTried：%+v", c)
	}
}
