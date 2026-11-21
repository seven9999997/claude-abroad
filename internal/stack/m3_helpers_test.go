package stack

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"claude-abroad/internal/directdial"
	"claude-abroad/internal/fakedns"
	"claude-abroad/internal/rules"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

// 按源端口段区分应用（m3-plan M3-5 的测试约定）。
const (
	portA      = 20000 // com.app.a：proxy:wg-a
	portB      = 21000 // com.app.b：proxy:clash-b
	portC      = 22000 // com.app.c：direct
	portClaude = 23000 // com.app.c，但识别为 Claude 进程
	portOther  = 24000 // 没有规则的应用
)

// fakeIdent 按源端口段给出应用 ID，记录调用次数；delay 非零时每次识别先等一会儿。
type fakeIdent struct {
	calls atomic.Int64
	delay time.Duration
}

func (f *fakeIdent) Identify(_ context.Context, _ string, src, _ netip.AddrPort) (rules.AppID, bool) {
	f.calls.Add(1)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	switch p := src.Port(); {
	case p >= portA && p < portA+1000:
		return "com.app.a", false
	case p >= portB && p < portB+1000:
		return "com.app.b", false
	case p >= portC && p < portC+1000:
		return "com.app.c", false
	case p >= portClaude && p < portClaude+1000:
		return "com.app.c", true
	}
	return "com.app.other", false
}

// fakeDirect 是假的直连拨号模块：记录请求，按需回 pinned，其余返回一个回显的连接。
type fakeDirect struct {
	mu     sync.Mutex
	calls  []string // "tcp host:port" 或 "udp ip:port"
	pinned map[string]bool
	fail   bool
}

func (f *fakeDirect) Dial(ctx context.Context, network, host string, ip netip.Addr, port uint16) (net.Conn, error) {
	target := host
	if host == "" {
		target = ip.String()
	}
	f.mu.Lock()
	f.calls = append(f.calls, network+" "+target+":"+itoa(int(port)))
	pinned, fail := f.pinned[target], f.fail
	f.mu.Unlock()
	if pinned {
		return nil, directdial.ErrPinned
	}
	if fail {
		return nil, directdial.ErrDialFailed
	}
	a, b := net.Pipe()
	if network == "udp" {
		go func() { // 一次读一个数据报，原样回
			buf := make([]byte, 65535)
			for {
				n, err := b.Read(buf)
				if err != nil {
					b.Close()
					return
				}
				if _, err := b.Write(buf[:n]); err != nil {
					return
				}
			}
		}()
	} else {
		go func() { _, _ = io.Copy(b, b); b.Close() }()
	}
	return a, nil
}

func (f *fakeDirect) list() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

const m3Rules = `
rules:
  - {app: com.app.a, action: "proxy:wg-a"}
  - {app: com.app.b, action: "proxy:clash-b"}
  - {app: com.app.c, action: direct}
local_domains: ["*.corp.example"]
`

type m3Env struct {
	*proxyEnv
	ident  *fakeIdent
	direct *fakeDirect
	cfg    atomic.Pointer[rules.Config]
}

func newM3Env(t *testing.T, y string) *m3Env {
	t.Helper()
	e := &m3Env{ident: &fakeIdent{}, direct: &fakeDirect{pinned: map[string]bool{}}}
	pool := fakedns.DefaultPool()
	e.setRules(t, y, pool)
	pe := newProxyEnvWith(t, pool, Options{}, func(p *Proxy) {
		p.Rules = func() *rules.Config { return e.cfg.Load() }
		p.ExitUp = func(x string) bool { return x == "wg-a" || x == "clash-b" }
		p.Ident = e.ident
		p.Direct = e.direct
	})
	e.proxyEnv = pe
	return e
}

func (e *m3Env) setRules(t *testing.T, y string, pool *fakedns.Pool) {
	t.Helper()
	c, err := rules.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	e.cfg.Store(c)
	pool.SetLocalMatcher(c.IsLocal)
}

// dialFrom 从指定源端口拨 TCP。
func (c *client) dialFrom(port uint16, dst netip.AddrPort, timeout time.Duration) (*gonet.TCPConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	local := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(clientIP.As4()), Port: port}
	return gonet.DialTCPWithBind(ctx, c.s, local, full(dst), ipv4.ProtocolNumber)
}

// udpFrom 从指定源端口开一个连向 dst 的 UDP 套接字。
func (c *client) udpFrom(t *testing.T, port uint16, dst netip.AddrPort) *gonet.UDPConn {
	t.Helper()
	local := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(clientIP.As4()), Port: port}
	remote := full(dst)
	u, err := gonet.DialUDP(c.s, &local, &remote, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.Close() })
	return u
}

// echoOnce 写 data，读回同样长度，比较。
func echoOnce(c net.Conn, data []byte) error {
	_ = c.SetDeadline(time.Now().Add(ioWait))
	if _, err := c.Write(data); err != nil {
		return err
	}
	got := make([]byte, len(data))
	if _, err := io.ReadFull(c, got); err != nil {
		return err
	}
	if string(got) != string(data) {
		return errors.New("回显的字节不一致")
	}
	return nil
}

// tlsHello 用 crypto/tls 生成一个 ClientHello（经 net.Pipe 截获，不碰网络）。
func tlsHello(t testing.TB, cfg *tls.Config) []byte {
	t.Helper()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() { _ = tls.Client(a, cfg).Handshake() }()
	_ = b.SetReadDeadline(time.Now().Add(5 * time.Second))
	var got []byte
	buf := make([]byte, 4096)
	for {
		n, err := b.Read(buf)
		got = append(got, buf[:n]...)
		if len(got) >= 5 && len(got) >= 5+int(binary.BigEndian.Uint16(got[3:5])) || err != nil {
			return got
		}
	}
}

func echConfigList(t testing.TB) []byte {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	u16 := func(b []byte, v int) []byte { return binary.BigEndian.AppendUint16(b, uint16(v)) }
	vec := func(b, v []byte) []byte { return append(u16(b, len(v)), v...) }
	c := []byte{7}
	c = u16(c, 0x0020)
	c = vec(c, k.PublicKey().Bytes())
	c = vec(c, []byte{0, 1, 0, 1})
	c = append(c, 0, byte(len("public.example")))
	c = append(c, "public.example"...)
	c = u16(c, 0)
	return vec(nil, vec(u16(nil, 0xfe0d), c))
}

// quicInitial 构造一个带 SNI 的 QUIC v1 客户端 Initial 数据报（1200 字节，RFC 9001 的密钥推导）。
func quicInitial(t testing.TB, sni string) []byte {
	t.Helper()
	// ClientHello 正文：版本、随机数、会话、密码套件、压缩、扩展（只有 SNI）。
	u16 := func(b []byte, v int) []byte { return binary.BigEndian.AppendUint16(b, uint16(v)) }
	vec := func(b, v []byte) []byte { return append(u16(b, len(v)), v...) }
	body := append([]byte{3, 3}, make([]byte, 32)...)
	body = append(body, 0)
	body = vec(body, []byte{0x13, 0x01})
	body = append(body, 1, 0)
	list := append([]byte{0}, vec(nil, []byte(sni))...)
	ext := u16(nil, 0)
	ext = vec(ext, vec(nil, list))
	body = vec(body, ext)
	hello := append([]byte{1, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}, body...)
	frame := []byte{0x06, 0x00}
	frame = append(frame, byte(0x40|len(hello)>>8), byte(len(hello)))
	frame = append(frame, hello...)

	dcid := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	salt := []byte{0x38, 0x76, 0x2c, 0xf7, 0xf5, 0x59, 0x34, 0xb3, 0x4d, 0x17, 0x9a, 0xe6, 0xa4, 0xc8, 0x0c, 0xad, 0xcc, 0xbb, 0x7f, 0x0a}
	label := func(secret []byte, l string, n int) []byte {
		full := "tls13 " + l
		info := u16(nil, n)
		info = append(info, byte(len(full)))
		info = append(info, full...)
		info = append(info, 0)
		out, err := hkdf.Expand(sha256.New, secret, string(info), n)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	initial, _ := hkdf.Extract(sha256.New, dcid, salt)
	secret := label(initial, "client in", 32)
	key, iv, hp := label(secret, "quic key", 16), label(secret, "quic iv", 12), label(secret, "quic hp", 16)
	hdr := []byte{0xc3, 0, 0, 0, 1, byte(len(dcid))}
	hdr = append(hdr, dcid...)
	hdr = append(hdr, 0, 0)
	pad := 1200 - (len(hdr) + 2 + 4 + len(frame) + 16)
	frame = append(frame, make([]byte, pad)...)
	hdr = u16(hdr, 0x4000|(4+len(frame)+16))
	pnOff := len(hdr)
	hdr = append(hdr, 0, 0, 0, 0)
	kb, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(kb)
	pkt := aead.Seal(append([]byte(nil), hdr...), iv, frame, hdr)
	hb, _ := aes.NewCipher(hp)
	var mask [16]byte
	hb.Encrypt(mask[:], pkt[pnOff+4:pnOff+20])
	pkt[0] ^= mask[0] & 0x0f
	for i := 0; i < 4; i++ {
		pkt[pnOff+i] ^= mask[1+i]
	}
	return pkt
}
