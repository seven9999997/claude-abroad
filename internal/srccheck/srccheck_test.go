package srccheck

import (
	"go/ast"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recorder struct {
	testing.TB
	errs int
}

func (r *recorder) Errorf(string, ...any) { r.errs++ }
func (r *recorder) Helper()               {}

func copyCase(t *testing.T, name string) string {
	dir := t.TempDir()
	src, err := os.ReadFile(filepath.Join("testdata", name, name+".go.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".go"), src, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestNoExecCatchesViolations(t *testing.T) {
	r := &recorder{TB: t}
	NoExec(r, copyCase(t, "bad"))
	// os/exec 导入、exec.Command 调用、"/sbin/route" 字面量各一处。
	if r.errs != 3 {
		t.Fatalf("应报 3 处，实际 %d", r.errs)
	}
}

func TestNoExecAcceptsClean(t *testing.T) {
	r := &recorder{TB: t}
	NoExec(r, copyCase(t, "good"))
	if r.errs != 0 {
		t.Fatalf("干净的源码报了 %d 处", r.errs)
	}
}

func copyNamed(t *testing.T, name string) string {
	dir := t.TempDir()
	src, err := os.ReadFile(filepath.Join("testdata", name, name+".go.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".go"), src, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestNoDialCatchesViolations(t *testing.T) {
	r := &recorder{TB: t}
	// 改名导入的 net.Dial、net.Dialer（两处）、net.ListenPacket、syscall.Socket、unix.Socket、http.Get。
	if n := NoDial(r, copyNamed(t, "dialbad"), nil); n != 7 || r.errs != 7 {
		t.Fatalf("应报 7 处，实际 %d（%d）", n, r.errs)
	}
	// 白名单按文件放开。
	r = &recorder{TB: t}
	n := NoDial(r, copyNamed(t, "dialbad"), map[string][]string{"dialbad.go": {"net.Dialer", "net.Dial"}})
	if n != 4 {
		t.Fatalf("放开 net.Dialer、net.Dial 后应剩 4 处，实际 %d", n)
	}
}

func TestNoDialAcceptsClean(t *testing.T) {
	r := &recorder{TB: t}
	if n := NoDial(r, copyNamed(t, "dialgood"), nil); n != 0 || r.errs != 0 {
		t.Fatalf("干净的源码报了 %d 处", n)
	}
}

// W-1 验收 d：检查器自身——合格的占位、纯计算通过；返回 nil 错误、return true、有 error 却不报“尚未支持”、
// 纯计算里做系统调用都被抓到。
func TestCheckPlaceholderRules(t *testing.T) {
	src := `package x
import ("os"; "claude-abroad/internal/platform")
func good() error { return platform.Unsupported("x") }
func good2() (int, error) { return 0, platform.Unsupported("x") }
func pure(d string) []string { return []string{d} }
func pureFalse(err error) bool { return false }
func noop() {}
func nilErr() error { return nil }
func fakeOK() (int, error) { if false { return 0, platform.Unsupported("x") }; return 1, nil }
func checkTrue() bool { return true }
func silentErr() error { return os.ErrNotExist }
func impure() string { h, _ := os.Hostname(); return h }
`
	af := parseSrc(t, src)
	want := map[string]bool{"good": false, "good2": false, "pure": false, "pureFalse": false, "noop": false,
		"nilErr": true, "fakeOK": true, "checkTrue": true, "silentErr": true, "impure": true}
	for _, d := range af.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		got := len(checkPlaceholder(fd, false)) > 0
		if got != want[fd.Name.Name] {
			t.Errorf("%s：违规=%v，应为 %v：%v", fd.Name.Name, got, want[fd.Name.Name], checkPlaceholder(fd, false))
		}
	}
}

func TestBuildsOn(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, c := range map[string]struct {
		body            string
		win, mac, linux bool
	}{
		"a_windows.go": {"package x\n", true, false, false},
		"b.go":         {"//go:build windows\n\npackage x\n", true, false, false},
		"c.go":         {"//go:build unix\n\npackage x\n", false, true, true},
		"d.go":         {"//go:build !windows\n\npackage x\n", false, true, true},
		"e_darwin.go":  {"package x\n", false, true, false},
		"f.go":         {"//go:build !linux\n\npackage x\n", true, true, false},
		"g.go":         {"package x\n", true, true, true},
	} {
		p := write(name, c.body)
		if buildsOn(p, name, "windows", "amd64") != c.win || buildsOn(p, name, "darwin", "arm64") != c.mac || buildsOn(p, name, "linux", "arm64") != c.linux {
			t.Errorf("%s 判断不对", name)
		}
	}
}

// W-1 验收 d：仓库里所有只在 Windows 上编译的文件，占位都只会失败。例外逐个列出并写理由。
func TestRepoWindowsPlaceholdersFail(t *testing.T) {
	root := filepath.Join("..", "..")
	files := WindowsOnlyFiles(t, root)
	if len(files) < 8 {
		t.Fatalf("只找到 %d 个只在 Windows 上编译的文件，检查可能没生效：%v", len(files), files)
	}
	t.Logf("检查了 %d 个文件：%v", len(files), files)
	// 例外：这些替身和 Linux 共用（!darwin），本来就一律返回错误（各自的 errors.New 文字），W-1 不改它们的文字，
	// 免得改变 Linux 上的行为；它们仍不能返回 nil 错误或 true——这一条对例外也检查（见下）。
	shared := "和 Linux 共用的替身，已经一律失败（errors.New），W-1 不改 Linux 上的文字"
	allow := map[string]string{
		"cmd/abroadd/utun_other.go:openUtun":             shared,
		"cmd/m0-r2-probe/bound_other.go:bindToInterface": shared,
		"internal/claudeproc/signer_other.go:TeamID":     shared,
		"internal/procinfo/sys_other.go:Snapshot":        shared,
		"internal/procinfo/sys_other.go:Info":            shared,
		"internal/procinfo/sys_other.go:List":            shared,
		"internal/procinfo/sys_other.go:Env":             shared,
		"internal/usernet/dialer_other.go:bindControl":   shared,
		// W-2a：Windows 断网保护的系统层，是真正的实现不是占位。参数全部来自 spec.go 的纯函数（Mac 上有单测），
		// 结构体大小和冒烟测试在 wfp_windows_test.go（Windows 上、需要管理员权限）。
		"internal/wfp/wfp_windows.go:*": "W-2a 的 WFP 系统层（真正的实现）",
		// W-2a：第一级探查的 Windows 实现（真正的探查，不是占位）。步骤表、预测、判定、脱敏在 steps.go（Mac 上有单测）。
		"cmd/abroad-wfp-probe/probe_windows.go:*": "W-2a 的第一级探查（真正的实现）",
		// M5-6：命令行的客户端包在 Windows 上分辨“socket 文件在、没有进程在听”（WSAECONNREFUSED），只做错误比较。
		"internal/apiclient/refused_windows.go:isRefused": "M5-6 的错误分类（只比较错误码，不是占位）",
	}
	PlaceholdersFail(t, root, allow)
	// 例外也不能假装成功：不返回 nil 错误、不 return true。
	for key := range allow {
		rel, fn, _ := strings.Cut(key, ":")
		if fn == "*" {
			if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
				t.Errorf("例外 %s 指向的文件不存在", key)
			}
			continue
		}
		af := Parse(t, filepath.Join(root, rel))
		found := false
		for _, d := range af.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == fn {
				found = true
				for _, m := range checkPlaceholder(fd, false) {
					if !strings.Contains(m, "没有返回“本平台尚未支持”") {
						t.Errorf("例外 %s：%s", key, m)
					}
				}
			}
		}
		if !found {
			t.Errorf("例外 %s 已不存在，请从名单里删掉", key)
		}
	}
}
