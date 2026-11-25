//go:build !windows

package vmmgr

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func waitDone(t *testing.T, p Proc, what string) {
	t.Helper()
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatalf("%s：进程没有退出", what)
	}
}

// 环境变量不继承（不带代理变量）；Terminate 让它退出。
func TestExecEnvAndTerminate(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	var out syncBuf
	l := &ExecLauncher{Path: "/bin/sh", Args: []string{"-c", `env; echo ready; while :; do sleep 1; done`}, Output: &out}
	p, err := l.Launch()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200 && !strings.Contains(out.String(), "ready"); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Contains(out.String(), "PROXY") {
		t.Fatalf("子进程继承了代理变量：%s", out.String())
	}
	if err := p.Terminate(); err != nil {
		t.Fatal(err)
	}
	waitDone(t, p, "Terminate")
	if p.ExitErr() == nil {
		t.Fatal("被 SIGTERM 结束应有退出原因")
	}
	if p.Terminate() != nil || p.Kill() != nil {
		t.Fatal("已退出后再发信号应无害")
	}
}

// 不理 SIGTERM 的进程：Kill 结束它。
func TestExecKill(t *testing.T) {
	var out syncBuf
	l := &ExecLauncher{Path: "/bin/sh", Args: []string{"-c", `trap "" TERM; echo ready; while :; do sleep 1; done`}, Output: &out}
	p, err := l.Launch()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200 && !strings.Contains(out.String(), "ready"); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	_ = p.Terminate()
	select {
	case <-p.Done():
		t.Fatal("应该不理 SIGTERM")
	case <-time.After(300 * time.Millisecond):
	}
	_ = p.Kill()
	waitDone(t, p, "Kill")
}

// 父进程不在了：标准输入的写端关闭，子进程读到 EOF（abroad-vm --parent-watch 靠这个退出）。
func TestExecStdinEOFWhenParentGone(t *testing.T) {
	l := &ExecLauncher{Path: "/bin/sh", Args: []string{"-c", `cat >/dev/null; exit 0`}}
	pr, err := l.Launch()
	if err != nil {
		t.Fatal(err)
	}
	p := pr.(*execProc)
	select {
	case <-p.Done():
		t.Fatal("写端还在时子进程不应退出")
	case <-time.After(200 * time.Millisecond):
	}
	p.w.Close() // 模拟 abroadd 退出：内核关闭它持有的写端
	waitDone(t, p, "EOF")
	if p.ExitErr() != nil {
		t.Fatalf("读到 EOF 后应正常退出：%v", p.ExitErr())
	}
}

func TestExecBadPath(t *testing.T) {
	if _, err := (&ExecLauncher{}).Launch(); err == nil {
		t.Fatal("没有路径应报错")
	}
	if _, err := (&ExecLauncher{Path: "/nonexistent/abroad-vm"}).Launch(); err == nil {
		t.Fatal("不存在的路径应报错")
	}
}

// 以专用用户运行时附加组只有它自己的主组（不带 root 的附加组）；不切用户时没有凭据。
func TestSysCredentialDropsGroups(t *testing.T) {
	if sysCredential(nil) != nil {
		t.Fatal("不切用户时不应有凭据")
	}
	c := sysCredential(&Credential{UID: 299, GID: 298})
	if c.Uid != 299 || c.Gid != 298 || c.NoSetGroups || len(c.Groups) != 1 || c.Groups[0] != 298 {
		t.Fatalf("凭据 %+v", c)
	}
}
