package sniff

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"sort"

	"claude-abroad/internal/rules"
)

const (
	quicV1 = 0x00000001
	quicV2 = 0x6b3343cf
	// MinQUICInitial 是客户端 Initial 数据报的最小长度（RFC 9000 14.1）。
	MinQUICInitial = 1200
	maxCryptoData  = 64 << 10
)

var (
	saltV1 = []byte{0x38, 0x76, 0x2c, 0xf7, 0xf5, 0x59, 0x34, 0xb3, 0x4d, 0x17, 0x9a, 0xe6, 0xa4, 0xc8, 0x0c, 0xad, 0xcc, 0xbb, 0x7f, 0x0a}
	saltV2 = []byte{0x0d, 0xed, 0xe3, 0xde, 0xf7, 0x00, 0xa6, 0xdb, 0x81, 0x93, 0x81, 0xbe, 0x6e, 0x26, 0x9d, 0xcb, 0xf9, 0xbd, 0x2e, 0xd9}
)

// MayBeQUIC 判断数据报是否符合“可能是 QUIC 客户端 Initial”的形状：首字节高两位都是 1（长头加固定位），
// 长度不小于 1200 字节（m3-plan v2.3 补充 2）。符合形状的数据报永远不会被判成 NotTLS。
func MayBeQUIC(d []byte) bool {
	return len(d) >= MinQUICInitial && d[0]&0xc0 == 0xc0
}

// QUIC 判定 UDP 流的第一个数据报。只处理数据报里的第一个包；ClientHello 跨数据报时判成 QUICNoName。
func QUIC(d []byte) Result {
	noName := Result{Kind: rules.QUICNoName}
	if MayBeTLS(d) {
		return noName // 偏严：像 TLS 记录头的 UDP 数据也不放成 NotTLS
	}
	if !MayBeQUIC(d) {
		return Result{Kind: rules.NotTLS}
	}
	payload, ok := openInitial(d)
	if !ok {
		return noName
	}
	hello, ok := cryptoStream(payload)
	if !ok || len(hello) < 4 || hello[0] != hsClientHello {
		return noName
	}
	ml := int(hello[1])<<16 | int(hello[2])<<8 | int(hello[3])
	if len(hello) < 4+ml {
		return noName // ClientHello 跨数据报
	}
	return clientHello(hello[4:4+ml], rules.QUICName, rules.QUICNoName, rules.QUICNoName)
}

type initialKeys struct {
	aead cipher.AEAD
	iv   []byte
	hp   cipher.Block
}

func expandLabel(secret []byte, label string, n int) ([]byte, error) {
	full := "tls13 " + label
	info := make([]byte, 0, 4+len(full))
	info = binary.BigEndian.AppendUint16(info, uint16(n))
	info = append(info, byte(len(full)))
	info = append(info, full...)
	info = append(info, 0)
	return hkdf.Expand(sha256.New, secret, string(info), n)
}

// clientInitialKeys 按 RFC 9001 5.2（v1）和 RFC 9369 3.3（v2）推导客户端 Initial 的密钥。
func clientInitialKeys(version uint32, dcid []byte) (*initialKeys, bool) {
	salt, pre := saltV1, "quic "
	if version == quicV2 {
		salt, pre = saltV2, "quicv2 "
	}
	initial, err := hkdf.Extract(sha256.New, dcid, salt)
	if err != nil {
		return nil, false
	}
	secret, err1 := expandLabel(initial, "client in", 32)
	key, err2 := expandLabel(secret, pre+"key", 16)
	iv, err3 := expandLabel(secret, pre+"iv", 12)
	hp, err4 := expandLabel(secret, pre+"hp", 16)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return nil, false
	}
	kb, err := aes.NewCipher(key)
	if err != nil {
		return nil, false
	}
	aead, err := cipher.NewGCM(kb)
	if err != nil {
		return nil, false
	}
	hb, err := aes.NewCipher(hp)
	if err != nil {
		return nil, false
	}
	return &initialKeys{aead: aead, iv: iv, hp: hb}, true
}

// varint 读 QUIC 的变长整数。
func (p *parser) varint() (uint64, bool) {
	if len(p.b) < 1 {
		return 0, false
	}
	n := 1 << (p.b[0] >> 6)
	if len(p.b) < n {
		return 0, false
	}
	v := uint64(p.b[0] & 0x3f)
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(p.b[i])
	}
	p.b = p.b[n:]
	return v, true
}

// openInitial 去掉头部保护并解密第一个包，返回明文的帧。不修改输入。
func openInitial(d []byte) ([]byte, bool) {
	if len(d) < 7 {
		return nil, false
	}
	version := binary.BigEndian.Uint32(d[1:5])
	typ := (d[0] >> 4) & 3
	switch {
	case version == quicV1 && typ == 0, version == quicV2 && typ == 1:
	default:
		return nil, false // 版本不认识，或不是 Initial
	}
	p := parser{b: d[5:]}
	dcid, ok := p.vec(1)
	if !ok || len(dcid) > 20 {
		return nil, false
	}
	scid, ok := p.vec(1)
	if !ok || len(scid) > 20 {
		return nil, false
	}
	tl, ok := p.varint()
	if !ok || tl > uint64(len(p.b)) || !p.skip(int(tl)) {
		return nil, false
	}
	length, ok := p.varint()
	if !ok || length > uint64(len(p.b)) || length < 20 {
		return nil, false
	}
	pnOff := len(d) - len(p.b)
	end := pnOff + int(length)
	if pnOff+4+16 > end {
		return nil, false
	}
	keys, ok := clientInitialKeys(version, dcid)
	if !ok {
		return nil, false
	}
	pkt := append([]byte(nil), d[:end]...)
	var mask [16]byte
	keys.hp.Encrypt(mask[:], pkt[pnOff+4:pnOff+20])
	pkt[0] ^= mask[0] & 0x0f
	pnLen := int(pkt[0]&3) + 1
	var pn uint64
	for i := 0; i < pnLen; i++ {
		pkt[pnOff+i] ^= mask[1+i]
		pn = pn<<8 | uint64(pkt[pnOff+i])
	}
	nonce := append([]byte(nil), keys.iv...)
	for i := 0; i < 8; i++ {
		nonce[len(nonce)-1-i] ^= byte(pn >> (8 * i))
	}
	hdr := pkt[:pnOff+pnLen]
	plain, err := keys.aead.Open(nil, nonce, pkt[pnOff+pnLen:end], hdr)
	if err != nil {
		return nil, false
	}
	return plain, true
}

// cryptoStream 解析帧，按偏移重组 CRYPTO 帧，返回从偏移 0 起连续的部分。
// 只认 PADDING、PING、CRYPTO；遇到别的帧判为说不清。
func cryptoStream(frames []byte) ([]byte, bool) {
	type frag struct {
		off  uint64
		data []byte
	}
	var frags []frag
	total := 0
	p := parser{b: frames}
	for !p.empty() {
		t, ok := p.varint()
		if !ok {
			return nil, false
		}
		switch t {
		case 0x00, 0x01: // PADDING、PING
		case 0x06:
			off, ok1 := p.varint()
			n, ok2 := p.varint()
			if !ok1 || !ok2 || n > uint64(len(p.b)) || off > maxCryptoData {
				return nil, false
			}
			total += int(n)
			if total > maxCryptoData {
				return nil, false
			}
			frags = append(frags, frag{off, p.b[:n]})
			p.skip(int(n))
		default:
			return nil, false
		}
	}
	sort.SliceStable(frags, func(i, j int) bool { return frags[i].off < frags[j].off })
	var out []byte
	for _, f := range frags {
		end := f.off + uint64(len(f.data))
		if f.off > uint64(len(out)) {
			break // 有缺口：后面的数据在别的数据报里
		}
		if end > uint64(len(out)) {
			out = append(out, f.data[uint64(len(out))-f.off:]...)
		}
	}
	return out, true
}
