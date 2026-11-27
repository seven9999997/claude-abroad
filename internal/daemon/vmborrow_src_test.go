package daemon

import (
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"claude-abroad/internal/srccheck"
)

// 借用的互斥入口只许 vmborrow.go 调用（反例见 internal/srccheck 的 TestMethodCallsOutsideCatchesViolations）。
func TestExclusiveHoldOnlyInVMBorrow(t *testing.T) {
	bad, ok := srccheck.MethodCallsOutside(t, filepath.Join("..", ".."), "ExclusiveHold", map[string]bool{"internal/daemon/vmborrow.go": true})
	if bad != 0 || ok < 1 {
		t.Fatalf("违规 %d 处，vmborrow.go 里 %d 处", bad, ok)
	}
}

// 验收 e：面向用户的输出不出现原因码和“通道不通”这类内部说法。
//   - Go（internal/exitcheck 自身除外）：不把 Cause 转成字符串拼文字（string(x.Cause)），字符串字面量里没有这些说法；
//   - 应用（app/Sources）、脚本（scripts/**/*.sh）：文字里没有 channel_down、通道不通。
func TestNoInternalCauseWordsInUserOutput(t *testing.T) {
	root := filepath.Join("..", "..")
	words := []string{"channel_down", "通道不通", "数据通道不可用"}
	n := 0
	for _, rel := range srccheck.GoFiles(t, root) {
		if strings.HasPrefix(filepath.ToSlash(rel), "internal/exitcheck/") {
			continue
		}
		n++
		af := srccheck.Parse(t, filepath.Join(root, rel))
		ast.Inspect(af, func(x ast.Node) bool {
			switch v := x.(type) {
			case *ast.CallExpr:
				if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "string" && len(v.Args) == 1 {
					if s, ok := v.Args[0].(*ast.SelectorExpr); ok && s.Sel.Name == "Cause" {
						t.Errorf("%s 把原因码转成字符串（请用 exitcheck.CauseText）", rel)
					}
				}
			case *ast.BasicLit:
				if v.Kind == token.STRING {
					s, _ := strconv.Unquote(v.Value)
					for _, w := range words {
						if strings.Contains(s, w) {
							t.Errorf("%s 的字符串里有内部说法 %q：%q", rel, w, s)
						}
					}
				}
			}
			return true
		})
	}
	if n < 100 {
		t.Fatalf("只扫到 %d 个 Go 文件", n)
	}
	m := 0
	for _, dir := range []string{"app/Sources", "scripts"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				if d != nil && d.IsDir() && (d.Name() == "out" || d.Name() == ".build") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".swift") && !strings.HasSuffix(p, ".sh") {
				return nil
			}
			m++
			b, _ := os.ReadFile(p)
			for _, w := range words[:2] {
				if strings.Contains(string(b), w) {
					t.Errorf("%s 里有内部说法 %q", p, w)
				}
			}
			return nil
		})
	}
	if m < 20 {
		t.Fatalf("只扫到 %d 个应用和脚本文件", m)
	}
}
