package main

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"claude-abroad/internal/agent"
)

// pid1Sys 是 PID 1 路径用的假系统：记录是否关机；KernelRelease 可以设成 panic。
type pid1Sys struct {
	panicOnKernel bool
	poweroff      int
	syncs         int
}

func (s *pid1Sys) Mount(string, string, string, uintptr, string) error { return nil }
func (s *pid1Sys) MoveMount(string, string) error                      { return nil }
func (s *pid1Sys) LoadModule([]byte) error                             { return nil }
func (s *pid1Sys) KernelRelease() (string, error) {
	if s.panicOnKernel {
		panic("模拟 panic")
	}
	return "", syscall.ENOSYS
}
func (s *pid1Sys) SwitchRoot(string, []string) error { return syscall.ENOSYS }
func (s *pid1Sys) Sync()                             { s.syncs++ }
func (s *pid1Sys) PowerOff()                         { s.poweroff++ }
func (s *pid1Sys) DisableCtrlAltDel() error          { return nil }
func (s *pid1Sys) Spawn([]string) (int, error)       { return 0, syscall.ENOENT }
func (s *pid1Sys) WaitAny() (agent.WaitResult, error) {
	return agent.WaitResult{}, syscall.ECHILD
}
func (s *pid1Sys) Kill(int, syscall.Signal) error                      { return nil }
func (s *pid1Sys) ListenVsock(uint32) (net.Listener, error)            { return nil, syscall.ENOSYS }
func (s *pid1Sys) SetTime(time.Time) error                             { return nil }
func (s *pid1Sys) LoopbackUp(context.Context) error                    { return nil }
func (s *pid1Sys) ConfigureNet(context.Context, agent.NetConfig) error { return nil }

// 作为 PID 1 时，任何路径最后都 sync + 关机。
func TestPID1AlwaysPowersOff(t *testing.T) {
	for name, c := range map[string]struct {
		args  []string
		panic bool
	}{
		"/init 启动（boot 失败）":   {[]string{"/init"}, false},
		"boot 子命令":            {[]string{"/sbin/abroad-agent", "boot"}, false},
		"init 子命令（serve 起不来）": {[]string{"/sbin/abroad-agent", "init"}, false},
		"不认识的子命令":             {[]string{"/sbin/abroad-agent", "whatever"}, false},
		"没有参数":                {[]string{"/sbin/abroad-agent"}, false},
		"serve 不能当 PID 1":     {[]string{"/sbin/abroad-agent", "serve"}, false},
		"boot 里 panic":        {[]string{"/init"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			s := &pid1Sys{panicOnKernel: c.panic}
			var w bytes.Buffer
			dispatch(s, c.args, true, &w, t.TempDir())
			if s.poweroff != 1 || s.syncs != 1 {
				t.Fatalf("应 sync + 关机各一次：sync %d，poweroff %d\n%s", s.syncs, s.poweroff, w.String())
			}
			if c.panic && !strings.Contains(w.String(), "模拟 panic") {
				t.Errorf("panic 应打印到控制台：%s", w.String())
			}
		})
	}
}

func TestNonPID1Dispatch(t *testing.T) {
	s := &pid1Sys{}
	for args, want := range map[string]int{
		"version": 0,
		"boot":    2,
		"init":    2,
		"":        2,
		"serve":   1, // 非 linux 上 KernelRelease 失败
	} {
		var w bytes.Buffer
		argv := []string{"abroad-agent"}
		if args != "" {
			argv = append(argv, args)
		}
		if got := dispatch(s, argv, false, &w, t.TempDir()); got != want {
			t.Errorf("%q：退出码 %d，应为 %d（%s）", args, got, want, w.String())
		}
	}
	if s.poweroff != 0 {
		t.Error("不是 PID 1 时不能关机")
	}
}

// PID 1 路径（boot、init）的源码里不许出现 os.Exit、log.Fatal*、log.Panic*：
// PID 1 退出等于内核 panic。os.Exit 只允许出现在 exit.go（只在不是 PID 1 时调用）。
func TestNoExitOnPID1Paths(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	more, _ := filepath.Glob("../../internal/agent/*.go")
	files = append(files, more...)
	banned := map[string]bool{"os.Exit": true, "log.Fatal": true, "log.Fatalf": true, "log.Fatalln": true,
		"log.Panic": true, "log.Panicf": true, "log.Panicln": true, "syscall.Exit": true, "unix.Exit": true}
	checked := 0
	for _, p := range files {
		if strings.HasSuffix(p, "_test.go") || filepath.Base(p) == "exit.go" {
			continue
		}
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && banned[id.Name+"."+sel.Sel.Name] {
				t.Errorf("%s 用了 %s.%s", p, id.Name, sel.Sel.Name)
			}
			return true
		})
	}
	if checked < 5 {
		t.Fatalf("只扫到 %d 个文件，路径不对？", checked)
	}
}
