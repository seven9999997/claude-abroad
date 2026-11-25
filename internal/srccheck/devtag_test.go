package srccheck

import (
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// devOnly 判断文件的构建约束是否要求 abroaddev（不带这个标签就不编译）。
func devOnly(t *testing.T, path string) bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "package ") {
			break
		}
		if !constraint.IsGoBuild(line) {
			continue
		}
		expr, err := constraint.Parse(line)
		if err != nil {
			t.Fatalf("%s：%v", path, err)
		}
		// 不带 abroaddev 时，其余标签全取真、全取假，约束都不成立，才算“只在开发构建里”。
		allOther := func(v bool) func(string) bool {
			return func(tag string) bool { return tag != "abroaddev" && v }
		}
		return !expr.Eval(allOther(true)) && !expr.Eval(allOther(false))
	}
	return false
}

// stringLits 取文件里所有字符串字面量（不看注释）。
func stringLits(t *testing.T, path string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING {
			out = append(out, bl.Value)
		}
		return true
	})
	return out
}

func identsUsed(t *testing.T, path string) map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			m[x.Name] = true
		case *ast.SelectorExpr:
			m[x.Sel.Name] = true
		}
		return true
	})
	return m
}

// M6-1 验收 c：开发参数的名字、假系统和假主代理的接线、agent 的开发出口，只出现在带 abroaddev 约束的文件里。
func TestDevOnlyInTaggedFiles(t *testing.T) {
	root := filepath.Join("..", "..")
	devFlagNames := []string{"fake-system", "fake-main-proxy", "dev-skip-claude-kill", "dev-skip-region", "dev-resolver-test-domain", "outbound-open", "dev-agent"}
	type rule struct {
		dir     string
		strs    []string // 字符串字面量里不能出现
		idents  []string // 不能引用的标识符
		defines []string // 只能在开发文件里定义的类型或函数
	}
	rules := []rule{
		{dir: "cmd/abroadd", strs: []string{"fake-system", "fake-main-proxy", "dev-skip-claude-kill", "dev-skip-region", "dev-resolver-test-domain"}, idents: []string{"NewFakeSystem", "WithFakeMainProxy"}},
		{dir: "cmd/abroad-vm", strs: []string{"outbound-open", "dev-agent"}, idents: []string{"CmdlineDev"}},
		{dir: "cmd/abroad-agent", strs: []string{"dev-exits", "abroad.dev"}},
		{dir: "internal/agent", strs: []string{"/v1/dev-exits", "/v1/dev/netns-probe", "/v1/dev/exit-peer"},
			idents: []string{"ParseDevCmdline"}, defines: []string{"DevExits", "DevPeer", "NetnsProbe", "devExitTable", "ParseDevCmdline", "newProbeOps"}},
		// 发布构建里的提示和日志不提开发参数的名字（M6-1 建议改）。
		{dir: "internal/claudeproc", strs: devFlagNames},
		{dir: "internal/platform", strs: devFlagNames},
		{dir: "internal/daemon", strs: devFlagNames},
	}
	checked := 0
	for _, r := range rules {
		files, _ := filepath.Glob(filepath.Join(root, r.dir, "*.go"))
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") || devOnly(t, path) {
				continue
			}
			checked++
			name := filepath.Join(r.dir, filepath.Base(path))
			for _, lit := range stringLits(t, path) {
				for _, s := range r.strs {
					if strings.Contains(lit, s) {
						t.Errorf("%s：字符串 %s 里有开发用的 %q（只能在带 abroaddev 约束的文件里）", name, lit, s)
					}
				}
			}
			used := identsUsed(t, path)
			for _, id := range r.idents {
				if used[id] {
					t.Errorf("%s：引用了 %s（只能在带 abroaddev 约束的文件里）", name, id)
				}
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range f.Decls {
				switch x := d.(type) {
				case *ast.FuncDecl:
					for _, def := range r.defines {
						if x.Recv == nil && x.Name.Name == def {
							t.Errorf("%s：定义了开发用的 %s", name, def)
						}
					}
				case *ast.GenDecl:
					for _, sp := range x.Specs {
						if ts, ok := sp.(*ast.TypeSpec); ok {
							for _, def := range r.defines {
								if ts.Name.Name == def {
									t.Errorf("%s：定义了开发用的 %s", name, def)
								}
							}
						}
					}
				}
			}
		}
	}
	if checked < 20 {
		t.Fatalf("只检查了 %d 个文件，检查可能没生效", checked)
	}
	// 反例：开发文件确实被认作开发文件。
	for _, f := range []string{"cmd/abroadd/devflags_dev.go", "cmd/abroad-vm/devflags_dev.go", "internal/agent/serve_dev.go", "internal/agent/devexits.go", "internal/agent/netnsprobe_linux.go"} {
		if !devOnly(t, filepath.Join(root, f)) {
			t.Errorf("%s 应只在开发构建里编译", f)
		}
	}
	if devOnly(t, filepath.Join(root, "cmd/abroadd/main.go")) || devOnly(t, filepath.Join(root, "cmd/abroad-vm/net_unix.go")) {
		t.Error("普通文件被误认成开发文件")
	}
}
