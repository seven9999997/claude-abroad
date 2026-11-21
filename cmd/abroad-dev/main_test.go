package main

import (
	"bytes"
	"claude-abroad/internal/daemon"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"claude-abroad/internal/agent"
	"claude-abroad/internal/devupstream"
	"claude-abroad/internal/mihomocfg"
	"claude-abroad/internal/usernet"
	"claude-abroad/internal/wire"
)

// fakeData 在一个 unix socket 上起 wire 服务端：按帧头决定回应。
func fakeData(t *testing.T) string {
	dir, err := os.MkdirTemp("", "adp") // 短路径：unix socket 有长度上限
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				sess, err := wire.Server(c, wire.DefaultSessionConfig())
				if err != nil {
					return
				}
				for {
					st, err := sess.AcceptStream()
					if err != nil {
						return
					}
					go func() {
						defer st.Close()
						h, err := wire.ReadHeader(st)
						if err != nil {
							return
						}
						if h.Exit != "main" {
							wire.WriteReply(st, wire.Reply{Code: wire.CodeUnknownExit})
							return
						}
						wire.WriteReply(st, wire.Reply{OK: true})
						if h.Net == wire.UDP {
							dc := wire.NewDatagramConn(st)
							buf := make([]byte, wire.MaxDatagram)
							n, _ := dc.ReadDatagram(buf)
							dc.WriteDatagram(append([]byte("re:"), buf[:n]...))
							return
						}
						buf := make([]byte, 3)
						io.ReadFull(st, buf)
						st.Write(append([]byte("echo:"), buf...))
					}()
				}
			}()
		}
	}()
	return p
}

func runProbe(t *testing.T, args ...string) (int, ProbeResult, string) {
	var out, errb bytes.Buffer
	code := probe(args, &out, &errb)
	var r ProbeResult
	if out.Len() > 0 {
		if err := json.Unmarshal(out.Bytes(), &r); err != nil {
			t.Fatalf("输出不是 JSON：%q", out.String())
		}
	}
	return code, r, errb.String()
}

func TestProbeTCP(t *testing.T) {
	sock := fakeData(t)
	code, r, _ := runProbe(t, "--data-sock", sock, "--exit", "main", "--net", "tcp", "--target", "127.0.0.1:1090", "--send-hex", "050102")
	if code != 0 || !r.OK || r.RecvHex != "6563686f3a050102" || !r.EOF {
		t.Fatalf("TCP：%d %+v", code, r)
	}
}

func TestProbeElapsed(t *testing.T) {
	sock := fakeData(t)
	// 假服务读满 3 字节才回；只发 1 字节时读不到应答，直到 read-timeout（200ms）。
	code, r, _ := runProbe(t, "--data-sock", sock, "--target", "127.0.0.1:1", "--send-hex", "01", "--read-timeout", "200ms")
	if code != 0 || !r.Timeout || r.ElapsedMs < 150 || r.ElapsedMs > 2000 {
		t.Fatalf("elapsed_ms 应约等于读超时：%d %+v", code, r)
	}
}

func TestProbeRejected(t *testing.T) {
	sock := fakeData(t)
	code, r, _ := runProbe(t, "--data-sock", sock, "--exit", "nope", "--net", "tcp", "--target", "example.com:443")
	if code != 0 || r.OK || r.Code != "unknown_exit" {
		t.Fatalf("被拒时应原样打出 code，退出码 0：%d %+v", code, r)
	}
}

func TestProbeUDP(t *testing.T) {
	sock := fakeData(t)
	code, r, _ := runProbe(t, "--data-sock", sock, "--exit", "main", "--net", "udp", "--target", "[::ffff:127.0.0.1]:1053", "--send-hex", "abcd")
	if code != 0 || !r.OK || r.RecvHex != "72653aabcd" {
		t.Fatalf("UDP：%d %+v", code, r)
	}
}

func TestProbeErrors(t *testing.T) {
	if code, _, _ := runProbe(t, "--data-sock", filepath.Join(t.TempDir(), "missing.sock"), "--target", "127.0.0.1:1"); code != 1 {
		t.Errorf("data.sock 不存在应退出 1：%d", code)
	}
	for name, args := range map[string][]string{
		"没有 data-sock": {"--target", "127.0.0.1:1"},
		"target 不对":    {"--data-sock", "x", "--target", "nope"},
		"net 不对":       {"--data-sock", "x", "--net", "icmp", "--target", "127.0.0.1:1"},
		"hex 不对":       {"--data-sock", "x", "--target", "127.0.0.1:1", "--send-hex", "zz"},
		"未知参数":         {"--bogus"},
	} {
		if code, _, _ := runProbe(t, args...); code != 2 {
			t.Errorf("%s：应退出 2，实际 %d", name, code)
		}
	}
}

func TestSocksSubcommand(t *testing.T) {
	sock := fakeData(t)
	ctx, cancel := context.WithCancel(context.Background())
	var out, errb bytes.Buffer
	sw := &syncWriter{w: &out}
	done := make(chan int, 1)
	go func() {
		done <- socks(ctx, []string{"--data-sock", sock, "--listen", "127.0.0.1:0"}, sw, &errb)
	}()
	var addr string
	for i := 0; i < 100 && addr == ""; i++ {
		time.Sleep(10 * time.Millisecond)
		if s := sw.String(); strings.HasPrefix(s, "listening ") {
			addr = strings.TrimSpace(strings.TrimPrefix(s, "listening "))
		}
	}
	if addr == "" {
		t.Fatal("没有打出监听地址")
	}
	// 经 SOCKS 连到假数据面：它回显前 3 字节，前面加 "echo:"。
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write([]byte{5, 1, 0})
	io.ReadFull(c, make([]byte, 2))
	c.Write([]byte{5, 1, 0, 3, 7, 'e', 'x', '.', 't', 'e', 's', 't', 0, 80})
	rep := make([]byte, 10)
	io.ReadFull(c, rep)
	c.Write([]byte("abc"))
	got, _ := io.ReadAll(c)
	c.Close()
	if rep[1] != 0 || string(got) != "echo:abc" {
		t.Fatalf("经 socks 子命令往返：rep=%d %q", rep[1], got)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("退出码 %d：%s", code, errb.String())
	}
	var out2 bytes.Buffer
	if code := socks(context.Background(), []string{"--data-sock", sock, "--listen", "0.0.0.0:0"}, &out2, &errb); code != 2 {
		t.Fatal("非回环监听地址应拒绝")
	}
}

type syncWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func (s *syncWriter) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.String()
}

// fakeCtl 在短路径 unix socket 上起一个假的控制面，记录收到的 PUT 正文。
func fakeCtl(t *testing.T) (string, func() [][]byte) {
	dir, err := os.MkdirTemp("", "adc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p := filepath.Join(dir, "c.sock")
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var bodies [][]byte
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.WriteHeader(200)
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return p, func() [][]byte { mu.Lock(); defer mu.Unlock(); return bodies }
}

func writeNodes(t *testing.T, server string) string {
	p := filepath.Join(t.TempDir(), "nodes.yaml")
	os.WriteFile(p, []byte("proxies:\n - {name: exit, type: socks5, server: "+server+", port: 17890, udp: true}\n"), 0o600)
	return p
}

func TestApplyMainProxy(t *testing.T) {
	nodes := writeNodes(t, "127.0.0.1")
	var out, errb bytes.Buffer
	if code := applyMainProxy(context.Background(), []string{"--nodes", nodes, "--selected", "exit", "--print-allow"}, &out, &errb); code != 0 {
		t.Fatalf("print-allow：%d %s", code, errb.String())
	}
	if out.String() != "127.0.0.1:17890\n" {
		t.Fatalf("白名单：%q", out.String())
	}
	sock, bodies := fakeCtl(t)
	out.Reset()
	if code := applyMainProxy(context.Background(), []string{"--nodes", nodes, "--selected", "exit", "--ctl-sock", sock}, &out, &errb); code != 0 {
		t.Fatalf("下发：%d %s", code, errb.String())
	}
	got := bodies()
	if len(got) != 1 || mihomocfg.Validate(got[0]) != nil || !strings.Contains(string(got[0]), "192.168.127.254") ||
		strings.Contains(string(got[0]), "server: 127.0.0.1") {
		t.Fatalf("下发的配置应通过校验且第一跳改写为 192.168.127.254：%q", got)
	}
	if strings.Contains(out.String()+errb.String(), "proxies") {
		t.Fatal("输出里不应有 YAML 正文")
	}
	if !strings.Contains(out.String(), "applied udp=false") || !strings.Contains(string(got[0]), "udp: false") {
		t.Fatalf("回环第一跳默认不转 UDP：%q", out.String())
	}
	out.Reset()
	if code := applyMainProxy(context.Background(), []string{"--nodes", nodes, "--selected", "exit", "--loopback-udp", "--ctl-sock", sock}, &out, &errb); code != 0 {
		t.Fatalf("--loopback-udp 下发：%d %s", code, errb.String())
	}
	if got := bodies(); len(got) != 2 || !strings.Contains(out.String(), "applied udp=true") || !strings.Contains(string(got[1]), "udp: true") || mihomocfg.Validate(got[1]) != nil {
		t.Fatalf("--loopback-udp 应转 UDP：%q", out.String())
	}

	// localhost：报错、提示 127.0.0.1、不发请求。
	bad := writeNodes(t, "localhost")
	errb.Reset()
	if code := applyMainProxy(context.Background(), []string{"--nodes", bad, "--selected", "exit", "--ctl-sock", sock}, &out, &errb); code != 1 {
		t.Fatalf("localhost 应退出 1：%d", code)
	}
	if !strings.Contains(errb.String(), "127.0.0.1") || len(bodies()) != 2 {
		t.Fatalf("应提示改写且不发请求：%s，请求数 %d", errb.String(), len(bodies()))
	}
	if code := applyMainProxy(context.Background(), []string{"--nodes", nodes, "--selected", "exit"}, &out, &errb); code != 2 {
		t.Fatal("缺 --print-allow / --ctl-sock 应退出 2")
	}
	if code := applyMainProxy(context.Background(), []string{"--nodes", nodes, "--selected", "exit", "--print-allow", "--print-first-hop"}, &out, &errb); code != 2 {
		t.Fatal("--print-allow 和 --print-first-hop 同时给应退出 2")
	}
	// 回环第一跳：--print-first-hop 没有输出（走白名单）。
	out.Reset()
	if code := applyMainProxy(context.Background(), []string{"--nodes", nodes, "--selected", "exit", "--print-first-hop"}, &out, &errb); code != 0 || out.String() != "" {
		t.Fatalf("回环第一跳不应有 --first-hop：%d %q", code, out.String())
	}
	// 域名第一跳：打印 host:port，并能被 abroad-vm 原样接受。
	dom := writeNodes(t, "Node.Example.com")
	out.Reset()
	if code := applyMainProxy(context.Background(), []string{"--nodes", dom, "--selected", "exit", "--print-first-hop"}, &out, &errb); code != 0 || out.String() != "node.example.com:17890\n" {
		t.Fatalf("--print-first-hop：%d %q %s", code, out.String(), errb.String())
	}
	if _, err := usernet.ParseFirstHops([]string{strings.TrimSpace(out.String())}); err != nil {
		t.Fatalf("应能被 ParseFirstHops 接受：%v", err)
	}
}

func TestUpstreamListenMustBeLoopback(t *testing.T) {
	var out, errb bytes.Buffer
	if code := upstream(context.Background(), []string{"--listen", "0.0.0.0:0"}, &out, &errb); code != 2 {
		t.Fatalf("非回环应退出 2：%d", code)
	}
}

type upExits struct{}

func (upExits) Lookup(tag string) (bool, bool) { return tag == "main", true }

// realData 在临时 unix socket 上起真实的 agent.DataPlane，后面接 devupstream。
func realData(t *testing.T) string {
	dir, err := os.MkdirTemp("", "adt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	ul, uu, err := devupstream.Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	upDone, dpDone := make(chan struct{}), make(chan struct{})
	go func() { (&devupstream.Server{}).Serve(ctx, ul, uu); close(upDone) }()
	dp := &agent.DataPlane{Exits: upExits{},
		Timing: agent.DataTiming{DialTimeout: 5 * time.Second, UDPIdle: 5 * time.Second},
		Socks: func(e string) agent.SocksDialer {
			return agent.SocksDialer{Addr: ul.Addr().String(), User: e, Password: "0123456789abcdef0123456789abcdef"}
		}}
	go func() { dp.Serve(ctx, l); close(dpDone) }()
	t.Cleanup(func() { cancel(); l.Close(); <-dpDone; <-upDone })
	return p
}

func TestTunsimCmd(t *testing.T) {
	var out, errb bytes.Buffer
	for _, args := range [][]string{{}, {"--data-sock"}, {"--data-sock", "x", "--udp-port", "0"}, {"--data-sock", "x", "extra"},
		{"--data-sock", "x", "--udp-target", "198.51.100.7"}, {"--data-sock", "x", "--udp-target", "198.51.100.7:0"},
		{"--data-sock", "x", "--udp-target", "[2001:db8::1]:53"}, {"--data-sock", "x", "--udp-target", "198.18.1.5:53"},
		{"--data-sock", "x", "--udp-target", "127.0.0.1:53"}, {"--data-sock", "x", "--udp-target", "0.1.2.3:53"},
		{"--data-sock", "x", "--udp-target", "169.254.1.1:53"}, {"--data-sock", "x", "--udp-target", "224.0.0.251:5353"},
		{"--data-sock", "x", "--udp-target", "255.255.255.255:67"},
		{"--data-sock", "x", "--doh-name", "a..b"}, {"--data-sock", "x", "--doh-name", strings.Repeat("a", 254)},
		{"--data-sock", "x", "--doh-name", "bad name.com"}, {"--data-sock", "x", "--doh-name", "."}} {
		if code := tunsimCmd(context.Background(), args, &out, &errb); code != 2 {
			t.Errorf("%v 应退出 2：%d", args, code)
		}
	}
	errb.Reset()
	if code := tunsimCmd(context.Background(), []string{"--data-sock", filepath.Join(t.TempDir(), "missing.sock")}, &out, &errb); code != 1 ||
		!strings.Contains(errb.String(), "data.sock 不存在") {
		t.Fatalf("data.sock 不存在应退出 1 并给出可读错误：%d %q", code, errb.String())
	}
	sock := realData(t)
	out.Reset()
	errb.Reset()
	if code := tunsimCmd(context.Background(), []string{"--data-sock", sock, "--timeout", "10s"}, &out, &errb); code != 0 {
		t.Fatalf("应全部通过：%d %s %s", code, out.String(), errb.String())
	}
	line := strings.TrimSpace(out.String())
	var rep map[string]any
	if strings.Count(line, "\n") != 0 || json.Unmarshal([]byte(line), &rep) != nil || rep["ok"] != true {
		t.Fatalf("应输出一行 JSON 且 ok：%q", line)
	}
	if strings.Contains(line, "127.0.0.1") || strings.Contains(line, sock) {
		t.Fatalf("输出里不应有地址或路径：%q", line)
	}
}

func TestPFRender(t *testing.T) {
	var out, errb bytes.Buffer
	if code := pfRender([]string{"--utun", "utun9", "--lan", "192.168.1.0/24", "--lan", "160.79.104.0/23", "--vmif", "bridge100"}, &out, &errb); code != 0 {
		t.Fatalf("应成功：%d %s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "user nobody") || !strings.Contains(s, "on utun9 all") || !strings.Contains(s, "192.168.1.0/24") ||
		strings.Contains(s, "160.79.104.0") || !strings.HasSuffix(s, "block drop out quick all\n") {
		t.Fatalf("规则不对：\n%s", s)
	}
	for _, args := range [][]string{{"--lan", "x"}, {"--utun", "utun 9"}, {"--user", ""}, {"extra"}} {
		if code := pfRender(args, &out, &errb); code != 2 {
			t.Errorf("%v 应退出 2", args)
		}
	}
}

// M2-9c：--save-store 保存的主代理存储，abroadd 的 MainProxyFirstHop 能读出第一跳。
func TestApplyMainProxySaveStore(t *testing.T) {
	dir := t.TempDir()
	nodes := filepath.Join(dir, "nodes.yaml")
	if err := os.WriteFile(nodes, []byte("proxies:\n - {name: remote, type: socks5, server: node.example.com, port: 1080}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(dir, "store")
	var out, errb bytes.Buffer
	if rc := applyMainProxy(context.Background(), []string{"--nodes", nodes, "--selected", "remote", "--save-store", store}, &out, &errb); rc != 0 {
		t.Fatalf("%d %s", rc, errb.String())
	}
	host, err := daemon.MainProxyFirstHop(store)()
	if err != nil || host != "node.example.com" {
		t.Fatalf("%q %v", host, err)
	}
}
