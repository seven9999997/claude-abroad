package procinfo

import (
	"go/ast"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"claude-abroad/internal/srccheck"
)

// h. 不执行外部命令；除了 sysctl 和只读地读文件，不碰系统；不打日志；没有写操作；没有网络调用。
func TestSourceRules(t *testing.T) {
	srccheck.NoExec(t, ".")
	allowed := map[string]bool{"claude-abroad/internal/rules": true, "golang.org/x/sys/unix": true}
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af := srccheck.Parse(t, f)
		for _, im := range af.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			if (strings.Contains(strings.SplitN(p, "/", 2)[0], ".") || strings.HasPrefix(p, "claude-abroad/")) && !allowed[p] {
				t.Errorf("%s 导入了 %s", f, p)
			}
			if p == "net" || strings.HasPrefix(p, "net/") && p != "net/netip" || p == "log" || p == "log/slog" || p == "os/exec" || p == "syscall" {
				t.Errorf("%s 导入了 %s", f, p)
			}
		}
		ast.Inspect(af, func(n ast.Node) bool {
			x, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := x.X.(*ast.Ident)
			if !ok {
				return true
			}
			name := id.Name + "." + x.Sel.Name
			switch {
			case id.Name == "os" && x.Sel.Name != "Stat" && x.Sel.Name != "Open":
				t.Errorf("%s 用了 %s（只允许 os.Stat、os.Open 只读）", f, name)
			case id.Name == "fmt" && strings.HasPrefix(x.Sel.Name, "Print"):
				t.Errorf("%s 调用了 %s", f, name)
			case id.Name == "unix" && x.Sel.Name != "SysctlRaw" && x.Sel.Name != "SysctlKinfoProc" && x.Sel.Name != "SysctlKinfoProcSlice" && x.Sel.Name != "KinfoProc":
				t.Errorf("%s 用了 %s（只允许 sysctl 读取）", f, name)
			}
			return true
		})
	}
}
