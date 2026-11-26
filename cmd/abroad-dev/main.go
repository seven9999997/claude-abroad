// abroad-dev 是开发用工具（不进发布包），只依赖 data.sock / ctl.sock，以及 internal/wire、devsocks、
// devupstream、managedproxy（只用来从节点文件生成 VMRuntime，不读本机任何代理软件的配置）。
//
//	abroad-dev upstream [--listen 127.0.0.1:0] [--log 文件]
//	abroad-dev apply-main-proxy --nodes <节点 YAML> --selected <节点名> [--chain a,b] [--loopback-udp] (--print-allow | --print-first-hop | --ctl-sock <路径>)
//
// upstream 是开发用上游（本机回环上的固定应答 SOCKS5，不向外拨号，见 internal/devupstream）。
// apply-main-proxy 用临时 Store 导入节点文件、生成虚拟机主代理配置：--print-allow 只把出站白名单
// 每行一项打到标准输出（给 abroad-vm 的 --allow-outbound）；--ctl-sock 经控制面 PUT /v1/main-proxy。
// --print-first-hop 把路线第一跳每行一项打到标准输出（给 abroad-vm 的 --first-hop；回环和局域网第一跳走白名单，
// 这时没有输出）。
// --loopback-udp：第一跳是本机回环代理时也转 UDP（默认不转，见 managedproxy.VMOptions）。
// 配置正文含凭据，不打到任何输出。
//
//	abroad-dev socks --data-sock <路径> [--listen 127.0.0.1:1080] [--exit main]
//
// socks 在本机回环上开一个 SOCKS5 入口（CONNECT、UDP ASSOCIATE），经 data.sock 交给虚拟机的数据面；
// 域名目标按域名原样交过去，不在本机解析。启动后打一行 "listening <地址>"，SIGINT/SIGTERM 退出。
//
//	abroad-dev probe --data-sock <路径> --exit main --net tcp|udp --target host:port
//	                 [--send-hex 十六进制] [--read-timeout 3s] [--app abroad-dev-probe]
//
// probe 经 data.sock 开一条数据流：写帧头、读回应帧；成功时发 --send-hex，
// 然后读到 EOF 或超时（UDP 读一个数据报）。向标准输出打一行 JSON：
//
//	{"ok":bool,"code":"…","handshake_ms":N,"elapsed_ms":N,"recv_hex":"…","eof":bool,"timeout":bool}
//
// 退出码：0 拿到了回应帧（不管 ok 真假）；1 连不上 data.sock 或会话、帧层出错；2 参数错。
//
//	abroad-dev tunsim --data-sock <路径> [--name tunsim.test] [--udp-port 4500] [--udp-target IPv4:端口] [--doh-name 域名] [--timeout 30s]
//
// tunsim 在用户态模拟“本机应用 → utun → 网络栈 → data.sock → 虚拟机”（见 internal/tunsim）：不需要 root，
// 不建真实 utun，不碰宿主网络，只连 data.sock。按顺序跑 DNS、TCP（假地址）、UDP、TCP（真实 IP）、
// 无映射被拒、关闸门、关闭七步，向标准输出打一行 JSON 报告。
// --doh-name：多跑一步 h_doh，经主代理的 DoH（1.1.1.1:443，系统根证书）查这个域名的 TXT（真实节点上用；
// 开发上游答不了 DoH，vm-up 的阶段 E 不带它）。
// --udp-target：UDP 那一步改发往这个真实 IP（主代理答不了 DoH 时用，见 internal/tunsim.Config.UDPTarget）。
// 退出码：0 全部通过；1 有一步失败或连不上 data.sock；2 参数错。
//
//	abroad-dev resolver-lock domains|content|test-domain [--form default|loopback] [--port 15353]
//
// resolver-lock 打印解析锁的域名清单或锁文件内容（internal/resolverlock，M6-3）：给 scripts/m6/resolver 的真机脚本用，
// 脚本和 abroadd 写的内容同一来源。只打印，不写任何文件。
//
//	abroad-dev pf-render [--user nobody] [--utun utun9] [--lan 192.168.1.0/24]… [--vmif bridge100]… [--lockdown]
//
// pf-render 把 internal/pf.Render 生成的规则打到标准输出（给 scripts/dev/pf-syntax.sh 做 pfctl -n 语法检查；
// 本机没有 _abroad 用户，所以默认用 nobody）。--lan 经 pf.LANPrefixes 处理（丢掉公网网段，加上组播、链路本地）。
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"claude-abroad/internal/daemonsim"
	"claude-abroad/internal/devsocks"
	"claude-abroad/internal/devupstream"
	"claude-abroad/internal/directdial"
	"claude-abroad/internal/fakedns"
	"claude-abroad/internal/managedproxy"
	"claude-abroad/internal/pf"
	"claude-abroad/internal/procinfo"
	"claude-abroad/internal/r3"
	"claude-abroad/internal/resolverlock"
	"claude-abroad/internal/tunsim"
	"claude-abroad/internal/wire"
)

func main() {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "probe":
			os.Exit(probe(os.Args[2:], os.Stdout, os.Stderr))
		case "exit-flows":
			os.Exit(exitFlows(os.Args[2:], os.Stdout, os.Stderr))
		case "socks", "upstream":
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if os.Args[1] == "socks" {
				os.Exit(socks(ctx, os.Args[2:], os.Stdout, os.Stderr))
			}
			os.Exit(upstream(ctx, os.Args[2:], os.Stdout, os.Stderr))
		case "apply-main-proxy":
			os.Exit(applyMainProxy(context.Background(), os.Args[2:], os.Stdout, os.Stderr))
		case "pf-render":
			os.Exit(pfRender(os.Args[2:], os.Stdout, os.Stderr))
		case "resolver-lock":
			os.Exit(resolverLockCmd(os.Args[2:], os.Stdout, os.Stderr))
		case "tunsim":
			os.Exit(tunsimCmd(context.Background(), os.Args[2:], os.Stdout, os.Stderr))
		case "daemon-sim":
			os.Exit(daemonSimCmd(context.Background(), os.Args[2:], os.Stdout, os.Stderr))
		case "r3":
			os.Exit(r3Cmd(context.Background(), os.Args[2:], os.Stdout, os.Stderr))
		case "direct-probe":
			os.Exit(directProbe(context.Background(), os.Args[2:], os.Stdout, os.Stderr))
		case "ident-sample":
			os.Exit(identSample(os.Args[2:], os.Stdout, os.Stderr))
		case "claude-facts":
			os.Exit(claudeFacts(os.Args[2:], os.Stdout, os.Stderr))
		case "claude-settings":
			os.Exit(claudeSettingsCmd(os.Args[2:], os.Stdout, os.Stderr))
		case "integrity-check":
			os.Exit(integrityCheck(os.Args[2:], os.Stdout, os.Stderr))
		case "integrity-manifest":
			os.Exit(integrityManifest(os.Args[2:], os.Stdout, os.Stderr))
		}
	}
	fmt.Fprintln(os.Stderr, "用法：abroad-dev probe|exit-flows|socks|upstream|apply-main-proxy|tunsim|daemon-sim|pf-render|r3|direct-probe|ident-sample|claude-facts|integrity-manifest|integrity-check|claude-settings …（见源码头注释）")
	os.Exit(2)
}

// socks 运行本地 SOCKS5 入口，直到 ctx 结束。退出码：0 正常结束；1 运行出错；2 参数错。
func socks(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("socks", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sock := fs.String("data-sock", "", "abroad-vm 的 data.sock")
	listen := fs.String("listen", "127.0.0.1:1080", "本地监听地址（只能是回环）")
	exit := fs.String("exit", "main", "出口标签")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ap, err := netip.ParseAddrPort(*listen)
	if *sock == "" || err != nil || !ap.Addr().IsLoopback() {
		fmt.Fprintln(stderr, "abroad-dev socks: 必须指定 --data-sock，--listen 只能是回环地址:端口")
		return 2
	}
	l, err := net.Listen("tcp", ap.String())
	if err != nil {
		fmt.Fprintln(stderr, "abroad-dev socks: 监听：", err)
		return 1
	}
	cfg := wire.DefaultSessionConfig()
	d := &wire.Dialer{Config: cfg, Dial: func(ctx context.Context) (net.Conn, error) {
		var nd net.Dialer
		return nd.DialContext(ctx, "unix", *sock)
	}}
	defer d.Close()
	fmt.Fprintln(stdout, "listening", l.Addr().String())
	srv := &devsocks.Server{Open: d.Open, Exit: *exit}
	if err := srv.Serve(ctx, l); err != nil {
		fmt.Fprintln(stderr, "abroad-dev socks:", err)
		return 1
	}
	return 0
}

// ProbeResult 是 probe 打到标准输出的一行。
type ProbeResult struct {
	OK          bool   `json:"ok"`
	Code        string `json:"code,omitempty"`
	HandshakeMs int64  `json:"handshake_ms"`
	ElapsedMs   int64  `json:"elapsed_ms"` // 从回应帧到读结束（EOF、超时或读满）
	RecvHex     string `json:"recv_hex"`
	EOF         bool   `json:"eof"`
	Timeout     bool   `json:"timeout"`
}

func probe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sock := fs.String("data-sock", "", "abroad-vm 的 data.sock")
	exit := fs.String("exit", "main", "出口标签")
	netw := fs.String("net", "tcp", "tcp 或 udp")
	target := fs.String("target", "", "目标 host:port（IP 字面量填帧头的 ip，否则填 host）")
	sendHex := fs.String("send-hex", "", "握手成功后发出的字节（十六进制）；UDP 时是一个数据报")
	readTimeout := fs.Duration("read-timeout", 3*time.Second, "读应答的上限")
	app := fs.String("app", "abroad-dev-probe", "帧头里的 app")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	h, payload, err := buildProbe(*sock, *exit, *netw, *target, *sendHex, *app)
	if err != nil {
		fmt.Fprintln(stderr, "abroad-dev probe:", err)
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", *sock)
	if err != nil {
		fmt.Fprintln(stderr, "abroad-dev probe: 连 data.sock：", err)
		return 1
	}
	defer c.Close()
	cfg := wire.DefaultSessionConfig()
	cfg.KeepAliveInterval = -1
	sess, err := wire.Client(c, cfg)
	if err != nil {
		fmt.Fprintln(stderr, "abroad-dev probe: 建会话：", err)
		return 1
	}
	defer sess.Close()
	st, err := sess.Open()
	if err != nil {
		fmt.Fprintln(stderr, "abroad-dev probe: 开流：", err)
		return 1
	}
	defer st.Close()

	start := time.Now()
	_ = st.SetDeadline(time.Now().Add(12 * time.Second))
	if err := wire.WriteHeader(st, h); err != nil {
		fmt.Fprintln(stderr, "abroad-dev probe: 写帧头：", err)
		return 1
	}
	rep, err := wire.ReadReply(st)
	if err != nil {
		fmt.Fprintln(stderr, "abroad-dev probe: 读回应帧：", err)
		return 1
	}
	res := ProbeResult{OK: rep.OK, Code: string(rep.Code), HandshakeMs: time.Since(start).Milliseconds()}
	if rep.OK {
		readStart := time.Now()
		_ = st.SetDeadline(time.Now().Add(*readTimeout))
		var got []byte
		if h.Net == wire.UDP {
			dc := wire.NewDatagramConn(st)
			if err := dc.WriteDatagram(payload); err != nil {
				fmt.Fprintln(stderr, "abroad-dev probe: 发数据报：", err)
				return 1
			}
			buf := make([]byte, wire.MaxDatagram)
			n, err := dc.ReadDatagram(buf)
			got = buf[:n]
			classify(&res, err)
		} else {
			if len(payload) > 0 {
				if _, err := st.Write(payload); err != nil {
					fmt.Fprintln(stderr, "abroad-dev probe: 发数据：", err)
					return 1
				}
			}
			buf := make([]byte, 64<<10)
			for {
				n, err := st.Read(buf)
				got = append(got, buf[:n]...)
				if err != nil {
					classify(&res, err)
					break
				}
				if len(got) >= 64<<10 {
					break
				}
			}
		}
		res.RecvHex = hex.EncodeToString(got)
		res.ElapsedMs = time.Since(readStart).Milliseconds()
	}
	b, _ := json.Marshal(res)
	fmt.Fprintln(stdout, string(b))
	return 0
}

func classify(res *ProbeResult, err error) {
	var ne net.Error
	switch {
	case err == nil:
	case errors.Is(err, io.EOF):
		res.EOF = true
	case errors.As(err, &ne) && ne.Timeout(), errors.Is(err, os.ErrDeadlineExceeded):
		res.Timeout = true
	default:
		res.EOF = true // 流被对端关掉（yamux 报 stream closed / reset）也算结束
	}
}

func buildProbe(sock, exit, netw, target, sendHex, app string) (wire.Header, []byte, error) {
	if sock == "" {
		return wire.Header{}, nil, errors.New("必须指定 --data-sock")
	}
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return wire.Header{}, nil, fmt.Errorf("--target 应为 host:port：%v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return wire.Header{}, nil, fmt.Errorf("--target 的端口不对：%v", err)
	}
	h := wire.Header{V: wire.Version, Exit: exit, Net: netw, Port: port, App: app}
	if a, err := netip.ParseAddr(host); err == nil {
		h.IP = a.String()
	} else {
		h.Host = host
	}
	if err := h.Validate(); err != nil {
		return wire.Header{}, nil, fmt.Errorf("帧头不合法：%v", err)
	}
	payload, err := hex.DecodeString(sendHex)
	if err != nil {
		return wire.Header{}, nil, fmt.Errorf("--send-hex 不是十六进制：%v", err)
	}
	return h, payload, nil
}

// upstream 运行开发用上游，直到 ctx 结束。
func upstream(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("upstream", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "127.0.0.1:0", "监听地址（只能是 127.0.0.1；TCP 和 UDP 同端口）")
	logPath := fs.String("log", "", "请求日志（JSON 行）；默认标准输出")
	name := fs.String("name", "", "非空时 /whoami 回这个名字（分辨请求经的是哪个出口）")
	pidFile := fs.String("pid-file", "", "开始监听后把自己的进程号写进这个文件，退出时删掉（真机脚本按它结束进程）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ap, err := netip.ParseAddrPort(*listen)
	if err != nil || ap.Addr() != netip.MustParseAddr("127.0.0.1") {
		fmt.Fprintln(stderr, "abroad-dev upstream: --listen 只能是 127.0.0.1:端口")
		return 2
	}
	var logw io.Writer = stdout
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			fmt.Fprintln(stderr, "abroad-dev upstream: 打开日志：", err)
			return 1
		}
		defer f.Close()
		logw = f
	}
	l, u, err := devupstream.Listen(int(ap.Port()))
	if err != nil {
		fmt.Fprintln(stderr, "abroad-dev upstream: 监听：", err)
		return 1
	}
	if *pidFile != "" {
		if err := os.WriteFile(*pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
			l.Close()
			u.Close()
			fmt.Fprintln(stderr, "abroad-dev upstream: 写 PID 文件：", err)
			return 1
		}
		defer os.Remove(*pidFile)
	}
	fmt.Fprintln(stdout, "listening", l.Addr().String())
	if err := (&devupstream.Server{Log: logw, Name: *name}).Serve(ctx, l, u); err != nil {
		fmt.Fprintln(stderr, "abroad-dev upstream:", err)
		return 1
	}
	return 0
}

// applyMainProxy 从节点文件生成虚拟机主代理配置，打印白名单或经 ctl.sock 下发。
func applyMainProxy(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("apply-main-proxy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	nodes := fs.String("nodes", "", "节点 YAML（Clash/Mihomo 的 proxies）")
	selected := fs.String("selected", "", "最终出口的节点名")
	chain := fs.String("chain", "", "显式代理链（节点名，逗号分隔，最后一个是最终出口）")
	printAllow := fs.Bool("print-allow", false, "只打印出站白名单（每行一项），不连任何 socket")
	printHop := fs.Bool("print-first-hop", false, "只打印路线第一跳（每行一项，给 abroad-vm 的 --first-hop），不连任何 socket")
	ctlSock := fs.String("ctl-sock", "", "abroad-vm 的 ctl.sock")
	loopbackUDP := fs.Bool("loopback-udp", false, "第一跳是本机回环代理时也转 UDP（开发用）")
	saveStore := fs.String("save-store", "", "把导入并选好节点的主代理存储保存到这个目录（给 abroadd 的 --main-proxy-store；M2-9c）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	modes := 0
	for _, on := range []bool{*printAllow, *printHop, *ctlSock != "", *saveStore != ""} {
		if on {
			modes++
		}
	}
	if *nodes == "" || *selected == "" || modes != 1 {
		fmt.Fprintln(stderr, "abroad-dev apply-main-proxy: 需要 --nodes、--selected，以及 --print-allow、--print-first-hop、--ctl-sock、--save-store 四选一")
		return 2
	}
	data, err := os.ReadFile(*nodes)
	if err != nil {
		fmt.Fprintln(stderr, "abroad-dev apply-main-proxy: 读节点文件：", err)
		return 1
	}
	dir, err := os.MkdirTemp("", "abroad-dev-store")
	if err != nil {
		fmt.Fprintln(stderr, "abroad-dev apply-main-proxy:", err)
		return 1
	}
	defer os.RemoveAll(dir)
	st, _ := managedproxy.Load(dir)
	if err := st.Import("dev", "", data, "dev", 0, ""); err != nil {
		fmt.Fprintln(stderr, "abroad-dev apply-main-proxy: 导入节点：", err)
		return 1
	}
	ids := map[string]string{}
	for _, src := range st.Sources {
		for _, n := range src.Nodes {
			ids[n.Name] = n.ID
		}
	}
	st.Selected = ids[*selected]
	if *chain != "" {
		for _, name := range strings.Split(*chain, ",") {
			st.Chain = append(st.Chain, ids[strings.TrimSpace(name)])
		}
	}
	vc, err := st.VMRuntimeConfigWith(managedproxy.VMOptions{LoopbackUDP: *loopbackUDP})
	if err != nil {
		fmt.Fprintln(stderr, "abroad-dev apply-main-proxy:", err)
		return 1
	}
	if *saveStore != "" {
		if err := st.Save(*saveStore); err != nil {
			fmt.Fprintln(stderr, "abroad-dev apply-main-proxy: 保存主代理存储：", err)
			return 1
		}
		fmt.Fprintln(stdout, "saved")
		return 0
	}
	if *printAllow {
		for _, a := range vc.Allow {
			fmt.Fprintln(stdout, a)
		}
		return 0
	}
	if *printHop {
		for _, h := range vc.FirstHop {
			fmt.Fprintln(stdout, h)
		}
		return 0
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", *ctlSock)
		}}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, "http://abroad-agent/v1/main-proxy", bytes.NewReader(vc.YAML))
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(stderr, "abroad-dev apply-main-proxy: 下发：", err)
		return 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "abroad-dev apply-main-proxy: 下发返回 %d：%s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		return 1
	}
	fmt.Fprintf(stdout, "applied udp=%v\n", vc.UDP)
	return 0
}

// tunsimCmd 跑一次用户态全链路模拟（M2-5c），打印一行 JSON 报告。
func tunsimCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tunsim", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sock := fs.String("data-sock", "", "abroad-vm 的 data.sock")
	name := fs.String("name", "tunsim.test", "查询和连接用的域名")
	udpPort := fs.Int("udp-port", 4500, "UDP 回显用的端口")
	timeout := fs.Duration("timeout", 30*time.Second, "每一步的上限")
	udpTarget := fs.String("udp-target", "", "UDP 那一步改发往这个真实 IPv4:端口（主代理答不了 DoH 时用）")
	dohName := fs.String("doh-name", "", "多跑一步：经主代理的 DoH 查这个域名的 TXT")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var target netip.AddrPort
	if *udpTarget != "" {
		t, err := parseUDPTarget(*udpTarget)
		if err != nil {
			fmt.Fprintln(stderr, "abroad-dev tunsim:", err)
			return 2
		}
		target = t
	}
	if *dohName != "" {
		if _, err := fakedns.Normalize(*dohName); err != nil {
			fmt.Fprintln(stderr, "abroad-dev tunsim: --doh-name 不是合法的域名")
			return 2
		}
	}
	if *sock == "" || fs.NArg() > 0 || *udpPort <= 0 || *udpPort > 65535 || *timeout <= 0 {
		fmt.Fprintln(stderr, "abroad-dev tunsim: 需要 --data-sock；--udp-port 在 1–65535；--timeout 为正")
		return 2
	}
	if st, err := os.Stat(*sock); err != nil || st.Mode()&os.ModeSocket == 0 {
		fmt.Fprintf(stderr, "abroad-dev tunsim: data.sock 不存在或不是 socket（%s）：请先用 scripts/dev/vm-up.sh 或 abroad-vm run 启动虚拟机\n", *sock)
		return 1
	}
	rep := tunsim.Run(ctx, tunsim.Config{DataSock: *sock, Name: *name, UDPPort: *udpPort, Timeout: *timeout, UDPTarget: target, DoHName: *dohName})
	b, _ := json.Marshal(rep)
	fmt.Fprintln(stdout, string(b))
	if !rep.OK {
		return 1
	}
	return 0
}

// parseUDPTarget 校验 --udp-target：IPv4、端口非 0，且不是 fake 段、0/8、回环、链路本地、组播、广播
// （这些目标网络栈会直接拒绝，跑了也没有意义）。
func parseUDPTarget(v string) (netip.AddrPort, error) {
	ap, err := netip.ParseAddrPort(v)
	if err != nil || !ap.Addr().Is4() || ap.Port() == 0 {
		return netip.AddrPort{}, fmt.Errorf("--udp-target 应为 IPv4:端口（端口非 0）")
	}
	for _, p := range []string{"198.18.0.0/15", "0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4", "255.255.255.255/32"} {
		if netip.MustParsePrefix(p).Contains(ap.Addr()) {
			return netip.AddrPort{}, fmt.Errorf("--udp-target 不能在 %s 里", p)
		}
	}
	return ap, nil
}

// pfRender 打印 pf 规则（只生成文本，不调用 pfctl）。
func pfRender(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pf-render", flag.ContinueOnError)
	fs.SetOutput(stderr)
	user := fs.String("user", "nobody", "放行去公网的用户（本机没有 _abroad，语法检查用 nobody）")
	utun := fs.String("utun", "", "我们的 utun（为空表示开机时还没有 utun）")
	lockdown := fs.Bool("lockdown", false, "最严的一组（M6-2b：开机时 guard 读不出参数时用；不放行任何用户、局域网、网桥）")
	var lans, vmifs listFlag
	fs.Var(&lans, "lan", "直连网段，可重复")
	fs.Var(&vmifs, "vmif", "虚拟机网桥接口，可重复")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return 2
	}
	var direct []netip.Prefix
	for _, l := range lans {
		p, err := netip.ParsePrefix(l)
		if err != nil {
			fmt.Fprintln(stderr, "abroad-dev pf-render: --lan 不是网段：", l)
			return 2
		}
		direct = append(direct, p)
	}
	c := pf.Config{UtunName: *utun, VMIfs: vmifs, User: *user}
	if *lockdown {
		c = pf.Config{Lockdown: true}
	}
	if len(direct) > 0 && !*lockdown {
		c.LAN = pf.LANPrefixes(direct)
	}
	out, err := pf.Render(c)
	if err != nil {
		fmt.Fprintln(stderr, "abroad-dev pf-render:", err)
		return 2
	}
	_, _ = stdout.Write(out)
	return 0
}

// listFlag 是可重复的字符串参数。
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// daemonSimCmd：abroad-dev daemon-sim --vm-dir <目录> --state-dir <目录> --exit-check-host <主机>
// [--udp-target IPv4:端口] [--log <文件>] [--timeout 15s]（M2-9b-2 决定 9）。
// 在进程内启动 daemon（系统层假的、数据面真的，utun 是管道），经本地接口走 启用 → 停用 → 再启用 → 恢复，
// 各阶段跑客户端探测；输出一行 JSON 报告（不含地址），全部通过时退出 0。不碰宿主机网络（只连 unix socket）。
func daemonSimCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("daemon-sim", flag.ContinueOnError)
	fs.SetOutput(stderr)
	vmDir := fs.String("vm-dir", "", "abroad-vm 的状态目录（ctl.sock、host.sock、data.sock）")
	stateDir := fs.String("state-dir", "", "daemon 的状态目录（应不存在；路径要短）")
	exitHost := fs.String("exit-check-host", "", "出口检查的主机")
	udpTarget := fs.String("udp-target", "", "UDP 探测发往这个真实 IPv4:端口")
	logPath := fs.String("log", "", "daemon 日志写到这个文件")
	timeout := fs.Duration("timeout", 15*time.Second, "单步时限")
	multi := fs.Bool("multi", false, "M3-7：多出口多应用的阶段（需要 abroad-vm 带 --direct-sock、开发出口 wg-a、clash-b）")
	directSock := fs.String("direct-sock", "", "--multi：abroad-vm 的直连拨号模块 socket")
	directTarget := fs.String("direct-target", "", "--multi：直连应用连的 IPv4:端口（默认 1.1.1.1:443；本机不能经物理网卡到公网时用局域网目标，端口不能是 443）")
	exitsFile := fs.String("exits", "", "M4-8：分流出口的阶段，参数是 agent /v1/dev/exit-peer?action=up 的应答文件（含测试密钥，0600）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *vmDir == "" || *stateDir == "" || *exitHost == "" || fs.NArg() > 0 || *timeout <= 0 || (*multi && *directSock == "") {
		fmt.Fprintln(stderr, "abroad-dev daemon-sim: 需要 --vm-dir、--state-dir、--exit-check-host")
		return 2
	}
	if err := daemonsim.EnsureFresh(*stateDir); err != nil {
		fmt.Fprintln(stderr, "abroad-dev daemon-sim:", err)
		return 2
	}
	var dtarget netip.AddrPort
	if *directTarget != "" {
		t, err := netip.ParseAddrPort(*directTarget)
		if err != nil || !t.Addr().Is4() || t.Port() == 0 {
			fmt.Fprintln(stderr, "abroad-dev daemon-sim: --direct-target 要是 IPv4:端口")
			return 2
		}
		dtarget = t
	}
	var target netip.AddrPort
	if *udpTarget != "" {
		t, err := parseUDPTarget(*udpTarget)
		if err != nil {
			fmt.Fprintln(stderr, "abroad-dev daemon-sim:", err)
			return 2
		}
		target = t
	}
	var lw io.Writer
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			fmt.Fprintln(stderr, "abroad-dev daemon-sim:", err)
			return 1
		}
		defer f.Close()
		lw = f
	}
	base := daemonsim.Config{VMDir: *vmDir, StateDir: *stateDir, ExitHost: *exitHost, UDPTarget: target, Log: lw, Timeout: *timeout}
	if *exitsFile != "" {
		erep := daemonsim.RunExits(ctx, daemonsim.ExitsConfig{Config: base, ExitsFile: *exitsFile})
		b, _ := json.Marshal(erep)
		fmt.Fprintln(stdout, string(b))
		if !erep.OK {
			return 1
		}
		return 0
	}
	if *multi {
		mrep := daemonsim.RunMulti(ctx, daemonsim.MultiConfig{Config: base, DirectSock: *directSock, DirectTarget: dtarget})
		b, _ := json.Marshal(mrep)
		fmt.Fprintln(stdout, string(b))
		if !mrep.OK {
			return 1
		}
		return 0
	}
	rep := daemonsim.Run(ctx, base)
	b, _ := json.Marshal(rep)
	fmt.Fprintln(stdout, string(b))
	if !rep.OK {
		return 1
	}
	return 0
}

// r3Cmd 测量查连接表识别进程的准确率（风险 R3，m3-plan M3-3 验收 g）。只连本机回环。
// curl 模式需要 root（普通用户看不到别的进程的套接字）；self 模式普通用户也能跑。
// 退出码：0 达标；1 未达标或出错；2 参数错。
func r3Cmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("r3", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mode := fs.String("mode", "curl", "curl（需要 root）或 self")
	n := fs.Int("n", 1000, "连接总数")
	batch := fs.Int("batch", 0, "每批并发数（0 表示一次全部并发）")
	minRate := fs.Float64("min-rate", 95, "识别率下限（百分比）")
	maxP99 := fs.Duration("max-p99", 0, "99 分位耗时上限（0 表示不看）")
	curl := fs.String("curl", "/usr/bin/curl", "curl 的路径")
	retries := fs.Int("retries", 3, "查不到时的重试次数")
	delay := fs.Duration("retry-delay", 20*time.Millisecond, "重试间隔")
	if err := fs.Parse(args); err != nil || *n <= 0 || *n > 5000 {
		return 2
	}
	if *mode == "curl" && os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "curl 模式需要 root：普通用户在连接表里只能看到本进程的套接字。请用 sudo 运行，或改用 --mode self。")
		return 2
	}
	id := &procinfo.Identifier{Table: procinfo.SysTable{}, Procs: procinfo.SysProcs{}, Bundle: (&procinfo.FileBundles{}).Read,
		Retries: *retries, RetryDelay: *delay}
	rep, err := r3.Run(ctx, r3.Options{Mode: *mode, N: *n, Batch: *batch, CurlPath: *curl, Ident: id, MinRate: *minRate, MaxP99: *maxP99})
	fmt.Fprint(stdout, rep.String())
	if err != nil {
		fmt.Fprintln(stderr, "测量失败：", err)
		return 1
	}
	if !rep.Pass() {
		return 1
	}
	return 0
}

// directProbe 经直连拨号模块连一个目标（m3-plan M3-4 验收 h）：报告是否连上、本端地址属于哪块网卡。
// 证明的是“走的是绑物理网卡的拨号”，不是目标可达。输出一行 JSON。退出码：0 连上；1 没连上；2 参数错。
func directProbe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("direct-probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sock := fs.String("sock", "", "直连拨号模块的 socket")
	host := fs.String("host", "", "目标域名（与 --ip 二选一）")
	ipS := fs.String("ip", "", "目标 IPv4")
	port := fs.Uint("port", 443, "目标端口")
	network := fs.String("net", "tcp", "tcp 或 udp")
	timeout := fs.Duration("timeout", 10*time.Second, "整体时限（批量模式下是每次拨号的时限）")
	count := fs.Int("count", 1, "拨号次数；大于 1 时是批量模式（只能 --ip、tcp），输出汇总")
	conc := fs.Int("concurrency", 1, "批量模式的并发数")
	serverUID := fs.Int("server-uid", -1, "直连拨号模块运行的用户（默认当前用户；真机脚本里是测试用户）")
	tlsName := fs.String("tls-name", "", "批量模式：TLS 握手的服务器名（默认目标 IP；证书照常校验）")
	own := fs.Bool("own-config", false, "带自己的配置（很大的代数、不带 DNS 和追加名单）：模块的配置由 abroadd 下发时用（m3-plan v2.10）")
	if err := fs.Parse(args); err != nil || *sock == "" || (*host == "") == (*ipS == "") || *port == 0 || *port > 65535 ||
		*count < 1 || *count > 10000 || *conc < 1 || *conc > MaxBatchConcurrency || (*count > 1 && (*ipS == "" || *network != "tcp")) {
		fmt.Fprintln(stderr, "用法：abroad-dev direct-probe --sock <路径> (--host <域名> | --ip <IPv4>) [--port 443] [--net tcp]"+
			" [--count N --concurrency M（批量，只能 --ip、tcp）] [--server-uid UID] [--own-config]")
		return 2
	}
	var ip netip.Addr
	if *ipS != "" {
		a, err := netip.ParseAddr(*ipS)
		if err != nil {
			return 2
		}
		ip = a
	}
	uid := uint32(os.Getuid())
	if *serverUID >= 0 {
		uid = uint32(*serverUID)
	}
	cli := &directdial.Client{Path: *sock, ServerUID: uid}
	if *own {
		cli.SetConfigSource(ownConfig)
	}
	if *count > 1 {
		name := *tlsName
		if name == "" {
			name = ip.String()
		}
		tgt := netip.AddrPortFrom(ip, uint16(*port))
		rep := runBatch(ctx, cli, tgt, tgt, *count, *conc, *timeout,
			&tls.Config{ServerName: name, MinVersion: tls.VersionTLS12}, ifaceOf)
		b, _ := json.Marshal(rep)
		fmt.Fprintln(stdout, string(b))
		if rep.OK != rep.Count {
			return 1
		}
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	start := time.Now()
	c, err := cli.Dial(ctx, *network, *host, ip, uint16(*port))
	out := map[string]any{"ms": time.Since(start).Milliseconds()}
	if err != nil {
		var ce *directdial.CodeError
		if errors.As(err, &ce) {
			out["code"] = ce.Code
		} else {
			out["error"] = err.Error()
		}
		out["ok"] = false
		b, _ := json.Marshal(out)
		fmt.Fprintln(stdout, string(b))
		return 1
	}
	defer c.Close()
	local := c.LocalAddr().String()
	out["ok"] = true
	out["interface"] = ifaceOf(c.LocalAddr())
	out["local_is_private"] = strings.HasPrefix(local, "10.") || strings.HasPrefix(local, "192.168.") || strings.HasPrefix(local, "172.")
	b, _ := json.Marshal(out)
	fmt.Fprintln(stdout, string(b))
	return 0
}

// ifaceOf 找出本端地址所在的网卡名（只给网卡名，不输出地址）。
func ifaceOf(a net.Addr) string {
	ap, err := netip.ParseAddrPort(a.String())
	if err != nil {
		return ""
	}
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		addrs, _ := ifc.Addrs()
		for _, x := range addrs {
			if p, err := netip.ParsePrefix(x.String()); err == nil && p.Addr().Unmap() == ap.Addr().Unmap() {
				return ifc.Name
			}
		}
	}
	return ""
}

func resolverLockCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "用法：abroad-dev resolver-lock domains|content|test-domain [--form default|loopback] [--port 15353]")
		return 2
	}
	what := args[0]
	fs := flag.NewFlagSet("resolver-lock", flag.ContinueOnError)
	fs.SetOutput(stderr)
	form := fs.String("form", "default", "写法：default（nameserver 198.18.0.2）或 loopback（127.0.0.1 加端口）")
	port := fs.Uint("port", 15353, "loopback 写法的端口（不用 5353：那是 mDNS 的端口）")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 0 {
		return 2
	}
	switch what {
	case "test-domain":
		fmt.Fprintln(stdout, resolverlock.TestDomain)
		return 0
	case "domains":
		for _, d := range resolverlock.Domains() {
			fmt.Fprintln(stdout, d)
		}
		return 0
	case "content":
		f := resolverlock.Default()
		switch *form {
		case "default":
		case "loopback":
			if *port == 0 || *port > 65535 {
				fmt.Fprintln(stderr, "端口不对")
				return 2
			}
			f = resolverlock.Loopback(uint16(*port))
		default:
			fmt.Fprintln(stderr, "写法只能是 default 或 loopback")
			return 2
		}
		b, err := resolverlock.Content(f)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		_, _ = stdout.Write(b)
		return 0
	}
	fmt.Fprintln(stderr, "只支持 domains、content、test-domain")
	return 2
}
