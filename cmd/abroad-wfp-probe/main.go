package main

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"time"
)

const usage = `用法：abroad-wfp-probe [选项]

WR1 第一级探查：在 Windows 防火墙（WFP）里装一组“动态”过滤器（程序一退出系统自动删除），
检查默认拦截、回环、局域网、IPv6、已建立的连接、按账户放行是否符合预期。
装上过滤器期间本机不能上外网（局域网照常），最长持续 --seconds 秒，或直到结束本程序。

  --yes               确认开始（不带时只打印计划和预测，不装任何过滤器）
  --rehearse          演练：流程完全相同，但只装一条“拦回环上 TCP 端口 1”的过滤器，网络不受影响
  --hang              （只能和 --rehearse 一起用）装上过滤器后故意停住，验证硬时限：到点进程以退出码 6 退出
  --seconds N         时限，默认 120，范围 20–300
  --allow-sid SID     另一个本地账户的 SID（按 README 用这个账户执行一条命令，验证按账户放行）
  --keep-host IP      远程执行时开发机的地址（必须在局域网内；用来预测远程连接不会断）
  --out 文件          结果文件，默认是程序所在目录下的 probe-result.txt
  --child 文件        （由另一个账户运行）连一次公网，把结果写进这个文件，然后退出

需要以管理员身份运行（--child 除外）。出了问题：结束本程序即可恢复；仍不行就执行 abroad-wfp-clean --delete。
`

type options struct {
	Yes      bool
	Rehearse bool
	Hang     bool
	Duration time.Duration
	AllowSID string
	Keep     netip.Addr
	Out      string
	Child    string
	Help     bool
}

func parseArgs(args []string) (options, error) {
	o := options{Duration: DefaultDuration}
	need := func(i int) (string, error) {
		if i+1 >= len(args) {
			return "", fmt.Errorf("%s 后面要跟一个值", args[i])
		}
		return args[i+1], nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--yes":
			o.Yes = true
		case "--rehearse":
			o.Rehearse = true
		case "--hang":
			o.Hang = true
		case "-h", "--help", "/?":
			o.Help = true
		case "--seconds", "--allow-sid", "--keep-host", "--out", "--child":
			v, err := need(i)
			if err != nil {
				return o, err
			}
			i++
			switch a {
			case "--seconds":
				if o.Duration, err = ParseDuration(v); err != nil {
					return o, err
				}
				if o.Duration < MinDuration || o.Duration > MaxDuration {
					return o, fmt.Errorf("--seconds 要在 %d 到 %d 之间", int(MinDuration.Seconds()), int(MaxDuration.Seconds()))
				}
			case "--allow-sid":
				o.AllowSID = v
			case "--keep-host":
				ip, err := netip.ParseAddr(v)
				if err != nil || !ip.Is4() {
					return o, errors.New("--keep-host 要是 IPv4 地址")
				}
				o.Keep = ip
			case "--out":
				o.Out = v
			case "--child":
				o.Child = v
			}
		default:
			return o, fmt.Errorf("不认识的参数 %q", a)
		}
	}
	if o.Hang && !o.Rehearse {
		return o, errors.New("--hang 只能和 --rehearse 一起用（正式模式下不允许故意停住）")
	}
	if o.Child != "" && (o.Yes || o.Rehearse || o.Hang || o.AllowSID != "" || o.Keep.IsValid()) {
		return o, errors.New("--child 不能和别的选项一起用")
	}
	return o, nil
}

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
