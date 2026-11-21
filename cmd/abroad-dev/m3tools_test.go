package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-abroad/internal/directdial"
	"claude-abroad/internal/procinfo"
)

// redirectDialer 是直连拨号模块的“物理网卡”：不论目的，都连到本机的 to（拨出的是真实 TCP 连接，fd 才能传回来）。
type redirectDialer struct {
	to   string
	fail bool
}

func (d *redirectDialer) DialDirect(ctx context.Context, network string, _ netip.AddrPort) (net.Conn, error) {
	if d.fail {
		return nil, errors.New("注入的拨号失败")
	}
	var nd net.Dialer
	return nd.DialContext(ctx, "tcp", d.to)
}

// startDirect 起进程内真实的直连拨号模块（生产形态：没有 --dns，配置由客户端按代数补发）。
func startDirect(t *testing.T, d directdial.BoundDialer) *directdial.Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "bt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "d.sock")
	me := uint32(os.Getuid())
	srv := &directdial.Server{Dialer: d, AllowUID: func(u uint32) bool { return u == me }}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	cli := &directdial.Client{Path: path, ServerUID: me}
	cli.SetConfigSource(ownConfig)
	return cli
}

var batchTarget = netip.MustParseAddrPort("93.184.216.34:443") // 只是请求里的目标；实际连到本机的假服务器

// k. 批量模式：握手成功、握手失败、拨号失败三种计数；并发不超过设定值（上限 10）；输出里没有地址。
func TestBatchCounts(t *testing.T) {
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	ts.Config.ErrorLog = log.New(io.Discard, "", 0) // 证书校验不过那一档会让服务端打日志
	ts.StartTLS()
	defer ts.Close()
	roots := x509.NewCertPool()
	roots.AddCert(ts.Certificate())
	good := &tls.Config{ServerName: "example.com", RootCAs: roots, MinVersion: tls.VersionTLS12}
	tsAddr := netip.MustParseAddrPort(ts.Listener.Addr().String())

	// 握手成功：经真实的直连拨号模块（含 stale_config 补发）拿到 fd，再做 TLS 握手。
	cli := startDirect(t, &redirectDialer{to: tsAddr.String()})
	rep := runBatch(context.Background(), cli, batchTarget, tsAddr, 40, 20, 5*time.Second, good, nil)
	if rep.OK != 40 || rep.DialFail != 0 || rep.HandshakeFail != 0 {
		t.Fatalf("应全部成功：%+v", rep)
	}
	if rep.MaxInflight > MaxBatchConcurrency || rep.MaxInflight < 1 {
		t.Fatalf("并发应被限制在 %d 以内：%d", MaxBatchConcurrency, rep.MaxInflight)
	}
	rep = runBatch(context.Background(), cli, batchTarget, tsAddr, 10, 3, 5*time.Second, good, nil)
	if rep.OK != 10 || rep.MaxInflight > 3 {
		t.Fatalf("并发应不超过设定值 3：%+v", rep)
	}

	// 证书校验不过（服务器名不对）：握手失败。
	bad := good.Clone()
	bad.ServerName = "wrong.example"
	rep = runBatch(context.Background(), cli, batchTarget, tsAddr, 5, 2, 5*time.Second, bad, nil)
	if rep.OK != 0 || rep.HandshakeFail != 5 || rep.Errors["handshake_cert"] != 5 {
		t.Fatalf("证书校验不过应计握手失败：%+v", rep)
	}

	// 连上就被关（在途 fd 被冲掉时的表现）：握手失败（EOF）。
	plain, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	go func() {
		for {
			c, err := plain.Accept()
			if err != nil {
				return
			}
			_, _ = io.CopyN(io.Discard, c, 1)
			c.Close()
		}
	}()
	pAddr := netip.MustParseAddrPort(plain.Addr().String())
	cli2 := startDirect(t, &redirectDialer{to: pAddr.String()})
	rep = runBatch(context.Background(), cli2, batchTarget, pAddr, 5, 2, 5*time.Second, good, nil)
	// 对端读了一个字节就关：客户端看到 EOF 或 RST（取决于时序），都计握手失败。
	if rep.OK != 0 || rep.HandshakeFail != 5 || rep.Errors["handshake_eof"]+rep.Errors["handshake_reset"] != 5 {
		t.Fatalf("连接被对端关闭应计握手失败：%+v", rep)
	}

	// 拨号失败：模块回 dial_failed。
	cli3 := startDirect(t, &redirectDialer{fail: true})
	rep = runBatch(context.Background(), cli3, batchTarget, tsAddr, 5, 2, 5*time.Second, good, nil)
	if rep.OK != 0 || rep.DialFail != 5 || rep.Codes[directdial.CodeDialFailed] != 5 || rep.HandshakeFail != 0 {
		t.Fatalf("应计拨号失败：%+v", rep)
	}

	// 对端不是期望的地址：计入拨号失败。
	rep = runBatch(context.Background(), cli, batchTarget, batchTarget, 3, 1, 5*time.Second, good, nil)
	if rep.RemoteMismatch != 3 || rep.DialFail != 3 {
		t.Fatalf("对端不符应计拨号失败：%+v", rep)
	}

	// 输出里没有地址。
	b, _ := json.Marshal(rep)
	if strings.Contains(string(b), "127.0.0.1") || strings.Contains(string(b), "93.184") {
		t.Fatalf("输出里不应有地址：%s", b)
	}
}

// ident-sample：按类计数，只列 system: 名单，不输出 path: 和 Bundle ID。
func TestIdentSample(t *testing.T) {
	tb := &procinfo.FakeTable{Seq: map[string][][]procinfo.Entry{
		"tcp": {{{PID: 10}, {PID: 10}, {PID: 11}, {PID: 12}, {PID: 0}, {PID: 13}}},
		"udp": {{{PID: 14}}},
	}}
	pr := &procinfo.FakeProcs{Procs: map[int]procinfo.Proc{
		10: {PID: 10, Path: "/usr/sbin/mDNSResponder"},
		11: {PID: 11, Path: "/Users/alice/bin/tool"},
		12: {PID: 12, Path: "/Applications/Foo.app/Contents/MacOS/Foo"},
		14: {PID: 14, Path: "/usr/libexec/configd"},
	}}
	bundle := func(string) (string, error) { return "com.example.foo", nil }
	rep := identSampleRun(tb, pr, bundle)
	if rep.Entries != 7 || rep.WithPID != 6 || rep.Identified != 5 {
		t.Fatalf("计数：%+v", rep)
	}
	if rep.SystemIDs["system:mDNSResponder"] != 2 || rep.SystemIDs["system:configd"] != 1 || rep.Kinds["path"] != 1 ||
		rep.Kinds["bundle"] != 1 || rep.Kinds["unknown"] != 1 || rep.BundleIDs != 1 {
		t.Fatalf("分类：%+v", rep)
	}
	b, _ := json.Marshal(rep)
	if strings.Contains(string(b), "alice") || strings.Contains(string(b), "com.example") {
		t.Fatalf("不应输出路径和 Bundle ID：%s", b)
	}
}

// upstream 的 --pid-file：开始监听后写自己的进程号，退出时删掉。
func TestUpstreamPIDFile(t *testing.T) {
	dir := t.TempDir()
	pf := filepath.Join(dir, "up.pid")
	ctx, cancel := context.WithCancel(context.Background())
	out, errb := &syncWriter{w: &bytes.Buffer{}}, &syncWriter{w: &bytes.Buffer{}}
	done := make(chan int)
	go func() {
		done <- upstream(ctx, []string{"--pid-file", pf, "--log", filepath.Join(dir, "up.jsonl")}, out, errb)
	}()
	var got string
	for i := 0; i < 100; i++ {
		if b, err := os.ReadFile(pf); err == nil {
			got = strings.TrimSpace(string(b))
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got != itoa(os.Getpid()) {
		t.Fatalf("PID 文件内容：%q", got)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("退出码 %d：%s", code, errb.String())
	}
	if _, err := os.Stat(pf); !os.IsNotExist(err) {
		t.Fatal("退出时应删掉 PID 文件")
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
