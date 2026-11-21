package sniff

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"claude-abroad/internal/rules"
)

func wantKind(t *testing.T, name string, got Result, kind rules.SniffKind, host string) {
	t.Helper()
	if got.Kind != kind || got.Name != host {
		t.Errorf("%s：得到 %+v，期望 kind=%v name=%q", name, got, kind, host)
	}
}

func tlsFinal(t *testing.T, b []byte) Result {
	t.Helper()
	r, more := TLS(b)
	if more {
		t.Fatalf("输入完整时不应还要更多字节（%d 字节）", len(b))
	}
	return r
}

// a. TLS
func TestTLSGoClient(t *testing.T) {
	wantKind(t, "带 SNI", tlsFinal(t, goHello(t, &tls.Config{ServerName: "Claude.AI"})), rules.TLSName, "claude.ai")
	wantKind(t, "不带 SNI", tlsFinal(t, goHello(t, &tls.Config{InsecureSkipVerify: true})), rules.TLSNoName, "")
	wantKind(t, "SNI 是 IP（Go 不发）", tlsFinal(t, goHello(t, &tls.Config{ServerName: "203.0.113.1"})), rules.TLSNoName, "")
	ech := goHello(t, &tls.Config{
		ServerName:                     "secret.example",
		MinVersion:                     tls.VersionTLS13,
		EncryptedClientHelloConfigList: echConfigList(t, "public.example"),
	})
	if !bytes.Contains(ech, []byte("public.example")) {
		t.Fatal("ECH 样本的外层应带公开名（说明 ECH 确实生效）")
	}
	wantKind(t, "带 ECH（外层有 SNI）", tlsFinal(t, ech), rules.TLSECH, "")
}

func TestTLSCurlSample(t *testing.T) {
	// 本机 curl 8.7.1（LibreSSL）经回环发出的真实 ClientHello，SNI 是 example.test。
	// 里面只有随机数和密钥交换的公开部分，没有本机信息。
	wantKind(t, "curl", tlsFinal(t, readHex(t, "curl-clienthello.hex")), rules.TLSName, "example.test")
}

func TestTLSShapes(t *testing.T) {
	msg := hsMsg(hsClientHello, helloBody(sniExt("www.example.com")))
	wantKind(t, "跨 2 个记录", tlsFinal(t, records(msg, 10)), rules.TLSName, "www.example.com")
	wantKind(t, "跨 3 个记录", tlsFinal(t, records(msg, 3, 30)), rules.TLSName, "www.example.com")
	big := hsMsg(hsClientHello, helloBody(sniExt("big.example.com"), ext{0x0015, make([]byte, 6000)}))
	wantKind(t, "大于 4 KiB", tlsFinal(t, records(big, 4000)), rules.TLSName, "big.example.com")
	huge := hsMsg(hsClientHello, helloBody(sniExt("huge.example.com"), ext{0x0015, make([]byte, MaxPeek)}))
	wantKind(t, "超过 MaxPeek", tlsFinal(t, records(huge, 16000, 16000)), rules.TLSNoName, "")
	wantKind(t, "握手类型不是 ClientHello", tlsFinal(t, records(hsMsg(2, helloBody(sniExt("a.example"))))), rules.TLSNoName, "")
	wantKind(t, "SNI 不合法", tlsFinal(t, hello(sniExt("bad_-.x-"))), rules.TLSNoName, "")
	wantKind(t, "SNI 是 IP", tlsFinal(t, hello(sniExt("160.79.104.10"))), rules.TLSNoName, "")
	wantKind(t, "多个 SNI 条目", tlsFinal(t, hello(sniExt("a.example", "claude.ai"))), rules.TLSNoName, "")
	wantKind(t, "扩展重复", tlsFinal(t, hello(sniExt("a.example"), sniExt("b.example"))), rules.TLSNoName, "")
	wantKind(t, "带 ECH 扩展（构造）", tlsFinal(t, hello(sniExt("a.example"), ext{extECH, []byte{0}})), rules.TLSECH, "")
	wantKind(t, "没有扩展", tlsFinal(t, records(hsMsg(hsClientHello, helloBody()[:len(helloBody())-2]))), rules.TLSNoName, "")
	wantKind(t, "名字的类型不是 host_name", tlsFinal(t, hello(ext{extServerName, vec16(nil, append([]byte{1}, vec16(nil, []byte("a.example"))...))})), rules.TLSNoName, "")
	// 长度字段自相矛盾：各处长度改大、改小，都判 TLSNoName，不越界。
	good := hello(sniExt("www.example.com"))
	for i := 5; i < len(good); i++ {
		for _, delta := range []byte{1, 0xff} {
			b := append([]byte(nil), good...)
			b[i] += delta
			r, more := TLS(b)
			if !more && r.Kind == rules.NotTLS {
				t.Fatalf("改了第 %d 字节后判成了 NotTLS", i)
			}
		}
	}
	// 截断：每个前缀都要么还要更多字节，要么不是 NotTLS。
	for i := 1; i < len(good); i++ {
		r, more := TLS(good[:i])
		if !more || r.Kind == rules.NotTLS {
			t.Fatalf("前 %d 字节：%+v more=%v", i, r, more)
		}
	}
	// 记录中间夹了别的内容类型。
	b := append(records(msg, 10)[:15], 0x15, 0x03, 0x03, 0x00, 0x02, 0x01, 0x00)
	wantKind(t, "中间夹 alert 记录", tlsFinal(t, b), rules.TLSNoName, "")
	wantKind(t, "记录长度为 0", tlsFinal(t, []byte{0x16, 0x03, 0x01, 0, 0}), rules.TLSNoName, "")
}

// b. QUIC
func TestQUICVectors(t *testing.T) {
	wantKind(t, "RFC 9001 A.2（v1）", QUIC(readHex(t, "rfc9001-a2-client-initial.hex")), rules.QUICName, "example.com")
	wantKind(t, "RFC 9369 A.2（v2）", QUIC(readHex(t, "rfc9369-a2-client-initial.hex")), rules.QUICName, "example.com")
}

// RFC 9001 A.2 的未保护 CRYPTO 帧（不含 PADDING）。
const rfcCrypto = "060040f1010000ed0303ebf8fa56f12939b9584a3896472ec40bb863cfd3e86804fe3a47f06a2b69484c000004130113" +
	"02010000c000000010000e00000b6578616d706c652e636f6dff01000100000a00080006001d00170018001000070005" +
	"04616c706e000500050100000000003300260024001d00209370b2c9caa47fbabaf4559fedba753de171fa71f50f1ce1" +
	"5d43e994ec74d748002b0003020304000d0010000e0403050306030203080408050806002d00020101001c00024001003900320408ffffffffffffffff05048000ffff07048000ffff0801100104800075300901100f088394c8f03e51570806048000ffff"

func TestQUICSealMatchesRFC(t *testing.T) {
	// 测试用的加密函数按 RFC 的参数重新生成 A.2 的包，应与 RFC 给出的密文逐字节相同。
	fr, err := hex.DecodeString(rfcCrypto)
	if err != nil {
		t.Fatal(err)
	}
	dcid, _ := hex.DecodeString("8394c8f03e515708")
	got := sealInitial(t, quicV1, dcid, 2, fr)
	want := readHex(t, "rfc9001-a2-client-initial.hex")
	if !bytes.Equal(got, want) {
		t.Fatalf("加密结果与 RFC 不同（%d/%d 字节）", len(got), len(want))
	}
}

func TestQUICShapes(t *testing.T) {
	dcid := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	ch := hsMsg(hsClientHello, helloBody(sniExt("q.example.com")))
	for _, v := range []uint32{quicV1, quicV2} {
		wantKind(t, "完整", QUIC(sealInitial(t, v, dcid, 0, cryptoFrame(0, ch))), rules.QUICName, "q.example.com")
		// 乱序，中间夹 PADDING 和 PING。
		var fr []byte
		fr = append(fr, cryptoFrame(40, ch[40:])...)
		fr = append(fr, 0, 0, 0x01)
		fr = append(fr, cryptoFrame(0, ch[:20])...)
		fr = append(fr, 0x01)
		fr = append(fr, cryptoFrame(20, ch[20:40])...)
		wantKind(t, "乱序加 PADDING、PING", QUIC(sealInitial(t, v, dcid, 1, fr)), rules.QUICName, "q.example.com")
		// 重叠的片段也能重组。
		fr = append(cryptoFrame(0, ch[:30]), cryptoFrame(10, ch[10:])...)
		wantKind(t, "重叠", QUIC(sealInitial(t, v, dcid, 1, fr)), rules.QUICName, "q.example.com")
		// 跨数据报：只有前半部分。
		wantKind(t, "跨数据报", QUIC(sealInitial(t, v, dcid, 0, cryptoFrame(0, ch[:len(ch)/2]))), rules.QUICNoName, "")
		wantKind(t, "缺开头", QUIC(sealInitial(t, v, dcid, 0, cryptoFrame(10, ch[10:]))), rules.QUICNoName, "")
		// 认证标签不对。
		bad := sealInitial(t, v, dcid, 0, cryptoFrame(0, ch))
		bad[len(bad)-1] ^= 1
		wantKind(t, "认证标签不对", QUIC(bad), rules.QUICNoName, "")
		// 不认识的帧。
		wantKind(t, "不认识的帧", QUIC(sealInitial(t, v, dcid, 0, append([]byte{0x1e}, cryptoFrame(0, ch)...))), rules.QUICNoName, "")
		// 带 ECH、没有 SNI。
		wantKind(t, "带 ECH", QUIC(sealInitial(t, v, dcid, 0, cryptoFrame(0, hsMsg(1, helloBody(sniExt("q.example.com"), ext{extECH, []byte{0}}))))), rules.QUICNoName, "")
		wantKind(t, "没有 SNI", QUIC(sealInitial(t, v, dcid, 0, cryptoFrame(0, hsMsg(1, helloBody(ext{0x000a, []byte{0, 0}}))))), rules.QUICNoName, "")
	}
	// 版本不认识，但符合形状。
	unk := sealInitial(t, quicV1, dcid, 0, cryptoFrame(0, ch))
	copy(unk[1:5], []byte{0xff, 0, 0, 0x1d})
	wantKind(t, "版本不认识", QUIC(unk), rules.QUICNoName, "")
	// 短于 1200 字节的长头包、短头包、随机数据：NotTLS。
	full := sealInitial(t, quicV1, dcid, 0, cryptoFrame(0, ch))
	wantKind(t, "短于 1200 的长头包", QUIC(full[:1199]), rules.NotTLS, "")
	short := append([]byte(nil), full...)
	short[0] = 0x43
	wantKind(t, "短头包", QUIC(short), rules.NotTLS, "")
	rnd := bytes.Repeat([]byte{0x2a, 0x91, 0x07}, 500)
	wantKind(t, "随机数据", QUIC(rnd), rules.NotTLS, "")
	wantKind(t, "空数据报", QUIC(nil), rules.NotTLS, "")
	wantKind(t, "像 TLS 记录头的 UDP", QUIC([]byte{0x16, 0x03, 0x01, 0, 5}), rules.QUICNoName, "")
}

// c. HTTP
func TestHTTP(t *testing.T) {
	ok := func(name, req, host string) {
		t.Helper()
		r, more := HTTP([]byte(req))
		if more {
			t.Fatalf("%s：不应还要更多", name)
		}
		wantKind(t, name, r, rules.HTTPHost, host)
	}
	ok("GET", "GET / HTTP/1.1\r\nHost: WWW.Example.COM\r\n\r\n", "www.example.com")
	ok("POST 带端口", "POST /x HTTP/1.1\r\nUser-Agent: t\r\nhost: api.example.com:8080\r\n\r\nbody", "api.example.com")
	ok("CONNECT", "CONNECT claude.ai:443 HTTP/1.1\r\nHost: other.example\r\n\r\n", "claude.ai")
	ok("只用 LF", "HEAD / HTTP/1.0\nHost: a.example\n\n", "a.example")
	not := func(name, req string) {
		t.Helper()
		r, more := HTTP([]byte(req))
		if more || r.Kind != rules.NotTLS {
			t.Errorf("%s：得到 %+v more=%v，期望 NotTLS", name, r, more)
		}
	}
	not("没有 Host", "GET / HTTP/1.1\r\nUser-Agent: x\r\n\r\n")
	not("方法不在白名单", "FOO / HTTP/1.1\r\nHost: a.example\r\n\r\n")
	not("头部超长", "GET / HTTP/1.1\r\nX: "+strings.Repeat("a", MaxHTTPHeader)+"\r\nHost: a.example\r\n\r\n")
	not("头部超长且没收完", "GET / HTTP/1.1\r\nX: "+strings.Repeat("a", MaxHTTPHeader))
	not("多个 Host", "GET / HTTP/1.1\r\nHost: a.example\r\nHost: claude.ai\r\n\r\n")
	not("Host 是 IP", "GET / HTTP/1.1\r\nHost: 160.79.104.10\r\n\r\n")
	not("Host 不合法", "GET / HTTP/1.1\r\nHost: bad_-.x-\r\n\r\n")
	not("请求行不对", "GET /\r\nHost: a.example\r\n\r\n")
	if _, more := HTTP([]byte("GET / HTTP/1.1\r\nHost: a.exa")); !more {
		t.Error("头部没收完时应还要更多")
	}
	if _, more := HTTP([]byte("GE")); !more {
		t.Error("方法的前缀应还要更多")
	}
}

// d. PeekTCP
type sender struct {
	c      net.Conn
	sent   chan []byte
	cancel chan struct{}
}

// pipe 返回服务端的连接（被测的一端）和客户端；send 在后台按节奏写，结束时把写出的全部字节发到 sent。
func pipe(t *testing.T, data []byte, every time.Duration, chunk int) (net.Conn, *sender) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	s := &sender{c: b, sent: make(chan []byte, 1), cancel: make(chan struct{})}
	go func() {
		var out []byte
		for len(data) > 0 {
			n := min(chunk, len(data))
			if _, err := b.Write(data[:n]); err != nil {
				break
			}
			out = append(out, data[:n]...)
			data = data[n:]
			if every > 0 {
				select {
				case <-time.After(every):
				case <-s.cancel:
					s.sent <- out
					return
				}
			}
		}
		s.sent <- out
	}()
	return a, s
}

func TestPeekTCPServerFirst(t *testing.T) {
	a, _ := pipe(t, nil, 0, 1)
	start := time.Now()
	r, got, err := PeekTCP(context.Background(), a)
	d := time.Since(start)
	if err != nil || r.Kind != rules.Timeout || len(got) != 0 {
		t.Fatalf("%+v %q %v", r, got, err)
	}
	if d < 250*time.Millisecond || d > 600*time.Millisecond {
		t.Fatalf("应在约 300 毫秒返回，实际 %v", d)
	}
}

func TestPeekTCPNoResidualDeadline(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		_, _ = b.Write([]byte("XYZ-own-protocol"))
		time.Sleep(450 * time.Millisecond)
		_, _ = b.Write([]byte("later"))
	}()
	r, got, err := PeekTCP(context.Background(), a)
	if err != nil || r.Kind != rules.NotTLS || string(got) != "XYZ-own-protocol" {
		t.Fatalf("%+v %q %v", r, got, err)
	}
	buf := make([]byte, 16)
	n, err := a.Read(buf)
	if err != nil || string(buf[:n]) != "later" {
		t.Fatalf("返回后应没有残留的读期限：%q %v", buf[:n], err)
	}
}

func TestPeekTCPWholeHello(t *testing.T) {
	ch := hello(sniExt("fast.example"))
	a, s := pipe(t, ch, 0, len(ch))
	start := time.Now()
	r, got, err := PeekTCP(context.Background(), a)
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("完整 ClientHello 应立即返回，用了 %v", d)
	}
	wantKind(t, "完整", r, rules.TLSName, "fast.example")
	if err != nil || !bytes.Equal(got, ch) {
		t.Fatalf("已读字节应与发送的一致：%v", err)
	}
	<-s.sent
}

func TestPeekTCPSlowHello(t *testing.T) {
	ch := hello(sniExt("slow.example"))
	a, s := pipe(t, ch, 10*time.Millisecond, 1)
	r, got, err := PeekTCP(context.Background(), a)
	if err != nil || r.Kind != rules.TLSNoName {
		t.Fatalf("%+v %v", r, err)
	}
	// 之后继续读：前缀加后续正好是全部发送的字节，不丢不重。
	rest := make(chan []byte)
	go func() { b, _ := io.ReadAll(a); rest <- b }()
	sent := <-s.sent
	s.c.Close()
	all := append(append([]byte(nil), got...), <-rest...)
	if !bytes.Equal(all, ch) || !bytes.Equal(sent, ch) {
		t.Fatalf("字节丢了或重了：已读 %d + 后续 = %d，发送 %d", len(got), len(all), len(ch))
	}
}

func TestPeekTCPOneByte(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() { _, _ = b.Write([]byte{0x16}) }()
	r, got, err := PeekTCP(context.Background(), a)
	if err != nil || r.Kind != rules.TLSNoName || !bytes.Equal(got, []byte{0x16}) {
		t.Fatalf("%+v %v %v", r, got, err)
	}
}

func TestPeekTCPOwnProtocolImmediate(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() { _, _ = b.Write([]byte{0x00, 0x00, 0x00, 0x10, 'w', 'x'}) }()
	start := time.Now()
	r, _, err := PeekTCP(context.Background(), a)
	if err != nil || r.Kind != rules.NotTLS || time.Since(start) > 50*time.Millisecond {
		t.Fatalf("%+v %v %v", r, err, time.Since(start))
	}
}

func TestPeekTCPEOF(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	go func() { _, _ = b.Write([]byte{0x16, 0x03}); b.Close() }()
	r, _, err := PeekTCP(context.Background(), a)
	if err != nil || r.Kind != rules.TLSNoName {
		t.Fatalf("对端关闭时按已读内容判：%+v %v", r, err)
	}
}

func TestPeekTCPCancel(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	start := time.Now()
	_, _, err := PeekTCP(ctx, a)
	if err == nil || time.Since(start) > 150*time.Millisecond {
		t.Fatalf("ctx 取消应及时返回：%v %v", err, time.Since(start))
	}
	// 取消后同样没有残留的读期限。
	go func() { time.Sleep(50 * time.Millisecond); _, _ = b.Write([]byte("z")) }()
	buf := make([]byte, 1)
	if _, err := a.Read(buf); err != nil {
		t.Fatalf("取消后应没有残留的读期限：%v", err)
	}
}

func TestPeekTCPLoopback(t *testing.T) {
	// 真实的回环 TCP 连接（只在本机回环上）：一次到达的 ClientHello。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("不能在回环上监听：", err)
	}
	defer ln.Close()
	ch := hello(sniExt("loop.example"))
	go func() {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err == nil {
			_, _ = c.Write(ch)
			time.Sleep(500 * time.Millisecond)
			c.Close()
		}
	}()
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, got, err := PeekTCP(context.Background(), c)
	if err != nil || !bytes.Equal(got, ch) {
		t.Fatal(err)
	}
	wantKind(t, "回环", r, rules.TLSName, "loop.example")
}

// e. 偏严的不变量（表驱动部分；模糊测试里再交叉核对）。
func TestNotTLSOnlyWhenClearlyNot(t *testing.T) {
	inputs := [][]byte{
		{0x16}, {0x16, 0x03}, {0x16, 0x03, 0x01}, {0x16, 0x03, 0x01, 0x00}, {0x14, 0x03, 0x03, 0, 1, 1},
		{0x16, 0x04}, {0x16, 0x03, 0x05}, []byte("GET"), []byte("XYZ"), {0xc3, 0, 0, 0, 1},
	}
	for _, b := range inputs {
		if r := Final(b); r.Kind == rules.NotTLS && MayBeTLS(b) {
			t.Errorf("%x：TLS 记录头的前缀被判成了 NotTLS", b)
		}
	}
	if Final([]byte{0x16, 0x04}).Kind != rules.NotTLS {
		t.Error("0x16 0x04 不是 TLS 记录头的前缀，应是 NotTLS")
	}
}
