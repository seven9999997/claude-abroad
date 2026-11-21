package pf

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// M2-7c a：BeforeEnable 在 -E 之前恰好一次；返回错误时没有 -E；pf 已启用时不调用。
func TestBeforeEnable(t *testing.T) {
	ctx := context.Background()
	e := newPatrolEnv(t)
	var order []string
	calls := 0
	e.p.C.BeforeEnable = func() error {
		calls++
		order = append(order, "before:"+itoa(len(e.f.Calls())))
		return nil
	}
	if err := e.p.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("BeforeEnable 调用 %d 次", calls)
	}
	cs := e.f.Calls()
	if i := slices.Index(cs, "enable"); i < 0 || order[0] != "before:"+itoa(i) {
		t.Fatalf("BeforeEnable 应紧挨在 -E 之前：%v %v", order, cs)
	}
	// 已启用：不调用。
	if err := e.p.Step(ctx); err != nil || calls != 1 {
		t.Fatalf("%v %d", err, calls)
	}
	// 返回错误：没有 -E，Ensure 返回该错误。
	e.f.Disable()
	boom := errors.New("boom")
	e.p.C.BeforeEnable = func() error { return boom }
	e.f.ResetCalls()
	if err := e.p.Step(ctx); !errors.Is(err, boom) {
		t.Fatalf("%v", err)
	}
	if slices.Contains(e.f.Calls(), "enable") {
		t.Fatal("BeforeEnable 失败后仍调用了 -E")
	}
	if e.p.Protected() {
		t.Fatal("应判失效")
	}
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}

// M2-7c a：Flush 之后暂停；Apply 恢复，首轮 Load + Kill。
func TestPatrolFlushPauses(t *testing.T) {
	ctx := context.Background()
	e := newPatrolEnv(t)
	e.step(t)
	if err := e.p.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if !e.p.Paused() || e.p.Protected() {
		t.Fatal("Flush 后应暂停且不受保护")
	}
	e.f.ResetCalls()
	for range 3 {
		if err := e.p.Step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if c := e.f.Calls(); len(c) != 0 {
		t.Fatalf("暂停期间有 pfctl 调用：%v", c)
	}
	if e.f.Active(DefaultAnchor) || e.p.Protected() {
		t.Fatal("锚点应为空")
	}
	kills := e.f.Kills()
	if err := e.p.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	c := e.f.Calls()
	if !slices.Contains(c, "load") || e.f.Kills() != kills+1 || !e.p.Protected() || e.p.Paused() {
		t.Fatalf("Apply 首轮应 Load + Kill：%v", c)
	}
}

// M2-7c a：Run 与 Flush 并发；Flush 返回后锚点不会再被加载。
func TestPatrolRunFlushRace(t *testing.T) {
	e := newPatrolEnv(t)
	e.p.Interval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); e.p.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	if err := e.p.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.f.ResetCalls()
	time.Sleep(30 * time.Millisecond)
	cancel()
	wg.Wait()
	if c := e.f.Calls(); slices.Contains(c, "load") || e.f.Active(DefaultAnchor) {
		t.Fatalf("Flush 之后又加载了：%v", c)
	}
}
