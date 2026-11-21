package procinfo

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

// 连接表（sysctl net.inet.{tcp,udp}.pcblist_n）的布局，2026-11-19 在 macOS 27.2.1（26B131）上只读实测：
//   - 开头是 24 字节的 xinpgen，结尾是同样 24 字节的 xinpgen；
//   - 中间每条记录以 uint32 长度、uint32 种类开头，按 8 字节对齐前进；
//   - 一个 pcb 由 XSO_INPCB 开头，后面跟 XSO_SOCKET、XSO_RCVBUF、XSO_SNDBUF、XSO_STATS（TCP 还有 XSO_TCPCB）。
//
// 只按下表认识的“种类 + 长度”解析，遇到不认识的整体报错，不猜（m3-plan v2.4 补充 1）。
const (
	kindSocket = 0x001
	kindRcvBuf = 0x002
	kindSndBuf = 0x004
	kindStats  = 0x008
	kindInpcb  = 0x010
	kindTcpcb  = 0x020

	genLen = 24
)

var knownLen = map[uint32]uint32{
	kindSocket: 104,
	kindRcvBuf: 32,
	kindSndBuf: 32,
	kindStats:  136,
	kindInpcb:  104,
	kindTcpcb:  204,
}

// xinpcb_n 和 xsocket_n 里用到的字段偏移（实测）。
const (
	inpPtr    = 8  // xi_inpp
	inpFPort  = 16 // inp_fport，网络字节序
	inpLPort  = 18 // inp_lport，网络字节序
	inpVFlag  = 44 // inp_vflag：0x1 IPv4，0x2 IPv6
	inpFAddr  = 48 // 远端地址（16 字节的联合体，IPv4 在 +12）
	inpLAddr  = 64 // 本地地址
	soPcb     = 28 // so_pcb，与 xi_inpp 相同
	soProto   = 36 // xso_protocol
	soUID     = 64
	soLastPID = 68
	soEPID    = 72 // 委托方进程号（NSURLSession 等代为建立的连接），非零时优先
)

// Entry 是连接表里的一条：本地、远端地址和归属进程号。
type Entry struct {
	Local, Remote netip.AddrPort
	PID           int
	UID           uint32
}

// ErrLayout 表示连接表的布局不是实测过的样子。
var ErrLayout = errors.New("连接表的布局不认识")

func roundup8(n int) int { return (n + 7) &^ 7 }

// ParsePCBList 解析 pcblist_n 的原始字节。proto 是期望的协议号（TCP 6、UDP 17）。
func ParsePCBList(b []byte, proto int) ([]Entry, error) {
	le := binary.LittleEndian
	if len(b) < 2*genLen || le.Uint32(b) != genLen {
		return nil, fmt.Errorf("%w：开头不是 xinpgen", ErrLayout)
	}
	var out []Entry
	var cur *Entry
	var curPtr []byte
	haveSock := false
	flush := func() error {
		if cur != nil {
			if !haveSock {
				return fmt.Errorf("%w：连接记录后面没有套接字记录", ErrLayout)
			}
			out = append(out, *cur)
		}
		cur, haveSock = nil, false
		return nil
	}
	off := genLen
	for {
		if off+8 > len(b) {
			return nil, fmt.Errorf("%w：记录越界", ErrLayout)
		}
		l, k := le.Uint32(b[off:]), le.Uint32(b[off+4:])
		if l == genLen && off+genLen == len(b) {
			if err := flush(); err != nil {
				return nil, err
			}
			return out, nil // 结尾的 xinpgen
		}
		want, ok := knownLen[k]
		if !ok || l != want {
			return nil, fmt.Errorf("%w：种类 %#x 的长度 %d", ErrLayout, k, l)
		}
		if off+int(l) > len(b) {
			return nil, fmt.Errorf("%w：记录越界", ErrLayout)
		}
		r := b[off : off+int(l)]
		switch k {
		case kindInpcb:
			if err := flush(); err != nil {
				return nil, err
			}
			e, err := parseInpcb(r)
			if err != nil {
				return nil, err
			}
			cur, curPtr = &e, r[inpPtr:inpPtr+8]
		case kindSocket:
			if cur == nil || haveSock {
				return nil, fmt.Errorf("%w：套接字记录的位置不对", ErrLayout)
			}
			if !bytes.Equal(r[soPcb:soPcb+8], curPtr) {
				return nil, fmt.Errorf("%w：套接字记录对不上连接记录", ErrLayout)
			}
			if int(int32(le.Uint32(r[soProto:]))) != proto {
				return nil, fmt.Errorf("%w：协议号不对", ErrLayout)
			}
			pid := int(int32(le.Uint32(r[soEPID:])))
			if pid == 0 {
				pid = int(int32(le.Uint32(r[soLastPID:])))
			}
			cur.PID, cur.UID, haveSock = pid, le.Uint32(r[soUID:]), true
		default:
			if cur == nil {
				return nil, fmt.Errorf("%w：附属记录前面没有连接记录", ErrLayout)
			}
		}
		off += roundup8(int(l))
	}
}

func parseInpcb(r []byte) (Entry, error) {
	be := binary.BigEndian
	vflag := r[inpVFlag]
	addr := func(at int) (netip.Addr, error) {
		switch {
		case vflag&0x1 != 0:
			return netip.AddrFrom4([4]byte(r[at+12 : at+16])), nil
		case vflag&0x2 != 0:
			return netip.AddrFrom16([16]byte(r[at : at+16])).Unmap(), nil
		}
		return netip.Addr{}, fmt.Errorf("%w：地址族标志 %#x", ErrLayout, vflag)
	}
	la, err := addr(inpLAddr)
	if err != nil {
		return Entry{}, err
	}
	fa, err := addr(inpFAddr)
	if err != nil {
		return Entry{}, err
	}
	return Entry{
		Local:  netip.AddrPortFrom(la, be.Uint16(r[inpLPort:])),
		Remote: netip.AddrPortFrom(fa, be.Uint16(r[inpFPort:])),
	}, nil
}

// match 在一份快照里找流的归属进程：TCP 和已连接的 UDP 按“本地端口 + 远端地址和端口”，
// 未连接的 UDP 按本地端口。返回进程号；没找到返回 0；匹配到多个不同进程返回 -1。
func match(entries []Entry, network string, src, dst netip.AddrPort) int {
	pid := 0
	for _, e := range entries {
		if e.Local.Port() != src.Port() || e.PID <= 0 {
			continue
		}
		unconnected := network == "udp" && e.Remote.Port() == 0 && (e.Remote.Addr().IsUnspecified() || !e.Remote.Addr().IsValid())
		if !unconnected && (e.Remote.Port() != dst.Port() || e.Remote.Addr() != dst.Addr().Unmap()) {
			continue
		}
		switch {
		case pid == 0:
			pid = e.PID
		case pid != e.PID:
			return -1
		}
	}
	return pid
}

// ParseProcArgs 解析 kern.procargs2：int32 argc，可执行文件路径，若干 NUL 填充，然后是 argc 个参数。
func ParseProcArgs(b []byte) (path string, args []string, err error) {
	if len(b) < 4 {
		return "", nil, errors.New("procargs2 太短")
	}
	argc := int(int32(binary.LittleEndian.Uint32(b)))
	rest := b[4:]
	i := bytes.IndexByte(rest, 0)
	if i <= 0 {
		return "", nil, errors.New("procargs2 没有可执行文件路径")
	}
	path = string(rest[:i])
	rest = rest[i:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	if argc < 0 || argc > 4096 {
		return "", nil, errors.New("procargs2 的参数个数不对")
	}
	for n := 0; n < argc && len(rest) > 0; n++ {
		j := bytes.IndexByte(rest, 0)
		if j < 0 {
			j = len(rest)
		}
		args = append(args, string(rest[:j]))
		if j == len(rest) {
			break
		}
		rest = rest[j+1:]
	}
	return path, args, nil
}
