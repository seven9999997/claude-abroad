package main

import (
	"go/ast"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"claude-abroad/internal/srccheck"
)

// e. 源码检查（guard 入口和 internal/guard）：
//   - 不拨号、不监听、不联网（不导入 net、net/http、os/exec；只许 net/netip）；
//   - pf 操作只有“加载到自己的锚点”（Load）和“启用”（Enable）：不调用 Flush、Release、KillStates、Ensure、
//     Status、Run 这些方法，没有 pfctl 的 -d、-F、-X 字面量，不给 Controller 指定别的锚点（Anchor 字段）。
func TestGuardSource(t *testing.T) {
	dirs := []string{".", filepath.Join("..", "..", "internal", "guard")}
	forbiddenImports := map[string]bool{"net": true, "net/http": true, "os/exec": true, "crypto/tls": true}
	forbiddenSel := map[string]bool{"Flush": true, "Release": true, "KillStates": true, "Ensure": true, "Status": true, "Run": true}
	files := 0
	for _, dir := range dirs {
		srccheck.NoDial(t, dir, nil)
		paths, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, p := range paths {
			if strings.HasSuffix(p, "_test.go") {
				continue
			}
			files++
			af := srccheck.Parse(t, p)
			for _, im := range af.Imports {
				ip, _ := strconv.Unquote(im.Path.Value)
				if forbiddenImports[ip] {
					t.Errorf("%s 导入了 %s", p, ip)
				}
			}
			for _, s := range srccheck.StringLits(af) {
				for _, f := range strings.Fields(s) {
					if f == "-d" || f == "-F" || f == "-X" || f == "-e" {
						t.Errorf("%s 里出现了 pfctl 参数 %s", p, f)
					}
				}
			}
			ast.Inspect(af, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok && forbiddenSel[sel.Sel.Name] {
						// guard.Run 是本包自己的入口，不是 pf 操作
						if id, ok := sel.X.(*ast.Ident); ok && id.Name == "guard" && sel.Sel.Name == "Run" {
							return true
						}
						t.Errorf("%s 调用了 %s（guard 的 pf 操作只能是 Load、Enable）", p, sel.Sel.Name)
					}
				case *ast.KeyValueExpr:
					if k, ok := x.Key.(*ast.Ident); ok && k.Name == "Anchor" {
						t.Errorf("%s 给 pf 指定了锚点（只能用本项目默认的锚点）", p)
					}
				}
				return true
			})
		}
	}
	if files < 3 {
		t.Fatalf("只查了 %d 个文件", files)
	}
}
