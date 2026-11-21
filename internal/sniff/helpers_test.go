package sniff

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func readHex(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	d, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func u16(b []byte, v int) []byte { return binary.BigEndian.AppendUint16(b, uint16(v)) }

func vec16(b, v []byte) []byte { return append(u16(b, len(v)), v...) }

// ext 是一个 TLS 扩展。
type ext struct {
	typ  int
	data []byte
}

func sniExt(names ...string) ext {
	var list []byte
	for _, n := range names {
		list = append(list, 0)
		list = vec16(list, []byte(n))
	}
	return ext{extServerName, vec16(nil, list)}
}

// helloBody 构造 ClientHello 正文（不含握手头）。
func helloBody(exts ...ext) []byte {
	b := []byte{0x03, 0x03}
	b = append(b, make([]byte, 32)...) // random
	b = append(b, 0)                   // session id
	b = vec16(b, []byte{0x13, 0x01})   // cipher suites
	b = append(b, 1, 0)                // compression
	var e []byte
	for _, x := range exts {
		e = u16(e, x.typ)
		e = vec16(e, x.data)
	}
	return vec16(b, e)
}

// hsMsg 加上 4 字节的握手头。
func hsMsg(typ byte, body []byte) []byte {
	n := len(body)
	return append([]byte{typ, byte(n >> 16), byte(n >> 8), byte(n)}, body...)
}

// records 把握手消息切成若干个 TLS 记录（按 sizes 切，最后一块放剩余部分）。
func records(msg []byte, sizes ...int) []byte {
	var out []byte
	for _, s := range sizes {
		if s > len(msg) {
			s = len(msg)
		}
		out = append(out, recHandshake, 0x03, 0x01)
		out = vec16(out, msg[:s])
		msg = msg[s:]
	}
	if len(msg) > 0 {
		out = append(out, recHandshake, 0x03, 0x01)
		out = vec16(out, msg)
	}
	return out
}

func hello(exts ...ext) []byte { return records(hsMsg(hsClientHello, helloBody(exts...))) }

// goHello 用 crypto/tls 的客户端生成一个真实的 ClientHello（经 net.Pipe 截获，不碰网络）。
func goHello(t testing.TB, cfg *tls.Config) []byte {
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
		if _, more := TLS(got); !more || err != nil {
			return got
		}
	}
}

// echConfigList 构造一个合法的 ECHConfigList（X25519、HKDF-SHA256、AES-128-GCM），给 crypto/tls 客户端用。
func echConfigList(t testing.TB, publicName string) []byte {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var c []byte
	c = append(c, 7)                     // config_id
	c = u16(c, 0x0020)                   // kem: DHKEM(X25519, HKDF-SHA256)
	c = vec16(c, k.PublicKey().Bytes())  // public_key
	c = vec16(c, []byte{0, 1, 0, 1})     // HKDF-SHA256 + AES-128-GCM
	c = append(c, 0)                     // maximum_name_length
	c = append(c, byte(len(publicName))) // public_name
	c = append(c, publicName...)
	c = u16(c, 0) // extensions
	cfg := u16(nil, 0xfe0d)
	cfg = vec16(cfg, c)
	return vec16(nil, cfg)
}

// sealInitial 按 RFC 9001/9369 构造一个客户端 Initial 数据报（填充到 1200 字节），用来生成 QUIC 的测试样本。
func sealInitial(t testing.TB, version uint32, dcid []byte, pn uint32, frames []byte) []byte {
	t.Helper()
	keys, ok := clientInitialKeys(version, dcid)
	if !ok {
		t.Fatal("推导密钥失败")
	}
	first := byte(0xc3) // 长头、固定位、Initial（v1 类型 0）、4 字节包号
	if version == quicV2 {
		first = 0xd3 // v2 的 Initial 类型是 1
	}
	hdr := []byte{first}
	hdr = binary.BigEndian.AppendUint32(hdr, version)
	hdr = append(hdr, byte(len(dcid)))
	hdr = append(hdr, dcid...)
	hdr = append(hdr, 0, 0) // scid 空，token 空
	// 填充帧，使整个数据报正好 1200 字节：头 + 2 字节长度 + 4 字节包号 + 帧 + 16 字节标签。
	pad := MinQUICInitial - (len(hdr) + 2 + 4 + len(frames) + 16)
	if pad > 0 {
		frames = append(frames, make([]byte, pad)...)
	}
	length := 4 + len(frames) + 16
	hdr = binary.BigEndian.AppendUint16(hdr, uint16(0x4000|length))
	pnOff := len(hdr)
	hdr = binary.BigEndian.AppendUint32(hdr, pn)
	nonce := append([]byte(nil), keys.iv...)
	for i := 0; i < 8; i++ {
		nonce[len(nonce)-1-i] ^= byte(uint64(pn) >> (8 * i))
	}
	pkt := keys.aead.Seal(append([]byte(nil), hdr...), nonce, frames, hdr)
	var mask [16]byte
	keys.hp.Encrypt(mask[:], pkt[pnOff+4:pnOff+20])
	pkt[0] ^= mask[0] & 0x0f
	for i := 0; i < 4; i++ {
		pkt[pnOff+i] ^= mask[1+i]
	}
	return pkt
}

// cryptoFrame 构造一个 CRYPTO 帧（偏移和长度都用 4 字节的变长整数）。
func cryptoFrame(off int, data []byte) []byte {
	b := []byte{0x06}
	b = binary.BigEndian.AppendUint32(b, uint32(0x80000000|off))
	b = binary.BigEndian.AppendUint32(b, uint32(0x80000000|len(data)))
	return append(b, data...)
}
