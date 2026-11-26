package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"

	"claude-abroad/internal/apiclient"
	"claude-abroad/internal/rules"

	"go.yaml.in/yaml/v3"
)

// command 是一个子命令：path 是命令词，route 是它对应的路由 ID（级别从路由表取），later 非空表示以后的版本提供。
type command struct {
	path  []string
	route string
	later string
	run   func(a *app, args []string) int
}

// commands 是全部子命令（design 11.3）。
var commands = []command{
	{path: []string{"status"}, route: "status", run: cmdStatus},
	{path: []string{"check"}, route: "envcheck-run", run: cmdCheck},
	{path: []string{"up"}, route: "enable", run: cmdUp},
	{path: []string{"down"}, route: "disable", run: cmdDown},
	{path: []string{"restore"}, route: "restore", run: cmdRestore},
	{path: []string{"uninstall"}, route: "uninstall", run: cmdUninstall},
	{path: []string{"apps"}, route: "apps", run: cmdApps},
	{path: []string{"main", "show"}, route: "main-get", run: cmdMainShow},
	{path: []string{"main", "exit"}, route: "main-exit", run: cmdMainExit},
	{path: []string{"main", "nodes"}, route: "main-sources", run: cmdMainNodes},
	{path: []string{"main", "check"}, route: "main-check", run: cmdMainCheck},
	{path: []string{"main", "add-source"}, route: "main-source-add", run: cmdMainAddSource},
	{path: []string{"main", "add-node"}, route: "main-source-add", run: cmdMainAddNode},
	{path: []string{"main", "rm-source"}, route: "main-source-delete", run: cmdMainRmSource},
	{path: []string{"main", "refresh"}, route: "main-source-refresh", run: cmdMainRefresh},
	{path: []string{"main", "use"}, route: "main-select", run: cmdMainUse},
	{path: []string{"main", "confirm-lan"}, route: "main-lan-confirm", run: cmdMainConfirmLAN},
	{path: []string{"main", "apply"}, route: "main-proxy-apply", run: cmdMainApply},
	{path: []string{"exit", "ls"}, route: "exits-list", run: cmdExitLs},
	{path: []string{"exit", "check"}, route: "exit-check", run: cmdExitCheck},
	{path: []string{"exit", "add"}, route: "exit-put", run: cmdExitAdd},
	{path: []string{"exit", "rm"}, route: "exit-delete", run: cmdExitRm},
	{path: []string{"app", "ls"}, route: "rules-get", run: cmdAppLs},
	{path: []string{"app", "set"}, route: "rules-put", run: cmdAppSet},
	{path: []string{"app", "rm"}, route: "rules-put", run: cmdAppRm},
	{path: []string{"region", "show"}, route: "region-get", run: cmdRegionShow},
	{path: []string{"region", "set"}, route: "region-put", run: cmdRegionSet},
	{path: []string{"region", "auto"}, route: "region-put", run: cmdRegionAuto},
	{path: []string{"engine", "ls"}, route: "engines", run: cmdEngineLs},
	{path: []string{"launchguard"}, route: "launchguard", run: cmdLaunchGuard},
	{path: []string{"diagnostics"}, route: "diagnostics", run: cmdDiagnostics},
	{path: []string{"engine", "install"}, later: "引擎的安装和升级随更新机制提供"},
	{path: []string{"engine", "rollback"}, later: "引擎的回滚随更新机制提供"},
}

// notInCLI 是路由表里有、但命令行不单独提供的接口（别名、内部用、只读的细节），测试据此核对覆盖。
var notInCLI = map[string]string{
	"restore-alias":         "回国的别名，命令行用 restore",
	"restore-preview":       "设置窗口用；命令行的 restore 在确认前已经打印同样的清单",
	"exit-get":              "单个出口的摘要，exit ls 已包含",
	"envcheck-get":          "最近一轮的结果，status 已包含",
	"envcheck-settings-get": "设置窗口用",
	"envcheck-settings-put": "设置窗口用",
	"job-get":               "命令行用同步执行",
	"events":                "status --watch",
	"preflight":             "启用被拦下时已经列出",
	"setup-get":             "安装引导是应用里的流程",
	"setup-complete":        "安装引导是应用里的流程",
	"envcheck-device":       "由 check --device 调用（M6-6a）",
}

func findCommand(words []string) (*command, []string) {
	var best *command
	for i := range commands {
		c := &commands[i]
		if len(words) < len(c.path) {
			continue
		}
		ok := true
		for j, w := range c.path {
			if words[j] != w {
				ok = false
			}
		}
		if ok && (best == nil || len(c.path) > len(best.path)) {
			best = c
		}
	}
	if best == nil {
		return nil, nil
	}
	return best, words[len(best.path):]
}

// readInput 读用户给的输入文件（"-" 是标准输入）。命令行里唯一的读文件的地方。
func (a *app) readInput(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(io.LimitReader(a.stdin, 16<<20))
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读不了 %s", path)
	}
	return b, nil
}

// flags 是简单的“--名字 值”和开关解析；sensitive 里的名字一律拒绝（敏感内容不经参数传入）。
type flagSet struct {
	vals  map[string]string
	bools map[string]bool
	multi map[string][]string
	pos   []string
}

func parseFlags(args []string, withValue, switches, multi []string) (flagSet, error) {
	f := flagSet{vals: map[string]string{}, bools: map[string]bool{}, multi: map[string][]string{}}
	in := func(list []string, s string) bool {
		for _, x := range list {
			if x == s {
				return true
			}
		}
		return false
	}
	for i := 0; i < len(args); i++ {
		s := args[i]
		switch {
		case in(switches, s):
			f.bools[s] = true
		case in(withValue, s) || in(multi, s):
			if i+1 >= len(args) {
				return f, fmt.Errorf("%s 后面要跟一个值", s)
			}
			if in(multi, s) {
				f.multi[s] = append(f.multi[s], args[i+1])
			} else {
				f.vals[s] = args[i+1]
			}
			i++
		case strings.HasPrefix(s, "--"):
			return f, fmt.Errorf("不认识的选项 %s", s)
		default:
			f.pos = append(f.pos, s)
		}
	}
	return f, nil
}

// rejectSecrets 拒绝经参数传入的敏感内容：订阅地址（像 URL 的参数）、--url、--pass 一类的选项、--conf。
func (a *app) rejectSecrets(args []string) bool {
	for _, s := range args {
		low := strings.ToLower(s)
		switch {
		case low == "--url", low == "--pass", low == "--password", low == "--user", low == "--conf", low == "--content",
			strings.HasPrefix(low, "--url="), strings.HasPrefix(low, "--pass"), strings.HasPrefix(low, "--conf="):
			fmt.Fprintln(a.stderr, "订阅地址、密码、出口配置不能写在命令行参数里（会留在进程列表和 shell 历史里）：请用 --file、--url-file 或 --stdin")
			return true
		case strings.Contains(low, "://"):
			fmt.Fprintln(a.stderr, "订阅地址不能写在命令行参数里（会留在进程列表和 shell 历史里）：请把它存进文件后用 --url-file，或用 --stdin（配置文件用 --file）")
			return true
		}
	}
	return false
}

func usageErr(a *app, msg string) int {
	fmt.Fprintf(a.stderr, "%s\n（abroad --help 看用法）\n", msg)
	return ExitUsage
}

// ---- 子命令 ----

func cmdStatus(a *app, args []string) int {
	f, err := parseFlags(args, nil, []string{"--watch"}, nil)
	if err != nil {
		return usageErr(a, err.Error())
	}
	if f.bools["--watch"] {
		return cmdWatch(a)
	}
	resp, code := a.call("GET", "/v1/status", nil)
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() {
		var st struct {
			State    string `json:"state"`
			GateOpen bool   `json:"gate_open"`
			Gate     struct {
				Reasons []string `json:"reasons"`
			} `json:"gate"`
			Security *struct {
				CallerVerification string `json:"caller_verification"`
				Note               string `json:"note"`
			} `json:"security"`
			Region *struct {
				Target *struct{ Timezone, Lang string } `json:"target"`
			} `json:"region"`
			Integrity *struct {
				OK       bool   `json:"ok"`
				Checked  bool   `json:"checked"`
				Note     string `json:"note"`
				Problems []struct {
					File string `json:"file"`
					Kind string `json:"kind"`
				} `json:"problems"`
			} `json:"integrity"`
			Prepare *struct {
				Done  bool     `json:"done"`
				Notes []string `json:"notes"`
			} `json:"prepare"`
		}
		_ = json.Unmarshal(resp.Body, &st)
		fmt.Fprintf(a.stdout, "状态：%s\n闸门：%s\n", st.State, map[bool]string{true: "开", false: "关"}[st.GateOpen])
		if len(st.Gate.Reasons) > 0 {
			fmt.Fprintf(a.stdout, "关闭原因：%s\n", strings.Join(st.Gate.Reasons, "、"))
		}
		if st.Region != nil && st.Region.Target != nil {
			fmt.Fprintf(a.stdout, "地区：%s，%s\n", st.Region.Target.Timezone, st.Region.Target.Lang)
		}
		if st.Security != nil && st.Security.CallerVerification == "disabled" {
			fmt.Fprintf(a.stdout, "注意：%s\n", st.Security.Note)
		}
		if ig := st.Integrity; ig != nil {
			switch {
			case ig.OK && !ig.Checked:
				fmt.Fprintf(a.stdout, "完整性校验：%s\n", ig.Note)
			case ig.OK:
				fmt.Fprintln(a.stdout, "完整性校验：通过")
			default:
				var parts []string
				for _, p := range ig.Problems {
					parts = append(parts, p.File+"（"+map[string]string{"missing": "不见了", "mismatch": "内容不对", "manifest": "清单被改过或读不出"}[p.Kind]+"）")
				}
				fmt.Fprintf(a.stdout, "完整性校验不通过：%s。只保留断网规则、不能启用，请重新安装。\n", strings.Join(parts, "、"))
			}
		}
		if st.Prepare != nil {
			if !st.Prepare.Done {
				fmt.Fprintln(a.stdout, "首次准备：没有完成")
			}
			for _, n := range st.Prepare.Notes {
				fmt.Fprintf(a.stdout, "首次准备：%s\n", n)
			}
		}
	})
}

func cmdWatch(a *app) int {
	fmt.Fprintln(a.stdout, "持续显示事件（Ctrl+C 退出）")
	err := a.client.Events(a.ctx, func(ev apiclient.Event) {
		if a.json {
			fmt.Fprintln(a.stdout, ev.Data)
			return
		}
		var se struct{ Source, Kind, Detail string }
		_ = json.Unmarshal([]byte(ev.Data), &se)
		fmt.Fprintf(a.stdout, "%s/%s %s\n", se.Source, se.Kind, se.Detail)
	})
	switch {
	case err == nil:
		return ExitOK
	case err == apiclient.ErrStreamClosed:
		fmt.Fprintln(a.stderr, "和后台服务的连接断开了（后台服务可能在重启）")
		return ExitNoDaemon
	}
	if se, ok := err.(*apiclient.StatusError); ok {
		fmt.Fprintln(a.stderr, se.Text)
		return ExitBusy
	}
	return a.connErr(err)
}

func cmdCheck(a *app, args []string) int {
	f, err := parseFlags(args, nil, []string{"--device"}, nil)
	if err != nil {
		return usageErr(a, err.Error())
	}
	if f.bools["--device"] { // 设备体检（M6-6a）：字体等，只提示，不参与放行
		resp, code := a.call("POST", "/v1/envcheck/device", nil)
		if code != ExitOK {
			return code
		}
		return a.out(resp, func() {
			var rep struct {
				Findings []struct{ Font, File, Location string } `json:"findings"`
				Note     string                                  `json:"note"`
			}
			_ = json.Unmarshal(resp.Body, &rep)
			if len(rep.Findings) == 0 {
				fmt.Fprintln(a.stdout, "设备体检：没有发现随中文办公软件安装的字体")
				return
			}
			fmt.Fprintf(a.stdout, "设备体检：发现 %d 个随中文办公软件安装的字体\n", len(rep.Findings))
			for _, x := range rep.Findings {
				fmt.Fprintf(a.stdout, "  %s：%s（%s）\n", x.Location, x.Font, x.File)
			}
			fmt.Fprintln(a.stdout, rep.Note)
		})
	}
	resp, code := a.call("POST", "/v1/envcheck/run", nil)
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() {
		var rep struct {
			Hard  string                                      `json:"hard"`
			Items []struct{ ID, Name, Result, Detail string } `json:"items"`
		}
		_ = json.Unmarshal(resp.Body, &rep)
		fmt.Fprintf(a.stdout, "硬性项：%s\n", rep.Hard)
		for _, it := range rep.Items {
			fmt.Fprintf(a.stdout, "  %s %s：%s %s\n", it.ID, it.Name, it.Result, it.Detail)
		}
	})
}

func printState(a *app, resp apiclient.Response) int {
	return a.out(resp, func() {
		var st struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal(resp.Body, &st)
		fmt.Fprintf(a.stdout, "完成，现在的状态：%s\n", st.State)
	})
}

func cmdUp(a *app, args []string) int {
	if len(args) > 0 {
		return usageErr(a, "up 不带参数")
	}
	resp, code := a.confirmed("POST", "/v1/enable", nil)
	if code != ExitOK {
		return code
	}
	return printState(a, resp)
}

func cmdDown(a *app, args []string) int {
	if len(args) > 0 {
		return usageErr(a, "down 不带参数")
	}
	resp, code := a.call("POST", "/v1/disable", nil)
	if code != ExitOK {
		return code
	}
	return printState(a, resp)
}

// cmdUninstall 一键卸载（设计 10.3）：两步确认，终端里要输入 uninstall（或 --yes）；可加 --kill-browsers。
func cmdUninstall(a *app, args []string) int {
	f, err := parseFlags(args, nil, []string{"--kill-browsers"}, nil)
	if err != nil {
		return usageErr(a, err.Error())
	}
	var body map[string]any
	if f.bools["--kill-browsers"] {
		body = map[string]any{"kill_browsers": true}
	}
	resp, code := a.confirmedWord("POST", "/v1/actions/uninstall", body, "uninstall")
	if code != ExitOK {
		printUninstallSteps(a, resp.Body, true) // 失败时也列出走到了哪一步（M6-5a 补审建议改）
		return code
	}
	return a.out(resp, func() { printUninstallSteps(a, resp.Body, false) })
}

// printUninstallSteps 列出卸载的步骤：✓ 完成；✗ 失败停在这一步；… 还没轮到（例如要由安装器完成的那一步）。
func printUninstallSteps(a *app, body []byte, failed bool) {
	var v struct {
		Steps []struct {
			Name string
			Done bool
			Note string
		} `json:"steps"`
		Complete bool `json:"complete"`
	}
	if json.Unmarshal(body, &v) != nil || len(v.Steps) == 0 {
		return
	}
	for i, s := range v.Steps {
		mark := "✓"
		switch {
		case s.Done:
		case failed && i == len(v.Steps)-1:
			mark = "✗"
		default:
			mark = "…"
		}
		note := ""
		if s.Note != "" {
			note = "（" + s.Note + "）"
		}
		fmt.Fprintf(a.stdout, "%s %s%s\n", mark, s.Name, note)
	}
	switch {
	case failed:
		fmt.Fprintln(a.stdout, "卸载停在打 ✗ 的那一步；处理后重试 sudo abroad uninstall，已完成的步骤不会重复出问题")
	case !v.Complete:
		fmt.Fprintln(a.stdout, "还有步骤要由安装器完成（见上）")
	}
}

func cmdRestore(a *app, args []string) int {
	f, err := parseFlags(args, nil, []string{"--force", "--kill-browsers"}, nil)
	if err != nil {
		return usageErr(a, err.Error())
	}
	if f.bools["--force"] {
		fmt.Fprintln(a.stderr, "restore --force 改成了单独的应急程序：后台服务不在、或状态文件损坏时，先按应急文档停掉后台服务，再执行 sudo abroad-rescue。后台服务正常时请用 abroad restore。")
		return ExitUsage
	}
	var body map[string]any
	if f.bools["--kill-browsers"] { // 用户显式选择：同时结束确认过的浏览器（M6-4b-3）
		body = map[string]any{"kill_browsers": true}
	}
	resp, code := a.confirmed("POST", "/v1/restore", body)
	if code != ExitOK {
		return code
	}
	return printState(a, resp)
}

func cmdApps(a *app, args []string) int {
	resp, code := a.call("GET", "/v1/apps", nil)
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() {
		var v struct {
			Apps []struct {
				Name  string   `json:"name"`
				Conns int      `json:"conns"`
				Exits []string `json:"exits"`
			} `json:"apps"`
		}
		_ = json.Unmarshal(resp.Body, &v)
		if len(v.Apps) == 0 {
			fmt.Fprintln(a.stdout, "现在没有在联网的应用")
		}
		for _, x := range v.Apps {
			fmt.Fprintf(a.stdout, "%s  %d 个连接  %s\n", x.Name, x.Conns, strings.Join(x.Exits, "、"))
		}
	})
}

// ---- 主代理 ----

type nodeRef struct{ ID, Name, Type, Source string }

func (a *app) nodes() ([]nodeRef, int) {
	resp, code := a.call("GET", "/v1/main-proxy/sources", nil)
	if code != ExitOK {
		return nil, code
	}
	var vs []struct {
		ID, Name string
		Nodes    []struct{ ID, Name, Type string } `json:"nodes"`
	}
	_ = json.Unmarshal(resp.Body, &vs)
	var out []nodeRef
	for _, s := range vs {
		for _, n := range s.Nodes {
			out = append(out, nodeRef{ID: n.ID, Name: n.Name, Type: n.Type, Source: s.Name})
		}
	}
	return out, ExitOK
}

// resolveNode 按 ID 或名字找节点（名字重复时要求用 ID）。
func (a *app) resolveNode(ref string) (string, int) {
	ns, code := a.nodes()
	if code != ExitOK {
		return "", code
	}
	var hits []nodeRef
	for _, n := range ns {
		if n.ID == ref {
			return n.ID, ExitOK
		}
		if n.Name == ref {
			hits = append(hits, n)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0].ID, ExitOK
	case 0:
		fmt.Fprintf(a.stderr, "没有叫 %s 的节点（abroad main nodes 看全部）\n", ref)
	default:
		fmt.Fprintf(a.stderr, "有 %d 个节点叫 %s，请用节点 ID（abroad main nodes 看全部）\n", len(hits), ref)
	}
	return "", ExitUsage
}

func cmdMainShow(a *app, args []string) int {
	resp, code := a.call("GET", "/v1/main-proxy", nil)
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() {
		var v struct {
			Selected *struct{ Name, Type string } `json:"selected"`
			Chain    []struct{ Name string }      `json:"chain"`
			Applied  bool                         `json:"applied"`
			Reselect bool                         `json:"needs_reselect"`
			LAN      bool                         `json:"lan_pending"`
			Sources  int                          `json:"sources"`
		}
		_ = json.Unmarshal(resp.Body, &v)
		if v.Selected == nil {
			fmt.Fprintf(a.stdout, "还没有选主代理节点（来源 %d 个）\n", v.Sources)
			return
		}
		fmt.Fprintf(a.stdout, "主代理：%s（%s），%s\n", v.Selected.Name, v.Selected.Type, map[bool]string{true: "已下发", false: "还没下发"}[v.Applied])
		if len(v.Chain) > 1 {
			var names []string
			for _, c := range v.Chain {
				names = append(names, c.Name)
			}
			fmt.Fprintf(a.stdout, "链：%s\n", strings.Join(names, " → "))
		}
		if v.Reselect {
			fmt.Fprintln(a.stdout, "订阅更新后选中的节点不在了：请重新选择")
		}
		if v.LAN {
			fmt.Fprintln(a.stdout, "第一跳是局域网节点，还没有确认：sudo abroad main confirm-lan <节点>")
		}
	})
}

// cmdMainExit 显示主代理出口的 IP 和地区（来自出口基准；status 里不含出口 IP，只有这里显示）。
func cmdMainExit(a *app, args []string) int {
	if len(args) > 0 {
		return usageErr(a, "main exit 不带参数")
	}
	resp, code := a.call("GET", "/v1/main-proxy/exit", nil)
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() {
		var v struct {
			Available                           bool
			IP, Country, Region, City, Timezone string
			Note                                string
		}
		_ = json.Unmarshal(resp.Body, &v)
		if !v.Available {
			fmt.Fprintf(a.stdout, "主代理出口：%s\n", v.Note)
			return
		}
		place := v.Country
		for _, x := range []string{v.Region, v.City} {
			if x != "" {
				place += " " + x
			}
		}
		fmt.Fprintf(a.stdout, "主代理出口：%s（%s，%s）\n", v.IP, place, v.Timezone)
	})
}

func cmdMainNodes(a *app, args []string) int {
	resp, code := a.call("GET", "/v1/main-proxy/sources", nil)
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() {
		var vs []struct {
			ID, Name, Kind string
			Nodes          []struct{ ID, Name, Type string } `json:"nodes"`
		}
		_ = json.Unmarshal(resp.Body, &vs)
		for _, s := range vs {
			fmt.Fprintf(a.stdout, "%s（%s，%d 个节点）  来源 ID %s\n", s.Name, s.Kind, len(s.Nodes), s.ID)
			for _, n := range s.Nodes {
				fmt.Fprintf(a.stdout, "  %s  %s  %s\n", n.ID, n.Name, n.Type)
			}
		}
	})
}

func cmdMainCheck(a *app, args []string) int {
	resp, code := a.call("POST", "/v1/main-proxy/check", nil)
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() {
		var b struct{ Country, Timezone string }
		_ = json.Unmarshal(resp.Body, &b)
		fmt.Fprintf(a.stdout, "主代理可用：出口在 %s（%s）\n", b.Country, b.Timezone)
	})
}

func cmdMainAddSource(a *app, args []string) int {
	if a.rejectSecrets(args) {
		return ExitUsage
	}
	f, err := parseFlags(args, []string{"--file", "--url-file", "--name"}, []string{"--stdin", "--via-host"}, nil)
	if err != nil {
		return usageErr(a, err.Error())
	}
	n := 0
	for _, x := range []bool{f.vals["--file"] != "", f.vals["--url-file"] != "", f.bools["--stdin"]} {
		if x {
			n++
		}
	}
	if n != 1 || len(f.pos) > 0 {
		return usageErr(a, "add-source 要从 --file、--url-file、--stdin 里选一种")
	}
	name := f.vals["--name"]
	body := map[string]any{"name": name, "via_host": f.bools["--via-host"]}
	var data []byte
	switch {
	case f.vals["--file"] != "":
		data, err = a.readInput(f.vals["--file"])
	case f.vals["--url-file"] != "":
		data, err = a.readInput(f.vals["--url-file"])
	default:
		data, err = a.readInput("-")
	}
	if err != nil {
		return usageErr(a, err.Error())
	}
	text := strings.TrimSpace(string(data))
	if u, perr := url.Parse(text); f.vals["--url-file"] != "" || perr == nil && (u.Scheme == "http" || u.Scheme == "https") && !strings.Contains(text, "\n") {
		body["kind"], body["url"] = "subscription", text
	} else {
		body["kind"], body["content"] = "yaml", string(data)
	}
	if name == "" {
		body["name"] = "命令行添加的来源"
	}
	resp, code := a.call("POST", "/v1/main-proxy/sources", body)
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() {
		var r struct{ ID string }
		_ = json.Unmarshal(resp.Body, &r)
		fmt.Fprintf(a.stdout, "已添加来源（ID %s）\n", r.ID)
	})
}

func cmdMainAddNode(a *app, args []string) int {
	if a.rejectSecrets(args) {
		return ExitUsage
	}
	f, err := parseFlags(args, []string{"--file", "--name"}, []string{"--stdin"}, nil)
	if err != nil {
		return usageErr(a, err.Error())
	}
	if (f.vals["--file"] == "") == !f.bools["--stdin"] || len(f.pos) > 0 {
		return usageErr(a, "add-node 要用 --file <节点 YAML> 或 --stdin")
	}
	path := f.vals["--file"]
	if path == "" {
		path = "-"
	}
	data, err := a.readInput(path)
	if err != nil {
		return usageErr(a, err.Error())
	}
	var m map[string]any
	if yaml.Unmarshal(data, &m) != nil || m == nil {
		return usageErr(a, "节点文件读不懂：要一个 YAML 映射（name、type、server、port …），或带 proxies: 的列表")
	}
	name := f.vals["--name"]
	if n, ok := m["name"].(string); ok && name == "" {
		name = n // 单个节点：默认用节点自己的名字
	}
	if name == "" {
		name = "手动节点"
	}
	body := map[string]any{"kind": "yaml", "name": name, "content": string(data)}
	if _, list := m["proxies"]; !list {
		body = map[string]any{"kind": "node", "name": name, "node": m}
	}
	resp, code := a.call("POST", "/v1/main-proxy/sources", body)
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() { fmt.Fprintln(a.stdout, "已添加节点") })
}

func cmdMainRmSource(a *app, args []string) int {
	if len(args) != 1 {
		return usageErr(a, "rm-source 要一个来源 ID")
	}
	resp, code := a.call("DELETE", "/v1/main-proxy/sources/"+url.PathEscape(args[0]), nil)
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() { fmt.Fprintln(a.stdout, "已删除来源") })
}

func cmdMainRefresh(a *app, args []string) int {
	f, err := parseFlags(args, nil, []string{"--via-host"}, nil)
	if err != nil || len(f.pos) != 1 {
		return usageErr(a, "refresh 要一个来源 ID")
	}
	resp, code := a.call("POST", "/v1/main-proxy/sources/"+url.PathEscape(f.pos[0])+"/refresh", map[string]any{"via_host": f.bools["--via-host"]})
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() {
		var r struct {
			Reselect bool `json:"needs_reselect"`
		}
		_ = json.Unmarshal(resp.Body, &r)
		fmt.Fprintln(a.stdout, "订阅已更新")
		if r.Reselect {
			fmt.Fprintln(a.stdout, "选中的节点不在了：请重新选择（正在运行的主代理不受影响）")
		}
	})
}

func cmdMainUse(a *app, args []string) int {
	f, err := parseFlags(args, nil, nil, []string{"--via"})
	if err != nil || len(f.pos) != 1 {
		return usageErr(a, "use 要一个节点（名字或 ID），中转节点用 --via")
	}
	exit, code := a.resolveNode(f.pos[0])
	if code != ExitOK {
		return code
	}
	body := map[string]any{"selected": exit}
	if vias := f.multi["--via"]; len(vias) > 0 {
		var chain []string
		for _, v := range vias {
			id, code := a.resolveNode(v)
			if code != ExitOK {
				return code
			}
			chain = append(chain, id)
		}
		body["chain"] = append(chain, exit)
	}
	resp, code := a.confirmed("PUT", "/v1/main-proxy", body)
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() {
		var r struct {
			Saved bool   `json:"saved"`
			Note  string `json:"note"`
		}
		_ = json.Unmarshal(resp.Body, &r)
		if r.Saved {
			fmt.Fprintln(a.stdout, r.Note)
			return
		}
		fmt.Fprintln(a.stdout, "主代理已更换")
	})
}

// cmdMainApply 经当前的主代理跑一轮带位置查询的检查，建立出口基准（启用前必须有）；已启用且地区会变时两步确认。
func cmdMainApply(a *app, args []string) int {
	f, err := parseFlags(args, []string{"--rule"}, nil, nil)
	if err != nil || len(f.pos) > 0 {
		return usageErr(a, "main apply [--rule <规则>]")
	}
	var body map[string]any
	if r := f.vals["--rule"]; r != "" {
		body = map[string]any{"rule": r}
	}
	resp, code := a.confirmed("POST", "/v1/main-proxy/apply", body)
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() {
		var v struct {
			Baseline struct{ Country, Timezone string } `json:"baseline"`
			Note     string                             `json:"note"`
		}
		_ = json.Unmarshal(resp.Body, &v)
		fmt.Fprintf(a.stdout, "已建立出口基准：出口在 %s（%s）\n", v.Baseline.Country, v.Baseline.Timezone)
		if v.Note != "" {
			fmt.Fprintln(a.stdout, v.Note)
		}
	})
}

func cmdMainConfirmLAN(a *app, args []string) int {
	if len(args) != 1 {
		return usageErr(a, "confirm-lan 要一个节点（名字或 ID）")
	}
	id, code := a.resolveNode(args[0])
	if code != ExitOK {
		return code
	}
	resp, code := a.call("POST", "/v1/main-proxy/lan-confirm", map[string]any{"node": id})
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() { fmt.Fprintln(a.stdout, "已确认这个局域网节点") })
}

// ---- 分流出口 ----

func cmdExitLs(a *app, args []string) int {
	resp, code := a.call("GET", "/v1/exits", nil)
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() {
		var v struct {
			Exits []struct {
				Tag  string `json:"tag"`
				Kind string `json:"kind"`
			} `json:"exits"`
		}
		_ = json.Unmarshal(resp.Body, &v)
		if len(v.Exits) == 0 {
			fmt.Fprintln(a.stdout, "没有分流出口：所有流量走主代理")
		}
		for _, x := range v.Exits {
			fmt.Fprintf(a.stdout, "%s  %s\n", x.Tag, x.Kind)
		}
	})
}

func cmdExitCheck(a *app, args []string) int {
	if len(args) != 1 {
		return usageErr(a, "check 要一个出口标签")
	}
	resp, code := a.call("POST", "/v1/exits/"+url.PathEscape(args[0])+"/check", nil)
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() {
		var r struct {
			Cached bool `json:"cached"`
			Status struct {
				Avail struct {
					Up     bool   `json:"up"`
					Reason string `json:"reason"`
				} `json:"avail"`
			} `json:"status"`
		}
		_ = json.Unmarshal(resp.Body, &r)
		fmt.Fprintf(a.stdout, "出口 %s：%s %s\n", args[0], map[bool]string{true: "可用", false: "不可用"}[r.Status.Avail.Up], r.Status.Avail.Reason)
	})
}

func cmdExitAdd(a *app, args []string) int {
	if a.rejectSecrets(args) {
		return ExitUsage
	}
	f, err := parseFlags(args, []string{"--kind", "--file"}, []string{"--stdin"}, nil)
	if err != nil || len(f.pos) != 1 || f.vals["--kind"] == "" || (f.vals["--file"] == "") == !f.bools["--stdin"] {
		return usageErr(a, "exit add <标签> --kind wireguard|openvpn|clash|sharelink --file <配置文件>（或 --stdin）；sharelink 是每行一个的分享链接（ss、vmess、vless、trojan、hysteria2、tuic）")
	}
	path := f.vals["--file"]
	if path == "" {
		path = "-"
	}
	data, err := a.readInput(path)
	if err != nil {
		return usageErr(a, err.Error())
	}
	resp, code := a.call("PUT", "/v1/exits/"+url.PathEscape(f.pos[0]), map[string]any{"kind": f.vals["--kind"], "conf": string(data)})
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() { fmt.Fprintf(a.stdout, "已保存出口 %s\n", f.pos[0]) })
}

func cmdExitRm(a *app, args []string) int {
	if len(args) != 1 {
		return usageErr(a, "rm 要一个出口标签")
	}
	resp, code := a.call("DELETE", "/v1/exits/"+url.PathEscape(args[0]), nil)
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() { fmt.Fprintf(a.stdout, "已删除出口 %s\n", args[0]) })
}

// ---- 应用分流 ----

func (a *app) currentRules() (*rules.Config, int) {
	resp, code := a.call("GET", "/v1/rules", nil)
	if code != ExitOK {
		return nil, code
	}
	c, err := rules.Parse(resp.Body)
	if err != nil {
		fmt.Fprintln(a.stderr, "读不懂后台服务给的规则")
		return nil, ExitFail
	}
	return c, ExitOK
}

func (a *app) putRules(c *rules.Config) (apiclient.Response, int) {
	b, err := c.Marshal()
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return apiclient.Response{}, ExitFail
	}
	return a.call("PUT", "/v1/rules?drop_affected=1", b)
}

func cmdAppLs(a *app, args []string) int {
	resp, code := a.call("GET", "/v1/rules", nil)
	if code != ExitOK {
		return code
	}
	if a.json {
		c, err := rules.Parse(resp.Body)
		if err != nil {
			return ExitFail
		}
		type rr struct{ App, Action string }
		var out []rr
		for _, r := range c.Rules {
			out = append(out, rr{string(r.App), r.Action.String()})
		}
		b, _ := json.Marshal(map[string]any{"default": c.Default.String(), "rules": out})
		fmt.Fprintln(a.stdout, string(b))
		return ExitOK
	}
	a.stdout.Write(resp.Body)
	return ExitOK
}

func cmdAppSet(a *app, args []string) int {
	if len(args) != 2 {
		return usageErr(a, "app set <应用> direct|block|main|proxy:<出口>")
	}
	act := args[1]
	if act == "main" {
		act = "proxy:main"
	}
	action, err := rules.ParseAction(act)
	if err != nil {
		return usageErr(a, "动作只能是 direct、block、main 或 proxy:<出口>")
	}
	c, code := a.currentRules()
	if code != ExitOK {
		return code
	}
	id := rules.NormalizeAppID(rules.AppID(args[0]))
	found := false
	for i := range c.Rules {
		if c.Rules[i].App == id {
			c.Rules[i].Action, found = action, true
		}
	}
	if !found {
		c.Rules = append(c.Rules, rules.Rule{App: id, Action: action})
	}
	resp, code := a.putRules(c)
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() { fmt.Fprintf(a.stdout, "%s → %s\n", id, action) })
}

func cmdAppRm(a *app, args []string) int {
	if len(args) != 1 {
		return usageErr(a, "app rm <应用>")
	}
	c, code := a.currentRules()
	if code != ExitOK {
		return code
	}
	id := rules.NormalizeAppID(rules.AppID(args[0]))
	var keep []rules.Rule
	for _, r := range c.Rules {
		if r.App != id {
			keep = append(keep, r)
		}
	}
	if len(keep) == len(c.Rules) {
		fmt.Fprintf(a.stderr, "没有 %s 的规则\n", id)
		return ExitFail
	}
	c.Rules = keep
	resp, code := a.putRules(c)
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() { fmt.Fprintf(a.stdout, "已删除 %s 的规则（改走默认）\n", id) })
}

// ---- 地区 ----

func cmdRegionShow(a *app, args []string) int {
	resp, code := a.call("GET", "/v1/region", nil)
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() {
		var v struct {
			Target *struct{ Timezone, Lang, Locale, Source string } `json:"target"`
		}
		_ = json.Unmarshal(resp.Body, &v)
		if v.Target == nil {
			fmt.Fprintln(a.stdout, "还没有锁定目标地区")
			return
		}
		fmt.Fprintf(a.stdout, "目标地区：%s，%s，%s（%s）\n", v.Target.Timezone, v.Target.Lang, v.Target.Locale, v.Target.Source)
	})
}

func cmdRegionSet(a *app, args []string) int {
	f, err := parseFlags(args, []string{"--locale"}, nil, nil)
	if err != nil || len(f.pos) != 2 {
		return usageErr(a, "region set <时区> <语言> [--locale <地区格式>]")
	}
	body := map[string]any{"mode": "manual", "timezone": f.pos[0], "lang": f.pos[1]}
	if l := f.vals["--locale"]; l != "" {
		body["locale"] = l
	}
	return regionPut(a, body)
}

func cmdRegionAuto(a *app, args []string) int {
	if len(args) > 0 {
		return usageErr(a, "region auto 不带参数")
	}
	return regionPut(a, map[string]any{"mode": "exit"})
}

func regionPut(a *app, body map[string]any) int {
	resp, code := a.confirmed("PUT", "/v1/region", body)
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() { fmt.Fprintln(a.stdout, "地区已切换") })
}

// ---- 引擎 ----

// cmdDiagnostics 输出诊断包（M6-6a）：后台已整段脱敏的 JSON，原样写到标准输出（重定向到文件交给维护者）。
func cmdDiagnostics(a *app, args []string) int {
	if len(args) > 0 {
		return usageErr(a, "diagnostics 不接受参数（重定向到文件：abroad diagnostics > 诊断包.json）")
	}
	resp, code := a.call("GET", "/v1/diagnostics", nil)
	if code != ExitOK {
		return code
	}
	a.stdout.Write(resp.Body)
	if len(resp.Body) > 0 && resp.Body[len(resp.Body)-1] != '\n' {
		fmt.Fprintln(a.stdout)
	}
	return ExitOK
}

// cmdLaunchGuard 显示启动管控（M6-4b）：现在是否允许启动 Claude、四层各自是否生效、第三层的覆盖范围、最近拦截。
func cmdLaunchGuard(a *app, args []string) int {
	resp, code := a.call("GET", "/v1/launchguard", nil)
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() {
		var v struct {
			Allowed bool `json:"allowed"`
			Layers  []struct {
				Layer     int
				Name      string
				Effective bool
				Note      string
			} `json:"layers"`
			Coverage []struct {
				Name   string
				Status string
				Reason string
			} `json:"coverage"`
			Recent []struct{} `json:"recent"`
		}
		_ = json.Unmarshal(resp.Body, &v)
		if v.Allowed {
			fmt.Fprintln(a.stdout, "现在允许启动 Claude")
		} else {
			fmt.Fprintln(a.stdout, "现在不允许启动 Claude：打开后会被马上结束")
		}
		for _, l := range v.Layers {
			st := "未生效"
			if l.Effective {
				st = "生效"
			}
			note := ""
			if l.Note != "" {
				note = "（" + l.Note + "）"
			}
			fmt.Fprintf(a.stdout, "  第%d层 %s：%s%s\n", l.Layer, l.Name, st, note)
		}
		fmt.Fprintln(a.stdout, "第三层的覆盖范围：")
		for _, c := range v.Coverage {
			st := map[string]string{"covered": "覆盖", "not_covered": "不覆盖", "by_signature": "视签名而定"}[c.Status]
			if st == "" {
				st = "未知"
			}
			fmt.Fprintf(a.stdout, "  %s：%s。%s\n", c.Name, st, c.Reason)
		}
		if len(v.Recent) > 0 {
			fmt.Fprintf(a.stdout, "最近拦下 %d 次\n", len(v.Recent))
		}
	})
}

func cmdEngineLs(a *app, args []string) int {
	resp, code := a.call("GET", "/v1/engines", nil)
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() {
		var v struct {
			Reachable bool `json:"reachable"`
			Version   string
			Engines   []struct {
				Name, State string
				Restarts    int
			} `json:"engines"`
		}
		_ = json.Unmarshal(resp.Body, &v)
		if !v.Reachable {
			fmt.Fprintln(a.stdout, "虚拟机不可达")
			return
		}
		sort.Slice(v.Engines, func(i, j int) bool { return v.Engines[i].Name < v.Engines[j].Name })
		fmt.Fprintf(a.stdout, "虚拟机：agent %s\n", v.Version)
		for _, e := range v.Engines {
			fmt.Fprintf(a.stdout, "  %s：%s（重启 %d 次）\n", e.Name, e.State, e.Restarts)
		}
	})
}
