package physif

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPhysicalInterfaceFollowsHardwareRouteAndSkipsTUN(t *testing.T) {
	hardware := "Hardware Port: Wi-Fi\nDevice: en0\nHardware Port: USB Ethernet\nDevice: en7\n"
	nwi := "IPv4 network interface information\n utun7 : flags : 0x7\n reach : 0x00000002 (Reachable)\n en7 : flags : 0x7\n reach : 0x00000002 (Reachable)\n en0 : flags : 0x7\n reach : 0x00000002 (Reachable)\nIPv6 network interface information\n"
	for _, tc := range []struct {
		name, route, want string
		usable            map[string]bool
	}{
		{"wifi default", "interface: en0", "en0", map[string]bool{"en0": true, "en7": true, "utun7": true}},
		{"ethernet default", "interface: en7", "en7", map[string]bool{"en0": true, "en7": true}},
		{"TUN default uses physical priority", "interface: utun7", "en7", map[string]bool{"en0": true, "en7": true, "utun7": true}},
		{"unavailable ethernet", "interface: en7", "en0", map[string]bool{"en0": true}},
		{"no physical connection", "interface: utun7", "", map[string]bool{"utun7": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectPhysicalInterface(tc.route, hardware, nwi, tc.usable)
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	if _, err := selectPhysicalInterface("interface: en0", hardware, strings.ReplaceAll(nwi, "(Reachable)", "(Not Reachable)"), map[string]bool{"en0": true, "en7": true}); err == nil {
		t.Fatal("selected an unreachable interface")
	}
}

func TestCheckNameRejectsVirtual(t *testing.T) {
	for _, n := range []string{"utun0", "lo0", "bridge100", "vmenet0", ""} {
		if checkName(n) == nil {
			t.Errorf("%q 应当作失败", n)
		}
	}
	for _, n := range []string{"en0", "en7"} {
		if checkName(n) != nil {
			t.Errorf("%q 应接受", n)
		}
	}
}

// fakeQuery 是可控的查询函数。
type fakeQuery struct {
	mu    sync.Mutex
	name  string
	err   error
	block chan struct{} // 非 nil 时查询卡住直到关闭
	calls atomic.Int32
}

func (f *fakeQuery) set(name string, err error) {
	f.mu.Lock()
	f.name, f.err = name, err
	f.mu.Unlock()
}

func (f *fakeQuery) q(ctx context.Context) (string, error) {
	f.calls.Add(1)
	f.mu.Lock()
	b := f.block
	f.mu.Unlock()
	if b != nil {
		select {
		case <-b:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.name, f.err
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newW(f *fakeQuery) (*Watcher, *clock) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	w := NewWatcher(f.q, 2*time.Second)
	w.Now = c.now
	return w, c
}

func waitIdle(t *testing.T, w *Watcher) {
	t.Helper()
	for range 200 {
		w.mu.Lock()
		idle := w.inflight == nil
		w.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("后台查询没有结束")
}

func TestWatcherFreshAndBackgroundRefresh(t *testing.T) {
	f := &fakeQuery{name: "en0"}
	w, c := newW(f)
	ctx := context.Background()
	if n, err := w.Current(ctx); n != "en0" || err != nil || f.calls.Load() != 1 {
		t.Fatalf("首次应同步查：%q %v %d", n, err, f.calls.Load())
	}
	c.add(time.Second)
	if n, _ := w.Current(ctx); n != "en0" || f.calls.Load() != 1 {
		t.Fatal("新鲜值不应触发查询")
	}
	// 过了 every：立即返回旧值，即使查询卡住；后台只跑一个刷新。
	c.add(2 * time.Second)
	block := make(chan struct{})
	f.mu.Lock()
	f.block, f.name = block, "en7"
	f.mu.Unlock()
	start := time.Now()
	for range 20 {
		if n, err := w.Current(ctx); n != "en0" || err != nil {
			t.Fatalf("应立即返回旧值：%q %v", n, err)
		}
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("查询卡住时 Current 也应在 100ms 内返回")
	}
	time.Sleep(20 * time.Millisecond)
	if f.calls.Load() != 2 {
		t.Fatalf("后台应只跑一个刷新：%d", f.calls.Load())
	}
	close(block)
	waitIdle(t, w)
	if n, _ := w.Current(ctx); n != "en7" {
		t.Fatalf("刷新完成后应拿到新值：%q", n)
	}
}

func TestWatcherInvalidateQueriesSync(t *testing.T) {
	f := &fakeQuery{name: "en0"}
	w, _ := newW(f)
	ctx := context.Background()
	w.Current(ctx)
	f.set("en7", nil)
	w.Invalidate()
	if w.Last() != "" {
		t.Fatal("作废后 Last 应为空")
	}
	if n, _ := w.Current(ctx); n != "en7" || f.calls.Load() != 2 {
		t.Fatalf("Invalidate 后应同步查：%q %d", n, f.calls.Load())
	}
	// 作废后查询失败：不再用作废的旧值。
	w.Invalidate()
	f.set("", errors.New("down"))
	if n, err := w.Current(ctx); err == nil || n != "" {
		t.Fatalf("作废后查询失败应报错：%q %v", n, err)
	}
}

func TestWatcherInvalidateDuringInflightQuery(t *testing.T) {
	f := &fakeQuery{name: "en0"}
	w, c := newW(f)
	ctx := context.Background()
	w.Current(ctx) // 第 1 次查询：en0
	c.add(3 * time.Second)
	block := make(chan struct{})
	f.mu.Lock()
	f.block = block
	f.mu.Unlock()
	if n, _ := w.Current(ctx); n != "en0" { // 触发后台刷新（第 2 次查询，卡住）
		t.Fatal("应返回旧值")
	}
	time.Sleep(20 * time.Millisecond)
	w.Invalidate() // 网卡切换：绑定失败
	type res struct {
		n   string
		err error
	}
	got := make(chan res, 1)
	go func() { n, err := w.Current(ctx); got <- res{n, err} }()
	time.Sleep(20 * time.Millisecond)
	f.mu.Lock()
	f.block, f.name = nil, "en7" // 不论在途查询读到什么，它是作废之前起的，结果都要丢弃
	f.mu.Unlock()
	close(block)
	r := <-got
	if r.n != "en7" || r.err != nil {
		t.Fatalf("作废之前起的查询结果应丢弃，再查一次拿到新名：%q %v", r.n, r.err)
	}
	if f.calls.Load() != 3 {
		t.Fatalf("作废后应再查一次（共 3 次，其中作废后 2 次），实际 %d", f.calls.Load())
	}
}

func TestWatcherConcurrentFirstCallsCoalesce(t *testing.T) {
	block := make(chan struct{})
	f := &fakeQuery{name: "en0", block: block}
	w, _ := newW(f)
	var wg sync.WaitGroup
	var bad atomic.Int32
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if n, err := w.Current(context.Background()); n != "en0" || err != nil {
				bad.Add(1)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(block)
	wg.Wait()
	if f.calls.Load() != 1 || bad.Load() != 0 {
		t.Fatalf("并发 50 个首次调用应只查一次：%d 次，%d 个失败", f.calls.Load(), bad.Load())
	}
}

func TestWatcherFailureUsesStaleThenFails(t *testing.T) {
	f := &fakeQuery{name: "en0"}
	w, c := newW(f)
	ctx := context.Background()
	w.Current(ctx)
	f.set("", errors.New("down"))
	c.add(10 * time.Second) // every ≤ 年龄 ≤ MaxStale：旧值，后台刷新失败
	if n, err := w.Current(ctx); n != "en0" || err != nil {
		t.Fatalf("MaxStale 内应返回旧值：%q %v", n, err)
	}
	waitIdle(t, w)
	if n, err := w.Current(ctx); n != "en0" || err != nil {
		t.Fatalf("刷新失败后 MaxStale 内仍用旧值：%q %v", n, err)
	}
	c.add(MaxStale) // 超过 MaxStale：同步查，失败即报错
	if n, err := w.Current(ctx); err == nil || n != "" {
		t.Fatalf("超过 MaxStale 且查询失败应报错：%q %v", n, err)
	}
}

func TestWatcherFailureCachedWithoutOldValue(t *testing.T) {
	f := &fakeQuery{err: errors.New("down")}
	w, c := newW(f)
	ctx := context.Background()
	for range 5 {
		if _, err := w.Current(ctx); err == nil {
			t.Fatal("应报错")
		}
	}
	if f.calls.Load() != 1 {
		t.Fatalf("失败结果应缓存 1 秒：%d 次查询", f.calls.Load())
	}
	c.add(FailCacheFor + time.Millisecond)
	f.set("en0", nil)
	if n, err := w.Current(ctx); n != "en0" || err != nil || f.calls.Load() != 2 {
		t.Fatalf("1 秒后应重新查询：%q %v %d", n, err, f.calls.Load())
	}
}

func TestWatcherVirtualNamesAreFailures(t *testing.T) {
	for _, n := range []string{"utun0", "lo0", "bridge100", "vmenet0", ""} {
		f := &fakeQuery{name: n}
		w, _ := newW(f)
		if got, err := w.Current(context.Background()); err == nil || got != "" {
			t.Errorf("%q 应当作失败：%q %v", n, got, err)
		}
	}
}

func TestWatcherCallerContextOnlyBoundsWait(t *testing.T) {
	block := make(chan struct{})
	f := &fakeQuery{name: "en0", block: block}
	w, _ := newW(f)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := w.Current(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("调用方超时应返回 ctx 错误：%v", err)
	}
	close(block) // 查询不跟调用方的 ctx 走，照样完成并更新缓存
	waitIdle(t, w)
	if n, _ := w.Current(context.Background()); n != "en0" || f.calls.Load() != 1 {
		t.Fatalf("查询应继续完成：%q %d", n, f.calls.Load())
	}
}

// physif 不能 import managedproxy 或 usernet。
func TestNoForbiddenImports(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, p := range files {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if path == "claude-abroad/internal/managedproxy" || path == "claude-abroad/internal/usernet" {
				t.Errorf("%s import 了 %s", p, path)
			}
		}
	}
}
