package fakedns

import (
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Unix(1_800_000_000, 0)

func smallPool(t *testing.T, n int) *Pool {
	t.Helper()
	first := netip.MustParseAddr("198.18.1.0")
	last := first
	for range n - 1 {
		last = last.Next()
	}
	p, err := NewPool(first, last)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mustAssign(t *testing.T, p *Pool, name string, now time.Time) netip.Addr {
	t.Helper()
	a, err := p.Assign(name, now)
	if err != nil {
		t.Fatalf("Assign(%s)：%v", name, err)
	}
	return a
}

// a. 稳定性
func TestStableAcrossPoolsAndSpellings(t *testing.T) {
	p1, p2 := DefaultPool(), DefaultPool()
	a1 := mustAssign(t, p1, "claude.ai", t0)
	a2 := mustAssign(t, p2, "claude.ai", t0)
	if a1 != a2 {
		t.Fatalf("两个全新的池应给出同一地址：%s %s", a1, a2)
	}
	if b := mustAssign(t, p1, "Claude.AI.", t0); b != a1 || p1.Stats().Mappings != 1 {
		t.Fatalf("大小写、末尾点不同的写法应是同一个映射：%s %d", b, p1.Stats().Mappings)
	}
}

func TestNormalize(t *testing.T) {
	for _, bad := range []string{"", ".", "a..b", "-" + strings.Repeat("a", 63) + "x.com", strings.Repeat("a.", 127) + "com", "a b.com", "例子.com", "a/b"} {
		if _, err := Normalize(bad); !errors.Is(err, ErrBadName) {
			t.Errorf("%q 应拒绝", bad)
		}
	}
	for in, want := range map[string]string{"Example.COM.": "example.com", "_dmarc.example.com": "_dmarc.example.com", "localhost": "localhost"} {
		if got, err := Normalize(in); err != nil || got != want {
			t.Errorf("%q → %q %v", in, got, err)
		}
	}
	if _, err := DefaultPool().Assign("bad name", t0); !errors.Is(err, ErrBadName) {
		t.Error("Assign 应拒绝不合法的域名")
	}
}

// b. 冲突顺延与并发
func TestCollisionsAndConcurrency(t *testing.T) {
	p := smallPool(t, 16)
	seen := map[netip.Addr]bool{}
	for i := range 16 {
		a := mustAssign(t, p, fmt.Sprintf("n%d.example.com", i), t0)
		if seen[a] || !p.Contains(a) {
			t.Fatalf("地址重复或越界：%s", a)
		}
		seen[a] = true
	}

	d := DefaultPool()
	var mu sync.Mutex
	got := map[string]map[netip.Addr]bool{}
	var wg sync.WaitGroup
	for g := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 40 {
				name := fmt.Sprintf("c%d.example.com", (g+i)%20)
				a, err := d.Assign(name, t0)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				if got[name] == nil {
					got[name] = map[netip.Addr]bool{}
				}
				got[name][a] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	owner := map[netip.Addr]string{}
	for name, addrs := range got {
		if len(addrs) != 1 {
			t.Fatalf("同名应同地址：%s → %v", name, addrs)
		}
		for a := range addrs {
			if o, dup := owner[a]; dup {
				t.Fatalf("异名应异地址：%s 和 %s 都是 %s", o, name, a)
			}
			owner[a] = name
		}
	}
}

// c. 范围与性能
func TestFillDefaultPool(t *testing.T) {
	p := DefaultPool()
	reserved := netip.MustParsePrefix("198.18.0.0/24")
	top := netip.MustParseAddr("198.19.255.255")
	start := time.Now()
	n := int(p.size)
	for i := range n {
		a := mustAssign(t, p, "h"+strconv.Itoa(i)+".example.com", t0)
		if reserved.Contains(a) || a == top || !netip.MustParsePrefix("198.18.0.0/15").Contains(a) {
			t.Fatalf("地址越界：%s", a)
		}
	}
	took := time.Since(start)
	t.Logf("填满 %d 条用时 %s", n, took)
	if took > 20*time.Second {
		t.Fatalf("填满太慢（%s），可能有 O(n²)", took)
	}
	if p.Stats().Mappings != n {
		t.Fatalf("映射数 %d", p.Stats().Mappings)
	}
	if _, err := p.Assign("one-more.example.com", t0.Add(time.Minute)); !errors.Is(err, ErrPoolFull) {
		t.Fatalf("全部不满 30 分钟时应 ErrPoolFull：%v", err)
	}
}

// d. 淘汰
func TestEvictionLRUAndMinKeep(t *testing.T) {
	p := smallPool(t, 4)
	addr := map[string]netip.Addr{}
	for i, n := range []string{"a.com", "b.com", "c.com", "d.com"} {
		addr[n] = mustAssign(t, p, n, t0.Add(time.Duration(i)*time.Minute))
	}
	// 10 分钟后：全部不满 30 分钟 → ErrPoolFull，计数加一，映射不变。
	if _, err := p.Assign("x.com", t0.Add(10*time.Minute)); !errors.Is(err, ErrPoolFull) || p.Stats().Full != 1 {
		t.Fatalf("应 ErrPoolFull 并计数：%v %+v", err, p.Stats())
	}
	for n, a := range addr {
		if got, _ := p.Lookup(a); got != n {
			t.Fatalf("池满被拒后已有映射不应变：%s → %s", a, got)
		}
	}
	// b 在 20 分钟时又被查询过：最久没用的是 a。
	mustAssign(t, p, "b.com", t0.Add(20*time.Minute))
	e := mustAssign(t, p, "e.com", t0.Add(40*time.Minute))
	if e != addr["a.com"] {
		t.Fatalf("应淘汰最久没用的 a.com 并复用它的地址：%s", e)
	}
	if got, _ := p.Lookup(addr["a.com"]); got != "e.com" {
		t.Fatalf("旧地址应查到新域名：%s", got)
	}
	// a 再来：淘汰下一个最久没用的 c（b 在 20 分钟时用过，比 c 新）。
	if a := mustAssign(t, p, "a.com", t0.Add(41*time.Minute)); a == addr["a.com"] || a != addr["c.com"] {
		t.Fatalf("被淘汰的域名再 Assign 应得到新地址（c 的）：%s", a)
	}
	if p.Stats().Evictions != 2 {
		t.Fatalf("淘汰计数：%+v", p.Stats())
	}
}

func TestEvictionRespectsRefs(t *testing.T) {
	p := smallPool(t, 2)
	clock := t0
	p.Now = func() time.Time { return clock }
	a := mustAssign(t, p, "a.com", t0)
	mustAssign(t, p, "b.com", t0.Add(time.Minute))
	name, releaseA, ok := p.Acquire(a, t0.Add(2*time.Minute))
	if !ok || name != "a.com" {
		t.Fatal("Acquire 应拿到 a.com")
	}
	// 40 分钟后：a 有引用，不能淘汰；只能淘汰 b。
	c := mustAssign(t, p, "c.com", t0.Add(40*time.Minute))
	if got, _ := p.Lookup(a); got != "a.com" || c == a {
		t.Fatal("有引用的映射不应被淘汰")
	}
	_, releaseC, _ := p.Acquire(c, t0.Add(40*time.Minute))
	if _, err := p.Assign("d.com", t0.Add(90*time.Minute)); !errors.Is(err, ErrPoolFull) {
		t.Fatalf("全部在用时应 ErrPoolFull：%v", err)
	}
	clock = t0.Add(90 * time.Minute)
	releaseA()
	releaseA() // 幂等
	p.mu.Lock()
	refs := p.byName["a.com"].refs
	p.mu.Unlock()
	if refs != 0 {
		t.Fatalf("release 两次后引用数应为 0：%d", refs)
	}
	// a 刚释放（最近使用 = 90 分钟），不满 30 分钟，仍不能淘汰。
	if _, err := p.Assign("d.com", t0.Add(100*time.Minute)); !errors.Is(err, ErrPoolFull) {
		t.Fatal("释放时刷新了最近使用时间，不满 30 分钟不能淘汰")
	}
	if d := mustAssign(t, p, "d.com", t0.Add(121*time.Minute)); d != a {
		t.Fatalf("释放并满 30 分钟后才可淘汰：%s", d)
	}
	releaseC()
}

func TestClockGoingBackwardsNotEvictable(t *testing.T) {
	p := smallPool(t, 1)
	mustAssign(t, p, "a.com", t0.Add(time.Hour))
	if _, err := p.Assign("b.com", t0); !errors.Is(err, ErrPoolFull) {
		t.Fatal("最近使用时间比 now 还晚时按刚用过处理")
	}
}

// e. Acquire 的边界
func TestAcquireRejects(t *testing.T) {
	p := DefaultPool()
	mustAssign(t, p, "a.com", t0)
	for _, s := range []string{"198.18.1.7", "198.18.0.2", "198.19.255.255", "10.0.0.1", "::1"} {
		a := netip.MustParseAddr(s)
		if p.Contains(a) && s != "198.18.1.7" {
			t.Errorf("%s 不应在可分配范围里", s)
		}
		if _, rel, ok := p.Acquire(a, t0); ok || rel != nil {
			t.Errorf("%s 应 ok=false", s)
		}
		if _, ok := p.Lookup(a); ok {
			t.Errorf("Lookup(%s) 应 false", s)
		}
	}
}

// j. 源码检查：地址池和落盘只用标准库，且不 import 本项目的包（M2-4b 起只检查这两个文件）。
// 例外：internal/platform（W-1：落盘的不跟随符号链接打开挪到了那里；它自己只用标准库、不做网络访问，见它的源码检查）。
func TestImportsStdlibOnly(t *testing.T) {
	for _, p := range []string{"pool.go", "persist.go"} {
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
			if strings.Contains(strings.SplitN(path, "/", 2)[0], ".") || (strings.HasPrefix(path, "claude-abroad/") && path != "claude-abroad/internal/platform") {
				t.Errorf("%s import 了 %s（4a 只用标准库）", p, path)
			}
		}
	}
}

// M2-4b 第 0 步：Prune
func TestPrune(t *testing.T) {
	p := smallPool(t, 4)
	clock := t0
	p.Now = func() time.Time { return clock }
	a := mustAssign(t, p, "a.com", t0)
	b := mustAssign(t, p, "b.com", t0)
	mustAssign(t, p, "c.com", t0.Add(20*24*time.Hour))
	_, releaseB, _ := p.Acquire(b, t0) // b 在用
	now := t0.Add(31 * 24 * time.Hour)
	if n := p.Prune(now, MaxAge); n != 1 {
		t.Fatalf("应只清掉 a（b 在用，c 不到 30 天）：%d", n)
	}
	if _, ok := p.Lookup(a); ok {
		t.Fatal("a 应被清掉")
	}
	if got, _ := p.Lookup(b); got != "b.com" {
		t.Fatal("在用的 b 不应被清掉")
	}
	// Prune 之后再 release 仍然正常。
	clock = now
	releaseB()
	p.mu.Lock()
	refs, inLRU := p.byName["b.com"].refs, p.byName["b.com"].elem != nil
	p.mu.Unlock()
	if refs != 0 || !inLRU {
		t.Fatalf("Prune 之后 release 应正常：refs=%d inLRU=%v", refs, inLRU)
	}
	// 腾出的空位能被新域名按哈希起点重新占用：a 再来拿回原地址（起点就是它原来的位置，或顺延到空位）。
	if again := mustAssign(t, p, "a.com", now); again != a {
		t.Fatalf("a 再来应按哈希起点拿回空出来的地址：%s（原来 %s）", again, a)
	}
	// 填满剩下的空位：不应与现有地址冲突。
	seen := map[netip.Addr]bool{}
	for _, n := range []string{"a.com", "b.com", "c.com"} {
		addr := mustAssign(t, p, n, now)
		seen[addr] = true
	}
	d := mustAssign(t, p, "d.com", now)
	if seen[d] || p.Stats().Mappings != 4 {
		t.Fatalf("新域名应占用剩下的空位：%s %+v", d, p.Stats())
	}
}
