//go:build integration && darwin

package procinfo

import (
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// f. 只读实跑（m3-plan M3-3）：只读 sysctl 和文件，只连本机回环，不改宿主机网络，不用 sudo。
// 以普通用户运行时只能看到自己用户的套接字和进程路径（m3-plan v2.4 的实测结论）。
func sysIdentifier() *Identifier {
	return &Identifier{Table: SysTable{}, Procs: SysProcs{}, Bundle: (&FileBundles{}).Read}
}

func addrPort(a net.Addr) netip.AddrPort { return netip.MustParseAddrPort(a.String()) }

func self(t *testing.T) string {
	p, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p
}

// samePath 比较两个路径（/var 是 /private/var 的符号链接）。
func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

func TestReadOnlyIdentifySelf(t *testing.T) {
	id := sysIdentifier()
	me := self(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	for i := 0; i < 5; i++ {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		r := id.Identify(bg, "tcp", addrPort(c.LocalAddr()), addrPort(ln.Addr()))
		c.Close()
		if r.PID != os.Getpid() || !samePath(r.Path, me) {
			t.Fatalf("TCP 第 %d 条：%+v，期望本进程 %d", i, r, os.Getpid())
		}
	}
	u, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	for i := 0; i < 3; i++ {
		c, err := net.DialUDP("udp", nil, u.LocalAddr().(*net.UDPAddr))
		if err != nil {
			t.Fatal(err)
		}
		r := id.Identify(bg, "udp", addrPort(c.LocalAddr()), addrPort(u.LocalAddr()))
		c.Close()
		if r.PID != os.Getpid() || !samePath(r.Path, me) {
			t.Fatalf("UDP 第 %d 条：%+v", i, r)
		}
	}
	t.Logf("TCP 5 条、UDP 3 条都识别为本测试进程（pid %d）", os.Getpid())
}

// 普通用户只能在连接表里看到本进程自己的套接字（2026-11-19 在 macOS 27.2.1 实测，netstat 也一样），
// 所以识别别的进程要 root。会话里跳过，M3-8 的用户脚本以 root 跑。
func TestReadOnlyIdentifyChild(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("普通用户看不到别的进程的套接字，需要 root（M3-8）")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	cmd := exec.Command("/usr/bin/nc", "127.0.0.1", port)
	stdin, _ := cmd.StdinPipe()
	if err := cmd.Start(); err != nil {
		t.Skip("起不了 nc：", err)
	}
	defer func() { stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r := sysIdentifier().Identify(bg, "tcp", addrPort(c.RemoteAddr()), addrPort(ln.Addr()))
	if r.PID != cmd.Process.Pid || r.Path != "/usr/bin/nc" || r.App != "path:/usr/bin/nc" {
		t.Fatalf("子进程：%+v，期望 nc 的进程 %d", r, cmd.Process.Pid)
	}
	t.Logf("子进程 nc（pid %d）识别为 %s", cmd.Process.Pid, r.App)
}

// h. 只读实跑：读一个子进程（测试程序自己，带上设置的环境变量）的环境，能看到变量名（值不出来）。
// 子进程睡 2 秒后自己退出，不向它发任何信号。
func TestReadOnlyOwnEnv(t *testing.T) {
	if os.Getenv("ABROAD_ENV_HELPER") == "1" {
		time.Sleep(2 * time.Second)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestReadOnlyOwnEnv$")
	cmd.Env = append(os.Environ(), "ABROAD_ENV_HELPER=1", "HTTPS_PROXY=http://proxy.invalid:1", "LANG=en_US.UTF-8")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	time.Sleep(300 * time.Millisecond)
	env, err := SysProcs{}.Env(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env.ProxyVars, "HTTPS_PROXY") || env.Lang != "en_US.UTF-8" {
		t.Fatalf("%+v", env)
	}
	for _, v := range env.ProxyVars {
		if strings.Contains(v, "proxy.invalid") {
			t.Fatal("值不应出来")
		}
	}
}
