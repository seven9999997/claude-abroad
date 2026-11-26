package pf

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand/v2"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func mustRender(t *testing.T, c Config) []byte {
	t.Helper()
	b, err := Render(c)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func pk(iface, user, proto string, sport, dport uint16, dst string) Packet {
	return Packet{If: iface, User: user, Proto: proto, SrcPort: sport, DstPort: dport, Dst: netip.MustParseAddr(dst)}
}

// d. 求值表
func TestEvalTable(t *testing.T) {
	full := mustRender(t, fullCfg)
	nolan := mustRender(t, Config{UtunName: "utun5", VMIfs: fullCfg.VMIfs, User: "_abroad"})
	boot := mustRender(t, Config{LAN: fullCfg.LAN, VMIfs: fullCfg.VMIfs, User: "_abroad"})
	evil := mustRender(t, Config{UtunName: "utun5", LAN: LANPrefixes(pfx("160.79.104.0/23")), User: "_abroad"})
	cases := []struct {
		name  string
		rules []byte
		p     Packet
		want  bool
	}{
		{"inet6 on en0", full, pk("en0", "_abroad", "tcp", 1000, 443, "2606:4700::1"), false},
		{"::1 在 lo0 放行", full, pk("lo0", "alice", "tcp", 1000, 443, "::1"), true},
		{"inet6 在我们的 utun 上仍被拦", full, pk("utun5", "_abroad", "tcp", 1000, 443, "2606:4700::1"), false},
		{"inet6 on utun", full, pk("utun5", "alice", "udp", 1000, 443, "2606:4700::1"), false},
		{"lo0 放行", full, pk("lo0", "alice", "tcp", 1000, 8080, "127.0.0.1"), true},
		{"我们的 utun 放行", full, pk("utun5", "alice", "tcp", 1000, 443, "1.1.1.1"), true},
		{"我们的 utun 上 icmp 也放行（栈里丢）", full, pk("utun5", "alice", "icmp", 0, 0, "1.1.1.1"), true},
		{"DHCP 广播", full, pk("en0", "root", "udp", 68, 67, "255.255.255.255"), true},
		{"DHCP 单播到私有", full, pk("en0", "root", "udp", 68, 67, "192.168.1.1"), true},
		{"DHCP 到公网被拦", full, pk("en0", "alice", "udp", 68, 67, "8.8.8.8"), false},
		{"_abroad tcp 到公网", full, pk("en0", "_abroad", "tcp", 50000, 443, "203.0.113.5"), true},
		{"_abroad udp 到公网", full, pk("en0", "_abroad", "udp", 50000, 443, "203.0.113.5"), true},
		{"_abroad icmp 到公网被拦", full, pk("en0", "_abroad", "icmp", 0, 0, "203.0.113.5"), false},
		{"到 LAN 放行", full, pk("en0", "alice", "tcp", 1000, 631, "192.168.1.20"), true},
		{"到组播放行", full, pk("en0", "alice", "udp", 5353, 5353, "224.0.0.251"), true},
		{"LAN 关闭时同一个包被拦", nolan, pk("en0", "alice", "tcp", 1000, 631, "192.168.1.20"), false},
		{"恶意 DHCP 发的公网直连网段不放行", evil, pk("en0", "alice", "tcp", 1000, 443, "160.79.104.10"), false},
		{"vmif 到私网放行", full, pk("bridge100", "alice", "tcp", 1000, 22, "192.168.64.2"), true},
		{"vmif 到公网被拦", full, pk("bridge100", "alice", "tcp", 1000, 443, "1.1.1.1"), false},
		{"en0 普通用户 tcp 到公网", full, pk("en0", "alice", "tcp", 1000, 443, "1.1.1.1"), false},
		{"en0 普通用户 udp 到公网", full, pk("en0", "alice", "udp", 1000, 443, "1.1.1.1"), false},
		{"en0 普通用户 icmp 到公网", full, pk("en0", "alice", "icmp", 0, 0, "1.1.1.1"), false},
		{"别人的 utun9 tcp", full, pk("utun9", "alice", "tcp", 1000, 443, "1.1.1.1"), false},
		{"别人的 utun9 udp", full, pk("utun9", "alice", "udp", 1000, 443, "1.1.1.1"), false},
		{"别人的 utun9 icmp", full, pk("utun9", "alice", "icmp", 0, 0, "1.1.1.1"), false},
		{"新接口 en7 tcp", full, pk("en7", "alice", "tcp", 1000, 443, "1.1.1.1"), false},
		{"新接口 en7 udp", full, pk("en7", "alice", "udp", 1000, 443, "1.1.1.1"), false},
		{"新接口 en7 icmp", full, pk("en7", "alice", "icmp", 0, 0, "1.1.1.1"), false},
		{"root 在 en0 到公网", full, pk("en0", "root", "tcp", 1000, 443, "1.1.1.1"), false},
		{"开机配置 utun5 到公网被拦", boot, pk("utun5", "alice", "tcp", 1000, 443, "1.1.1.1"), false},
		{"开机配置 utun0 到公网被拦", boot, pk("utun0", "alice", "udp", 1000, 443, "1.1.1.1"), false},
	}
	for _, c := range cases {
		got, err := Eval(c.rules, c.p)
		if err != nil || got != c.want {
			t.Errorf("%s：%v（%v），应为 %v", c.name, got, err, c.want)
		}
	}
}

// e. 不认识的行报错；空规则不放行
func TestEvalUnknownAndEmpty(t *testing.T) {
	p := pk("lo0", "alice", "tcp", 1, 2, "127.0.0.1")
	for _, bad := range []string{
		"pass in quick all",
		"pass out all",
		"pass out quick on lo0 keep state",
		"pass out quick proto icmp all",
		"pass out quick from 10.0.0.0/8 to any",
		"anchor \"x\"",
		"# comment",
		"table <other> { 10.0.0.0/8 }",
		"pass out quick to <abroad_lan> port 80",
		"pass out quick inet to 999.1.1.1",
		"pass out quick all user",
		"pass out quick all no state",                // 没有 on 接口
		"pass out quick inet proto tcp all no state", // 带协议
		"block drop out quick on lo0 all no state",   // block
		"pass out quick on lo0 inet all no state",    // 带地址族
		"pass out quick on lo0 all no",               // 不完整
		"pass out quick inet to 10.0.0.0/8 no state", // 不在 all 之后
	} {
		if _, err := Eval([]byte(bad+"\n"), p); err == nil {
			t.Errorf("%q 应报不认识", bad)
		}
	}
	if ok, err := Eval(nil, p); ok || err != nil {
		t.Fatalf("空规则应不放行：%v %v", ok, err)
	}
	// golden 文件都能被求值器完整解析。
	files, _ := filepath.Glob("testdata/*.golden")
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if _, err := Eval(b, p); err != nil {
			t.Errorf("%s 应能解析：%v", f, err)
		}
	}
}

// f. 性质测试：公网 IPv4 目的、接口不是 lo0 也不是我们的 utun、用户不是放行用户 → 一律不放行；
// IPv6 且接口不是 lo0 → 一律不放行
func TestEvalProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(20261118, 6))
	privateLike := []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"),
	}
	public := func(a netip.Addr) bool {
		for _, p := range privateLike {
			if p.Contains(a) {
				return false
			}
		}
		return true
	}
	lanPool := pfx("10.0.0.0/8", "10.1.2.0/24", "172.16.5.0/24", "192.168.1.0/24", "192.168.64.0/24", "100.64.0.0/10", "169.254.0.0/16", "224.0.0.0/4")
	users := []string{"_abroad", "alice", "root", "nobody", "u1"}
	protos := []string{"tcp", "udp", "icmp"}
	checked := 0
	for i := range 10000 {
		c := Config{User: users[r.IntN(2)*3+0], UtunName: fmt.Sprintf("utun%d", r.IntN(10))}
		if r.IntN(4) == 0 {
			c.UtunName = ""
		}
		for range r.IntN(4) {
			c.LAN = append(c.LAN, lanPool[r.IntN(len(lanPool))])
		}
		for range r.IntN(3) {
			c.VMIfs = append(c.VMIfs, fmt.Sprintf("bridge%d", 100+r.IntN(3)))
		}
		rules := mustRender(t, c)
		ifaces := append([]string{"en0", "en7", "lo0", "utun9", fmt.Sprintf("utun%d", r.IntN(10)), "bridge100"}, c.VMIfs...)
		p := Packet{If: ifaces[r.IntN(len(ifaces))], User: users[r.IntN(len(users))], Proto: protos[r.IntN(3)],
			SrcPort: uint16(r.IntN(65536)), DstPort: uint16(r.IntN(65536))}
		if r.IntN(5) == 0 {
			var b [16]byte
			for j := range b {
				b[j] = byte(r.IntN(256))
			}
			b[0] = 0x20 | b[0]&0x1f
			p.Dst = netip.AddrFrom16(b)
		} else {
			p.Dst = netip.AddrFrom4([4]byte{byte(r.IntN(256)), byte(r.IntN(256)), byte(r.IntN(256)), byte(r.IntN(256))})
		}
		pass, err := Eval(rules, p)
		if err != nil {
			t.Fatalf("第 %d 组求值出错：%v", i, err)
		}
		if p.Dst.Is6() && p.If != "lo0" && pass {
			t.Fatalf("第 %d 组：IPv6（非 lo0）不应放行：%+v\n%s", i, p, rules)
		}
		if p.Dst.Is4() && public(p.Dst) && p.If != "lo0" && p.If != c.UtunName && p.User != c.User {
			checked++
			if pass {
				t.Fatalf("第 %d 组：公网目的、非 lo0、非我们的 utun、非放行用户却放行了：%+v\n%s", i, p, rules)
			}
		}
	}
	if checked < 3000 {
		t.Fatalf("覆盖的关键组太少：%d", checked)
	}
}

// g/h. 源码检查：internal/pf 里只有 exec.go 可以用 os/exec，且可执行路径只有 /sbin/pfctl；
// 整个仓库除 internal/pf/exec.go 外没有对 ExecRunner 的引用（会话里不执行真实 pfctl）。
func TestSourceNoExec(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, p := range files {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(p)
		src := string(b)
		if p != "exec.go" {
			for _, bad := range []string{"exec.Command", "os/exec"} {
				if strings.Contains(src, bad) {
					t.Errorf("%s 里出现了 %s（只有 exec.go 可以）", p, bad)
				}
			}
		}
		for _, bad := range []string{"net.Dial", "networksetup", "ifconfig"} {
			if strings.Contains(src, bad) {
				t.Errorf("%s 里出现了 %s", p, bad)
			}
		}
	}
	exe, _ := os.ReadFile("exec.go")
	paths := regexp.MustCompile(`"(/[^"]*)"`).FindAllStringSubmatch(string(exe), -1)
	for _, m := range paths {
		if m[1] != "/sbin/pfctl" {
			t.Errorf("exec.go 里出现了 /sbin/pfctl 以外的路径：%s", m[1])
		}
	}
	if !strings.Contains(string(exe), `exec.CommandContext(ctx, pfctlPath, args...)`) {
		t.Error("exec.go 应只执行 pfctlPath")
	}
	// 按语法树查真实的引用（注释不算；internal/agent 有个无关的同名类型，只查 pf.ExecRunner）。
	pfAbs, _ := filepath.Abs(".")
	filepath.WalkDir("../..", func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != "../.." && (d.Name() == "third_party" || d.Name() == "out" || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if err != nil {
			return nil
		}
		abs, _ := filepath.Abs(p)
		inPF := filepath.Dir(abs) == pfAbs
		if inPF && filepath.Base(p) == "exec.go" {
			return nil
		}
		// M2-9b-1 决定 3：生产执行器只在 internal/daemon/realexec.go 里取得。
		if realexec, _ := filepath.Abs("../daemon/realexec.go"); abs == realexec {
			return nil
		}
		// M6-2b：开机时的 guard 在自己的入口取得生产执行器（只有加载和启用，见 cmd/abroad-guard 的源码检查）。
		if guardMain, _ := filepath.Abs("../../cmd/abroad-guard/main.go"); abs == guardMain {
			return nil
		}
		// M6-5b：应急程序在自己的入口取得生产执行器（只清空和释放自己的锚点，见 cmd/abroad-rescue 的源码检查）。
		if rescueMain, _ := filepath.Abs("../../cmd/abroad-rescue/main.go"); abs == rescueMain {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && id.Name == "pf" && x.Sel.Name == "ExecRunner" {
					t.Errorf("%s 引用了 pf.ExecRunner（只有 internal/daemon/realexec.go、cmd/abroad-guard/main.go 和 cmd/abroad-rescue/main.go 可以）", p)
				}
			case *ast.Ident:
				if inPF && x.Name == "ExecRunner" {
					t.Errorf("%s 引用了 ExecRunner（只有 exec.go 可以）", p)
				}
			}
			return true
		})
		return nil
	})
}
