package vmmgr

import (
	"go/ast"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claude-abroad/internal/srccheck"
)

// g. 源码检查：真实的拉起（ExecLauncher）只在 cmd/abroadd 的真实模式入口构造；本包里只有定义处和它自己的测试。
// 会话里不以 root 运行、不执行真实模式，所以测试和开发工具一律用假的 Launcher。
func TestExecLauncherOnlyInRealEntry(t *testing.T) {
	root := filepath.Join("..", "..")
	allow := map[string]bool{
		filepath.Join("cmd", "abroadd", "real.go"):    true, // 真实模式入口
		filepath.Join("cmd", "abroadd", "vm_test.go"): true, // 只检查组装出来的参数，不调用 Launch
	}
	own := filepath.Join("internal", "vmmgr") + string(filepath.Separator)
	n := 0
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			switch d.Name() {
			case "third_party", "out", ".git", "testdata", "node_modules", ".build":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		n++
		if strings.HasPrefix(rel, own) {
			return nil
		}
		af := srccheck.Parse(t, p)
		ast.Inspect(af, func(x ast.Node) bool {
			if s, ok := x.(*ast.SelectorExpr); ok && s.Sel.Name == "ExecLauncher" && !allow[rel] {
				t.Errorf("%s 引用了 vmmgr.ExecLauncher（只允许 cmd/abroadd 的真实模式入口）", rel)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 100 {
		t.Fatalf("只扫到 %d 个文件，路径不对", n)
	}
}
