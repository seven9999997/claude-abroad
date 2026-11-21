package imagebuild

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func sampleRoot(t *testing.T) *Tree {
	tr := NewTree()
	random := make([]byte, 300<<10) // 不可压缩，跨 3 个数据块
	_, _ = rand.Read(random)
	must(t, tr.AddFile("/bin/busybox", 0o755, bytes.Repeat([]byte("busybox!"), 50000))) // 可压缩，跨多块
	must(t, tr.AddSymlink("/bin/sh", "/bin/busybox"))
	must(t, tr.AddFile("/usr/bin/random", 0o755, random))
	must(t, tr.AddFile("/etc/empty", 0o644, nil))
	must(t, tr.AddFile("/etc/secret", 0o600, []byte("token")))
	must(t, tr.Add(Entry{Path: "/etc/shadow", Mode: 0o640, GID: 42, Data: []byte("root:*:")}))
	must(t, tr.Add(Entry{Path: "/bin/bbsuid", Mode: 0o755 | fs.ModeSetuid, Data: []byte("suid")}))
	must(t, tr.Add(Entry{Path: "/tmp", Mode: fs.ModeDir | fs.ModeSticky | 0o777}))
	must(t, tr.AddDir("/empty-dir", 0o700))
	must(t, tr.AddSymlink("/etc/long-link", string(bytes.Repeat([]byte("x"), 200))))
	// 一个大目录：目录项和 inode 都跨过 8 KiB 的元数据块。
	for i := 0; i < 600; i++ {
		must(t, tr.AddSymlink(fmt.Sprintf("/usr/sbin/applet-%03d", i), "/bin/busybox"))
	}
	return tr
}

func treesEqual(t *testing.T, want, got *Tree) {
	t.Helper()
	w, g := want.Entries(), got.Entries()
	if len(w) != len(g) {
		t.Fatalf("条目数不一致：写入 %d，读回 %d", len(w), len(g))
	}
	for i := range w {
		a, b := w[i], g[i]
		if a.Path != b.Path {
			t.Fatalf("第 %d 项路径不一致：%s vs %s", i, a.Path, b.Path)
		}
		if a.Mode != b.Mode {
			t.Errorf("%s 的 mode 不一致：%v vs %v", a.Path, a.Mode, b.Mode)
		}
		if a.UID != b.UID || a.GID != b.GID {
			t.Errorf("%s 的属主不一致：%d:%d vs %d:%d", a.Path, a.UID, a.GID, b.UID, b.GID)
		}
		if a.Link != b.Link {
			t.Errorf("%s 的链接目标不一致", a.Path)
		}
		if !bytes.Equal(a.Data, b.Data) {
			t.Errorf("%s 的内容不一致（%d vs %d 字节）", a.Path, len(a.Data), len(b.Data))
		}
	}
}

func TestSquashfsRoundTrip(t *testing.T) {
	tr := sampleRoot(t)
	p := filepath.Join(t.TempDir(), "rootfs.img")
	must(t, Squashfs{}.Write(p, tr))
	raw, _ := os.ReadFile(p)
	if string(raw[:4]) != "hsqs" || len(raw)%4096 != 0 {
		t.Fatalf("镜像头或对齐不对：%q，%d 字节", raw[:4], len(raw))
	}
	got, err := Squashfs{}.Read(p)
	must(t, err)
	treesEqual(t, tr, got)
	if e, _ := got.Get("/"); e.UID != 0 || e.GID != 0 || e.Mode != fs.ModeDir|0o755 {
		t.Errorf("根目录应为 0755 root:root：%+v", e)
	}

	// 同样的输入，输出逐字节相同（可复现）。
	p2 := filepath.Join(t.TempDir(), "again.img")
	must(t, Squashfs{}.Write(p2, sampleRoot(t)))
	// 随机文件内容不同，所以换成固定内容再比较。
	a, b := NewTree(), NewTree()
	for _, x := range []*Tree{a, b} {
		must(t, x.AddFile("/a", 0o644, []byte("same")))
		must(t, x.AddSymlink("/b", "a"))
	}
	pa, pb := filepath.Join(t.TempDir(), "a.img"), filepath.Join(t.TempDir(), "b.img")
	must(t, Squashfs{}.Write(pa, a))
	must(t, Squashfs{}.Write(pb, b))
	ra, _ := os.ReadFile(pa)
	rb, _ := os.ReadFile(pb)
	if !bytes.Equal(ra, rb) {
		t.Error("同样的输入应生成同样的镜像")
	}
}

func TestSquashfsEmptyRoot(t *testing.T) {
	p := filepath.Join(t.TempDir(), "empty.img")
	must(t, Squashfs{}.Write(p, NewTree()))
	got, err := Squashfs{}.Read(p)
	must(t, err)
	if got.Len() != 1 {
		t.Fatalf("空树读回应只有根目录，实际 %d 项", got.Len())
	}
}

func TestSquashfsRejects(t *testing.T) {
	if err := NewTree().Add(Entry{Path: "/x", Mode: 0o4755}); err == nil {
		t.Error("权限位里混入 0o4000 应拒绝")
	}
	tr := NewTree()
	must(t, tr.AddCharDev("/dev/console", 0o600, 5, 1))
	if err := (Squashfs{}).Write(filepath.Join(t.TempDir(), "x.img"), tr); err == nil {
		t.Error("根镜像不支持设备节点，应报错")
	}
	if _, err := readSquashfs([]byte("not squashfs at all, not even close.....................................................................")); !errors.Is(err, errSquashfs) {
		t.Errorf("非 squashfs 应报 errSquashfs：%v", err)
	}
	// 破坏一个有效镜像的表位置。
	p := filepath.Join(t.TempDir(), "ok.img")
	must(t, Squashfs{}.Write(p, sampleRoot(t)))
	raw, _ := os.ReadFile(p)
	bad := append([]byte(nil), raw...)
	copy(bad[64:72], []byte{0xff, 0xff, 0xff, 0xff, 0, 0, 0, 0}) // inode 表起点
	if _, err := readSquashfs(bad); err == nil {
		t.Error("表位置被破坏应报错")
	}
}

// 不经过自写读取器，直接按字节检查内核挂载时会做的几项检查
// （fs/squashfs/super.c、id.c）。
func TestSquashfsKernelMountConstraints(t *testing.T) {
	tr := sampleRoot(t)
	must(t, tr.AddFile("/edge/exact-128k", 0o644, bytes.Repeat([]byte{1}, 128<<10)))
	must(t, tr.AddFile("/edge/exact-256k", 0o644, bytes.Repeat([]byte{2}, 256<<10)))
	for i := 0; i < 320; i++ {
		must(t, tr.AddDir(fmt.Sprintf("/many/d%03d", i), 0o755))
	}
	p := filepath.Join(t.TempDir(), "k.img")
	must(t, Squashfs{}.Write(p, tr))
	img, _ := os.ReadFile(p)
	le := binary.LittleEndian
	noIDs := uint64(le.Uint16(img[26:]))
	rootRef := le.Uint64(img[32:])
	bytesUsed := le.Uint64(img[40:])
	idTable := le.Uint64(img[48:])
	inodeTable := le.Uint64(img[64:])
	dirTable := le.Uint64(img[72:])
	if le.Uint32(img[0:]) != 0x73717368 || le.Uint16(img[28:]) != 4 || le.Uint16(img[30:]) != 0 {
		t.Fatal("魔数或版本不对")
	}
	if 1<<le.Uint16(img[22:]) != le.Uint32(img[12:]) {
		t.Fatal("block_log 与 block_size 不匹配")
	}
	if le.Uint16(img[24:])&0x0400 != 0 {
		t.Fatal("不应设置压缩器选项标志")
	}
	if noIDs == 0 {
		t.Fatal("no_ids 不能为 0")
	}
	if bytesUsed > uint64(len(img)) {
		t.Fatal("bytes_used 超过文件长度")
	}
	idBlocks := (noIDs*4 + 8191) / 8192
	if bytesUsed-idTable != 8*idBlocks {
		t.Fatalf("id 索引表长度应恰好为 8×块数：%d vs %d", bytesUsed-idTable, 8*idBlocks)
	}
	firstID := le.Uint64(img[idTable:])
	if !(inodeTable < dirTable && dirTable <= firstID && firstID < idTable) {
		t.Fatalf("表顺序不对：inode %d dir %d id0 %d idx %d", inodeTable, dirTable, firstID, idTable)
	}
	if idTable-firstID > 8194 {
		t.Fatal("ID 块超过一个元数据块的上限")
	}
	if rootRef&0xffff >= 8192 {
		t.Fatal("root 引用的块内偏移越界")
	}
	if le.Uint64(img[56:]) != ^uint64(0) || le.Uint64(img[80:]) != ^uint64(0) || le.Uint64(img[88:]) != ^uint64(0) || le.Uint32(img[16:]) != 0 {
		t.Fatal("xattr、fragment、export 应为空")
	}
	got, err := Squashfs{}.Read(p)
	must(t, err)
	treesEqual(t, tr, got)
}
