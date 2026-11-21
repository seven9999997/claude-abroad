package sniff

import (
	"encoding/binary"
	"net/netip"

	"claude-abroad/internal/rules"
)

const (
	recHandshake      = 0x16
	hsClientHello     = 0x01
	extServerName     = 0x0000
	extECH            = 0xfe0d
	maxRecordLen      = 1<<14 + 256 // TLSPlaintext 的上限（留出余量，超过即判不清）
	maxClientHelloLen = MaxPeek
)

// MayBeTLS 判断 b 是否可能是某个 TLS 记录头的前缀（偏严：内容类型 0x14–0x18 都算）。
// 是前缀的输入永远不会被判成 NotTLS（m3-plan v2.3 补充 1、验收 e）。
func MayBeTLS(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	if b[0] < 0x14 || b[0] > 0x18 {
		return false
	}
	if len(b) >= 2 && b[1] != 0x03 {
		return false
	}
	if len(b) >= 3 && b[2] > 0x04 {
		return false
	}
	return true
}

// TLS 判定 TCP 首包里的 TLS ClientHello。needMore 为真表示还要更多字节才能下结论。
// 输入不是 TLS 记录头的前缀时返回 NotTLS（交给调用方再看 HTTP）。
func TLS(b []byte) (r Result, needMore bool) {
	if len(b) == 0 {
		return Result{}, true
	}
	if !MayBeTLS(b) {
		return Result{Kind: rules.NotTLS}, false
	}
	noName := Result{Kind: rules.TLSNoName}
	var hs []byte
	off := 0
	for {
		if off+5 > len(b) {
			if off >= MaxPeek {
				return noName, false
			}
			return Result{}, true
		}
		h := b[off : off+5]
		if h[0] != recHandshake || h[1] != 0x03 || h[2] > 0x04 {
			return noName, false // ClientHello 收完之前出现别的记录
		}
		n := int(binary.BigEndian.Uint16(h[3:5]))
		if n == 0 || n > maxRecordLen {
			return noName, false
		}
		if off+5+n > len(b) {
			if off+5+n > MaxPeek {
				return noName, false
			}
			return Result{}, true
		}
		hs = append(hs, b[off+5:off+5+n]...)
		off += 5 + n
		if len(hs) >= 1 && hs[0] != hsClientHello {
			return noName, false
		}
		if len(hs) >= 4 {
			ml := int(hs[1])<<16 | int(hs[2])<<8 | int(hs[3])
			if ml > maxClientHelloLen {
				return noName, false
			}
			if len(hs) >= 4+ml {
				return clientHello(hs[4:4+ml], rules.TLSName, rules.TLSNoName, rules.TLSECH), false
			}
		}
		if off >= MaxPeek {
			return noName, false
		}
	}
}

// clientHello 解析 ClientHello 的正文（不含 4 字节的握手头）。任何长度矛盾、重复扩展、
// 多个 SNI 条目、不合法或 IP 字面量的 SNI，都判成 noName；带 ECH 扩展判成 ech。
func clientHello(m []byte, named, noName, ech rules.SniffKind) Result {
	bad := Result{Kind: noName}
	p := parser{b: m}
	if !p.skip(2+32) || !p.skipVec(1, 32) || !p.skipVec(2, 1<<16-2) || !p.skipVec(1, 255) {
		return bad
	}
	if p.empty() {
		return bad // 没有扩展：没有 SNI
	}
	exts, ok := p.vec(2)
	if !ok || !p.empty() {
		return bad
	}
	seen := map[uint16]bool{}
	var sni []byte
	hasECH := false
	e := parser{b: exts}
	for !e.empty() {
		t, ok1 := e.u16()
		data, ok2 := e.vec(2)
		if !ok1 || !ok2 {
			return bad
		}
		if seen[t] {
			return bad
		}
		seen[t] = true
		switch t {
		case extServerName:
			sni = data
		case extECH:
			hasECH = true
		}
	}
	if hasECH {
		return Result{Kind: ech}
	}
	if sni == nil {
		return bad
	}
	sp := parser{b: sni}
	list, ok := sp.vec(2)
	if !ok || !sp.empty() {
		return bad
	}
	lp := parser{b: list}
	typ, ok1 := lp.u8()
	name, ok2 := lp.vec(2)
	if !ok1 || !ok2 || typ != 0 || !lp.empty() { // 只收恰好一个 host_name 条目
		return bad
	}
	return nameResult(string(name), named, bad)
}

// nameResult 归一名字；不合法或是 IP 字面量时返回 bad。
func nameResult(name string, kind rules.SniffKind, bad Result) Result {
	if _, err := netip.ParseAddr(name); err == nil {
		return bad
	}
	n, err := rules.NormalizeName(name)
	if err != nil {
		return bad
	}
	return Result{Kind: kind, Name: n}
}

// parser 是只进不退的字节读取器，所有读取都先查边界。
type parser struct{ b []byte }

func (p *parser) empty() bool { return len(p.b) == 0 }

func (p *parser) skip(n int) bool {
	if n < 0 || n > len(p.b) {
		return false
	}
	p.b = p.b[n:]
	return true
}

func (p *parser) u8() (byte, bool) {
	if len(p.b) < 1 {
		return 0, false
	}
	v := p.b[0]
	p.b = p.b[1:]
	return v, true
}

func (p *parser) u16() (uint16, bool) {
	if len(p.b) < 2 {
		return 0, false
	}
	v := binary.BigEndian.Uint16(p.b)
	p.b = p.b[2:]
	return v, true
}

// vec 读一个带 1 或 2 字节长度前缀的向量。
func (p *parser) vec(lenBytes int) ([]byte, bool) {
	var n int
	switch lenBytes {
	case 1:
		v, ok := p.u8()
		if !ok {
			return nil, false
		}
		n = int(v)
	case 2:
		v, ok := p.u16()
		if !ok {
			return nil, false
		}
		n = int(v)
	}
	if n > len(p.b) {
		return nil, false
	}
	v := p.b[:n]
	p.b = p.b[n:]
	return v, true
}

func (p *parser) skipVec(lenBytes, max int) bool {
	v, ok := p.vec(lenBytes)
	return ok && len(v) <= max
}
