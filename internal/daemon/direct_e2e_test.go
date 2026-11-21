package daemon

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"claude-abroad/internal/directdial"
)

// echoDialer 是直连拨号模块的“物理网卡”：不论目的，都连到本机的回显服务（拨出的是真实的 TCP 连接，
// 服务端才能把 fd 传回来）。
type echoDialer struct {
	addr string
	mu   sync.Mutex
	n    int
}

func (d *echoDialer) DialDirect(ctx context.Context, network string, _ netip.AddrPort) (net.Conn, error) {
	if network != "tcp" {
		return nil, errors.New("测试只支持 tcp")
	}
	d.mu.Lock()
	d.n++
	d.mu.Unlock()
	var nd net.Dialer
	return nd.DialContext(ctx, "tcp", d.addr)
}

func (d *echoDialer) count() int { d.mu.Lock(); defer d.mu.Unlock(); return d.n }

// directModule 是进程内真实的直连拨号模块（生产形态：没有 --dns，等 abroadd 下发），可以停掉、重新拉起。
type directModule struct {
	t    *testing.T
	path string
	dial *echoDialer
	srv  *directdial.Server
	stop func()
}

func (m *directModule) start() {
	m.t.Helper()
	_ = os.Remove(m.path)
	me := uint32(os.Getuid())
	srv := &directdial.Server{Dialer: m.dial, AllowUID: func(u uint32) bool { return u == me }}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: m.path, Net: "unix"})
	if err != nil {
		m.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, ln); close(done) }()
	var once sync.Once
	m.srv, m.stop = srv, func() { once.Do(func() { cancel(); <-done }) }
	m.t.Cleanup(m.stop)
}

// 6. 端到端（m3-plan v2.9）：daemon + 真实的直连拨号模块。模块重启后直连自动补发配置；模块连不上时
// 改规则仍然成功；它恢复后第一条直连用新配置（新的置顶追加名单在模块一侧生效）。
func TestDirectE2EGenerations(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); c.Close() }()
		}
	}()
	dir, err := os.MkdirTemp("", "dde")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	m := &directModule{t: t, path: filepath.Join(dir, "direct.sock"), dial: &echoDialer{addr: echo.Addr().String()}}
	m.start()
	cli := &directdial.Client{Path: m.path, ServerUID: uint32(os.Getuid())}
	e := newM3DaemonWith(t, m3Rules, cli)
	pub := netip.MustParseAddrPort("93.184.216.34:443") // 应用 C 按 IP 直连（不经 fake-IP，不需要解析）

	// 应用 C 直连成功；模块收下的配置是当前代数。
	if err := e.echo(t, 22001, pub); err != nil {
		t.Fatalf("应用 C 直连：%v", err)
	}
	if m.dial.count() != 1 {
		t.Fatalf("应经直连拨号模块拨一次：%d", m.dial.count())
	}
	if s := status(t, e.d); s.Direct == nil || !s.Direct.Attached || !s.Direct.Configured {
		t.Fatalf("status 应显示直连拨号模块已收下当前配置：%+v", s.Direct)
	}
	// 模块重启：下一条直连自动补发，只多一次往返。
	m.stop()
	m.start()
	if err := e.echo(t, 22002, pub); err != nil {
		t.Fatalf("模块重启后的直连：%v", err)
	}
	if st := m.srv.Stats(); st["stale_config"] != 1 || st["config"] != 1 || st["ok"] != 2 {
		t.Fatalf("重启后应补发一次：%v", st)
	}
	// 模块连不上：改规则（加置顶追加名单）仍然成功。
	m.stop()
	gen0 := e.dp.directConfig().Gen
	withPin := m3Rules + "pinned_extra: [pin.example]\n"
	if code, msg := putRules(t, e.d, withPin, false); code != 200 {
		t.Fatalf("模块连不上时改规则应成功：%d %s", code, msg)
	}
	if g := e.dp.directConfig().Gen; g != gen0+1 {
		t.Fatalf("置顶追加名单变了代数应加一：%d → %d", gen0, g)
	}
	if s := status(t, e.d); s.Direct == nil || s.Direct.Configured {
		t.Fatalf("模块还没收下新配置：%+v", s.Direct)
	}
	if err := e.echo(t, 22003, pub); err == nil {
		t.Fatal("模块连不上时直连应失败")
	}
	// 模块恢复：第一条直连补发新配置（代数、名单都是新的），之后模块一侧拒绝新的置顶域名。
	m.start()
	if err := e.echo(t, 22004, pub); err != nil {
		t.Fatalf("模块恢复后的直连：%v", err)
	}
	if st := m.srv.Stats(); st["stale_config"] != 1 || st["config"] != 1 {
		t.Fatalf("恢复后应补发一次：%v", st)
	}
	if g, ok := cli.ConfiguredGen(); !ok || g != gen0+1 {
		t.Fatalf("模块应收下新代数：%d %v", g, ok)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cli.Dial(ctx, "tcp", "pin.example", netip.Addr{}, 443); !errors.Is(err, directdial.ErrPinned) {
		t.Fatalf("模块一侧应已按新的置顶追加名单拒绝：%v", err)
	}
	if s := status(t, e.d); !s.Direct.Configured {
		t.Fatal("status 应显示已收下当前配置")
	}
	if strings.Contains(e.logBuf.String(), "93.184.216.34") {
		t.Fatal("日志里不应有目的地址")
	}
}

// 建议改 3：替换网络栈的规则失败、而规则文件又没能写回旧内容时，记事件，错误里提示重新提交。
func TestRulesRestoreFileFails(t *testing.T) {
	e := newM3Daemon(t, m3Rules)
	writes := 0
	e.d.writeRules = func(p string, b []byte) error {
		writes++
		if writes == 1 {
			return writeFileAtomic(p, b)
		}
		return errors.New("注入的写回失败")
	}
	e.dp.syncFail = func() error { return errors.New("注入的同步失败") }
	other := strings.Replace(m3Rules, "*.corp.example", "*.intra.example", 1)
	code, msg := putRules(t, e.d, other, false)
	e.dp.syncFail = nil
	e.d.writeRules = nil
	if code != 500 || !strings.Contains(msg, "请重新提交一次规则") {
		t.Fatalf("应回错误并提示重新提交：%d %s", code, msg)
	}
	found := false
	for _, ev := range e.d.events.List() {
		found = found || ev.Kind == "rules_file_restore_failed"
	}
	if !found {
		t.Fatal("应记 rules_file_restore_failed 事件")
	}
	// 内存里仍是旧规则；重新提交后一致。
	want, _ := mustParseRules(t, m3Rules).Marshal()
	if _, b := call(t, e.d, "GET", "/v1/rules", nil); string(b) != string(want) {
		t.Fatal("内存应仍是旧规则")
	}
	if code, _ := putRules(t, e.d, m3Rules, false); code != 200 {
		t.Fatal("重新提交应成功")
	}
	if fb, _ := os.ReadFile(filepath.Join(e.dir, RulesFile)); string(fb) != string(want) {
		t.Fatal("重新提交后文件应与内存一致")
	}
}
