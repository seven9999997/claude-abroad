package imagebuild

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strconv"
	"strings"
)

// cpio newc 格式（Linux initramfs 用的格式）。见内核文档 early-userspace/buffer-format。

const (
	cpioMagic   = "070701"
	cpioHdrLen  = 110
	cpioTrailer = "TRAILER!!!"
)

// cpio 里的文件类型位（与 stat 的 S_IF* 相同）。
const (
	sIFMT  = 0o170000
	sIFDIR = 0o040000
	sIFREG = 0o100000
	sIFLNK = 0o120000
	sIFCHR = 0o020000
)

func unixMode(e *Entry) (uint32, error) {
	perm := uint32(e.Mode.Perm())
	if e.Mode&fs.ModeSetuid != 0 {
		perm |= 0o4000
	}
	if e.Mode&fs.ModeSetgid != 0 {
		perm |= 0o2000
	}
	if e.Mode&fs.ModeSticky != 0 {
		perm |= 0o1000
	}
	switch {
	case e.IsDir():
		return sIFDIR | perm, nil
	case e.IsSymlink():
		return sIFLNK | perm, nil
	case e.IsRegular():
		return sIFREG | perm, nil
	case e.IsCharDev():
		return sIFCHR | perm, nil
	}
	return 0, fmt.Errorf("%s：cpio 不支持的类型", e.Path)
}

func goMode(m uint32) (fs.FileMode, error) {
	perm := fs.FileMode(m & 0o777)
	if m&0o4000 != 0 {
		perm |= fs.ModeSetuid
	}
	if m&0o2000 != 0 {
		perm |= fs.ModeSetgid
	}
	if m&0o1000 != 0 {
		perm |= fs.ModeSticky
	}
	switch m & sIFMT {
	case sIFDIR:
		return fs.ModeDir | perm, nil
	case sIFLNK:
		return fs.ModeSymlink | perm, nil
	case sIFREG:
		return perm, nil
	case sIFCHR:
		return fs.ModeDevice | fs.ModeCharDevice | perm, nil
	}
	return 0, fmt.Errorf("不支持的 cpio 文件类型 %o", m&sIFMT)
}

func pad4(n int) int { return (4 - n%4) % 4 }

// WriteCPIO 把树写成 newc 格式。属主一律写成 0（initramfs 里只有 root），
// 时间戳为 0，便于复现。根目录本身不写（内核的 rootfs 已存在）。
func WriteCPIO(w io.Writer, t *Tree) error {
	bw := bufio.NewWriter(w)
	ino := uint32(1)
	write := func(name string, mode uint32, data []byte, rmaj, rmin uint32) error {
		hdr := fmt.Sprintf("%s%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x",
			cpioMagic, ino, mode, 0, 0, 1, 0, len(data), 0, 0, rmaj, rmin, len(name)+1, 0)
		ino++
		if _, err := bw.WriteString(hdr); err != nil {
			return err
		}
		if _, err := bw.WriteString(name + "\x00"); err != nil {
			return err
		}
		if _, err := bw.Write(make([]byte, pad4(cpioHdrLen+len(name)+1))); err != nil {
			return err
		}
		if _, err := bw.Write(data); err != nil {
			return err
		}
		_, err := bw.Write(make([]byte, pad4(len(data))))
		return err
	}
	for _, e := range t.Entries() {
		if e.Path == "/" {
			continue
		}
		mode, err := unixMode(e)
		if err != nil {
			return err
		}
		var data []byte
		switch {
		case e.IsSymlink():
			data = []byte(e.Link)
		case e.IsRegular():
			data = e.Data
		}
		if len(data) > 0xffffffff {
			return fmt.Errorf("%s：文件过大", e.Path)
		}
		if err := write(strings.TrimPrefix(e.Path, "/"), mode, data, e.DevMajor, e.DevMinor); err != nil {
			return err
		}
	}
	if err := write(cpioTrailer, 0, nil, 0, 0); err != nil {
		return err
	}
	return bw.Flush()
}

// CPIOEntry 是读出的一项，保留原始属主以便检查。
type CPIOEntry struct {
	Entry
	Ino   uint32
	NLink uint32
}

// ReadCPIO 解析一个 newc 格式的 cpio，读到 TRAILER!!! 为止。
func ReadCPIO(r io.Reader) ([]CPIOEntry, error) {
	data, err := io.ReadAll(io.LimitReader(r, 1<<30))
	if err != nil {
		return nil, err
	}
	entries, _, err := parseCPIO(data)
	return entries, err
}

// ReadCPIOAll 解析首尾相接的多个 cpio（initramfs 允许这样拼接，中间可能有 0 填充）。
func ReadCPIOAll(data []byte) ([]CPIOEntry, error) {
	var all []CPIOEntry
	for len(data) > 0 {
		i := 0
		for i < len(data) && data[i] == 0 {
			i++
		}
		data = data[i:]
		if len(data) == 0 {
			break
		}
		entries, n, err := parseCPIO(data)
		if err != nil {
			return nil, err
		}
		all = append(all, entries...)
		data = data[n:]
	}
	return all, nil
}

// parseCPIO 解析一个 cpio，返回条目和消耗的字节数。硬链接（同一 inode、nlink>1）中
// 只有最后一项带数据，这里把数据补给同 inode 的所有项。
func parseCPIO(b []byte) ([]CPIOEntry, int, error) {
	var out []CPIOEntry
	off := 0
	take := func(n int) ([]byte, error) {
		if n < 0 || off+n > len(b) {
			return nil, io.ErrUnexpectedEOF
		}
		p := b[off : off+n]
		off += n
		return p, nil
	}
	for {
		start := off
		hdr, err := take(cpioHdrLen)
		if err != nil {
			return nil, 0, fmt.Errorf("读取 cpio 头（偏移 %d）：%w", start, err)
		}
		if string(hdr[:6]) != cpioMagic {
			return nil, 0, fmt.Errorf("偏移 %d 处不是 newc 格式的 cpio 头", start)
		}
		f := make([]uint32, 13)
		for i := range f {
			v, err := strconv.ParseUint(string(hdr[6+8*i:14+8*i]), 16, 32)
			if err != nil {
				return nil, 0, fmt.Errorf("偏移 %d 处 cpio 头字段无效", start)
			}
			f[i] = uint32(v)
		}
		mode, uid, gid, nlink, size, namesize := f[1], f[2], f[3], f[4], f[6], f[11]
		if namesize == 0 || namesize > 4096 {
			return nil, 0, fmt.Errorf("偏移 %d 处文件名长度无效", start)
		}
		name, err := take(int(namesize) + pad4(cpioHdrLen+int(namesize)))
		if err != nil {
			return nil, 0, err
		}
		n := string(bytes.TrimRight(name[:namesize], "\x00"))
		data, err := take(int(size))
		if err != nil {
			return nil, 0, err
		}
		if _, err := take(pad4(int(size))); err != nil {
			return nil, 0, err
		}
		if n == cpioTrailer {
			break
		}
		gm, err := goMode(mode)
		if err != nil {
			return nil, 0, fmt.Errorf("%s：%w", n, err)
		}
		e := CPIOEntry{Entry: Entry{Path: path.Clean("/" + strings.TrimPrefix(n, "./")), Mode: gm, UID: int(uid), GID: int(gid),
			DevMajor: f[9], DevMinor: f[10]}, Ino: f[0], NLink: nlink}
		if gm&fs.ModeSymlink != 0 {
			e.Link = string(data)
		} else if gm.IsRegular() {
			e.Data = data
		}
		out = append(out, e)
	}
	byIno := map[uint32][]byte{}
	for _, e := range out {
		if e.IsRegular() && e.NLink > 1 && len(e.Data) > 0 {
			byIno[e.Ino] = e.Data
		}
	}
	for i := range out {
		if out[i].IsRegular() && out[i].NLink > 1 && len(out[i].Data) == 0 {
			out[i].Data = byIno[out[i].Ino]
		}
	}
	return out, off, nil
}
