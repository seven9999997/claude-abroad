package claudeproc

import (
	"go/ast"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claude-abroad/internal/srccheck"
)

// d. 源码检查（整个仓库）：
//   - 非测试文件里，只有 cmd/abroadd 的真实模式入口调用 NewRealSignaler；
//   - 测试文件里只有 c 的那一条（白名单）；
//   - syscall.Kill、unix.Kill、os.FindProcess 一类只出现在 signal_unix.go（只在 Linux 编译的文件除外：
//     那是虚拟机里的 agent 管它自己的子进程，不在 Mac 上运行）。
func TestNoOtherSignals(t *testing.T) {
	root := filepath.Join("..", "..")
	allowCtor := map[string]bool{
		filepath.Join("cmd", "abroadd", "real.go"):                     true,
		filepath.Join("internal", "claudeproc", "real_darwin_test.go"): true, // c：非 root 构造返回错误
		filepath.Join("internal", "claudeproc", "signal_unix.go"):      true, // 定义处
		filepath.Join("internal", "claudeproc", "signal_other.go"):     true,
	}
	killers := map[string]bool{"syscall.Kill": true, "unix.Kill": true, "os.FindProcess": true, "syscall.Kill4": true,
		"unix.Killpg": true, "syscall.Killpg": true}
	n := 0
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			switch d.Name() {
			case "third_party", "out", ".git", "testdata", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		n++
		af := srccheck.Parse(t, p)
		ast.Inspect(af, func(x ast.Node) bool {
			switch v := x.(type) {
			case *ast.SelectorExpr:
				if id, ok := v.X.(*ast.Ident); ok {
					name := id.Name + "." + v.Sel.Name
					// 只在 Linux 编译的文件（虚拟机里的 agent 管它自己的子进程）不在 Mac 上运行，不算。
					linuxOnly := strings.HasSuffix(rel, "_linux.go") || strings.HasSuffix(rel, "_linux_test.go")
					if killers[name] && !linuxOnly && rel != filepath.Join("internal", "claudeproc", "signal_unix.go") {
						t.Errorf("%s 用了 %s（发信号只能在 claudeproc/signal_unix.go）", rel, name)
					}
				}
				if v.Sel.Name == "NewRealSignaler" && !allowCtor[rel] {
					t.Errorf("%s 调用了 NewRealSignaler（只允许 cmd/abroadd 的真实模式入口）", rel)
				}
			case *ast.Ident:
				if v.Name == "NewRealSignaler" && !allowCtor[rel] && !strings.HasPrefix(rel, filepath.Join("internal", "claudeproc")+string(filepath.Separator)) {
					t.Errorf("%s 引用了 NewRealSignaler", rel)
				}
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
	// 包内：除定义处和 c 的测试外，没有直接调用（含不带包名的调用）。
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if f == "signal_unix.go" || f == "signal_other.go" || f == "real_darwin_test.go" || f == "src_test.go" {
			continue
		}
		b, _ := os.ReadFile(f)
		if strings.Contains(string(b), "NewRealSignaler(") {
			t.Errorf("%s 调用了 NewRealSignaler", f)
		}
	}
}
