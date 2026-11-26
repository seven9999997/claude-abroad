package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"claude-abroad/internal/rescue"
)

// 参数不对：退出码 2，什么都不做。
func TestBadArgs(t *testing.T) {
	for _, a := range [][]string{{"--force"}, {"extra"}, {"--state-dir", "relative"}, {"--dev-skip-claude-kill"}} {
		var so, se bytes.Buffer
		if code := run(a, strings.NewReader(""), &so, &se); code != rescue.ExitUsage {
			t.Errorf("%v：退出码 %d，应为 2", a, code)
		}
	}
}

// 不是 root：在做任何事之前退出（会话里不以 root 运行，这里走的就是真实入口）。
func TestNotRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 运行时不跑")
	}
	var so, se bytes.Buffer
	if code := run(nil, strings.NewReader("rescue\n"), &so, &se); code != rescue.ExitRefused {
		t.Fatalf("退出码 %d：%s", code, so.String())
	}
	if !strings.Contains(so.String(), "要以 root 运行") || !strings.Contains(so.String(), "什么都没改") {
		t.Fatalf("应说明要 root、什么都没改：%s", so.String())
	}
}

// 参数只有这三个：没有任何开发参数。
func TestFlags(t *testing.T) {
	var names []string
	b, _ := os.ReadFile("main.go")
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, "fs.") && strings.Contains(l, "Var(&o.") {
			names = append(names, strings.Split(strings.SplitN(l, `"`, 2)[1], `"`)[0])
		}
	}
	slices.Sort(names)
	if !reflect.DeepEqual(names, []string{"run-dir", "state-dir", "yes"}) {
		t.Fatalf("参数应只有 run-dir、state-dir、yes：%v", names)
	}
}

// 源码检查（验收 h）：应急程序和它的逻辑包不引用网络库、不执行外部命令、不监听；不碰系统服务管理；pf 只清空和释放
// 自己的锚点，没有“关闭”；没有开发构建的文件。
func TestSource(t *testing.T) {
	banImports := map[string]bool{"net": true, "net/http": true, "os/exec": true, "net/rpc": true, "net/url": true}
	for _, dir := range []string{".", filepath.Join("..", "..", "internal", "rescue")} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, p := range files {
			if strings.HasSuffix(p, "_test.go") {
				continue
			}
			src, _ := os.ReadFile(p)
			if strings.Contains(string(src), "abroaddev") {
				t.Errorf("%s：应急程序不能有开发构建", p)
			}
			for _, w := range []string{"launchctl", "pfctl", "Listen", "Disable(", "\"-d\"", "\"-E\"", "kill -", "os.FindProcess", "syscall.Kill"} {
				if strings.Contains(string(src), w) {
					t.Errorf("%s 里出现了 %q", p, w)
				}
			}
			f, err := parser.ParseFile(token.NewFileSet(), p, src, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, im := range f.Imports {
				if banImports[strings.Trim(im.Path.Value, `"`)] {
					t.Errorf("%s 引用了 %s", p, im.Path.Value)
				}
			}
		}
	}
	// pf 只经 rescue.PF：只有 Flush、Load（回国拦截那一组，M6-11b）和 Release 三个方法
	pfType := reflect.TypeOf((*rescue.PF)(nil)).Elem()
	var ms []string
	for i := 0; i < pfType.NumMethod(); i++ {
		ms = append(ms, pfType.Method(i).Name)
	}
	if !reflect.DeepEqual(ms, []string{"Flush", "Load", "Release"}) {
		t.Fatalf("rescue.PF 只能有 Flush、Load、Release：%v", ms)
	}
	// main.go 里 pf.Controller 只用默认锚点（不设 Anchor 字段）
	f, _ := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	ast.Inspect(f, func(n ast.Node) bool {
		if kv, ok := n.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Anchor" {
				t.Error("pf.Controller 不能改锚点（只用本项目自己的 com.apple/250.abroad）")
			}
		}
		return true
	})
}
