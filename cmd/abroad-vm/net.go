package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
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
}

// InterfaceFresh 是物理网卡查询结果的新鲜期。
const InterfaceFresh = 2 * time.Second

// 出站模式（netstats 的 outbound_mode）。
const (
	outboundOpen     = "open"      // --outbound-open：开发用，公网一律放行（M1 的行为）
	outboundFirstHop = "first_hop" // 默认：只放行白名单和路线第一跳（设计 4.5，M2-1a）
)

func newUserNet(o options, logf func(format string, a ...any)) (*userNet, error) {
	ref := usernet.NewPolicyRef(&usernet.Policy{Allow: o.allow})
	// 物理网卡：Watcher 是唯一的缓存（新鲜期 2 秒，过期先用旧值、后台刷新，30 秒以上同步查；M2-2）。
	// 绑定失败时作废，下次拨号重新查。
	w := physif.NewWatcher(physif.Query, InterfaceFresh)
	d := &usernet.Dialer{Rules: ref, Interface: w.Current, OnBindFailed: w.Invalidate}
	u := &userNet{dialer: d, ref: ref, mode: outboundOpen, logf: logf, exitLoopback: o.devAgent}
	var dnsx func(context.Context, *dns.Msg) (*dns.Msg, error) // nil：一律 REFUSED
	if !o.outboundOpen {
		u.mode = outboundFirstHop
		u.out = usernet.NewOutbound(o.allow, o.firstHops, nil)
		d.BeforeDial = u.out.BeforeDial
		eps := usernet.DefaultDoHEndpoints
		doh := &usernet.DoH{Endpoints: eps, Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return d.DialResolver(ctx, network, addr, eps)
		}}
		u.gwdns = &usernet.GatewayDNS{Out: u.out, Resolve: doh.Lookup}
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

func (u *userNet) logStats() {
	if u.logf != nil {
		st := u.stats()
		u.logf("网络计数（%s）：policy_denied=%d hop_denied=%d bind_failed=%d dial_ok=%d dial_ok_loopback=%d resolver_dial_ok=%d dns_answered=%d dns_refused=%d dns_failed=%d",
			st.OutboundMode, st.PolicyDenied, st.HopDenied, st.BindFailed, st.DialOK, st.DialOKLoopback, st.ResolverDialOK, st.Answered, st.Refused, st.Failed)
	}
}

// hostHandler 是 host.sock 上的诊断接口（只接受 abroad-vm 自己的用户和 root，见 peerListener）：
//   - GET /v1/netstats：计数（不含地址）；
//   - PUT /v1/exit-endpoints：整体替换出站白名单里分流出口的那一组（M4-8，m4-plan v2.12 决定 1）：
//     {"endpoints":[{"host":"…","proto":"tcp|udp","port":N}]}；不合法 422 并指出第几项，不改任何东西；
//     主代理第一跳那一组不受影响。开放模式（--outbound-open）下没有收紧，回 409。
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
			writeHostJSON(w, http.StatusConflict, map[string]string{"error": "开放模式（--outbound-open）下出站没有收紧，不需要端点集合"})
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
