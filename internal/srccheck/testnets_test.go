package srccheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M6-11b：回国拦截的测试段（文档保留地址）只在开发构建里有——常量和读取它的分支只能出现在带 abroaddev 标签的文件里
// （测试文件除外）。发布构建里没有任何办法加载测试段。
func TestReturnBlockTestNetsDevOnly(t *testing.T) {
	root := filepath.Join("..", "..")
	n := 0
	for _, top := range []string{"internal", "cmd"} {
		_ = filepath.WalkDir(filepath.Join(root, top), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, _ := os.ReadFile(p)
			s := string(b)
			if !strings.Contains(s, "2001:db8:abd") && !strings.Contains(s, "TestNets") {
				return nil
			}
			n++
			if !strings.Contains(s, "//go:build abroaddev") {
				t.Errorf("%s 不是开发构建的文件，却出现了回国拦截的测试段", p)
			}
			return nil
		})
	}
	if n == 0 {
		t.Fatal("没有找到测试段的定义（检查本身失效了）")
	}
}
