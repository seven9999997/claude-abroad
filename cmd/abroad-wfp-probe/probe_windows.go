//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"claude-abroad/internal/wfp"
	"claude-abroad/internal/wfprules"
)

// 探查用的公共目标：固定的公共 DNS 服务的地址（不是 Claude 相关的域名）。
var (
	public4   = netip.MustParseAddrPort("1.1.1.1:443")
	public4HT = netip.MustParseAddrPort("1.1.1.1:80")
	publicDNS = netip.MustParseAddrPort("1.1.1.1:53")
	public6   = netip.MustParseAddrPort("[2606:4700:4700::1111]:443")
)

func run(o options, stdout, stderr io.Writer) int {
	if o.Child != "" {
		return runChild(o.Child, stdout)
	}
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	if o.Out == "" {
		o.Out = filepath.Join(dir, "probe-result.txt")
	}
	selfSID, err := currentSID()
	if err != nil {
		fmt.Fprintf(stderr, "读当前账户失败：%v\n", err)
		return 4
	}
	lan, gw := localNet()
	p := Plan{Duration: o.Duration, AllowSID: o.AllowSID, SelfSID: selfSID, LAN: lan, Gateway: gw,
		Public4: public4, PublicDNS: publicDNS, Public6: public6, Keep: o.Keep, KeepPort: 22, Rehearse: o.Rehearse}
	if err := p.Validate(); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	filters, err := p.Filters() // 装哪一组过滤器只在这里决定
	if err != nil {
		fmt.Fprintf(stderr, "生成规则失败：%v\n", err)
		return 2
	}
	if ok, why := KeepPrediction(filters, p, selfSID); !ok {
		fmt.Fprintf(stderr, "离线求值器预测远程连接会被拦（%s），拒绝开始\n", why)
		return 2
	}
	var specs []wfp.FilterSpec
	for _, f := range filters {
		s, err := wfp.Encode(f)
		if err != nil {
			fmt.Fprintf(stderr, "换算规则失败：%v\n", err)
			return 2
		}
		specs = append(specs, s)
	}

	watcher, err := wfp.Open(false)
	if err != nil {
		fmt.Fprintf(stderr, "打开 Windows 防火墙（WFP）失败：%v\n请在“以管理员身份运行”的 PowerShell 里执行。\n", err)
		return 4
	}
	defer watcher.Close()
	before, err := watcher.Enumerate()
	if err != nil {
		fmt.Fprintf(stderr, "列出 WFP 对象失败：%v\n", err)
		return 4
	}
	rep := Report{Mode: p.ModeName(), Started: time.Now(), Duration: p.Duration, Rules: string(wfprules.Summary(filters)), CleanBefore: wfp.CleanPlan(before),
		OthersBefore: wfp.FormatOthers(wfp.SummarizeOthers(before.Others, before.Providers, before.Sublayers))}
	steps := p.Steps()
	preds := make([]Result, len(steps))
	for i, st := range steps {
		out, why := Predict(filters, st, p.Rehearse)
		preds[i] = Result{Step: st, Predicted: out, Why: why, Actual: NotRun}
	}
	if !wfp.NothingOurs(before) {
		fmt.Fprintf(stderr, "已经有 Claude 出国的 WFP 对象（上次没清理干净？），拒绝开始。先执行 abroad-wfp-clean --delete：\n  %s\n", strings.Join(rep.CleanBefore, "\n  "))
		return 5
	}
	if !o.Yes {
		if p.Rehearse {
			fmt.Fprintf(stdout, "演练计划（没有装任何过滤器；确认后加 --yes 再执行，只装一条拦回环端口 %d 的过滤器，网络不受影响，最长 %v）：\n", RehearsalPort, p.Duration)
		} else {
			fmt.Fprintf(stdout, "计划（没有装任何过滤器；确认后加 --yes 再执行，期间最长 %v 不能上外网）：\n", p.Duration)
		}
		for _, r := range preds {
			fmt.Fprintf(stdout, "- [%s] %s：预测 %s（%s）\n", r.Step.ID, r.Step.Title, r.Predicted, r.Why)
		}
		if o.AllowSID != "" {
			fmt.Fprintf(stdout, "\n探查期间请用另一个账户执行：\n  runas /user:<另一个账户> \"%s --child %s\"\n", exe, filepath.Join(dir, "child-result.txt"))
		}
		return 0
	}

	// 硬时限：无论哪一步卡住，到点进程退出，动态过滤器随之被系统删除。
	go func() {
		time.Sleep(p.Duration + 5*time.Second)
		fmt.Fprintln(stderr, "到达时限，强制退出（动态过滤器随进程退出被系统删除）")
		os.Exit(6)
	}()
	deadline := time.Now().Add(p.Duration)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	// 装过滤器之前先建立一条公网连接（established 那一项）。
	pre, preErr := (&net.Dialer{Timeout: 5 * time.Second}).Dial("tcp4", public4HT.String())
	// WSL 的基线：装过滤器之前同样探测一次（WSL 本来就不通时，过滤器期间的结果没有意义）。
	wslBase := observeWSL(ctx, public4)
	childFile := filepath.Join(dir, "child-result.txt")
	os.Remove(childFile)

	dyn, err := wfp.Open(true)
	if err != nil {
		fmt.Fprintf(stderr, "打开动态会话失败：%v\n", err)
		return 4
	}
	if err := dyn.Install(specs, false); err != nil {
		dyn.Close()
		fmt.Fprintf(stderr, "装过滤器失败（已整体回滚）：%v\n", err)
		return 4
	}
	fmt.Fprintf(stdout, "过滤器已装上（%s，%d 条，最长 %v）。\n", p.ModeName(), len(specs), p.Duration)
	if o.AllowSID != "" {
		fmt.Fprintf(stdout, "现在请用另一个账户执行：runas /user:<另一个账户> \"%s --child %s\"\n", exe, childFile)
	}

	for i := range preds {
		r := &preds[i]
		switch r.Step.Kind {
		case KindTCP:
			r.Actual, r.Detail = tryTCP(r.Step)
		case KindUDP:
			r.Actual, r.Detail = tryDNS(r.Step.Target)
		case KindEstablish:
			if preErr != nil {
				r.Actual, r.Detail = Unknown, "装过滤器之前就连不上："+errText(preErr)
			} else {
				r.Actual, r.Detail = tryEstablished(pre)
			}
		case KindChild:
			if o.AllowSID == "" {
				r.Actual, r.Detail = NotRun, "没有给 --allow-sid"
			} else {
				r.Actual, r.Detail = waitChild(ctx, childFile, deadline.Add(-10*time.Second))
			}
		case KindWSL:
			obs := observeWSL(ctx, r.Step.Target)
			obs.Baseline = &wslBase
			r.Actual, r.Detail = ClassifyWSL(obs)
		case KindEnum:
			r.Actual, r.Detail = checkEnum(watcher, filters)
		}
	}
	if pre != nil {
		pre.Close()
	}
	if err := dyn.Close(); err != nil {
		rep.Notes = append(rep.Notes, "关闭动态会话报错："+err.Error())
	}
	rep.Results = preds
	rep.Finished = time.Now()
	after, err := watcher.Enumerate()
	if err != nil {
		rep.Notes = append(rep.Notes, "结束后枚举失败："+err.Error())
	} else {
		rep.LeftAfter = wfp.CleanPlan(after)
		rep.OthersAfter = wfp.FormatOthers(wfp.SummarizeOthers(after.Others, after.Providers, after.Sublayers))
	}
	if o.Keep.IsValid() {
		rep.Notes = append(rep.Notes, "远程执行：开发机的 SSH 连接在探查期间是否保持，由开发机一侧记录")
	}
	text := Redact(rep.Format(), secrets())
	fmt.Fprint(stdout, text)
	if err := os.WriteFile(o.Out, []byte(text), 0o644); err != nil {
		fmt.Fprintf(stderr, "写结果文件失败：%v\n", err)
		return 4
	}
	fmt.Fprintf(stdout, "\n结果已写到 %s\n", o.Out)
	if len(rep.LeftAfter) > 0 {
		return 1
	}
	return 0
}

func secrets() []string {
	var out []string
	for _, k := range []string{"COMPUTERNAME", "USERNAME", "USERDOMAIN"} {
		if v := os.Getenv(k); v != "" {
			out = append(out, v)
		}
	}
	if h, err := os.Hostname(); err == nil {
		out = append(out, h)
	}
	return out
}

// classify 把连接错误分成：本机拦截（WSAEACCES）、包已出本机（对端拒绝、重置）、无法判断。
func classify(err error) (Outcome, string) {
	if err == nil {
		return Allowed, "连上了"
	}
	switch {
	case errors.Is(err, windows.WSAEACCES):
		return Blocked, "WSAEACCES（本机拒绝）"
	case errors.Is(err, windows.WSAECONNREFUSED):
		return Allowed, "对端拒绝（包已出本机）"
	case errors.Is(err, windows.WSAECONNRESET):
		return Allowed, "对端重置（包已出本机）"
	}
	return Unknown, errText(err)
}

func errText(err error) string {
	var errno windows.Errno
	if errors.As(err, &errno) {
		return fmt.Sprintf("错误码 %d", uint32(errno))
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "超时"
	}
	return "其他错误"
}

func tryTCP(st Step) (Outcome, string) {
	if st.Loopback && !st.Target.IsValid() {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return Unknown, "回环上监听失败"
		}
		defer ln.Close()
		go func() {
			if c, err := ln.Accept(); err == nil {
				c.Close()
			}
		}()
		c, err := net.DialTimeout("tcp4", ln.Addr().String(), 3*time.Second)
		if c != nil {
			c.Close()
		}
		return classify(err)
	}
	if !st.Target.Addr().IsValid() {
		return Unknown, "没有目标（例如找不到默认网关）"
	}
	network := "tcp4"
	if st.Target.Addr().Is6() {
		network = "tcp6"
	}
	c, err := net.DialTimeout(network, st.Target.String(), 4*time.Second)
	if c != nil {
		c.Close()
	}
	return classify(err)
}

func tryDNS(target netip.AddrPort) (Outcome, string) {
	c, err := net.Dial("udp4", target.String())
	if err != nil {
		return classify(err)
	}
	defer c.Close()
	// 查询 example.com 的 A 记录（ID 0x4142，RD）。
	q := []byte{0x41, 0x42, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0, 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 3, 'c', 'o', 'm', 0, 0, 1, 0, 1}
	if _, err := c.Write(q); err != nil {
		return classify(err)
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil {
		o, d := classify(err)
		return o, "发出了，没有回应：" + d
	}
	if n >= 2 && buf[0] == 0x41 && buf[1] == 0x42 {
		return Allowed, "收到回应"
	}
	return Unknown, "收到无法识别的回应"
}

func tryEstablished(c net.Conn) (Outcome, string) {
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("HEAD / HTTP/1.1\r\nHost: one.one.one.one\r\nConnection: close\r\n\r\n")); err != nil {
		o, d := classify(err)
		return o, "发送：" + d
	}
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if n > 0 {
		return Allowed, "装过滤器后仍收到回应（已建立的连接没有被重新评估）"
	}
	o, d := classify(err)
	return o, "接收：" + d
}

func waitChild(ctx context.Context, file string, until time.Time) (Outcome, string) {
	for time.Now().Before(until) {
		if b, err := os.ReadFile(file); err == nil && len(b) > 0 {
			line := strings.TrimSpace(string(b))
			switch {
			case strings.HasPrefix(line, string(Allowed)):
				return Allowed, "子命令：" + line
			case strings.HasPrefix(line, string(Blocked)):
				return Blocked, "子命令：" + line
			}
			return Unknown, "子命令：" + line
		}
		select {
		case <-ctx.Done():
			return NotRun, "时限内没有等到子命令的结果"
		case <-time.After(time.Second):
		}
	}
	return NotRun, "时限内没有等到子命令的结果"
}

// runChild 由另一个账户运行：连一次公网，把结果写进文件。
func runChild(file string, stdout io.Writer) int {
	o, d := tryTCP(Step{Target: public4})
	sid, _ := currentSID()
	line := fmt.Sprintf("%s（%s，账户 %s）\n", o, d, sid)
	fmt.Fprint(stdout, line)
	if err := os.WriteFile(file, []byte(line), 0o666); err != nil {
		fmt.Fprintf(stdout, "写结果文件失败：%v\n", err)
		return 4
	}
	return 0
}

// observeWSL 只调用系统自带的 wsl.exe：先列发行版，有的话在默认发行版里连一次目标。不安装、不修改任何东西。
func observeWSL(ctx context.Context, target netip.AddrPort) WSLObservation {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	exe := filepath.Join(root, "System32", "wsl.exe")
	if _, err := os.Stat(exe); err != nil {
		return WSLObservation{EntryMissing: true}
	}
	var o WSLObservation
	lc, lcancel := context.WithTimeout(ctx, 20*time.Second)
	defer lcancel()
	out, err := exec.CommandContext(lc, exe, "--list", "--quiet").Output()
	o.ListOut, o.ListFailed = out, err != nil
	if len(DecodeWSLList(out)) == 0 {
		return o
	}
	pc, pcancel := context.WithTimeout(ctx, 45*time.Second) // 第一次启动 WSL 的虚拟机要几秒到十几秒
	defer pcancel()
	pout, err := exec.CommandContext(pc, exe, "-e", "sh", "-c",
		WSLProbeScript(target.Addr().String(), fmt.Sprint(target.Port()))).CombinedOutput()
	o.ProbeOut = string(pout)
	if err != nil && !strings.Contains(o.ProbeOut, WSLMarkerOK) && !strings.Contains(o.ProbeOut, WSLMarkerFail) {
		if pc.Err() != nil {
			o.ProbeErr = "45 秒内没有结束"
		} else {
			o.ProbeErr = errText(err)
		}
	}
	return o
}

func checkEnum(e *wfp.Engine, want []wfprules.Filter) (Outcome, string) {
	snap, err := e.Enumerate()
	if err != nil {
		return Unknown, "枚举失败：" + err.Error()
	}
	var got []wfprules.Filter
	for _, s := range snap.Ours {
		f, err := wfp.Decode(s)
		if err != nil {
			return Blocked, "枚举到无法识别的过滤器：" + err.Error()
		}
		got = append(got, f)
	}
	if string(wfprules.Summary(got)) == string(wfprules.Summary(want)) {
		return Allowed, fmt.Sprintf("%d 条，与期望一致", len(got))
	}
	return Blocked, fmt.Sprintf("枚举到 %d 条，期望 %d 条，摘要不一致", len(got), len(want))
}

func currentSID() (string, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return tu.User.Sid.String(), nil
}

// localNet 读本机已连接网卡的 IPv4 直连网段和默认网关（GetAdaptersAddresses，含网关）。
func localNet() ([]netip.Prefix, netip.Addr) {
	const gaaIncludeGateways = 0x80
	var size uint32 = 16 << 10
	var buf []byte
	for i := 0; i < 3; i++ {
		buf = make([]byte, size)
		err := windows.GetAdaptersAddresses(windows.AF_INET, gaaIncludeGateways, 0, (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0])), &size)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) {
			return nil, netip.Addr{}
		}
	}
	var lan []netip.Prefix
	var gw netip.Addr
	for a := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0])); a != nil; a = a.Next {
		if a.OperStatus != windows.IfOperStatusUp || a.IfType == windows.IF_TYPE_SOFTWARE_LOOPBACK {
			continue
		}
		for u := a.FirstUnicastAddress; u != nil; u = u.Next {
			if ip, ok := sockaddrIP(u.Address); ok && ip.Is4() {
				lan = append(lan, netip.PrefixFrom(ip, int(u.OnLinkPrefixLength)).Masked())
			}
		}
		for g := a.FirstGatewayAddress; g != nil; g = g.Next {
			if ip, ok := sockaddrIP(g.Address); ok && ip.Is4() && !gw.IsValid() && ip.IsPrivate() {
				gw = ip
			}
		}
	}
	return lan, gw
}

func sockaddrIP(s windows.SocketAddress) (netip.Addr, bool) {
	if s.Sockaddr == nil {
		return netip.Addr{}, false
	}
	sa, err := s.Sockaddr.Sockaddr()
	if err != nil {
		return netip.Addr{}, false
	}
	if v4, ok := sa.(*syscall.SockaddrInet4); ok {
		return netip.AddrFrom4(v4.Addr), true
	}
	return netip.Addr{}, false
}
