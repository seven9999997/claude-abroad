package console

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"testing"
)

func TestHomeDirOf(t *testing.T) {
	lookup := func(id string) (*user.User, error) {
		switch id {
		case "501":
			return &user.User{Uid: "501", HomeDir: "/Users/alice"}, nil
		case "502":
			return &user.User{Uid: "502", HomeDir: "relative"}, nil
		}
		return nil, user.UnknownUserIdError(0)
	}
	if h, err := HomeDirOf(501, lookup); err != nil || h != "/Users/alice" {
		t.Fatalf("%q %v", h, err)
	}
	if _, err := HomeDirOf(502, lookup); err == nil {
		t.Fatal("不是绝对路径应拒绝")
	}
	if _, err := HomeDirOf(503, lookup); err == nil {
		t.Fatal("查不到应报错")
	}
}

// 有人登录时读得到主目录下的文件；没有人登录时一律“不存在”（调用方据此跳过）。
func TestHomeFS(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".zshrc"), []byte("export HTTPS_PROXY=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := HomeFS{Dir: func() (string, error) { return dir, nil }}
	if b, err := fs.ReadFile(h, ".zshrc"); err != nil || string(b) != "export HTTPS_PROXY=x\n" {
		t.Fatalf("%q %v", b, err)
	}
	none := HomeFS{Dir: func() (string, error) { return "", ErrNoConsoleUser }}
	if _, err := fs.ReadFile(none, ".zshrc"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("没有人登录时应是不存在：%v", err)
	}
	if _, err := h.Open("../etc/passwd"); err == nil {
		t.Fatal("不合法的路径应拒绝")
	}
}
