package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
	"github.com/miekg/dns"

	"claude-abroad/internal/directdial"
	"claude-abroad/internal/physif"
	"claude-abroad/internal/usernet"
	"claude-abroad/internal/vmhost"
)

// userNet 是虚拟机的用户态网络（设计 4.5）：每个 abroad-vm 进程一个 VirtualNetwork
// （它的 DNS、DHCP 服务没有关闭接口，每次开机重建会泄漏 goroutine），每次开机一对新的 socketpair。
//
// 底线 6：虚拟机宿主（vmhost）和用户态网络（usernet）只在这里经网卡 fd 组装，互不 import。
type userNet struct {
	vn     *virtualnetwork.VirtualNetwork
	dialer *usernet.Dialer
	ref    *usernet.PolicyRef
	mode   string              // outboundOpen 或 outboundFirstHop
	out    *usernet.Outbound   // 收紧模式的放行集合（开放模式为 nil）
	gwdns  *usernet.GatewayDNS // 收紧模式的网关 DNS（开放模式为 nil，网关 DNS 一律 REFUSED）
	// accept 把宿主一端的连接交给虚拟网络（可替换，测试用）。
	accept func(ctx context.Context, conn net.Conn) error
	logf   func(format string, a ...any)
	direct *directdial.Server // 直连拨号模块（诊断计数用；可为 nil）
	// exitLoopback：分流出口端点允许回环（只给开发构建的开发标记，同机对端）。
	exitLoopback bool
	// failLog 给第一跳拨号失败的事件限流（同一类别、同一个第一跳 failLogEvery 内只记一条）。
	failMu  sync.Mutex
	failLog map[string]time.Time
	now     func() time.Time // 测试注入；nil 用 time.Now
}

// failLogEvery 是同一类别、同一个第一跳的拨号失败事件的最短间隔（计数照常累加）。
const failLogEvery = 30 * time.Second

// InterfaceFresh 是物理网卡查询结果的新鲜期。
const InterfaceFresh = 2 * time.Second

// 出站模式（netstats 的 outbound_mode）。
const (
	outboundOpen     = "open"      // 开放模式：只有开发构建能打开，公网一律放行（M1 的行为）
	outboundFirstHop = "first_hop" // 默认：只放行白名单和路线第一跳（设计 4.5，M2-1a）
)

func newUserNet(o options, logf func(format string, a ...any)) (*userNet, error) {
	ref := usernet.NewPolicyRef(&usernet.Policy{Allow: o.allow})
	// 物理网卡：Watcher 是唯一的缓存（新鲜期 2 秒，过期先用旧值、后台刷新，30 秒以上同步查；M2-2）。
	// 绑定失败时作废，下次拨号重新查。
	w := physif.NewWatcher(physif.Query, InterfaceFresh)
	d := &usernet.Dialer{Rules: ref, Interface: w.Current, OnBindFailed: w.Invalidate}
	u := &userNet{dialer: d, ref: ref, mode: outboundOpen, logf: logf, exitLoopback: o.devAgent}
	d.OnDialFailed = u.dialFailed
	var dnsx func(context.Context, *dns.Msg) (*dns.Msg, error) // nil：一律 REFUSED
	if !o.outboundOpen {
		u.mode = outboundFirstHop
		u.out = usernet.NewOutbound(o.allow, o.firstHops, nil)
		d.BeforeDial = u.out.BeforeDial
		eps := usernet.DefaultDoHEndpoints
		doh := &usernet.DoH{Endpoints: eps, Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return d.DialResolver(ctx, network, addr, eps)
		}}
		u.gwdns = &usernet.GatewayDNS{Out: u.out, Resolve: doh.Lookup, OnFailed: u.resolveFailed}
		dnsx = u.gwdns.Exchange
	}
	cfg, err := usernet.NewConfiguration(d, ref, dnsx)
	if err != nil {
		return nil, err
	}
	vn, err := virtualnetwork.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("创建虚拟机网络：%w", err)
	}
	u.vn, u.accept = vn, vn.AcceptVfkit
	return u, nil
}

// NetStats 是 host.sock 的 GET /v1/netstats 应答：拨号器计数和当前绑定的物理网卡名（不含目标地址）。
type NetStats struct {
	usernet.Counters
	Interface  string `json:"interface"`
	AllowCount int    `json:"allow_count"` // 出站白名单条数（不给出地址）
	usernet.DNSCounters
	OutboundMode  string `json:"outbound_mode"`   // open 或 first_hop；M2-7 开闸门前核对它是 first_hop
	FirstHopCount int    `json:"first_hop_count"` // 第一跳条数（不给出地址和域名）
	// ExitEndpointCount 是分流出口端点的条数（M4-8；不给出地址和域名）。
	ExitEndpointCount int `json:"exit_endpoint_count"`
	// Direct 是直连拨号模块各种结果的计数（没有 --direct-sock 时为空；只有计数，不带目标）。
	Direct map[string]int64 `json:"direct,omitempty"`
}

func (u *userNet) stats() NetStats {
	st := NetStats{OutboundMode: u.mode}
	if u.dialer != nil {
		st.Counters, st.Interface = u.dialer.Counters(), u.dialer.CurrentInterface()
	}
	if u.out != nil {
		st.FirstHopCount = u.out.Hops()
		st.ExitEndpointCount = u.out.ExitEndpoints()
	}
	if u.gwdns != nil {
		st.DNSCounters = u.gwdns.Counters()
	}
	if u.ref != nil && u.ref.Load() != nil {
		st.AllowCount = len(u.ref.Load().Allow)
	}
	if u.direct != nil {
		st.Direct = u.direct.Stats()
	}
	return st
}

// close 关闭虚拟网络的协议栈（补丁 0007）。最多等 5 秒：虚拟机卡死、网络协程没收拾完时协议栈的协程可能退不出，
// 这时不再等（进程马上要退出），只记一笔。
func (u *userNet) close() {
	if u.vn == nil {
		return
	}
	done := make(chan struct{})
	go func() { u.vn.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		if u.logf != nil {
			u.logf("关闭虚拟网络超时（5 秒），不再等待")
		}
	}
}

func (u *userNet) logStats() {
	if u.logf != nil {
		st := u.stats()
		u.logf("网络计数（%s）：policy_denied=%d hop_denied=%d bind_failed=%d dial_ok=%d dial_ok_loopback=%d resolver_dial_ok=%d dns_answered=%d dns_refused=%d dns_failed=%d %s",
			st.OutboundMode, st.PolicyDenied, st.HopDenied, st.BindFailed, st.DialOK, st.DialOKLoopback, st.ResolverDialOK, st.Answered, st.Refused, st.Failed,
			dialFailText(st.DialFail))
	}
}

// dialFailText 是拨号失败各类别的计数（固定顺序，键是 dial_fail_类别）。
func dialFailText(m map[string]int64) string {
	parts := make([]string, 0, len(usernet.DialFailClasses))
	for _, c := range usernet.DialFailClasses {
		parts = append(parts, fmt.Sprintf("dial_fail_%s=%d", c, m[c]))
	}
	return strings.Join(parts, " ")
}

// hopLabel 是“第几个第一跳”的说法（不含地址）。
func (u *userNet) hopLabel(idx int) string {
	if idx > 0 {
		return fmt.Sprintf("第 %d 个第一跳", idx)
	}
	return "不是主代理的第一跳（分流出口端点、局域网节点或白名单）"
}

// dialFailed 是拨号器的 OnDialFailed：只记类别和第几个第一跳，限流。
func (u *userNet) dialFailed(network string, dst netip.AddrPort, class string) {
	idx := 0
	if u.out != nil {
		idx = u.out.HopIndex(dst)
	}
	u.logFail(idx, network, class)
}

// resolveFailed 是网关 DNS 的 OnFailed：第一跳域名解析失败算作这个第一跳的一次拨号失败（类别 dns）。
func (u *userNet) resolveFailed(name string) {
	idx := 0
	if u.out != nil {
		idx = u.out.HopIndexByName(name)
	}
	if idx == 0 {
		return // 分流出口端点的域名：只在 dns_failed 里计数
	}
	u.dialer.NoteResolveFailed()
	u.logFail(idx, "dns", usernet.DialFailDNS)
}

func (u *userNet) logFail(idx int, network, class string) {
	if u.logf == nil {
		return
	}
	now := time.Now
	if u.now != nil {
		now = u.now
	}
	key := fmt.Sprintf("%d/%s/%s", idx, network, class)
	t := now()
	u.failMu.Lock()
	if u.failLog == nil {
		u.failLog = map[string]time.Time{}
	}
	last, seen := u.failLog[key]
	if seen && t.Sub(last) < failLogEvery {
		u.failMu.Unlock()
		return
	}
	u.failLog[key] = t
	u.failMu.Unlock()
	u.logf("第一跳拨号失败：%s，%s，%s（同类 %s 内不再重复记）", u.hopLabel(idx), network, usernet.DialFailLabel(class), failLogEvery)
}

// hostHandler 是 host.sock 上的诊断接口（只接受 abroad-vm 自己的用户和 root，见 peerListener）：
//   - GET /v1/netstats：计数（不含地址）；
//   - PUT /v1/exit-endpoints：整体替换出站白名单里分流出口的那一组（M4-8，m4-plan v2.12 决定 1）：
//     {"endpoints":[{"host":"…","proto":"tcp|udp","port":N}]}；不合法 422 并指出第几项，不改任何东西；
//     主代理第一跳那一组不受影响。开放模式（开发构建）下没有收紧，回 409。
func (u *userNet) hostHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/exit-endpoints", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.Header().Set("Allow", http.MethodPut)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Endpoints []usernet.ExitEndpoint `json:"endpoints"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeHostJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不对"})
			return
		}
		if u.out == nil {
			writeHostJSON(w, http.StatusConflict, map[string]string{"error": "开放模式（开发构建）下出站没有收紧，不需要端点集合"})
			return
		}
		if err := u.out.SetExitEndpoints(req.Endpoints, u.exitLoopback); err != nil {
			writeHostJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if u.logf != nil {
			u.logf("分流出口的端点已替换（%d 个）", len(req.Endpoints)) // 不写地址和域名
		}
		writeHostJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(req.Endpoints)})
	})
	// PUT /v1/main-hops：整体替换出站白名单里主代理的那一组（M5-4，m5-plan v2.2 决定 1）：
	// {"hops":["host:port[,port|lo-hi…]"],"lan":[{"addr":"IPv4:端口","confirmed":true}]}；不合法 422，不改任何东西；
	// 分流出口那一组和启动时的静态白名单不受影响。回环一律拒绝，局域网节点要带“已经用户确认”的标记。
	mux.HandleFunc("/v1/main-hops", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.Header().Set("Allow", http.MethodPut)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req usernet.MainHops
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeHostJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不对"})
			return
		}
		if u.out == nil {
			writeHostJSON(w, http.StatusConflict, map[string]string{"error": "开放模式（开发构建）下出站没有收紧，不需要第一跳名单"})
			return
		}
		if err := u.out.SetMainHops(req); err != nil {
			writeHostJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if u.logf != nil {
			u.logf("主代理的第一跳名单已替换（第一跳 %d 项、局域网节点 %d 个）", len(req.Hops), len(req.LAN)) // 不写地址和域名
		}
		writeHostJSON(w, http.StatusOK, map[string]any{"ok": true, "hops": len(req.Hops), "lan": len(req.LAN)})
	})
	mux.HandleFunc("/v1/netstats", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(u.stats())
	})
	mux.HandleFunc("/", http.NotFound)
	return mux
}

func writeHostJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// newSocketpair 可替换（测试用）。
var newSocketpair = socketpair

// factory 包住创建虚拟机的工厂：每次开机建一对新的 socketpair，一端作 Spec.NetFD。
// 内层 Factory 失败时两端都关掉（Supervisor 在这条路径上不会调 ForceStop）。
func (u *userNet) factory(base vmhost.Factory) vmhost.Factory {
	return func(spec vmhost.Spec) (vmhost.Machine, error) {
		vmEnd, hostConn, err := newSocketpair()
		if err != nil {
			return nil, err
		}
		spec.NetFD = vmEnd
		m, err := base(spec)
		if err != nil {
			hostConn.Close()
			vmEnd.Close()
			return nil, err
		}
		return newNetMachine(m, vmEnd, hostConn, u.accept, u.logf), nil
	}
}

// netMachine 把一台虚拟机和它的网络绑在一起：
//   - Start 成功后才把宿主一端交给虚拟网络（AcceptVfkit）；
//   - 网络协程出错（或意外返回）时：置失败标记（此后 DialVsock 一律报错，巡检会发现并走
//     Supervisor 的强停和 ErrStuck 路径）、立即强停内层，算作意外停止，由 Supervisor 退避重启——
//     不能留一台没有网的虚拟机继续被当成健康；
//   - Done 只在内层真的停了、网络也收拾完之后才关闭。
//
// 停止顺序固定为：取消 ctx → 关 conn（SOCK_DGRAM 的对端关闭不会让 Read 返回，只能关本端）
// → 等 Accept 协程返回 → 关虚拟机端的 File（VZ 会 dup 这个 fd，这里持有到最后只为确定性）。
//
// 已知限制：虚拟机卡死不收包时，switch 的 txBuf 遇到 ENOBUFS 会持锁空转
// （gvisor-tap-vsock pkg/tap/switch.go），要等巡检强停后关掉 conn 才解开。
type netMachine struct {
	vmhost.Machine
	vmEnd    *os.File
	hostConn net.Conn
	accept   func(ctx context.Context, conn net.Conn) error
	logf     func(format string, a ...any)

	ctx        context.Context
	cancel     context.CancelFunc
	acceptDone chan struct{}
	done       chan struct{}
	stopOnce   sync.Once
	failed     atomic.Bool
	mu         sync.Mutex
	netErr     error
}

func newNetMachine(m vmhost.Machine, vmEnd *os.File, hostConn net.Conn,
	accept func(context.Context, net.Conn) error, logf func(string, ...any)) *netMachine {
	ctx, cancel := context.WithCancel(context.Background())
	return &netMachine{Machine: m, vmEnd: vmEnd, hostConn: hostConn, accept: accept, logf: logf,
		ctx: ctx, cancel: cancel, acceptDone: make(chan struct{}), done: make(chan struct{})}
}

// errNetDown 是虚拟机网络协程退出。
var errNetDown = errors.New("虚拟机网络中断")

func (n *netMachine) Start() error {
	if err := n.Machine.Start(); err != nil {
		close(n.acceptDone) // Accept 协程没起
		n.shutdownNet()
		close(n.done)
		return err
	}
	go func() {
		defer close(n.acceptDone)
		err := n.accept(n.ctx, n.hostConn)
		if n.ctx.Err() != nil {
			return // 正在停止：先取消再关 conn，Accept 返回什么都不算网络出错
		}
		if err == nil {
			err = errors.New("网络协程意外返回")
		}
		n.mu.Lock()
		n.netErr = fmt.Errorf("%w：%v", errNetDown, err)
		n.mu.Unlock()
		n.failed.Store(true)
		if n.logf != nil {
			n.logf("虚拟机网络中断（%v），强停虚拟机", err)
		}
		_ = n.Machine.ForceStop()
	}()
	go func() {
		<-n.Machine.Done()
		n.shutdownNet()
		close(n.done)
	}()
	return nil
}

// shutdownNet 按固定顺序收拾网络：取消 → 关 conn → 等 Accept 返回 → 关虚拟机端 File。
func (n *netMachine) shutdownNet() {
	n.stopOnce.Do(func() {
		n.cancel()
		n.hostConn.Close()
		<-n.acceptDone
		n.vmEnd.Close()
	})
}

func (n *netMachine) Done() <-chan struct{} { return n.done }

func (n *netMachine) Err() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.netErr != nil {
		return n.netErr
	}
	return n.Machine.Err()
}

// DialVsock 在网络出错后一律报错：万一 ForceStop 没生效，巡检也会把这台判为不健康。
func (n *netMachine) DialVsock(ctx context.Context, port uint32) (net.Conn, error) {
	if n.failed.Load() {
		return nil, errNetDown
	}
	return n.Machine.DialVsock(ctx, port)
}
