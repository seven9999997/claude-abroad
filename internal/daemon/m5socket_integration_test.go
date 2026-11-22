//go:build integration

package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// m5-plan v2.1 验收 j 的 root 部分（会话里不跑，留给用户）：以 root 运行，真实模式的 socket 放在 root 所有、0755 的目录里，
// socket 0666；以 root 连得上，属主和权限符合要求。
//
//	sudo go test -tags integration -run TestSharedSocketAsRoot ./internal/daemon/
func TestSharedSocketAsRoot(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("要以 root 运行")
	}
	sockDir := t.TempDir()
	if err := os.Chmod(sockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	o := e.opts()
	o.SocketDir = sockDir
	d := e.start(o)
	fi, err := os.Lstat(filepath.Join(sockDir, SocketName))
	if err != nil || fi.Mode().Perm() != 0o666 {
		t.Fatalf("socket 应是 0666：%v %v", fi, err)
	}
	if code, _ := call(t, d, "GET", "/v1/status", nil); code != 200 {
		t.Fatalf("root 应连得上：%d", code)
	}
}
