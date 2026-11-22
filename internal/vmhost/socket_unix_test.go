//go:build unix

package vmhost

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func privTemp(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "plsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	if err := os.Chmod(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestListenPrivateUnix(t *testing.T) {
	d := privTemp(t)
	p := filepath.Join(d, "a.sock")
	l, err := ListenPrivateUnix(p, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(p)
	if err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("应是 0600 的 socket：%v %v", fi, err)
	}
	if _, err := os.Lstat(p + "~"); !os.IsNotExist(err) {
		t.Fatal("临时名字应已改名")
	}
	go func() {
		if c, err := l.Accept(); err == nil {
			c.Write([]byte("ok"))
			c.Close()
		}
	}()
	c, err := net.Dial("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2)
	if _, err := c.Read(buf); err != nil || string(buf) != "ok" {
		t.Fatalf("%q %v", buf, err)
	}
	c.Close()
	l.Close()
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Fatal("关闭后应删除 socket")
	}
}

func TestListenPrivateUnixRejectsDir(t *testing.T) {
	d := privTemp(t)
	if err := os.Chmod(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ListenPrivateUnix(filepath.Join(d, "a.sock"), os.Getuid()); !errors.Is(err, ErrSocketDir) {
		t.Fatalf("0755 的目录应拒绝：%v", err)
	}
	if _, err := os.Lstat(filepath.Join(d, "a.sock~")); !os.IsNotExist(err) {
		t.Fatal("拒绝时不应留下任何 socket")
	}
	os.Chmod(d, 0o700)
	if _, err := ListenPrivateUnix(filepath.Join(d, "a.sock"), os.Getuid()+1); !errors.Is(err, ErrSocketDir) {
		t.Fatalf("属主不对应拒绝：%v", err)
	}
	link := filepath.Join(privTemp(t), "link")
	if err := os.Symlink(d, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ListenPrivateUnix(filepath.Join(link, "a.sock"), os.Getuid()); !errors.Is(err, ErrSocketDir) {
		t.Fatalf("目录是符号链接应拒绝：%v", err)
	}
	if _, err := ListenPrivateUnix(filepath.Join(d, "missing", "a.sock"), os.Getuid()); !errors.Is(err, ErrSocketDir) {
		t.Fatalf("目录不存在应拒绝：%v", err)
	}
	// 临时名字上有不是 socket 的文件：拒绝，不删它。
	os.WriteFile(filepath.Join(d, "b.sock~"), []byte("x"), 0o600)
	if _, err := ListenPrivateUnix(filepath.Join(d, "b.sock"), os.Getuid()); err == nil {
		t.Fatal("临时名字上有普通文件时应拒绝")
	}
}
