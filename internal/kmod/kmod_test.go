package kmod

import (
	"bytes"
	"compress/gzip"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
)

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(b)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestModulesResolve(t *testing.T) {
	dep := `kernel/fs/squashfs/squashfs.ko.gz: kernel/lib/xz/xz_dec.ko.gz
kernel/lib/xz/xz_dec.ko.gz:
kernel/drivers/block/virtio_blk.ko.gz:
kernel/net/vmw_vsock/vmw_vsock_virtio_transport.ko.gz: kernel/net/vmw_vsock/vmw_vsock_virtio_transport_common.ko.gz kernel/net/vmw_vsock/vsock.ko.gz
kernel/net/vmw_vsock/vmw_vsock_virtio_transport_common.ko.gz: kernel/net/vmw_vsock/vsock.ko.gz
kernel/net/vmw_vsock/vsock.ko.gz:
`
	d, err := ParseModulesDep(strings.NewReader(dep))
	if err != nil {
		t.Fatal(err)
	}
	order, err := d.Resolve([]string{"virtio-blk", "squashfs", "vmw_vsock_virtio_transport", "built_in_thing"}, map[string]bool{"built_in_thing": true})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(order, " ")
	want := "kernel/drivers/block/virtio_blk.ko.gz kernel/lib/xz/xz_dec.ko.gz kernel/fs/squashfs/squashfs.ko.gz " +
		"kernel/net/vmw_vsock/vsock.ko.gz kernel/net/vmw_vsock/vmw_vsock_virtio_transport_common.ko.gz kernel/net/vmw_vsock/vmw_vsock_virtio_transport.ko.gz"
	if got != want {
		t.Fatalf("加载顺序不对：\n%s", got)
	}
	if _, err := d.Resolve([]string{"overlay"}, nil); err == nil {
		t.Error("找不到的模块应报错")
	}
	cyc, _ := ParseModulesDep(strings.NewReader("a.ko: b.ko\nb.ko: a.ko\n"))
	if _, err := cyc.Resolve([]string{"a"}, nil); err == nil {
		t.Error("依赖有环应报错")
	}
	dangling, _ := ParseModulesDep(strings.NewReader("a.ko: missing.ko\n"))
	if _, err := dangling.Resolve([]string{"a"}, nil); err == nil {
		t.Error("依赖不存在应报错")
	}
	if _, err := ParseModulesDep(strings.NewReader("no colon here\n")); err == nil {
		t.Error("格式错误应报错")
	}
}

func TestDecompressModule(t *testing.T) {
	elf := []byte("\x7fELF....")
	if b, err := DecompressModule("a.ko.gz", gz(t, elf)); err != nil || !bytes.Equal(b, elf) {
		t.Errorf(".ko.gz 解压失败：%v", err)
	}
	if b, err := DecompressModule("a.ko", elf); err != nil || !bytes.Equal(b, elf) {
		t.Errorf(".ko 应原样返回：%v", err)
	}
	for _, p := range []string{"a.ko.xz", "a.ko.zst", "a.bin"} {
		if _, err := DecompressModule(p, elf); err == nil {
			t.Errorf("%s 应报不支持", p)
		}
	}
	if UncompressedName("kernel/a/b.ko.gz") != "kernel/a/b.ko" {
		t.Error("UncompressedName 不对")
	}
}

func TestLoaderLoadNames(t *testing.T) {
	elf := func(s string) []byte { return gz(t, []byte("\x7fELF"+s)) }
	dir := fstest.MapFS{
		"modules.dep":      {Data: []byte("kernel/a.ko.gz: kernel/b.ko.gz\nkernel/b.ko.gz:\nkernel/c.ko.gz: kernel/bad.ko.gz\nkernel/bad.ko.gz:\nkernel/d.ko.gz:\n")},
		"modules.builtin":  {Data: []byte("kernel/fs/tmpfs.ko\n")},
		"kernel/a.ko.gz":   {Data: elf("a")},
		"kernel/b.ko.gz":   {Data: elf("b")},
		"kernel/c.ko.gz":   {Data: elf("c")},
		"kernel/bad.ko.gz": {Data: elf("bad")},
		"kernel/d.ko.gz":   {Data: elf("d")},
	}
	var loaded []string
	l := Loader{Dir: dir, Load: func(img []byte) error {
		s := string(img[4:])
		switch s {
		case "bad":
			return syscall.ENOEXEC
		case "d":
			return syscall.EEXIST // 已加载，不算失败
		}
		loaded = append(loaded, s)
		return nil
	}}
	rs, err := l.LoadNames([]string{"a", "tmpfs", "c", "d"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(loaded, ","); got != "b,a" {
		t.Errorf("实际加载顺序 %s", got)
	}
	if got := strings.Join(Failed(rs), ","); got != "bad,c" {
		t.Errorf("失败列表 %s，结果 %+v", got, rs)
	}
	if len(rs) != 5 {
		t.Errorf("tmpfs 是内置的应跳过：%+v", rs)
	}
	if _, err := l.LoadNames([]string{"nope"}); err == nil {
		t.Error("找不到的模块应报错")
	}
	if _, err := (Loader{Dir: fstest.MapFS{}}).LoadNames([]string{"a"}); err == nil {
		t.Error("没有 modules.dep 应报错")
	}
}
