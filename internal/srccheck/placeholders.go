package srccheck

import (
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// W-1（design-windows v0.2 决定 3）：Windows 的占位实现必须失败，不能假装成功。
//
// PlaceholdersFail 遍历 root 下所有“Windows 上编译、macOS 上不编译”的非测试 .go 文件（构建约束或文件名让它在
// windows/amd64 上参与编译、在 darwin/arm64 上不参与；跳过 third_party、testdata）——也就是 Windows 实际用到的
// 平台替身，含和 Linux 共用的 *_other.go——逐个检查里面的函数和方法：
//   - 返回“本平台尚未支持”的（引用了 platform.Unsupported 或 platform.ErrUnsupported）：不能有 error 位置是 nil 的
//     return，也不能 return true；
//   - 否则只能是纯计算：没有 error 结果，不 return true，不调用 syscall、unix、windows、os、exec、net 包里的函数。
//
// allow 按“相对路径:函数名”列例外（值是理由）；“相对路径:*”表示整个文件是真正的实现而不是占位（理由里写清楚
// 它由什么测试守着）。返回发现的违规数（同时经 t.Errorf 报告）。
func PlaceholdersFail(t testing.TB, root string, allow map[string]string) int {
	t.Helper()
	bad := 0
	report := func(format string, a ...any) {
		t.Helper()
		bad++
		t.Errorf(format, a...)
	}
	files := WindowsOnlyFiles(t, root)
	for _, rel := range files {
		if _, ok := allow[rel+":*"]; ok {
			continue
		}
		af := Parse(t, filepath.Join(root, rel))
		inPlatform := af.Name.Name == "platform"
		for _, d := range af.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			key := rel + ":" + fd.Name.Name
			if _, ok := allow[key]; ok {
				continue
			}
			for _, msg := range checkPlaceholder(fd, inPlatform) {
				report("%s：%s", key, msg)
			}
		}
	}
	return bad
}

// WindowsOnlyFiles 返回 root 下 Windows 上编译、macOS 上不编译的非测试 .go 文件（相对路径，已排序）。
func WindowsOnlyFiles(t testing.TB, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "third_party", "testdata", ".git", ".claude", "node_modules", "out":
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		on := func(goos, arch string) bool { return buildsOn(p, name, goos, arch) }
		if on("windows", "amd64") && !on("darwin", "arm64") {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s：%v", root, err)
	}
	return out
}

var knownOS = map[string]bool{"windows": true, "darwin": true, "linux": true, "freebsd": true, "openbsd": true,
	"netbsd": true, "android": true, "ios": true, "plan9": true, "js": true, "wasip1": true, "solaris": true, "aix": true,
	"dragonfly": true, "illumos": true}
var knownArch = map[string]bool{"amd64": true, "arm64": true, "386": true, "arm": true, "wasm": true, "riscv64": true}
var unixOS = map[string]bool{"darwin": true, "linux": true, "freebsd": true, "openbsd": true, "netbsd": true,
	"android": true, "ios": true, "solaris": true, "aix": true, "dragonfly": true, "illumos": true}

// buildsOn 按文件名后缀（_GOOS、_GOARCH、_GOOS_GOARCH）和 //go:build 判断文件在 goos/arch 上是否参与编译
// （不带额外标签，如 abroaddev、integration）。
func buildsOn(path, name, goos, arch string) bool {
	base := strings.TrimSuffix(name, ".go")
	parts := strings.Split(base, "_")
	if n := len(parts); n >= 2 {
		last := parts[n-1]
		if knownArch[last] {
			if last != arch {
				return false
			}
			if n >= 3 && knownOS[parts[n-2]] && parts[n-2] != goos {
				return false
			}
		} else if knownOS[last] && last != goos {
			return false
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false
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
			return false
		}
		return expr.Eval(func(tag string) bool {
			return tag == goos || tag == arch || (tag == "unix" && unixOS[goos])
		})
	}
	return true
}

// impurePkgs 是“做系统调用”的包：纯计算的占位函数不能调用它们。
var impurePkgs = map[string]bool{"syscall": true, "unix": true, "windows": true, "os": true, "exec": true, "net": true}

func checkPlaceholder(fd *ast.FuncDecl, inPlatform bool) []string {
	var msgs []string
	refsUnsupported := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && id.Name == "platform" && (x.Sel.Name == "Unsupported" || x.Sel.Name == "ErrUnsupported") {
				refsUnsupported = true
			}
		case *ast.Ident:
			if inPlatform && (x.Name == "Unsupported" || x.Name == "ErrUnsupported") {
				refsUnsupported = true
			}
		}
		return true
	})
	errLast := false
	hasErr := false
	if fd.Type.Results != nil {
		list := fd.Type.Results.List
		for i, f := range list {
			if id, ok := f.Type.(*ast.Ident); ok && id.Name == "error" {
				hasErr = true
				if i == len(list)-1 {
					errLast = true
				}
			}
		}
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false // 闭包里的 return 不算这个函数的
		}
		r, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		for _, e := range r.Results {
			if id, ok := e.(*ast.Ident); ok && id.Name == "true" {
				msgs = append(msgs, "占位不能 return true（校验不能当作通过）")
			}
		}
		if errLast && len(r.Results) > 0 {
			if id, ok := r.Results[len(r.Results)-1].(*ast.Ident); ok && id.Name == "nil" {
				msgs = append(msgs, "占位不能返回 nil 错误（不能假装成功）")
			}
		}
		return true
	})
	if refsUnsupported {
		return msgs
	}
	if hasErr {
		msgs = append(msgs, "有 error 结果却没有返回“本平台尚未支持”（platform.Unsupported）")
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && impurePkgs[id.Name] {
				msgs = append(msgs, "不是纯计算：调用了 "+id.Name+"."+sel.Sel.Name)
			}
		}
		return true
	})
	return msgs
}

// parseSrc 解析源码字符串（checker 自身的单测用）。
func parseSrc(t testing.TB, src string) *ast.File {
	t.Helper()
	af, err := parser.ParseFile(token.NewFileSet(), "x.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	return af
}
