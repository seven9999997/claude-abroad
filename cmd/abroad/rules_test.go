package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"claude-abroad/internal/api"
	"claude-abroad/internal/apiclient"
	"claude-abroad/internal/daemon"
)

// stub 是一个只回固定应答的本地接口（测试后台服务不容易造出来的应答：忙、环境检测的结果）。
func stub(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "as")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p := filepath.Join(dir, "s")
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return p
}

func runSock(sock string, admin bool, args ...string) res {
	var out, errb bytes.Buffer
	cl := &apiclient.Client{Socket: sock}
	a := &app{stdin: strings.NewReader(""), stdout: &out, stderr: &errb, client: cl,
		isAdmin: func() bool { return admin }, isTerminal: func() bool { return false }}
	code := a.run(args)
	return res{code: code, out: out.String(), err: errb.String(), reqs: cl.Requests}
}

// check 的文本和 JSON 输出（假系统没有环境检测，用桩）。
func TestStubOutputs(t *testing.T) {
	p := stub(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"at":"2026-11-22T00:00:00Z","hard":"pass","items":[{"id":"06","name":"IPv6","level":"hard","result":"pass"},{"id":"15","name":"shell 配置","level":"warn","result":"fail","detail":".zshrc 第 2 行"}]}`)
	})
	x := runSock(p, false, "check")
	if x.code != 0 || !strings.Contains(x.out, "硬性项：pass") || !strings.Contains(x.out, "15 shell 配置：fail .zshrc 第 2 行") {
		t.Fatalf("%+v", x)
	}
	x = runSock(p, false, "--json", "check")
	if x.code != 0 || !strings.Contains(x.out, `"hard":"pass"`) {
		t.Fatalf("%+v", x)
	}
}

// 验收 e：六种退出码各一例，与帮助一致。
func TestExitCodes(t *testing.T) {
	busy := stub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"error":"有别的操作在进行"}`)
	})
	fail := stub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"没有这个出口"}`)
	})
	confirm := stub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"error":"需要确认","confirm":"0123456789abcdef0123456789abcdef","preview":{"processes":[]}}`)
	})
	cases := []struct {
		name string
		x    res
		want int
	}{
		{"成功", runSock(fail, false, "--help"), ExitOK},
		{"其他失败", runSock(fail, false, "exit", "check", "nope"), ExitFail},
		{"参数错误", runSock(fail, false, "frobnicate"), ExitUsage},
		{"被拒", runSock(fail, false, "exit", "rm", "wg"), ExitDenied},
		{"需要确认", runSock(confirm, true, "restore"), ExitConfirm},
		{"忙", runSock(busy, false, "down"), ExitBusy},
		{"连不上", runSock(filepath.Join(t.TempDir(), "nope"), false, "status"), ExitNoDaemon},
		{"以后的版本", runSock(fail, false, "engine", "install", "x"), ExitLater},
	}
	for _, c := range cases {
		if c.x.code != c.want {
			t.Errorf("%s：退出码 %d，应为 %d（%+v）", c.name, c.x.code, c.want, c.x)
		}
	}
	// 帮助里的退出码说明和常量一致。
	for code, word := range map[int]string{ExitOK: "成功", ExitFail: "其他失败", ExitUsage: "参数错误", ExitDenied: "被拒", ExitConfirm: "需要确认", ExitBusy: "忙", ExitNoDaemon: "连不上后台服务", ExitLater: "以后的版本提供"} {
		if !strings.Contains(usage, strconv.Itoa(code)+" "+word) {
			t.Errorf("帮助里没有“%d %s”", code, word)
		}
	}
}

// 验收 f：连不上的三种情况文字不同、可读、没有底层错误和堆栈。
func TestConnErrors(t *testing.T) {
	missing := runSock(filepath.Join(t.TempDir(), "nope"), false, "status")
	dir, _ := os.MkdirTemp("", "as")
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "s")
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	stale := runSock(p, false, "status")
	r := newRig(t, rigOpt{reject: true})
	rejected := r.run("status")
	texts := map[string]bool{}
	for name, x := range map[string]res{"不存在": missing, "没在听": stale, "被断开": rejected} {
		if x.code != ExitNoDaemon || x.out != "" {
			t.Errorf("%s：%+v", name, x)
		}
		for _, bad := range []string{"dial", "unix", "goroutine", "panic", ".go:"} {
			if strings.Contains(x.err, bad) {
				t.Errorf("%s：提示里有底层错误 %q：%s", name, bad, x.err)
			}
		}
		texts[x.err] = true
	}
	if len(texts) != 3 {
		t.Fatalf("三种情况的提示应各不相同：%v", texts)
	}
	if !strings.Contains(missing.err, "没有运行") || !strings.Contains(stale.err, "没有在监听") || !strings.Contains(rejected.err, "拒绝") {
		t.Fatalf("%q / %q / %q", missing.err, stale.err, rejected.err)
	}
}

// 子命令和路由表一一对上：每个子命令的路由 ID 都在路由表里；路由表的每个接口要么有子命令，要么在 notInCLI 里写了理由。
func TestRouteCoverage(t *testing.T) {
	ids := map[string]api.Route{}
	for _, r := range daemon.RouteTable() {
		ids[r.ID] = r
	}
	used := map[string]bool{}
	for _, c := range commands {
		if c.route == "" {
			continue // 后端还没有的子命令（引擎安装、回滚）
		}
		if _, ok := ids[c.route]; !ok {
			t.Errorf("%v：路由 %q 不在路由表里", c.path, c.route)
		}
		used[c.route] = true
	}
	for id := range ids {
		if !used[id] && notInCLI[id] == "" && id != "status" {
			t.Errorf("路由 %s 没有子命令，也没写理由", id)
		}
	}
	for id := range notInCLI {
		if _, ok := ids[id]; !ok {
			t.Errorf("notInCLI 里的 %s 不在路由表里", id)
		}
		if used[id] {
			t.Errorf("%s 既有子命令又写在 notInCLI 里", id)
		}
	}
	// 帮助里每个敏感子命令都带 sudo（帮助和路由表的级别一致）。
	for _, c := range commands {
		if c.later != "" || routeLevel(c.route) != api.Sensitive {
			continue
		}
		line := "sudo abroad " + strings.Join(c.path, " ")
		if !strings.Contains(usage, line) {
			t.Errorf("帮助里应写“%s”", line)
		}
	}
}

// 验收 h：源码检查。cmd/abroad 不执行外部命令、不写文件；读文件只在 readInput（用户给的输入）；
// 本地的 sudo 判断来自路由表（routeLevel 用 daemon.RouteTable，没有另写一张敏感子命令的表）。
func TestSourceRules(t *testing.T) {
	fset := token.NewFileSet()
	files, _ := filepath.Glob("*.go")
	forbidCalls := map[string]bool{
		"os.WriteFile": true, "os.Create": true, "os.OpenFile": true, "os.Mkdir": true, "os.MkdirAll": true, "os.Remove": true,
		"os.RemoveAll": true, "os.Rename": true, "os.Chmod": true, "os.Chown": true, "os.Symlink": true, "os.Link": true, "os.Truncate": true,
		"os.Open": true, "os.StartProcess": true, "syscall.Exec": true, "syscall.ForkExec": true, "os.Setenv": true,
	}
	allowImports := map[string]bool{"claude-abroad/internal/api": true, "claude-abroad/internal/apiclient": true, "claude-abroad/internal/daemon": true,
		"claude-abroad/internal/platform": true, "claude-abroad/internal/rules": true}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		checked++
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range af.Imports {
			path, _ := strconv.Unquote(im.Path.Value)
			switch {
			case path == "os/exec" || path == "plugin" || path == "unsafe" || path == "net/http":
				t.Errorf("%s 引入了 %s", f, path)
			case strings.HasPrefix(path, "claude-abroad/") && !allowImports[path]:
				t.Errorf("%s 引入了 %s（命令行只是接口的客户端）", f, path)
			}
		}
		ast.Inspect(af, func(n ast.Node) bool {
			fd, ok := n.(*ast.FuncDecl)
			if !ok {
				return true
			}
			ast.Inspect(fd, func(m ast.Node) bool {
				ce, ok := m.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := ce.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				name := id.Name + "." + sel.Sel.Name
				if forbidCalls[name] {
					t.Errorf("%s：%s 里调用了 %s", f, fd.Name.Name, name)
				}
				if name == "os.ReadFile" && fd.Name.Name != "readInput" {
					t.Errorf("%s：读文件只能在 readInput 里（%s 调用了 os.ReadFile）", f, fd.Name.Name)
				}
				return true
			})
			return false
		})
	}
	if checked < 2 {
		t.Fatal("没找到源码")
	}
	src, _ := os.ReadFile("main.go")
	if !regexp.MustCompile(`func routeLevel\(id string\) api\.Level \{\s*for _, r := range daemon\.RouteTable\(\)`).Match(src) {
		t.Fatal("routeLevel 应直接取 daemon.RouteTable()")
	}
	// 不另写敏感子命令的表：源码里不出现 api.Sensitive 的字面比较之外的级别表。
	all := string(src)
	if b, err := os.ReadFile("commands.go"); err == nil {
		all += string(b)
	}
	if strings.Count(all, "== api.Sensitive") != 1 || strings.Contains(all, "Level:") {
		t.Fatal("需要 sudo 的判断只能来自路由表（只在 run 里和 api.Sensitive 比较一次）")
	}
}
