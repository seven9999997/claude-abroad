package procinfo

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"sync"
	"testing"
	"time"

	"claude-abroad/internal/rules"
)

var bg = context.Background()

func ap(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

// bundles 是测试用的 Bundle 读取：按 .app 路径查表，没有的返回错误。
func bundles(m map[string]string) BundleReader {
	return func(app string) (string, error) {
		if id, ok := m[app]; ok {
			return id, nil
		}
		return "", errors.New("读不到 Info.plist")
	}
}

var testBundles = bundles(map[string]string{
	"/Applications/Google Chrome.app":             "com.google.Chrome",
	"/Applications/Claude.app":                    "com.anthropic.claudefordesktop",
	"/System/Applications/Utilities/Terminal.app": "com.apple.Terminal",
	"/Applications/iTerm.app":                     "com.googlecode.iterm2",
	"/Applications/Evil.app":                      "com.evil app",
	"/Applications/Visual Studio Code.app":        "com.microsoft.VSCode",
})

// a. 应用 ID 映射
func TestAppIDForPath(t *testing.T) {
	cases := map[string]rules.AppID{
		"/Applications/Google Chrome.app/Contents/Frameworks/Google Chrome Framework.framework/Helpers/Google Chrome Helper.app/Contents/MacOS/Google Chrome Helper": "com.google.chrome",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome": "com.google.chrome",
		"/opt/homebrew/bin/git":         "path:/opt/homebrew/bin/git",
		"/opt/homebrew//bin/../bin/git": "path:/opt/homebrew/bin/git",
		"/usr/bin/curl":                 "path:/usr/bin/curl",
		"/usr/sbin/mDNSResponder":       "system:mDNSResponder",
		"/usr/libexec/nsurlsessiond":    "system:nsurlsessiond",
		"/System/Library/Frameworks/WebKit.framework/Versions/A/XPCServices/com.apple.WebKit.Networking.xpc/Contents/MacOS/com.apple.WebKit.Networking": "system:com.apple.WebKit.Networking",
		"/Applications/NoPlist.app/Contents/MacOS/x": rules.Unknown, // Info.plist 读不出来：不退回成 path:
		"/Applications/Evil.app/Contents/MacOS/x":    rules.Unknown, // Bundle ID 不合法
		"relative/bin/x": rules.Unknown,
		"":               rules.Unknown,
		"/System/Applications/Utilities/Terminal.app/Contents/MacOS/Terminal": "com.apple.terminal",
	}
	for p, want := range cases {
		if got := AppIDForPath(p, testBundles); got != want {
			t.Errorf("%q → %q，期望 %q", p, got, want)
		}
	}
	if AppIDForPath("/Applications/Google Chrome.app/Contents/MacOS/x", nil) != rules.Unknown {
		t.Error("没有 Bundle 读取函数时应是 unknown")
	}
}

// 连接表和进程的假实现。
type world struct {
	table *FakeTable
	procs *FakeProcs
	id    *Identifier
}

func newWorld(procs map[int]Proc, snaps ...[]Entry) *world {
	w := &world{
		table: &FakeTable{Seq: map[string][][]Entry{"tcp": snaps, "udp": snaps}},
		procs: &FakeProcs{Procs: procs},
	}
	w.id = &Identifier{Table: w.table, Procs: w.procs, Bundle: testBundles, Retries: 2, RetryDelay: time.Millisecond}
	return w
}

var (
	src = ap("198.18.0.1:50000")
	dst = ap("198.18.1.7:443")
)

func conn(pid int) Entry { return Entry{Local: src, Remote: dst, PID: pid} }

// b. Claude 判定
func TestClaude(t *testing.T) {
	procs := map[int]Proc{
		10: {PID: 10, PPID: 1, Path: "/Applications/Claude.app/Contents/MacOS/Claude"},
		11: {PID: 11, PPID: 10, Path: "/Applications/Claude.app/Contents/Frameworks/Claude Helper.app/Contents/MacOS/Claude Helper"},
		20: {PID: 20, PPID: 1, Path: "/Users/u/.local/bin/claude"},
		21: {PID: 21, PPID: 1, Path: "/opt/homebrew/bin/node", Args: []string{"node", "/usr/local/bin/claude"}},
		22: {PID: 22, PPID: 1, Path: "/opt/homebrew/Caskroom/claude-code/1.0/claude"},
		23: {PID: 23, PPID: 1, Path: "/usr/local/bin/bun", Args: []string{"bun", "/x/node_modules/@anthropic-ai/claude-code/cli.js"}},
		24: {PID: 24, PPID: 1, Path: "/opt/homebrew/bin/node", Args: []string{"node", "server.js"}},
		// 终端里启动的 claude：Terminal → login → zsh → claude → zsh → node（MCP 服务）
		100: {PID: 100, PPID: 1, Path: "/System/Applications/Utilities/Terminal.app/Contents/MacOS/Terminal"},
		101: {PID: 101, PPID: 100, Path: "/usr/bin/login"},
		102: {PID: 102, PPID: 101, Path: "/bin/zsh"},
		103: {PID: 103, PPID: 102, Path: "/Users/u/.local/bin/claude"},
		104: {PID: 104, PPID: 103, Path: "/bin/zsh"},
		105: {PID: 105, PPID: 104, Path: "/opt/homebrew/bin/node", Args: []string{"node", "mcp.js"}},
		106: {PID: 106, PPID: 102, Path: "/usr/bin/curl"},
	}
	cases := []struct {
		name    string
		pid     int
		inherit bool
		app     rules.AppID
		claude  bool
	}{
		{"桌面版", 10, false, "com.anthropic.claudefordesktop", true},
		{"桌面版 Helper", 11, false, "com.anthropic.claudefordesktop", true},
		{"原生安装", 20, false, "path:/Users/u/.local/bin/claude", true},
		{"npm 全局安装（node + /usr/local/bin/claude）", 21, false, "path:/opt/homebrew/bin/node", true},
		{"Homebrew", 22, false, "path:/opt/homebrew/Caskroom/claude-code/1.0/claude", true},
		{"bun + 包名", 23, false, "path:/usr/local/bin/bun", true},
		{"普通 node", 24, false, "path:/opt/homebrew/bin/node", false},
		{"终端里的 claude，继承关", 103, false, "path:/Users/u/.local/bin/claude", true},
		{"终端里的 claude，继承开：不归到终端名下", 103, true, "path:/Users/u/.local/bin/claude", true},
		{"claude 的 MCP 子进程", 105, false, "path:/opt/homebrew/bin/node", true},
		{"claude 的 MCP 子进程，继承开", 105, true, "path:/opt/homebrew/bin/node", true},
		{"终端里的 curl，继承关", 106, false, "path:/usr/bin/curl", false},
		{"终端里的 curl，继承开", 106, true, "com.apple.terminal", false},
	}
	for _, c := range cases {
		w := newWorld(procs, []Entry{conn(c.pid)})
		w.id.InheritParent = c.inherit
		r := w.id.Identify(bg, "tcp", src, dst)
		if r.App != c.app || r.Claude != c.claude || r.PID != c.pid {
			t.Errorf("%s：%+v，期望 app=%s claude=%v", c.name, r, c.app, c.claude)
		}
	}
}

func TestClaudeDepthAndCycles(t *testing.T) {
	// 第 k 级祖先是 claude：k ≤ 16 算，第 17 级不算。
	for _, k := range []int{16, 17} {
		procs := map[int]Proc{1000: {PID: 1000, PPID: 1, Path: "/Users/u/.local/bin/claude"}}
		parent := 1000
		for i := k - 1; i >= 0; i-- {
			pid := 2000 + i
			procs[pid] = Proc{PID: pid, PPID: parent, Path: "/bin/zsh"}
			parent = pid
		}
		w := newWorld(procs, []Entry{conn(2000)})
		if r := w.id.Identify(bg, "tcp", src, dst); r.Claude != (k <= 16) {
			t.Errorf("claude 在第 %d 级：Claude=%v", k, r.Claude)
		}
	}
	// 祖先链成环：不死循环。
	procs := map[int]Proc{
		5: {PID: 5, PPID: 6, Path: "/bin/zsh"},
		6: {PID: 6, PPID: 7, Path: "/bin/bash"},
		7: {PID: 7, PPID: 5, Path: "/bin/sh"},
	}
	w := newWorld(procs, []Entry{conn(5)})
	w.id.InheritParent = true
	done := make(chan Result)
	go func() { done <- w.id.Identify(bg, "tcp", src, dst) }()
	select {
	case r := <-done:
		if r.Claude || r.App != "path:/bin/zsh" {
			t.Errorf("成环：%+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("祖先链成环时死循环")
	}
	// 祖先读不出来：只按本进程判。
	w = newWorld(map[int]Proc{8: {PID: 8, PPID: 9, Path: "/bin/zsh"}}, []Entry{conn(8)})
	if r := w.id.Identify(bg, "tcp", src, dst); r.Claude || r.App != "path:/bin/zsh" {
		t.Errorf("祖先读不出来：%+v", r)
	}
}

// e. 识别的整体行为
func TestIdentifyRetries(t *testing.T) {
	procs := map[int]Proc{7: {PID: 7, PPID: 1, Path: "/usr/bin/curl"}}
	w := newWorld(procs, nil, nil, []Entry{conn(7)})
	if r := w.id.Identify(bg, "tcp", src, dst); r.App != "path:/usr/bin/curl" {
		t.Fatalf("重试两次后应找到：%+v", r)
	}
	if n := w.table.Calls.Load(); n != 4 { // 3 次查找 + 1 次复核
		t.Fatalf("读连接表 %d 次，期望 4", n)
	}
	w = newWorld(procs, nil, nil, []Entry{conn(7)})
	w.id.Retries = 1
	if r := w.id.Identify(bg, "tcp", src, dst); r.App != rules.Unknown {
		t.Fatalf("只重试一次时应识别不到：%+v", r)
	}
	if n := w.table.Calls.Load(); n != 2 {
		t.Fatalf("读连接表 %d 次，期望 2", n)
	}
	// 重试间隔生效。
	w = newWorld(procs, nil, nil, nil)
	w.id.RetryDelay = 30 * time.Millisecond
	start := time.Now()
	w.id.Identify(bg, "tcp", src, dst)
	if d := time.Since(start); d < 60*time.Millisecond {
		t.Fatalf("两次重试的间隔没有生效：%v", d)
	}
}

func TestIdentifyFailuresAreUnknown(t *testing.T) {
	procs := map[int]Proc{
		7: {PID: 7, PPID: 1, Path: "/usr/bin/curl"},
		8: {PID: 8, PPID: 1, Path: "/usr/bin/nc"},
		9: {PID: 9, PPID: 1, Path: "/Applications/NoPlist.app/Contents/MacOS/x"},
		3: {PID: 3, PPID: 1, Path: ""},
	}
	cases := map[string]*world{
		"连接表里找不到":         newWorld(procs, nil),
		"匹配到多个进程":         newWorld(procs, []Entry{conn(7), conn(8)}),
		"进程已经退出":          newWorld(procs, []Entry{conn(55)}),
		"路径读不出来":          newWorld(procs, []Entry{conn(3)}),
		"Info.plist 读不出来": newWorld(procs, []Entry{conn(9)}),
		"进程号被复用（复核不一致）":   newWorld(procs, []Entry{conn(7)}, []Entry{conn(8)}),
		"复核时连接已消失":        newWorld(procs, []Entry{conn(7)}, nil),
	}
	tableErr := newWorld(procs, []Entry{conn(7)})
	tableErr.table.Err = errors.New("读连接表失败")
	cases["连接表读不出来"] = tableErr
	for name, w := range cases {
		w.id.Retries = 1
		if r := w.id.Identify(bg, "tcp", src, dst); r.App != rules.Unknown {
			t.Errorf("%s：应是 unknown，得到 %+v", name, r)
		}
	}
	// 同一个进程号的多条记录不算多个进程。
	w := newWorld(procs, []Entry{conn(7), conn(7)})
	if r := w.id.Identify(bg, "tcp", src, dst); r.App != "path:/usr/bin/curl" {
		t.Errorf("同一进程的两条记录：%+v", r)
	}
}

func TestIdentifyNotCached(t *testing.T) {
	procs := map[int]Proc{7: {PID: 7, PPID: 1, Path: "/usr/bin/curl"}}
	w := newWorld(procs, []Entry{conn(7)})
	w.id.Identify(bg, "tcp", src, dst)
	p1, t1 := w.procs.Calls.Load(), w.table.Calls.Load()
	w.id.Identify(bg, "tcp", src, dst)
	if w.procs.Calls.Load() == p1 || w.table.Calls.Load() == t1 {
		t.Fatal("同一个五元组第二次查，底层应仍被调用")
	}
}

func TestSnapshotShared(t *testing.T) {
	procs := map[int]Proc{7: {PID: 7, PPID: 1, Path: "/usr/bin/curl"}}
	w := newWorld(procs, []Entry{conn(7)})
	now := time.Unix(1000, 0)
	var mu sync.Mutex
	w.id.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	w.table.Hook = func(string) { time.Sleep(5 * time.Millisecond) }
	const N = 20
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := w.id.Identify(bg, "tcp", src, dst); r.App != "path:/usr/bin/curl" {
				t.Errorf("%+v", r)
			}
		}()
	}
	wg.Wait()
	// 每次识别有 1 次可共享的查找和 1 次必须新读的复核：N 个并发请求共享查找时，总次数远小于 2N。
	if n := w.table.Calls.Load(); n > N+3 {
		t.Fatalf("并发请求没有共享快照：读了 %d 次", n)
	}
	// 超过 20 毫秒后重新读。
	before := w.table.Calls.Load()
	mu.Lock()
	now = now.Add(21 * time.Millisecond)
	mu.Unlock()
	w.id.Identify(bg, "tcp", src, dst)
	if w.table.Calls.Load()-before != 2 {
		t.Fatalf("快照过期后应重新读：%d", w.table.Calls.Load()-before)
	}
}

// d. 连接表解析
func TestParsePCBSample(t *testing.T) {
	// 本机只读抓取（testdata/pcb-README.txt）：进程号改成 4242，指针改成假值。
	tb, err := os.ReadFile("testdata/pcb-tcp.bin")
	if err != nil {
		t.Fatal(err)
	}
	es, err := ParsePCBList(tb, 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 3 {
		t.Fatalf("TCP 应有 3 条（监听、客户端、服务端）：%+v", es)
	}
	if pid := match(es, "tcp", ap("127.0.0.1:60811"), ap("127.0.0.1:60810")); pid != 4242 {
		t.Fatalf("客户端连接应匹配到 4242：%d", pid)
	}
	if pid := match(es, "tcp", ap("127.0.0.1:60811"), ap("127.0.0.1:1")); pid != 0 {
		t.Fatalf("远端端口不同不应匹配：%d", pid)
	}
	ub, _ := os.ReadFile("testdata/pcb-udp.bin")
	us, err := ParsePCBList(ub, 17)
	if err != nil {
		t.Fatal(err)
	}
	// 已连接的 UDP：按远端匹配；未连接的（监听）：只按本地端口。
	if pid := match(us, "udp", ap("127.0.0.1:57671"), ap("127.0.0.1:59925")); pid != 4242 {
		t.Fatalf("已连接 UDP：%d", pid)
	}
	if pid := match(us, "udp", ap("127.0.0.1:57671"), ap("127.0.0.1:9")); pid != 0 {
		t.Fatalf("已连接 UDP 的远端不同不应匹配：%d", pid)
	}
	if pid := match(us, "udp", ap("127.0.0.1:59925"), ap("203.0.113.1:53")); pid != 4242 {
		t.Fatalf("未连接 UDP 只按本地端口：%d", pid)
	}
	if _, err := ParsePCBList(tb, 17); err == nil {
		t.Fatal("协议号不对时应报错")
	}
}

// mkPCB 构造一份连接表：每条 entry 一组 inpcb + socket + rcvbuf + sndbuf + stats（+ tcpcb）。
func mkPCB(tcp bool, es []Entry, epid map[int]int) []byte {
	le := binary.LittleEndian
	gen := func(n int) []byte {
		b := make([]byte, genLen)
		le.PutUint32(b, genLen)
		le.PutUint32(b[4:], uint32(n))
		return b
	}
	rec := func(kind uint32) []byte {
		b := make([]byte, roundup8(int(knownLen[kind])))
		le.PutUint32(b, knownLen[kind])
		le.PutUint32(b[4:], kind)
		return b
	}
	out := gen(len(es))
	for i, e := range es {
		in := rec(kindInpcb)
		le.PutUint64(in[inpPtr:], uint64(0x1000+i))
		binary.BigEndian.PutUint16(in[inpFPort:], e.Remote.Port())
		binary.BigEndian.PutUint16(in[inpLPort:], e.Local.Port())
		in[inpVFlag] = 1
		r4, l4 := e.Remote.Addr().As4(), e.Local.Addr().As4()
		copy(in[inpFAddr+12:], r4[:])
		copy(in[inpLAddr+12:], l4[:])
		so := rec(kindSocket)
		le.PutUint64(so[soPcb:], uint64(0x1000+i))
		proto := uint32(17)
		if tcp {
			proto = 6
		}
		le.PutUint32(so[soProto:], proto)
		le.PutUint32(so[soLastPID:], uint32(e.PID))
		le.PutUint32(so[soEPID:], uint32(epid[i]))
		out = append(out, in...)
		out = append(out, so...)
		out = append(out, rec(kindRcvBuf)...)
		out = append(out, rec(kindSndBuf)...)
		out = append(out, rec(kindStats)...)
		if tcp {
			out = append(out, rec(kindTcpcb)...)
		}
	}
	return append(out, gen(len(es))...)
}

func TestParsePCBConstructed(t *testing.T) {
	es := []Entry{conn(7), {Local: ap("198.18.0.1:50001"), Remote: dst, PID: 8}}
	b := mkPCB(true, es, map[int]int{1: 99})
	got, err := ParsePCBList(b, 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].PID != 7 || got[1].PID != 99 || got[0].Local != src || got[0].Remote != dst {
		t.Fatalf("%+v（委托进程号非零时应优先用它）", got)
	}
	// 记录长度与预期不符：整体报错。
	bad := append([]byte(nil), b...)
	binary.LittleEndian.PutUint32(bad[genLen:], 112)
	if _, err := ParsePCBList(bad, 6); !errors.Is(err, ErrLayout) {
		t.Fatalf("长度不符应整体报错：%v", err)
	}
	// 不认识的种类。
	bad = append([]byte(nil), b...)
	binary.LittleEndian.PutUint32(bad[genLen+4:], 0x400)
	if _, err := ParsePCBList(bad, 6); !errors.Is(err, ErrLayout) {
		t.Fatalf("不认识的种类应报错：%v", err)
	}
	// 套接字记录对不上连接记录。
	bad = append([]byte(nil), b...)
	binary.LittleEndian.PutUint64(bad[genLen+104+soPcb:], 0xdead)
	if _, err := ParsePCBList(bad, 6); !errors.Is(err, ErrLayout) {
		t.Fatalf("指针对不上应报错：%v", err)
	}
	// 截断、太短、开头不对：报错不越界。
	for _, n := range []int{0, 10, genLen, genLen + 50, len(b) - 1} {
		if _, err := ParsePCBList(b[:n], 6); err == nil {
			t.Fatalf("截断到 %d 字节应报错", n)
		}
	}
	// 空表。
	if got, err := ParsePCBList(mkPCB(false, nil, nil), 17); err != nil || len(got) != 0 {
		t.Fatalf("空表：%v %v", got, err)
	}
	// 多个进程匹配同一条流。
	if match([]Entry{conn(7), conn(8)}, "tcp", src, dst) != -1 {
		t.Fatal("多个进程应返回 -1")
	}
}

func TestParseProcArgs(t *testing.T) {
	b := binary.LittleEndian.AppendUint32(nil, 3)
	b = append(b, "/opt/homebrew/bin/node\x00\x00\x00\x00node\x00/usr/local/bin/claude\x00--flag\x00HOME=/x\x00"...)
	p, args, err := ParseProcArgs(b)
	if err != nil || p != "/opt/homebrew/bin/node" || fmt.Sprint(args) != "[node /usr/local/bin/claude --flag]" {
		t.Fatalf("%q %q %v", p, args, err)
	}
	for name, bad := range map[string][]byte{
		"空":      nil,
		"太短":     {1, 0, 0},
		"没有路径":   {1, 0, 0, 0, 0, 'x'},
		"参数个数太大": append(binary.LittleEndian.AppendUint32(nil, 1<<20), "/x\x00a\x00"...),
	} {
		if _, _, err := ParseProcArgs(bad); err == nil {
			t.Errorf("%s：应报错", name)
		}
	}
}
