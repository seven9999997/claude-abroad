package directdial

import (
	"go/ast"
	"path/filepath"
	"strings"
	"testing"

	"claude-abroad/internal/srccheck"
)

// i. 服务端里拨号和解析的出口各只有一处；整个包没有不经 BoundDialer 的拨号。
func TestSingleExits(t *testing.T) {
	// 白名单：client.go 连 unix socket（net.Dialer）、把收到的 fd 变成连接（net.FileConn）。
	srccheck.NoDial(t, ".", map[string][]string{"client.go": {"net.Dialer", "net.FileConn"}})

	calls := map[string][]string{} // 被调用的方法名 → 所在函数
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af := srccheck.Parse(t, f)
		for _, d := range af.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if sel, ok := c.Fun.(*ast.SelectorExpr); ok {
						switch sel.Sel.Name {
						case "DialDirect", "lookup", "dialOne", "lookupA":
							calls[sel.Sel.Name] = append(calls[sel.Sel.Name], f+":"+fn.Name.Name)
						}
					}
				}
				return true
			})
		}
	}
	want := map[string][]string{
		"DialDirect": {"resolve.go:exchange", "server.go:dialOne"}, // 解析查询、目标拨号各一处
		"dialOne":    {"server.go:serve"},
		"lookup":     {"server.go:serve"},
		"lookupA":    {"server.go:lookup"},
	}
	for name, w := range want {
		got := calls[name]
		if strings.Join(got, ",") != strings.Join(w, ",") {
			t.Errorf("%s 的调用点应是 %v，实际 %v", name, w, got)
		}
	}
}
