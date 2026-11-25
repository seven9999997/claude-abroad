package claudecfg

import (
	"go/ast"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claude-abroad/internal/srccheck"
)

// h. 本包不联网、不执行外部命令；真实的托管设置路径（/Library/Application Support/ClaudeCode）只出现在 abroadd 的真实模式入口。
func TestSettingsSource(t *testing.T) {
	srccheck.NoDial(t, ".", nil)
	srccheck.NoExec(t, ".")
	root := filepath.Join("..", "..")
	allow := filepath.Join("cmd", "abroadd", "real.go")
	n := 0
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "third_party", "out", ".git", "testdata", "node_modules", ".build":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		n++
		rel, _ := filepath.Rel(root, p)
		af := srccheck.Parse(t, p)
		// 本机代理端口的地址只在真实模式入口设置（假系统模式、开发工具默认不监听固定端口）
		ast.Inspect(af, func(n ast.Node) bool {
			if kv, ok := n.(*ast.KeyValueExpr); ok {
				if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "ClaudeProxyAddr" && rel != allow {
					t.Errorf("%s 设置了 ClaudeProxyAddr（只允许 %s）", rel, allow)
				}
			}
			return true
		})
		for _, s := range srccheck.StringLits(af) {
			if strings.Contains(s, "Application Support/ClaudeCode") && rel != allow {
				t.Errorf("%s 里出现了真实的托管设置路径（只允许 %s）", rel, allow)
			}
		}
		return nil
	})
	if n < 100 {
		t.Fatalf("只扫到 %d 个文件", n)
	}
}
