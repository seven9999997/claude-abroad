package main

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"claude-abroad/internal/claudeproc"
	"claude-abroad/internal/daemon"
	"claude-abroad/internal/resolverlock"
	"claude-abroad/internal/vmmgr"
)

// M6-2a：--vm-exec 相关的参数检查。
func TestCheckVMFlags(t *testing.T) {
	ok := realFlags{vmDir: "/v", mainStore: "m", exitHost: "h", pfUser: "_abroad"}
	good := ok
	good.vmExec, good.vmUser, good.vmArgs = "/usr/local/libexec/abroad-vm", "_abroad", []string{"--image-dir=/img"}
	if err := checkRealFlags(good); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*realFlags){
		"没有 vm-exec 却给了 vm-user": func(f *realFlags) { f.vmUser = "_abroad" },
		"没有 vm-exec 却给了 vm-arg":  func(f *realFlags) { f.vmArgs = []string{"--cpus=2"} },
		"vm-exec 不是绝对路径":         func(f *realFlags) { f.vmExec = "abroad-vm" },
		"vm-arg 里有 state-dir":    func(f *realFlags) { f.vmExec = "/x"; f.vmArgs = []string{"--state-dir=/tmp/x"} },
		"vm-arg 里有 parent-watch": func(f *realFlags) { f.vmExec = "/x"; f.vmArgs = []string{"-parent-watch"} },
	}
	for name, mut := range bad {
		f := ok
		mut(&f)
		if checkRealFlags(f) == nil {
			t.Errorf("%s 应被拒绝", name)
		}
	}
}

// 没有 --vm-exec：虚拟机由外部启动，和以前相同；有：由 abroadd 管理，参数固定带 --state-dir 和 --parent-watch。
func TestVMLifecycle(t *testing.T) {
	noLookup := func(string) (string, string, error) { t.Fatal("不应查用户"); return "", "", nil }
	v, err := vmLifecycle(realFlags{vmDir: "/v"}, noLookup, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v.(daemon.ExternalVM); !ok {
		t.Fatalf("没有 --vm-exec 时应是外部启动：%T", v)
	}
	lookup := func(name string) (string, string, error) {
		if name != "_abroad" {
			return "", "", errors.New("没有这个用户")
		}
		return "299", "298", nil
	}
	f := realFlags{vmDir: "/v", vmExec: "/opt/abroad-vm", vmUser: "_abroad", vmArgs: []string{"--image-dir=/img"}}
	v, err = vmLifecycle(f, lookup, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := v.(*vmmgr.Manager)
	if !ok {
		t.Fatalf("应是管理器：%T", v)
	}
	l := m.Launcher.(*vmmgr.ExecLauncher)
	if l.Path != "/opt/abroad-vm" || !reflect.DeepEqual(l.Args, []string{"run", "--state-dir", "/v", "--parent-watch", "--image-dir=/img"}) {
		t.Fatalf("参数 %q %q", l.Path, l.Args)
	}
	if l.RunAs == nil || l.RunAs.UID != 299 || l.RunAs.GID != 298 {
		t.Fatalf("用户 %+v", l.RunAs)
	}
	if m.Health == nil {
		t.Fatal("没有健康检查")
	}
	// 不给 --vm-user：和 abroadd 同一个用户
	f.vmUser = ""
	v, _ = vmLifecycle(f, noLookup, io.Discard)
	if v.(*vmmgr.Manager).Launcher.(*vmmgr.ExecLauncher).RunAs != nil {
		t.Fatal("不给 --vm-user 时不应切用户")
	}
	// 查不到用户：报错，不启动
	f.vmUser = "nobody-here"
	if _, err := vmLifecycle(f, lookup, io.Discard); err == nil {
		t.Fatal("查不到用户应报错")
	}
	if _, err := vmCredential("x", func(string) (string, string, error) { return "a", "1", nil }); err == nil {
		t.Fatal("uid 不是数字应报错")
	}
}

// M6-3：真实模式的解析锁指向 /etc/resolver、记录在状态目录、默认锁内置置顶域名；这里只组装，不写任何文件。
func TestResolverLockWiring(t *testing.T) {
	l := resolverLock(realFlags{stateDir: "/st"})
	if l.Dir != "/etc/resolver" || l.RecordDir != "/st" || len(l.Domains) != 0 {
		t.Fatalf("%+v", l)
	}
	l = resolverLock(realFlags{stateDir: "/st", resolverTest: true})
	if len(l.Domains) != 1 || l.Domains[0] != resolverlock.TestDomain {
		t.Fatalf("测试域名：%+v", l)
	}
}

// M6-3b：真实模式的托管设置指向 Claude Code 的托管设置目录、记录在状态目录；本机代理端口在回环上的固定端口。只组装，不写文件。
func TestClaudeSettingsWiring(t *testing.T) {
	s := claudeSettings(realFlags{stateDir: "/st"})
	if s.Dir != "/Library/Application Support/ClaudeCode/managed-settings.d" || s.RecordDir != "/st" {
		t.Fatalf("%+v", s)
	}
	if RealClaudeProxyAddr != "127.0.0.1:47821" {
		t.Fatal(RealClaudeProxyAddr)
	}
}

// M6-4b-1 返工（必须改 1）：启动管控只有带 --launch-control 才接——不带时解析锁、托管设置、第三层、首次准备、本机代理端口
// 全都没有（真实模式和 M6 之前相同，M2–M5 的真机脚本就是这样运行的）；带上是原来的行为。
func TestLaunchControlParts(t *testing.T) {
	g := &claudeproc.LaunchGuard{}
	off := launchControlParts(realFlags{stateDir: "/st"}, g)
	if off.lock != nil || off.settings != nil || off.guard != nil || off.firstRun || off.proxyAddr != "" {
		t.Fatalf("不带 --launch-control 时不应有任何管控：%+v", off)
	}
	on := launchControlParts(realFlags{stateDir: "/st", launchControl: true}, g)
	if on.lock == nil || on.lock.Dir != RealResolverDir || on.settings == nil || on.guard != g || !on.firstRun || on.proxyAddr != RealClaudeProxyAddr {
		t.Fatalf("带上时应接上全部：%+v", on)
	}
}

// --launch-control 是正式参数（发布构建也有）。
func TestLaunchControlFlagInRelease(t *testing.T) {
	var help bytes.Buffer
	run([]string{"-h"}, &help, nil)
	if !strings.Contains(help.String(), "launch-control") {
		t.Fatal("发布构建应有 --launch-control")
	}
}
