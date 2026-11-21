package vmhost

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shortTemp 返回一个短路径的临时目录（t.TempDir 在 macOS 上可能超过 unix socket 的长度上限）。
func shortTemp(t *testing.T) string {
	d, err := os.MkdirTemp("", "vmh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestStateDirPermsAndLock(t *testing.T) {
	dir := filepath.Join(shortTemp(t), "state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sd, err := OpenStateDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sd.Close()
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("目录权限应改成 0700：%v", fi.Mode().Perm())
	}
	l, err := ListenUnix(sd.CtlSock())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if fi, _ := os.Stat(sd.CtlSock()); fi.Mode().Perm() != 0o600 {
		t.Errorf("socket 权限应为 0600：%v", fi.Mode().Perm())
	}

	// 第二个实例拿不到锁，且第一个实例的 socket 还在。
	if _, err := OpenStateDir(dir); err == nil || !strings.Contains(err.Error(), "已有实例") {
		t.Fatalf("第二个实例应被拒：%v", err)
	}
	if _, err := os.Lstat(sd.CtlSock()); err != nil {
		t.Fatal("第一个实例的 socket 不应被删")
	}
	sd.Close()
	sd2, err := OpenStateDir(dir)
	if err != nil {
		t.Fatalf("放锁后应能再打开：%v", err)
	}
	sd2.Close()
}

func TestStateDirRejectsSymlink(t *testing.T) {
	base := shortTemp(t)
	real := filepath.Join(base, "real")
	_ = os.Mkdir(real, 0o700)
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStateDir(link); err == nil || !strings.Contains(err.Error(), "符号链接") {
		t.Fatalf("符号链接应被拒：%v", err)
	}
}

func TestListenUnix(t *testing.T) {
	dir := shortTemp(t)
	long := filepath.Join(dir, strings.Repeat("x", MaxSocketPath))
	if _, err := ListenUnix(long); err == nil || !strings.Contains(err.Error(), "超过") {
		t.Fatalf("超长路径应报错：%v", err)
	}

	// 残留的 socket 被替换。
	p := filepath.Join(dir, "s.sock")
	old, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	old.(*net.UnixListener).SetUnlinkOnClose(false)
	old.Close()
	l, err := ListenUnix(p)
	if err != nil {
		t.Fatalf("残留 socket 应被替换：%v", err)
	}
	l.Close()

	// 同名的普通文件不删。
	f := filepath.Join(dir, "plain")
	_ = os.WriteFile(f, []byte("keep"), 0o600)
	if _, err := ListenUnix(f); err == nil {
		t.Fatal("普通文件应报错")
	}
	if b, _ := os.ReadFile(f); string(b) != "keep" {
		t.Fatal("普通文件不应被删")
	}
}

func TestPrepareConsoleLog(t *testing.T) {
	p := filepath.Join(shortTemp(t), "console.log")
	if err := PrepareConsoleLog(p); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("权限应为 0600：%v", fi.Mode().Perm())
	}
	_ = os.WriteFile(p, make([]byte, MaxConsoleLog+1), 0o644)
	if err := PrepareConsoleLog(p); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p + ".1"); fi == nil || fi.Size() != MaxConsoleLog+1 {
		t.Fatal("超过阈值应轮转为 .1")
	}
	if fi, _ := os.Stat(p); fi.Size() != 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("轮转后应新建 0600 的空文件：%v %d", fi.Mode().Perm(), fi.Size())
	}
}

func TestSpecValidate(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"Image", "initramfs.gz", "rootfs.img", "modloop"} {
		_ = os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644)
	}
	s := SpecFromImageDir(dir, filepath.Join(dir, "console.log"))
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"console=hvc0", "panic=0", "abroad.ip=192.168.127.2/24", "abroad.gw=192.168.127.1"} {
		if !strings.Contains(s.Cmdline, want) {
			t.Errorf("cmdline 缺 %s：%s", want, s.Cmdline)
		}
	}
	bad := map[string]func(*Spec){
		"缺内核":     func(s *Spec) { s.Kernel += ".missing" },
		"根盘是目录":   func(s *Spec) { s.RootDisk = dir },
		"CPU 为 0": func(s *Spec) { s.CPUs = 0 },
		"CPU 太多":  func(s *Spec) { s.CPUs = 8 },
		"内存太小":    func(s *Spec) { s.MemoryMiB = 64 },
		"内存太大":    func(s *Spec) { s.MemoryMiB = 4096 },
		"没有控制台日志": func(s *Spec) { s.ConsoleLog = "" },
	}
	for name, f := range bad {
		c := s
		f(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s：应报错", name)
		}
	}
}

func TestSpecNetDevices(t *testing.T) {
	var s Spec
	if n := len(s.NetDevices()); n != 0 {
		t.Fatalf("NetFD 为空时应零块网卡：%d", n)
	}
	s.NetFD = os.Stdin
	d := s.NetDevices()
	if len(d) != 1 || d[0].FD != os.Stdin || d[0].MAC != "02:ab:c0:00:00:02" {
		t.Fatalf("NetFD 非空时应恰好一块文件句柄网卡：%+v", d)
	}
}

// W-1 建议改 1：取不到属主时目录被拒，不跳过检查。
func TestStateDirRejectsUnknownOwner(t *testing.T) {
	old := fileOwner
	t.Cleanup(func() { fileOwner = old })
	fileOwner = func(fs.FileInfo) (int, error) { return 0, errors.New("取不到文件的属主") }
	if _, err := OpenStateDir(t.TempDir()); err == nil || !strings.Contains(err.Error(), "属主") {
		t.Fatalf("取不到属主时应拒绝：%v", err)
	}
	fileOwner = func(fs.FileInfo) (int, error) { return os.Getuid() + 1, nil }
	if _, err := OpenStateDir(t.TempDir()); err == nil || !strings.Contains(err.Error(), "属主不是当前用户") {
		t.Fatalf("属主不对时应拒绝：%v", err)
	}
}
