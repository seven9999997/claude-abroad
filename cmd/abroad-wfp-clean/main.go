// abroad-wfp-clean 是 Windows 版断网保护的独立还原工具（design-windows 0.3、0.5 决定 3）：
// 删除 Claude 出国这个 WFP 提供者名下的全部过滤器、子层和提供者本身。
//
//   - 默认空跑：只列出将要删除的对象，不改任何东西；
//   - 带 --delete 才真删，删完再枚举一次，确认已经没有我们的对象；
//   - 可以重复执行；不读配置文件，不依赖 Claude 出国的任何服务；只按我们固定的提供者、子层键找对象，不碰别家的。
//
// 退出码：0 成功（空跑：列出了计划；真删：已经没有我们的对象）；1 真删后仍有残留；2 参数不对；
// 3 不是 Windows；4 打开或枚举 WFP 失败（多半是没有以管理员身份运行）。
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const usage = `用法：abroad-wfp-clean [--delete]

  不带参数   空跑：列出 Claude 出国留在 Windows 防火墙（WFP）里的过滤器、子层、提供者，不删除任何东西。
  --delete   真的删除它们，删完再检查一遍。

需要以管理员身份运行（在“以管理员身份运行”的 PowerShell 或命令提示符里执行）。
`

type options struct {
	Delete bool
	Help   bool
}

// parseArgs 只认 --delete 和 -h/--help；其他一律报错（避免把拼错的参数当成空跑或真删）。
func parseArgs(args []string) (options, error) {
	var o options
	for _, a := range args {
		switch a {
		case "--delete":
			o.Delete = true
		case "-h", "--help", "/?":
			o.Help = true
		default:
			return o, fmt.Errorf("不认识的参数 %q", a)
		}
	}
	return o, nil
}

// report 由计划、是否真删、删除结果、删后的检查算出要打印的文字和退出码。
func report(plan []string, del bool, delErr error, after []string) (string, int) {
	var b strings.Builder
	if len(plan) == 0 {
		b.WriteString("没有找到 Claude 出国的任何 WFP 对象，不需要还原。\n")
		return b.String(), 0
	}
	fmt.Fprintf(&b, "找到 %d 个 Claude 出国的 WFP 对象：\n", len(plan))
	for _, l := range plan {
		b.WriteString("  " + l + "\n")
	}
	if !del {
		b.WriteString("这是空跑，没有删除任何东西。要真的删除，加上 --delete 再执行一次。\n")
		return b.String(), 0
	}
	if delErr != nil {
		fmt.Fprintf(&b, "删除失败：%v\n", delErr)
	}
	if len(after) > 0 {
		fmt.Fprintf(&b, "删除后仍有 %d 个对象：\n", len(after))
		for _, l := range after {
			b.WriteString("  " + l + "\n")
		}
		b.WriteString("请再执行一次 abroad-wfp-clean --delete；仍然不行就重启电脑后再执行（详见 README 的应急步骤）。\n")
		return b.String(), 1
	}
	if delErr != nil {
		return b.String(), 1
	}
	b.WriteString("已全部删除，再次检查确认没有残留。\n")
	return b.String(), 0
}

// errNotWindows 是非 Windows 平台上的说明。
var errNotWindows = errors.New("abroad-wfp-clean 只在 Windows 上使用：它删除 Claude 出国留在 Windows 防火墙（WFP）里的规则")

func main() { os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr)) }

func realMain(args []string, stdout, stderr io.Writer) int {
	o, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}
	if o.Help {
		fmt.Fprint(stdout, usage)
		return 0
	}
	return run(o, stdout, stderr)
}
