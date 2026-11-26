// abroad 是 Claude 出国的命令行（M5-6，design 11.3、m5-plan v2.4）。它只是本地接口的一个客户端：不直接读写状态目录、
// 规则文件、secrets，不执行任何系统命令（源码检查固定）。
//
// 需要 sudo 的子命令由路由表的级别推出：敏感的先在本地检查是不是 root（Windows 上是“以管理员身份运行”），不是就提示并退出、
// 不发请求；按本次调用决定级别的（启用、切换地区、改规则）先不带 sudo 发请求，被拒时提示“这一次需要 sudo”和原因。
// --yes 只跳过命令行自己的文字确认，不能代替 sudo，也不能代替两步确认（预览照样打印）。
// 订阅地址、节点密码、出口配置不经命令行参数传入，只从文件或标准输入读，输出里不回显。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"claude-abroad/internal/api"
	"claude-abroad/internal/apiclient"
	"claude-abroad/internal/daemon"
	"claude-abroad/internal/platform"
)

// 退出码（写进帮助；design 11.3 v0.25）。
const (
	ExitOK       = 0
	ExitFail     = 1 // 其他失败（后台服务回了错误）
	ExitUsage    = 2 // 参数错误
	ExitDenied   = 3 // 被拒（权限：需要 sudo，或后台服务回 403）
	ExitConfirm  = 4 // 需要确认（没有确认就不执行）
	ExitBusy     = 5 // 冲突（忙：有别的操作在进行）
	ExitNoDaemon = 6 // 连不上后台服务
	ExitLater    = 7 // 在以后的版本提供
)

const usage = `用法：abroad [--json] [--yes] [--socket <路径>] <子命令> …

  status [--watch]                 状态（--watch 持续显示事件，Ctrl+C 退出）
  check [--device]                 立即重新跑一轮环境检测
  up | down                        启用 | 停用
  sudo abroad restore [--kill-browsers]   一键回国（两步确认；后台服务不在时用 sudo abroad-rescue）
  sudo abroad uninstall            卸载
  apps                             在联网的应用
  main show | nodes | check | exit 主代理：当前选择、全部节点、检查、出口的 IP 和地区
  sudo abroad main add-source --file <配置文件> | --url-file <存订阅地址的文件> | --stdin  [--name <名字>] [--via-host]
  sudo abroad main add-node --file <节点 YAML> [--name <名字>]
  sudo abroad main rm-source <来源 ID>
  main refresh <来源 ID> [--via-host]
  sudo abroad main use <节点> [--via <中转节点>…]
  sudo abroad main confirm-lan <节点>
  sudo abroad main apply [--rule <规则>]    经当前主代理建立出口基准（启用前要有）
  exit ls | check <标签>            分流出口
  sudo abroad exit add <标签> --kind wireguard|openvpn|clash --file <配置文件>
  sudo abroad exit rm <标签>
  app ls | set <应用> direct|block|main|proxy:<出口> | rm <应用>     应用分流（改成直连等放松保护的改动需要 sudo）
  rules local ls | add <域名>… | rm <域名>…      本地域名名单：走本机网络、不经代理（添加是放松，需要 sudo）
  rules pinned ls | add <域名>… | rm <域名>…     追加的置顶域名：永远走主代理（删除是放松，需要 sudo；内置的删不掉；不进解析锁）
  rules offline ls | add <应用>… | rm <应用>…    离线放行名单：停用或闸门关着时仍可直连的应用（添加是放松，需要 sudo）
  rules udp443 show | on | off                   拦截 UDP 443（关掉是放松，需要 sudo）
  region show | set <时区> <语言> [--locale <地区格式>] | auto      地区（地区会变时需要 sudo 和确认）
  engine ls | install <名字> | rollback <名字>
  launchguard                      启动管控：现在是否允许启动 Claude、四层是否生效
  diagnostics [--include-domains]  诊断包（脱敏；--include-domains 才带 Claude 访问过的域名清单）
  domains | domains clear          Claude 访问过的、不在内置 Claude 域名之下的域名（只观察）| 清空

首次使用：sudo abroad main add-source … → sudo abroad main use <节点> → sudo abroad main apply → abroad up

订阅地址、密码、出口配置不能写在命令行参数里（会留在进程列表和 shell 历史里），请用 --file、--url-file 或 --stdin。
--yes 只跳过命令行的文字确认，不能代替 sudo，也不能代替两步确认（预览照样打印）。
socket 默认是 ` + apiclient.DefaultSocket + `，可用环境变量 ABROAD_SOCKET 或 --socket 指定。

退出码：0 成功；1 其他失败；2 参数错误；3 被拒（需要 sudo 或权限不够）；4 需要确认；5 忙；6 连不上后台服务；7 以后的版本提供。
`

// app 是一次命令行运行的环境（测试注入）。
type app struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	isAdmin        func() bool
	isTerminal     func() bool
	skipLocalCheck bool // 只给测试：绕过本地的 sudo 检查，验证第二层（后台服务）的拒绝
	client         *apiclient.Client
	json, yes      bool
	ctx            context.Context
	route          string // 当前子命令对应的路由 ID
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a := &app{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, isAdmin: platform.IsAdmin, isTerminal: stdinIsTerminal, ctx: ctx}
	os.Exit(a.run(os.Args[1:]))
}

func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// run 解析全局选项并分派子命令。
func (a *app) run(args []string) int {
	if a.ctx == nil {
		a.ctx = context.Background()
	}
	sock := os.Getenv("ABROAD_SOCKET")
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			a.json = true
		case "--yes", "-y":
			a.yes = true
		case "--socket":
			if i+1 >= len(args) {
				fmt.Fprintln(a.stderr, "--socket 后面要跟路径")
				return ExitUsage
			}
			sock = args[i+1]
			i++
		case "-h", "--help", "help":
			fmt.Fprint(a.stdout, usage)
			return ExitOK
		default:
			rest = append(rest, args[i])
		}
	}
	if sock == "" {
		sock = apiclient.DefaultSocket
	}
	if a.client == nil {
		a.client = &apiclient.Client{Socket: sock}
	}
	if len(rest) == 0 {
		fmt.Fprint(a.stderr, usage)
		return ExitUsage
	}
	c, cargs := findCommand(rest)
	if c == nil {
		fmt.Fprintf(a.stderr, "不认识的子命令：%s\n\n%s", strings.Join(rest, " "), usage)
		return ExitUsage
	}
	if c.later != "" {
		fmt.Fprintf(a.stdout, "%s：在以后的版本提供（%s）\n", strings.Join(c.path, " "), c.later)
		return ExitLater
	}
	a.route = c.route
	// 第一层：敏感级别的子命令先在本地检查（级别来自路由表，不另写一张表）。
	if c.route != "" && routeLevel(c.route) == api.Sensitive && !a.isAdmin() && !a.skipLocalCheck {
		fmt.Fprintf(a.stderr, "%s：这是敏感操作。%s\n", strings.Join(c.path, " "), platform.AdminHint)
		return ExitDenied
	}
	return c.run(a, cargs)
}

// routeLevel 取路由表里的级别（没有这个 ID 时按敏感处理，宁严勿宽）。
func routeLevel(id string) api.Level {
	for _, r := range daemon.RouteTable() {
		if r.ID == id {
			return r.Level
		}
	}
	return api.Sensitive
}

// call 发请求并把常见的应答映射成退出码（0 表示调用方继续处理应答）。
func (a *app) call(method, path string, body any) (apiclient.Response, int) {
	resp, err := a.client.Do(a.ctx, method, path, body)
	if err != nil {
		return resp, a.connErr(err)
	}
	return resp, a.codeOf(resp)
}

func (a *app) connErr(err error) int {
	switch {
	case errors.Is(err, apiclient.ErrNotRunning):
		fmt.Fprintf(a.stderr, "%s。请确认后台服务在运行：sudo launchctl print system/com.claude-abroad.abroadd\n", err)
	case errors.Is(err, apiclient.ErrNoListener):
		fmt.Fprintf(a.stderr, "%s。稍等片刻再试；仍不行请查看后台服务的日志。\n", err)
	case errors.Is(err, apiclient.ErrRejected):
		fmt.Fprintf(a.stderr, "%s。请用当前登录的账号运行；敏感操作请用 sudo。\n", err)
	case errors.Is(err, context.Canceled):
		return ExitFail
	default:
		fmt.Fprintln(a.stderr, "连不上后台服务。请确认它在运行。")
	}
	return ExitNoDaemon
}

// codeOf 按错误应答里稳定的代码（不看文字，m5-plan v2.7 决定 0）映射成退出码；文字只用来打印。
// 后台没给代码时（旧版本）按状态码取默认代码。
func (a *app) codeOf(resp apiclient.Response) int {
	if resp.Code >= 200 && resp.Code < 300 {
		return ExitOK
	}
	code := apiclient.ErrorCodeOf(resp.Body)
	if code == "" {
		code = api.DefaultCode(resp.Code)
	}
	msg := apiclient.ErrorText(resp.Body)
	switch code {
	case api.CodeNeedsConfirm:
		return ExitConfirm // 调用方走确认流程
	case api.CodeNeedsAuthorization:
		if routeLevel(a.route) == api.PerCall {
			fmt.Fprintf(a.stderr, "这一次需要 sudo：%s\n", msg)
		} else {
			fmt.Fprintf(a.stderr, "被拒：%s\n", msg)
		}
		return ExitDenied
	case api.CodeConfirmInvalid:
		fmt.Fprintf(a.stderr, "确认没有通过：%s\n", msg)
		return ExitDenied
	case api.CodeBusy:
		fmt.Fprintf(a.stderr, "忙：%s\n", msg)
		return ExitBusy
	case api.CodeInUse:
		fmt.Fprintf(a.stderr, "正在使用：%s\n", msg)
		return ExitBusy
	case api.CodeUnsupported:
		fmt.Fprintf(a.stderr, "后台服务现在的模式不提供这项功能：%s\n", msg)
		return ExitFail
	case api.CodeRulesChanged:
		fmt.Fprintf(a.stderr, "冲突：%s\n", msg)
		return ExitBusy
	case api.CodeUninstalled:
		fmt.Fprintln(a.stderr, "Claude 出国已经一键卸载：要继续使用请重新安装。")
		return ExitDenied
	}
	fmt.Fprintln(a.stderr, msg)
	return ExitFail
}

// confirmed 是两步确认：第一次调用回 409 带预览时打印预览；带 --yes 自动提交确认码，否则在终端里问；不是终端又没有
// --yes 时不执行（需要确认）。body 是请求体（map），确认码放进 confirm 字段。
func (a *app) confirmed(method, path string, body map[string]any) (apiclient.Response, int) {
	return a.confirmedWord(method, path, body, "yes")
}

// confirmedWord 同 confirmed，但终端里要输入指定的确认词（卸载要输入 uninstall，设计 10.3）。
func (a *app) confirmedWord(method, path string, body map[string]any, word string) (apiclient.Response, int) {
	var first any
	if len(body) > 0 {
		first = body
	}
	resp, code := a.call(method, path, first)
	if code != ExitConfirm {
		return resp, code
	}
	var cn struct {
		Error   string `json:"error"`
		Confirm string `json:"confirm"`
		Preview struct {
			Processes []struct {
				PID      int
				App, Cmd string
			} `json:"processes"`
			Unverified []struct {
				PID       int
				App, Note string
			} `json:"unverified"`
			Browsers []struct {
				PID       int
				App, Note string
			} `json:"browsers"`
			BrowserNote string `json:"browser_note"`
		} `json:"preview"`
		Region *struct{ From, To string } `json:"region"`
	}
	_ = json.Unmarshal(resp.Body, &cn)
	pw := a.stdout // --json 时标准输出只放最终的应答，预览写到标准错误
	if a.json {
		pw = a.stderr
	}
	fmt.Fprintln(pw, cn.Error)
	if cn.Region != nil {
		fmt.Fprintf(pw, "地区：%s → %s\n", cn.Region.From, cn.Region.To)
	}
	fmt.Fprintf(pw, "会结束的 Claude 进程：%d 个\n", len(cn.Preview.Processes))
	for _, p := range cn.Preview.Processes {
		fmt.Fprintf(pw, "  %d  %s  %s\n", p.PID, p.App, p.Cmd)
	}
	for _, p := range cn.Preview.Unverified {
		fmt.Fprintf(pw, "  （没能确认、不会结束）%d  %s  %s\n", p.PID, p.App, p.Note)
	}
	if len(cn.Preview.Browsers) > 0 {
		fmt.Fprintf(pw, "正在运行的浏览器：%s\n", cn.Preview.BrowserNote)
		kill := body != nil && body["kill_browsers"] == true
		for _, p := range cn.Preview.Browsers {
			switch {
			case p.Note != "":
				fmt.Fprintf(pw, "  %d  %s（%s）\n", p.PID, p.App, p.Note)
			case kill:
				fmt.Fprintf(pw, "  %d  %s（会被结束）\n", p.PID, p.App)
			default:
				fmt.Fprintf(pw, "  %d  %s（不会结束；要结束请加 --kill-browsers）\n", p.PID, p.App)
			}
		}
	}
	if !a.yes {
		if !a.isTerminal() {
			fmt.Fprintln(a.stderr, "需要确认：请在终端里运行并确认，或加 --yes")
			if a.json {
				a.stdout.Write(resp.Body) // 两步确认的应答（预览）原样给脚本
				fmt.Fprintln(a.stdout)
			}
			return resp, ExitConfirm
		}
		fmt.Fprintf(pw, "确认执行？输入 %s 继续：", word)
		line, _ := bufio.NewReader(a.stdin).ReadString('\n')
		s := strings.ToLower(strings.TrimSpace(line))
		ok := s == word
		if word == "yes" {
			ok = ok || s == "y" || s == "是"
		}
		if !ok {
			fmt.Fprintln(a.stderr, "没有确认，什么都没有改动")
			return resp, ExitConfirm
		}
	}
	if body == nil {
		body = map[string]any{}
	}
	body["confirm"] = cn.Confirm
	return a.call(method, path, body)
}

// out 输出：--json 时原样打印应答，否则调用 text。
func (a *app) out(resp apiclient.Response, text func()) int {
	if a.json {
		if len(resp.Body) == 0 {
			fmt.Fprintln(a.stdout, `{"ok":true}`) // 没有应答体的成功（如删除）：给脚本一个合法的 JSON
			return ExitOK
		}
		a.stdout.Write(resp.Body)
		if len(resp.Body) > 0 && resp.Body[len(resp.Body)-1] != '\n' {
			fmt.Fprintln(a.stdout)
		}
		return ExitOK
	}
	text()
	return ExitOK
}
