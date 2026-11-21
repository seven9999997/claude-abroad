package imagebuild

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
)

// Squashfs 是一个最小的 squashfs 4.0 写入器和读取器，只覆盖根镜像需要的部分：
//   - 目录、普通文件、符号链接（basic 类型的 inode）；
//   - 文件数据按 128 KiB 分块，用 zlib 压缩（内核 CONFIG_SQUASHFS_ZLIB），压不小就原样存；
//   - 不用分片（NO_FRAGMENTS）、不用 xattr、元数据块不压缩；
//   - 属主、权限位、符号链接目标完全取自条目表，时间戳为 0，输出可复现。
//
// 格式参考内核 fs/squashfs/squashfs_fs.h 与 squashfs-tools 的格式说明。
// 选择自写而不用 go-diskfs：后者从宿主目录取文件（属主跟随宿主），且源码包很大、
// 在开发环境里下载不下来；这里的需求很窄，自写更容易用回读测试证明正确。
type Squashfs struct{}

const (
	sqMagic         = 0x73717368 // "hsqs"
	sqBlockSize     = 128 << 10
	sqBlockLog      = 17
	sqMetaSize      = 8192
	sqMetaUncomp    = 0x8000
	sqDataUncomp    = 1 << 24
	sqCompZlib      = 1
	sqNoTable       = ^uint64(0)
	sqNoFragment    = ^uint32(0)
	sqSuperLen      = 96
	sqTypeDir       = 1
	sqTypeFile      = 2
	sqTypeSymlink   = 3
	sqFlagNoFrag    = 0x0010
	sqFlagNoXattr   = 0x0200
	sqFlagUncInode  = 0x0001
	sqFlagUncFrag   = 0x0008
	sqFlagUncIDs    = 0x0800
	sqMaxDirListing = 0xffff - 3
)

var errSquashfs = errors.New("squashfs 格式错误")

// metaWriter 把元数据切成 8 KiB 的块（不压缩，块头带 0x8000）。
type metaWriter struct {
	out []byte // 已写出的完整块（含块头）
	cur []byte // 当前未满的块
}

// ref 返回当前写入位置的引用：(块在表内的偏移 << 16) | 块内偏移。
func (m *metaWriter) ref() (block uint32, offset uint16) {
	return uint32(len(m.out)), uint16(len(m.cur))
}

func (m *metaWriter) write(b []byte) {
	for len(b) > 0 {
		n := sqMetaSize - len(m.cur)
		if n > len(b) {
			n = len(b)
		}
		m.cur = append(m.cur, b[:n]...)
		b = b[n:]
		if len(m.cur) == sqMetaSize {
			m.flush()
		}
	}
}

func (m *metaWriter) flush() {
	if len(m.cur) == 0 {
		return
	}
	var h [2]byte
	binary.LittleEndian.PutUint16(h[:], uint16(len(m.cur))|sqMetaUncomp)
	m.out = append(m.out, h[:]...)
	m.out = append(m.out, m.cur...)
	m.cur = m.cur[:0]
}

func (m *metaWriter) bytes() []byte { m.flush(); return m.out }

type sqNode struct {
	e        *Entry
	name     string
	children []*sqNode
	ino      uint32
	// 写完 inode 后记下它的位置和类型，供父目录的目录项使用。
	refBlock  uint32
	refOffset uint16
	typ       uint16
	// 文件数据位置。
	dataStart uint32
	blocks    []uint32
}

// Write 把树写成 squashfs 镜像。
func (Squashfs) Write(dst string, t *Tree) error {
	root, count, err := sqBuildNodes(t)
	if err != nil {
		return err
	}
	img := make([]byte, sqSuperLen) // 超级块最后再填

	// 1. 文件数据。
	var dataErr error
	sqWalk(root, func(n *sqNode) {
		if dataErr != nil || !n.e.IsRegular() {
			return
		}
		if uint64(len(img)) > uint64(^uint32(0)) {
			dataErr = fmt.Errorf("镜像超过 4 GiB")
			return
		}
		n.dataStart = uint32(len(img))
		for off := 0; off < len(n.e.Data); off += sqBlockSize {
			end := off + sqBlockSize
			if end > len(n.e.Data) {
				end = len(n.e.Data)
			}
			blk := n.e.Data[off:end]
			var zb bytes.Buffer
			zw, _ := zlib.NewWriterLevel(&zb, zlib.BestCompression)
			_, _ = zw.Write(blk)
			_ = zw.Close()
			if zb.Len() < len(blk) {
				img = append(img, zb.Bytes()...)
				n.blocks = append(n.blocks, uint32(zb.Len()))
			} else {
				img = append(img, blk...)
				n.blocks = append(n.blocks, uint32(len(blk))|sqDataUncomp)
			}
		}
	})
	if dataErr != nil {
		return dataErr
	}

	// 2. inode 和目录表：后序遍历，子项的 inode 先写，父目录才能引用它们。
	ids := map[uint32]uint16{}
	var idList []uint32
	idx := func(id int) (uint16, error) {
		if id < 0 {
			return 0, fmt.Errorf("属主 ID 不能为负")
		}
		if i, ok := ids[uint32(id)]; ok {
			return i, nil
		}
		if len(idList) >= 0xffff {
			return 0, fmt.Errorf("属主 ID 太多")
		}
		ids[uint32(id)] = uint16(len(idList))
		idList = append(idList, uint32(id))
		return ids[uint32(id)], nil
	}
	var inodes, dirs metaWriter
	var walkErr error
	parentIno := map[*sqNode]uint32{root: count + 1}
	sqWalk(root, func(n *sqNode) {
		for _, c := range n.children {
			parentIno[c] = n.ino
		}
	})
	sqWalk(root, func(n *sqNode) {
		if walkErr != nil {
			return
		}
		uidx, err := idx(n.e.UID)
		if err != nil {
			walkErr = err
			return
		}
		gidx, err := idx(n.e.GID)
		if err != nil {
			walkErr = err
			return
		}
		var body []byte
		le := binary.LittleEndian
		switch {
		case n.e.IsDir():
			n.typ = sqTypeDir
			listBlock, listOff := dirs.ref()
			listing, err := sqDirListing(n.children)
			if err != nil {
				walkErr = fmt.Errorf("%s：%w", n.e.Path, err)
				return
			}
			dirs.write(listing)
			nlink := uint32(2)
			for _, c := range n.children {
				if c.e.IsDir() {
					nlink++
				}
			}
			body = make([]byte, 16)
			le.PutUint32(body[0:], listBlock)
			le.PutUint32(body[4:], nlink)
			le.PutUint16(body[8:], uint16(len(listing)+3))
			le.PutUint16(body[10:], listOff)
			le.PutUint32(body[12:], parentIno[n])
		case n.e.IsRegular():
			n.typ = sqTypeFile
			if uint64(len(n.e.Data)) > uint64(^uint32(0)) {
				walkErr = fmt.Errorf("%s 超过 4 GiB", n.e.Path)
				return
			}
			body = make([]byte, 16+4*len(n.blocks))
			le.PutUint32(body[0:], n.dataStart)
			le.PutUint32(body[4:], sqNoFragment)
			le.PutUint32(body[8:], 0)
			le.PutUint32(body[12:], uint32(len(n.e.Data)))
			for i, b := range n.blocks {
				le.PutUint32(body[16+4*i:], b)
			}
		case n.e.IsSymlink():
			n.typ = sqTypeSymlink
			body = make([]byte, 8+len(n.e.Link))
			le.PutUint32(body[0:], 1)
			le.PutUint32(body[4:], uint32(len(n.e.Link)))
			copy(body[8:], n.e.Link)
		default:
			walkErr = fmt.Errorf("%s：根镜像不支持这种文件类型", n.e.Path)
			return
		}
		hdr := make([]byte, 16)
		le.PutUint16(hdr[0:], n.typ)
		le.PutUint16(hdr[2:], sqPerm(n.e.Mode))
		le.PutUint16(hdr[4:], uidx)
		le.PutUint16(hdr[6:], gidx)
		le.PutUint32(hdr[8:], 0) // mtime
		le.PutUint32(hdr[12:], n.ino)
		n.refBlock, n.refOffset = inodes.ref()
		inodes.write(hdr)
		inodes.write(body)
	})
	if walkErr != nil {
		return walkErr
	}

	inodeStart := uint64(len(img))
	img = append(img, inodes.bytes()...)
	dirStart := uint64(len(img))
	img = append(img, dirs.bytes()...)

	// 3. ID 表：一个元数据块存 ID，后面跟指向它的 u64 指针。
	var idMeta metaWriter
	for _, id := range idList {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], id)
		idMeta.write(b[:])
	}
	idBlocks := idMeta.bytes()
	idBlockStart := uint64(len(img))
	img = append(img, idBlocks...)
	idTableStart := uint64(len(img))
	for off := 0; off < len(idBlocks); {
		var p [8]byte
		binary.LittleEndian.PutUint64(p[:], idBlockStart+uint64(off))
		img = append(img, p[:]...)
		size := int(binary.LittleEndian.Uint16(idBlocks[off:]) &^ sqMetaUncomp)
		off += 2 + size
	}
	bytesUsed := uint64(len(img))

	// 4. 超级块。
	le := binary.LittleEndian
	sb := img[:sqSuperLen]
	le.PutUint32(sb[0:], sqMagic)
	le.PutUint32(sb[4:], count)
	le.PutUint32(sb[8:], 0)
	le.PutUint32(sb[12:], sqBlockSize)
	le.PutUint32(sb[16:], 0)
	le.PutUint16(sb[20:], sqCompZlib)
	le.PutUint16(sb[22:], sqBlockLog)
	le.PutUint16(sb[24:], sqFlagNoFrag|sqFlagNoXattr|sqFlagUncInode|sqFlagUncFrag|sqFlagUncIDs)
	le.PutUint16(sb[26:], uint16(len(idList)))
	le.PutUint16(sb[28:], 4)
	le.PutUint16(sb[30:], 0)
	le.PutUint64(sb[32:], uint64(root.refBlock)<<16|uint64(root.refOffset))
	le.PutUint64(sb[40:], bytesUsed)
	le.PutUint64(sb[48:], idTableStart)
	le.PutUint64(sb[56:], sqNoTable) // xattr
	le.PutUint64(sb[64:], inodeStart)
	le.PutUint64(sb[72:], dirStart)
	le.PutUint64(sb[80:], sqNoTable) // fragment
	le.PutUint64(sb[88:], sqNoTable) // export

	// 块设备要求按 4 KiB 对齐。
	if pad := len(img) % 4096; pad != 0 {
		img = append(img, make([]byte, 4096-pad)...)
	}
	return os.WriteFile(dst, img, 0o644)
}

func sqPerm(m fs.FileMode) uint16 {
	p := uint16(m.Perm())
	if m&fs.ModeSetuid != 0 {
		p |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		p |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		p |= 0o1000
	}
	return p
}

// sqBuildNodes 把树转成节点，并按后序（子项在前）编号 inode，从 1 开始。
func sqBuildNodes(t *Tree) (*sqNode, uint32, error) {
	nodes := map[string]*sqNode{}
	for _, e := range t.Entries() {
		nodes[e.Path] = &sqNode{e: e, name: path.Base(e.Path)}
	}
	root := nodes["/"]
	if root == nil {
		return nil, 0, fmt.Errorf("树里没有根目录")
	}
	for p, n := range nodes {
		if p == "/" {
			continue
		}
		parent := nodes[path.Dir(p)]
		if parent == nil || !parent.e.IsDir() {
			return nil, 0, fmt.Errorf("%s 的上级不是目录", p)
		}
		if len(n.name) > 255 {
			return nil, 0, fmt.Errorf("%s：文件名超过 255 字节", p)
		}
		parent.children = append(parent.children, n)
	}
	for _, n := range nodes {
		sort.Slice(n.children, func(i, j int) bool { return n.children[i].name < n.children[j].name })
	}
	var count uint32
	sqWalk(root, func(n *sqNode) { count++; n.ino = count })
	return root, count, nil
}

// sqWalk 后序遍历：先子项，后自己。
func sqWalk(n *sqNode, f func(*sqNode)) {
	for _, c := range n.children {
		sqWalk(c, f)
	}
	f(n)
}

// sqDirListing 生成一个目录的目录项。同一组（header）里的子项必须在同一个
// inode 元数据块里、最多 256 个、inode 号与组基准差在 ±32767 以内。
func sqDirListing(children []*sqNode) ([]byte, error) {
	var out []byte
	le := binary.LittleEndian
	for i := 0; i < len(children); {
		base := children[i]
		j := i
		for j < len(children) && j-i < 256 && children[j].refBlock == base.refBlock &&
			int64(children[j].ino)-int64(base.ino) <= 32767 && int64(children[j].ino)-int64(base.ino) >= -32768 {
			j++
		}
		hdr := make([]byte, 12)
		le.PutUint32(hdr[0:], uint32(j-i-1))
		le.PutUint32(hdr[4:], base.refBlock)
		le.PutUint32(hdr[8:], base.ino)
		out = append(out, hdr...)
		for _, c := range children[i:j] {
			ent := make([]byte, 8+len(c.name))
			le.PutUint16(ent[0:], c.refOffset)
			le.PutUint16(ent[2:], uint16(int16(int64(c.ino)-int64(base.ino))))
			le.PutUint16(ent[4:], c.typ)
			le.PutUint16(ent[6:], uint16(len(c.name)-1))
			copy(ent[8:], c.name)
			out = append(out, ent...)
		}
		i = j
	}
	if len(out) > sqMaxDirListing {
		return nil, fmt.Errorf("目录项太多（%d 字节，basic 目录 inode 上限 %d）", len(out), sqMaxDirListing)
	}
	return out, nil
}

// ---- 读取 ----

type sqReader struct {
	img      []byte
	blockSz  uint32
	ids      []uint32
	inodes   []byte         // 解开后的 inode 表
	inodePos map[uint32]int // 块在表内的偏移 → 在 inodes 里的位置
	dirs     []byte
	dirPos   map[uint32]int
}

// Read 把镜像读回成树。
func (Squashfs) Read(src string) (*Tree, error) {
	img, err := os.ReadFile(src)
	if err != nil {
		return nil, err
	}
	return readSquashfs(img)
}

func readSquashfs(img []byte) (*Tree, error) {
	if len(img) < sqSuperLen {
		return nil, fmt.Errorf("%w：文件太短", errSquashfs)
	}
	le := binary.LittleEndian
	sb := img[:sqSuperLen]
	if le.Uint32(sb[0:]) != sqMagic {
		return nil, fmt.Errorf("%w：魔数不对", errSquashfs)
	}
	if le.Uint16(sb[28:]) != 4 {
		return nil, fmt.Errorf("%w：不是 4.x 版本", errSquashfs)
	}
	if c := le.Uint16(sb[20:]); c != sqCompZlib {
		return nil, fmt.Errorf("%w：只支持 zlib 压缩，实际是 %d", errSquashfs, c)
	}
	if le.Uint64(sb[80:]) != sqNoTable || le.Uint32(sb[16:]) != 0 {
		return nil, fmt.Errorf("%w：不支持分片", errSquashfs)
	}
	r := &sqReader{img: img, blockSz: le.Uint32(sb[12:])}
	used := le.Uint64(sb[40:])
	idTable := le.Uint64(sb[48:])
	inodeStart := le.Uint64(sb[64:])
	dirStart := le.Uint64(sb[72:])
	idCount := int(le.Uint16(sb[26:]))
	if used > uint64(len(img)) || inodeStart > dirStart || dirStart > idTable || idTable+8 > used {
		return nil, fmt.Errorf("%w：表的位置不对", errSquashfs)
	}
	firstIDBlock := le.Uint64(img[idTable:])
	if firstIDBlock < dirStart || firstIDBlock > idTable {
		return nil, fmt.Errorf("%w：ID 表位置不对", errSquashfs)
	}
	var err error
	if r.inodes, r.inodePos, err = sqReadMeta(img[inodeStart:dirStart]); err != nil {
		return nil, err
	}
	if r.dirs, r.dirPos, err = sqReadMeta(img[dirStart:firstIDBlock]); err != nil {
		return nil, err
	}
	idData, _, err := sqReadMeta(img[firstIDBlock:idTable])
	if err != nil {
		return nil, err
	}
	if len(idData) < 4*idCount {
		return nil, fmt.Errorf("%w：ID 表太短", errSquashfs)
	}
	for i := 0; i < idCount; i++ {
		r.ids = append(r.ids, le.Uint32(idData[4*i:]))
	}
	t := NewTree()
	rootRef := le.Uint64(sb[32:])
	if err := r.readInode(t, "/", uint32(rootRef>>16), uint16(rootRef), 0); err != nil {
		return nil, err
	}
	return t, nil
}

func sqReadMeta(b []byte) ([]byte, map[uint32]int, error) {
	var out []byte
	pos := map[uint32]int{}
	for off := 0; off < len(b); {
		if off+2 > len(b) {
			return nil, nil, fmt.Errorf("%w：元数据块头被截断", errSquashfs)
		}
		h := binary.LittleEndian.Uint16(b[off:])
		size := int(h &^ sqMetaUncomp)
		if size == 0 || size > sqMetaSize || off+2+size > len(b) {
			return nil, nil, fmt.Errorf("%w：元数据块长度不对", errSquashfs)
		}
		pos[uint32(off)] = len(out)
		blk := b[off+2 : off+2+size]
		if h&sqMetaUncomp == 0 {
			zr, err := zlib.NewReader(bytes.NewReader(blk))
			if err != nil {
				return nil, nil, fmt.Errorf("%w：元数据块解压失败", errSquashfs)
			}
			blk, err = io.ReadAll(io.LimitReader(zr, sqMetaSize))
			if err != nil {
				return nil, nil, fmt.Errorf("%w：元数据块解压失败", errSquashfs)
			}
		}
		out = append(out, blk...)
		off += 2 + size
	}
	return out, pos, nil
}

func (r *sqReader) at(data []byte, pos map[uint32]int, block uint32, offset uint16, n int) ([]byte, error) {
	p, ok := pos[block]
	if !ok {
		return nil, fmt.Errorf("%w：引用了不存在的元数据块", errSquashfs)
	}
	start := p + int(offset)
	if start+n > len(data) || n < 0 {
		return nil, fmt.Errorf("%w：越界读取元数据", errSquashfs)
	}
	return data[start : start+n], nil
}

func (r *sqReader) id(i uint16) (int, error) {
	if int(i) >= len(r.ids) {
		return 0, fmt.Errorf("%w：属主索引越界", errSquashfs)
	}
	return int(r.ids[i]), nil
}

func (r *sqReader) readInode(t *Tree, p string, block uint32, offset uint16, depth int) error {
	if depth > 64 {
		return fmt.Errorf("%w：目录嵌套过深", errSquashfs)
	}
	le := binary.LittleEndian
	hdr, err := r.at(r.inodes, r.inodePos, block, offset, 16)
	if err != nil {
		return err
	}
	typ, perm := le.Uint16(hdr[0:]), le.Uint16(hdr[2:])
	uid, err := r.id(le.Uint16(hdr[4:]))
	if err != nil {
		return err
	}
	gid, err := r.id(le.Uint16(hdr[6:]))
	if err != nil {
		return err
	}
	mode := fs.FileMode(perm & 0o777)
	if perm&0o4000 != 0 {
		mode |= fs.ModeSetuid
	}
	if perm&0o2000 != 0 {
		mode |= fs.ModeSetgid
	}
	if perm&0o1000 != 0 {
		mode |= fs.ModeSticky
	}
	e := Entry{Path: p, UID: uid, GID: gid}
	full, err := r.at(r.inodes, r.inodePos, block, offset, 32)
	switch typ {
	case sqTypeDir:
		if err != nil {
			return err
		}
		body := full[16:]
		e.Mode = fs.ModeDir | mode
		if err := t.Add(e); err != nil {
			return err
		}
		size := int(le.Uint16(body[8:])) - 3
		if size < 0 {
			return fmt.Errorf("%w：%s 目录大小不对", errSquashfs, p)
		}
		if size == 0 {
			return nil // 空目录：没有目录项，不读目录表
		}
		listing, err := r.at(r.dirs, r.dirPos, le.Uint32(body[0:]), le.Uint16(body[10:]), size)
		if err != nil {
			return err
		}
		return r.readDir(t, p, listing, depth)
	case sqTypeFile:
		if err != nil {
			return err
		}
		body := full[16:]
		start, frag, size := le.Uint32(body[0:]), le.Uint32(body[4:]), le.Uint32(body[12:])
		if frag != sqNoFragment {
			return fmt.Errorf("%w：%s 用了分片", errSquashfs, p)
		}
		nblocks := int((uint64(size) + uint64(r.blockSz) - 1) / uint64(r.blockSz))
		sizes, err := r.at(r.inodes, r.inodePos, block, offset, 32+4*nblocks)
		if err != nil {
			return err
		}
		data := make([]byte, 0, size)
		off := uint64(start)
		for i := 0; i < nblocks; i++ {
			bs := le.Uint32(sizes[32+4*i:])
			n := uint64(bs &^ sqDataUncomp)
			if off+n > uint64(len(r.img)) {
				return fmt.Errorf("%w：%s 的数据块越界", errSquashfs, p)
			}
			blk := r.img[off : off+n]
			if bs&sqDataUncomp == 0 {
				zr, err := zlib.NewReader(bytes.NewReader(blk))
				if err != nil {
					return fmt.Errorf("%w：%s 数据块解压失败", errSquashfs, p)
				}
				if blk, err = io.ReadAll(io.LimitReader(zr, int64(r.blockSz))); err != nil {
					return fmt.Errorf("%w：%s 数据块解压失败", errSquashfs, p)
				}
			}
			data = append(data, blk...)
			off += n
		}
		if uint32(len(data)) != size {
			return fmt.Errorf("%w：%s 的长度不对", errSquashfs, p)
		}
		e.Mode, e.Data = mode, data
		return t.Add(e)
	case sqTypeSymlink:
		b, err := r.at(r.inodes, r.inodePos, block, offset, 24)
		if err != nil {
			return err
		}
		n := int(le.Uint32(b[20:]))
		b, err = r.at(r.inodes, r.inodePos, block, offset, 24+n)
		if err != nil {
			return err
		}
		e.Mode, e.Link = fs.ModeSymlink|0o777, string(b[24:])
		return t.Add(e)
	}
	return fmt.Errorf("%w：%s 是不支持的 inode 类型 %d", errSquashfs, p, typ)
}

func (r *sqReader) readDir(t *Tree, dir string, b []byte, depth int) error {
	le := binary.LittleEndian
	for off := 0; off < len(b); {
		if off+12 > len(b) {
			return fmt.Errorf("%w：%s 目录头被截断", errSquashfs, dir)
		}
		count := int(le.Uint32(b[off:])) + 1
		block := le.Uint32(b[off+4:])
		off += 12
		for i := 0; i < count; i++ {
			if off+8 > len(b) {
				return fmt.Errorf("%w：%s 目录项被截断", errSquashfs, dir)
			}
			ioff := le.Uint16(b[off:])
			nlen := int(le.Uint16(b[off+6:])) + 1
			if off+8+nlen > len(b) {
				return fmt.Errorf("%w：%s 目录项被截断", errSquashfs, dir)
			}
			name := string(b[off+8 : off+8+nlen])
			off += 8 + nlen
			if name == "." || name == ".." || strings.Contains(name, "/") {
				return fmt.Errorf("%w：%s 下有非法文件名 %q", errSquashfs, dir, name)
			}
			if err := r.readInode(t, path.Join(dir, name), block, ioff, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}
