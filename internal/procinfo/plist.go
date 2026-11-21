package procinfo

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"unicode/utf16"
)

// Info.plist 的小解析器（m3-plan M3-3，architect 拍板自己写）：只取顶层 dict 的 CFBundleIdentifier。
// 输入来自任意应用包，在 root 进程里解析，所以有大小、深度、对象数的上限。
const (
	MaxPlistSize    = 1 << 20
	maxPlistDepth   = 16
	maxPlistObjects = 65536
	bundleIDKey     = "CFBundleIdentifier"
)

var errPlist = errors.New("Info.plist 格式不对")

// BundleIDFromPlist 从 Info.plist 的内容里取 CFBundleIdentifier，支持 XML 和 bplist00。
func BundleIDFromPlist(b []byte) (string, error) {
	if len(b) > MaxPlistSize {
		return "", errors.New("Info.plist 超过大小上限")
	}
	if bytes.HasPrefix(b, []byte("bplist00")) {
		return bplistBundleID(b)
	}
	return xmlBundleID(b)
}

func xmlBundleID(b []byte) (string, error) {
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = true
	depth, objects := 0, 0
	// 顶层 dict 的深度是 2（plist、dict）。
	var lastKey string
	inKey, inString := false, false
	var text strings.Builder
	found, id := false, ""
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", errPlist
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			objects++
			if depth > maxPlistDepth || objects > maxPlistObjects {
				return "", errors.New("Info.plist 嵌套太深或对象太多")
			}
			if depth == 1 && t.Name.Local != "plist" || depth == 2 && t.Name.Local != "dict" {
				if depth == 2 {
					return "", errPlist // 顶层不是 dict
				}
				return "", errPlist
			}
			text.Reset()
			inKey = depth == 3 && t.Name.Local == "key"
			inString = depth == 3 && t.Name.Local == "string"
		case xml.CharData:
			if inKey || inString {
				if text.Len()+len(t) > 4096 {
					return "", errPlist
				}
				text.Write(t)
			}
		case xml.EndElement:
			if depth == 3 {
				switch {
				case inKey:
					lastKey = text.String()
				case inString && lastKey == bundleIDKey:
					if found {
						return "", errPlist // 重复的键
					}
					found, id = true, text.String()
				}
				if !inKey {
					lastKey = ""
				}
				inKey, inString = false, false
			}
			depth--
		}
	}
	if !found {
		return "", errors.New("Info.plist 里没有 CFBundleIdentifier")
	}
	return id, nil
}

type bplist struct {
	b         []byte
	offsets   []uint64
	refSize   int
	visiting  map[uint64]bool
	objects   int
	maxOffset uint64
}

func bplistBundleID(b []byte) (string, error) {
	if len(b) < 8+32 {
		return "", errPlist
	}
	tr := b[len(b)-32:]
	offSize, refSize := int(tr[6]), int(tr[7])
	num := binary.BigEndian.Uint64(tr[8:])
	top := binary.BigEndian.Uint64(tr[16:])
	tableOff := binary.BigEndian.Uint64(tr[24:])
	if offSize < 1 || offSize > 8 || refSize < 1 || refSize > 8 || num == 0 || num > maxPlistObjects || top >= num {
		return "", errPlist
	}
	end := uint64(len(b) - 32)
	if tableOff < 8 || tableOff > end || num*uint64(offSize) > end-tableOff {
		return "", errPlist // 偏移表越界
	}
	p := &bplist{b: b, refSize: refSize, visiting: map[uint64]bool{}, maxOffset: tableOff}
	p.offsets = make([]uint64, num)
	for i := uint64(0); i < num; i++ {
		o := readUint(b[tableOff+i*uint64(offSize):], offSize)
		if o < 8 || o >= tableOff {
			return "", errPlist
		}
		p.offsets[i] = o
	}
	// 先把整张图走一遍：深度、环、对象数、越界都在这里查出来。
	if err := p.walk(top, 1); err != nil {
		return "", err
	}
	keys, vals, err := p.dict(top)
	if err != nil {
		return "", errPlist // 顶层不是 dict
	}
	found, id := false, ""
	for i, k := range keys {
		ks, err := p.str(k)
		if err != nil {
			return "", errors.New("Info.plist 的键不是字符串")
		}
		if ks != bundleIDKey {
			continue
		}
		if found {
			return "", errPlist
		}
		v, err := p.str(vals[i])
		if err != nil {
			return "", errPlist
		}
		found, id = true, v
	}
	if !found {
		return "", errors.New("Info.plist 里没有 CFBundleIdentifier")
	}
	return id, nil
}

func readUint(b []byte, n int) uint64 {
	var v uint64
	for i := 0; i < n; i++ {
		v = v<<8 | uint64(b[i])
	}
	return v
}

// header 读对象的标记和长度（长度为 0xF 时后面跟一个整数对象），返回数据起点。
func (p *bplist) header(ref uint64) (marker byte, count uint64, data uint64, err error) {
	if ref >= uint64(len(p.offsets)) {
		return 0, 0, 0, errPlist
	}
	o := p.offsets[ref]
	m := p.b[o]
	count = uint64(m & 0x0f)
	data = o + 1
	if m>>4 != 0x0 && m>>4 != 0x1 && m>>4 != 0x2 && m>>4 != 0x3 && count == 0x0f {
		if data >= p.maxOffset {
			return 0, 0, 0, errPlist
		}
		im := p.b[data]
		if im>>4 != 0x1 || im&0x0f > 3 {
			return 0, 0, 0, errPlist
		}
		n := uint64(1) << (im & 0x0f)
		if data+1+n > p.maxOffset {
			return 0, 0, 0, errPlist
		}
		count = readUint(p.b[data+1:], int(n))
		data += 1 + n
	}
	return m, count, data, nil
}

func (p *bplist) refs(data, n uint64) ([]uint64, error) {
	if n > maxPlistObjects || data+n*uint64(p.refSize) > p.maxOffset {
		return nil, errPlist
	}
	out := make([]uint64, n)
	for i := uint64(0); i < n; i++ {
		out[i] = readUint(p.b[data+i*uint64(p.refSize):], p.refSize)
	}
	return out, nil
}

func (p *bplist) dict(ref uint64) (keys, vals []uint64, err error) {
	m, n, data, err := p.header(ref)
	if err != nil || m>>4 != 0xd {
		return nil, nil, errPlist
	}
	all, err := p.refs(data, 2*n)
	if err != nil {
		return nil, nil, err
	}
	return all[:n], all[n:], nil
}

func (p *bplist) str(ref uint64) (string, error) {
	m, n, data, err := p.header(ref)
	if err != nil {
		return "", err
	}
	switch m >> 4 {
	case 0x5: // ASCII
		if n > 4096 || data+n > p.maxOffset {
			return "", errPlist
		}
		return string(p.b[data : data+n]), nil
	case 0x6: // UTF-16BE
		if n > 4096 || data+2*n > p.maxOffset {
			return "", errPlist
		}
		u := make([]uint16, n)
		for i := range u {
			u[i] = binary.BigEndian.Uint16(p.b[data+2*uint64(i):])
		}
		return string(utf16.Decode(u)), nil
	}
	return "", errPlist
}

// walk 检查整张对象图：深度不超过 16，没有环，对象总数有上限，容器里的引用都在范围内。
func (p *bplist) walk(ref uint64, depth int) error {
	if depth > maxPlistDepth {
		return errors.New("Info.plist 嵌套太深")
	}
	if p.visiting[ref] {
		return errors.New("Info.plist 的引用成环")
	}
	p.objects++
	if p.objects > maxPlistObjects {
		return errors.New("Info.plist 对象太多")
	}
	m, n, data, err := p.header(ref)
	if err != nil {
		return err
	}
	var children []uint64
	switch m >> 4 {
	case 0xa, 0xc: // array、set
		children, err = p.refs(data, n)
	case 0xd: // dict
		children, err = p.refs(data, 2*n)
	}
	if err != nil {
		return err
	}
	if len(children) == 0 {
		return nil
	}
	p.visiting[ref] = true
	for _, c := range children {
		if err := p.walk(c, depth+1); err != nil {
			return err
		}
	}
	delete(p.visiting, ref)
	return nil
}
