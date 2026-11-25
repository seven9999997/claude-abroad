package resolverlock

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claude-abroad/internal/srccheck"
)

// f. 源码检查：本包不联网、不执行外部命令；真实的 /etc/resolver 路径只出现在 abroadd 的真实模式入口
// （cmd/abroadd/real.go；测试、假系统模式一律用注入的临时目录）。
func TestLockSource(t *testing.T) {
	srccheck.NoDial(t, ".", nil)
	srccheck.NoExec(t, ".")
	root := filepath.Join("..", "..")
	allow := filepath.Join("cmd", "abroadd", "real.go")
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
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		n++
		for _, s := range srccheck.StringLits(srccheck.Parse(t, p)) {
			if strings.Contains(s, "/etc/resolver") && rel != allow {
				t.Errorf("%s 里出现了真实的 /etc/resolver（只允许 %s）", rel, allow)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 100 {
		t.Fatalf("只扫到 %d 个文件", n)
	}
}
