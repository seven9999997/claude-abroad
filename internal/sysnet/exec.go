package sysnet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"slices"
	"strings"
)

// Runner 执行一条系统命令（path 是绝对路径，args 不含程序名）。真实实现是 ExecRunner；
// 会话里的只读实跑必须包一层 ReadOnlyRunner；单测用记录型的假 Runner。
type Runner interface {
	Run(ctx context.Context, path string, args []string, stdin []byte) (stdout, stderr []byte, err error)
}

// 允许执行的程序（M2-7d 决定 1）。sysnet 里只有本文件可以出现这些路径和 os/exec。
const (
	PathRoute        = "/sbin/route"
	PathNetworksetup = "/usr/sbin/networksetup"
	PathScutil       = "/usr/sbin/scutil"
	PathIfconfig     = "/sbin/ifconfig"
	PathNetstat      = "/usr/sbin/netstat"
	PathPs           = "/bin/ps" // M2-8：冲突检查看有没有代理软件在运行
	// M4-5：改 Mac 的时区、自动时区、语言（m4-plan v2.4 决定 7）。用户级的语言经 launchctl asuser + sudo -u 执行。
	PathSystemsetup = "/usr/sbin/systemsetup"
	PathDefaults    = "/usr/bin/defaults"
	PathLaunchctl   = "/bin/launchctl"
	PathSudo        = "/usr/bin/sudo"
)

var allowedPaths = []string{PathRoute, PathNetworksetup, PathScutil, PathIfconfig, PathNetstat, PathPs,
	PathSystemsetup, PathDefaults, PathLaunchctl, PathSudo}

// ErrPathNotAllowed 是要执行的程序不在白名单里。
var ErrPathNotAllowed = errors.New("不允许执行的程序")

// ErrNotReadOnly 是 ReadOnlyRunner 拒绝了一条可能改动系统的命令。
var ErrNotReadOnly = errors.New("只读模式下拒绝执行改动类命令")

// ExecRunner 真正执行命令，只允许上面 10 个绝对路径。环境变量固定为 C 区域设置（输出格式稳定）。
type ExecRunner struct{}

// Run 实现 Runner。
func (ExecRunner) Run(ctx context.Context, path string, args []string, stdin []byte) ([]byte, []byte, error) {
	if !slices.Contains(allowedPaths, path) {
		return nil, nil, fmt.Errorf("%w：%q", ErrPathNotAllowed, path)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = []string{"LC_ALL=C", "LANG=C", "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	return so.Bytes(), se.Bytes(), err
}

// ReadOnlyRunner 只放行只读查询（M2-7d 决定 2），其余在执行前拒绝：
//   - route -n get <地址>
//   - netstat -rn -f inet
//   - networksetup -listallnetworkservices、networksetup -getinfo <服务>
//   - scutil --dns、scutil --nwi、scutil --proxy
//   - scutil 经 stdin，且每一行都是 show 或 list（空行和 quit 也可以）
//   - route -n get default、ps -axo comm（M2-8 增补）
type ReadOnlyRunner struct{ R Runner }

// Run 实现 Runner。
func (r ReadOnlyRunner) Run(ctx context.Context, path string, args []string, stdin []byte) ([]byte, []byte, error) {
	if err := readOnly(path, args, stdin); err != nil {
		return nil, nil, err
	}
	return r.R.Run(ctx, path, args, stdin)
}

func readOnly(path string, args []string, stdin []byte) error {
	deny := func() error { return fmt.Errorf("%w：%s %s", ErrNotReadOnly, path, strings.Join(args, " ")) }
	switch path {
	case PathRoute:
		if len(args) == 3 && args[0] == "-n" && args[1] == "get" && stdin == nil {
			if _, err := netip.ParseAddr(args[2]); err == nil || args[2] == "default" {
				return nil
			}
		}
	case PathPs:
		if slices.Equal(args, []string{"-axo", "comm"}) && stdin == nil {
			return nil
		}
	case PathNetstat:
		if slices.Equal(args, []string{"-rn", "-f", "inet"}) && stdin == nil {
			return nil
		}
	case PathNetworksetup:
		if stdin != nil {
			return deny()
		}
		if slices.Equal(args, []string{"-listallnetworkservices"}) {
			return nil
		}
		if len(args) == 2 && args[0] == "-getinfo" && ValidService(args[1]) == nil {
			return nil
		}
	case PathScutil:
		if stdin == nil && len(args) == 1 && (args[0] == "--dns" || args[0] == "--nwi" || args[0] == "--proxy") {
			return nil
		}
		if len(args) == 0 && stdin != nil {
			for _, l := range strings.Split(string(stdin), "\n") {
				f := strings.Fields(l)
				if len(f) == 0 || (len(f) == 1 && f[0] == "quit") {
					continue
				}
				if f[0] != "show" && f[0] != "list" {
					return deny()
				}
			}
			return nil
		}
	}
	return deny()
}
