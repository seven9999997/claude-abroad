// Package srccheck 是测试用的源码检查（M2-7a 验收 h）：确认某个包不执行外部命令。
package srccheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Rule 是对某个文件放开的检查（M2-7d 决定 5）。零值表示全部检查。
type Rule struct {
	AllowExec    bool // 允许导入 os/exec 和调用 exec.Command 等（只给 sysnet/exec.go）
	AllowCmdName bool // 字符串里允许出现命令名（darwin 实现文件拼命令、解析输出要用）
}

// NoExec 检查目录里的非测试 .go 文件：不导入 os/exec，不调用 exec.Command 等，
// 字符串字面量里不出现系统网络命令名（networksetup、ifconfig、pfctl、route、scutil）。
func NoExec(t testing.TB, dir string) { NoExecWith(t, dir, nil) }

// NoExecWith 同 NoExec，但按文件名（不含目录）对个别文件放开部分检查。
func NoExecWith(t testing.TB, dir string, rules map[string]Rule) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("没找到源码：%v", err)
	}
	banned := []string{"networksetup", "ifconfig", "pfctl", "route", "scutil"}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		rule := rules[filepath.Base(file)]
		af := Parse(t, file)
		for _, im := range af.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			if p == "os/exec" && !rule.AllowExec {
				t.Errorf("%s 导入了 os/exec", file)
			}
		}
		ast.Inspect(af, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && !rule.AllowExec {
					name := id.Name + "." + x.Sel.Name
					switch name {
					case "exec.Command", "exec.CommandContext", "syscall.Exec", "syscall.ForkExec", "os.StartProcess":
						t.Errorf("%s 调用了 %s", file, name)
					}
				}
			case *ast.BasicLit:
				if x.Kind == token.STRING && !rule.AllowCmdName {
					s, _ := strconv.Unquote(x.Value)
					for _, w := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '/' || r == '\t' }) {
						for _, b := range banned {
							if w == b {
								t.Errorf("%s 的字符串里出现了 %q：%q", file, b, s)
							}
						}
					}
				}
			}
			return true
		})
	}
}

// Parse 解析一个 Go 源文件。
func Parse(t testing.TB, file string) *ast.File {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	af, err := parser.ParseFile(token.NewFileSet(), file, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	return af
}

// StringLits 返回文件里所有字符串字面量的值。
func StringLits(af *ast.File) []string {
	var out []string
	ast.Inspect(af, func(n ast.Node) bool {
		if x, ok := n.(*ast.BasicLit); ok && x.Kind == token.STRING {
			if s, err := strconv.Unquote(x.Value); err == nil {
				out = append(out, s)
			}
		}
		return true
	})
	return out
}

// dialNames 是“直接拨号”的写法：net 包的拨号和监听函数、net.Dialer 类型、自己建套接字和连接的系统调用。
var dialNames = map[string]bool{
	"net.Dial": true, "net.DialTimeout": true, "net.DialTCP": true, "net.DialUDP": true, "net.DialIP": true,
	"net.DialUnix": true, "net.Dialer": true, "net.ListenPacket": true, "net.ListenUDP": true, "net.ListenIP": true,
	"net.Listen": true, "net.ListenTCP": true, "net.ListenUnix": true, "net.ListenUnixgram": true,
	"net.ListenConfig": true, "net.FileConn": true, "net.FilePacketConn": true,
	"syscall.Socket": true, "syscall.Connect": true, "syscall.Sendto": true,
	"unix.Socket": true, "unix.Connect": true, "unix.Sendto": true, "unix.SendmsgN": true,
	"http.Get": true, "http.Post": true, "http.Head": true, "http.PostForm": true, "http.DefaultClient": true,
	"http.DefaultTransport": true, "http.Transport": true, "http.Client": true,
}

// NoDial 检查目录里的非测试 .go 文件没有“直接拨号”（m3-plan M3-4 验收 i）：直连只能经直连拨号模块，
// 代理只能经数据通道。allow 按文件名（不含目录）列出该文件允许的写法，例如 {"client.go": {"net.Dialer"}}。
// 返回发现的违规数（同时经 t.Errorf 报告）。
func NoDial(t testing.TB, dir string, allow map[string][]string) int {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("没找到源码：%v", err)
	}
	bad := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		ok := map[string]bool{}
		for _, n := range allow[filepath.Base(file)] {
			ok[n] = true
		}
		af := Parse(t, file)
		alias := map[string]string{} // 导入名 → 包路径的最后一段（处理改名导入）
		for _, im := range af.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			last := p[strings.LastIndex(p, "/")+1:]
			name := last
			if im.Name != nil {
				name = im.Name.Name
			}
			switch p {
			case "net", "syscall", "golang.org/x/sys/unix", "net/http":
				alias[name] = map[string]string{"net": "net", "syscall": "syscall", "golang.org/x/sys/unix": "unix", "net/http": "http"}[p]
			}
		}
		ast.Inspect(af, func(n ast.Node) bool {
			x, isSel := n.(*ast.SelectorExpr)
			if !isSel {
				return true
			}
			id, isID := x.X.(*ast.Ident)
			if !isID {
				return true
			}
			pkg, known := alias[id.Name]
			if !known {
				return true
			}
			name := pkg + "." + x.Sel.Name
			if dialNames[name] && !ok[name] {
				t.Errorf("%s 用了 %s：直连只能经直连拨号模块，代理只能经数据通道", file, name)
				bad++
			}
			return true
		})
	}
	return bad
}

// skipDirs 是扫描整个仓库时跳过的目录。
var skipDirs = map[string]bool{"third_party": true, "out": true, ".git": true, ".claude": true, "testdata": true, "node_modules": true, ".build": true}

// GoFiles 返回 root 下全部非测试的 .go 文件（相对 root 的路径），跳过第三方、产物、测试数据目录。
func GoFiles(t testing.TB, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			rel, _ := filepath.Rel(root, p)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// MethodCallsOutside 检查 root 下非测试的 .go 文件：形如 x.<method>(...) 的调用只允许出现在 allow 列出的文件里
// （相对 root 的路径）。返回违规数（同时经 t.Errorf 报告）和允许的文件里的调用数。
func MethodCallsOutside(t testing.TB, root, method string, allow map[string]bool) (bad, allowed int) {
	t.Helper()
	for _, rel := range GoFiles(t, root) {
		af := Parse(t, filepath.Join(root, rel))
		ast.Inspect(af, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == method {
				if allow[filepath.ToSlash(rel)] {
					allowed++
				} else {
					bad++
					t.Errorf("%s 调用了 %s（只允许 %v）", rel, method, keys(allow))
				}
			}
			return true
		})
	}
	return bad, allowed
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
