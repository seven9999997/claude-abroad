package daemon

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"claude-abroad/internal/envcheck"
	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/gatesm"
	"claude-abroad/internal/pf"
	"claude-abroad/internal/preflight"
	"claude-abroad/internal/region"
	"claude-abroad/internal/rules"
	"claude-abroad/internal/state"
	"claude-abroad/internal/sysnet"
	"claude-abroad/internal/usererror"
	"claude-abroad/internal/wire"
)

// EnvDeps 是环境检测（M4-6a）的系统依赖。System.Env 为 nil 时不做环境检测（假系统的最小模式、daemon-sim）。
type EnvDeps struct {
	// DialV6 尝试连一个只有 IPv6 的地址（06）：返回 nil 表示连上了（泄漏）。
	DialV6 func(ctx context.Context) error
	// Resolve 经系统解析器解析（07）。
	Resolve func(ctx context.Context, name string) ([]netip.Addr, error)
	// Conflicts 是冲突检查里系统代理、PAC、自动发现、路由冲突这几项（11）。
	Conflicts func(ctx context.Context) []preflight.Finding
	// Extra 是额外的检查项（测试注入需要发探测的假项目、警告项）。
	Extra []envcheck.Item

	// ---- M4-6b ----
	// Probe 是本机发起探测的拨号（nil 用 envcheck.ProbeDial；测试指向本地假服务）。02、03、08、13、14 经它。
	Probe func(ctx context.Context, network, addr string) (net.Conn, error)
	// RootCAs 是 02/03（取 IP 的源）、10（Claude）校验证书用的根（nil 用系统根证书；测试注入假 CA）。
	RootCAs *x509.CertPool
	// IPSources、GeoSources 覆盖 exitcheck 的查询源（测试指向假服务；nil 用默认）。
	IPSources  []exitcheck.IPSource
	GeoSources []exitcheck.GeoSource
	// Echo 是 07 后半（经我们的 DNS 查回显解析器地址的记录）。
	Echo envcheck.EchoResolver
	// StunServers 覆盖 08 的服务器（nil 用默认）；RealDial 经直连拨号模块取一个连到 STUN 服务器的 UDP 连接（本机真实 IP）。
	StunServers []string
	RealDial    func(ctx context.Context) (net.Conn, error)
	// ProcEnvs 列出 Claude 根进程的环境（12、17）。
	ProcEnvs envcheck.ProcEnvs
	// Home 是当前登录用户的主目录（15、16）。
	Home func() (string, error)
	// ReachOpen 覆盖第 10 项的开流（nil 用数据面按出口开流；测试指向本地的 TLS 假服务器）。
	ReachOpen func(ctx context.Context, h wire.Header) (net.Conn, error)
	// Timeouts（测试调小；0 用默认）。
	StunTimeout, ReachTimeout time.Duration
}

// envDNSTestTimeout 是单项探测的时限。
const envProbeTimeout = 10 * time.Second

// takeoverView 是 01 接管就绪：复用监护的核对（pf 保护、utun、路由经我们的 utun、DNS 在）。不含地址。
func (d *Daemon) takeoverView(ctx context.Context) ([]string, error) {
	s := d.opts.Sys
	var probs []string
	if !d.ppf.P.Protected() {
		probs = append(probs, "断网保护没有生效")
	}
	name := d.tun.Name()
	if name == "" || !d.tun.Alive() {
		probs = append(probs, "虚拟网卡不在")
	}
	if s.Routes != nil {
		for _, a := range []string{"1.1.1.1", "129.0.0.1", "198.18.0.2"} {
			via, err := s.Routes.Via(ctx, netip.MustParseAddr(a))
			if err != nil {
				return nil, err
			}
			if name == "" || via != name {
				probs = append(probs, "路由没有经过 Claude 出国的虚拟网卡")
				break
			}
		}
		taken, err := s.Routes.Conflicts(ctx, name)
		if err != nil {
			return nil, err
		}
		if len(taken) > 0 {
			probs = append(probs, "别的软件的虚拟网卡占着路由")
		}
	}
	if s.DNS != nil {
		ok, err := s.DNS.Present(ctx)
		if err != nil {
			return nil, err
		}
		if !ok {
			probs = append(probs, "系统的 DNS 没有指向 Claude 出国")
		}
	}
	return probs, nil
}

// envItems 组装检查项。
func (d *Daemon) envItems(e *EnvDeps) []envcheck.Item {
	s := d.opts.Sys
	exitUp := func(string) bool { return false }
	if s.Data != nil {
		exitUp = s.Data.ExitUp
	}
	// 6b 的公共部分：从本机看出口（02、03、13、14、08）、锁定的目标、本机真实 IP。
	target := func() (region.Target, bool) {
		if d.regions == nil || !d.regionOn() {
			return region.Target{}, false
		}
		t, ok, err := d.regions.Locked()
		return t, ok && err == nil
	}
	checker := &exitcheck.Checker{Opener: envcheck.ProbeOpener{Dial: e.Probe}, RootCAs: e.RootCAs,
		IPSources: e.IPSources, GeoSources: e.GeoSources, Now: d.envNow}
	exit := &envcheck.ExitProbe{Checker: checker, Target: target, Now: d.envNow, Baseline: func() *exitcheck.Baseline {
		if s.Baseline == nil {
			return nil
		}
		b, err := exitcheck.LoadBaseline(s.Baseline.Path)
		if err != nil {
			return nil
		}
		return &b
	}}
	d.realIP = &envcheck.RealIP{Dial: e.RealDial}
	udpOff := func() bool { return s.Data != nil && s.Data.UDPUnsupported(rules.ExitMain) }
	fixer := &envcheck.RegionFixer{D: envcheck.RegionDeps{Target: target,
		Read: func(ctx context.Context) (sysnet.MacLocale, error) {
			if s.Locale == nil {
				return sysnet.MacLocale{}, errors.New("没有地区同步")
			}
			return s.Locale.Read(ctx)
		},
		Reapply: func(ctx context.Context) error {
			err := d.m.ReapplyRegion(ctx)
			if errors.Is(err, state.ErrBusy) {
				return fmt.Errorf("%w：%v", envcheck.ErrBusy, err)
			}
			return err
		}}}
	home := e.Home
	if home == nil {
		home = func() (string, error) { return "", errors.New("没有配置") }
	}
	procEnvs := e.ProcEnvs
	if procEnvs == nil {
		procEnvs = func(context.Context) ([]envcheck.ProcEnv, error) { return nil, errors.New("没有配置") }
	}
	var open func(ctx context.Context, h wire.Header) (net.Conn, error)
	if e.ReachOpen != nil {
		open = e.ReachOpen
	} else if s.Data != nil {
		open = s.Data.Streams().Open
	} else {
		open = func(context.Context, wire.Header) (net.Conn, error) { return nil, errors.New("没有数据面") }
	}
	hd := envcheck.HomeDeps{Home: home, Target: target}
	items := []envcheck.Item{
		envcheck.ExitIP(exit),
		envcheck.ExitRegion(exit),
		envcheck.Timezone(fixer),
		envcheck.Language(fixer),
		envcheck.STUN(envcheck.STUNDeps{Servers: e.StunServers, Dial: e.Probe, Real: d.realIP, Exit: exit, UDPUnsupported: udpOff, Timeout: e.StunTimeout}),
		envcheck.ClaudeReach(envcheck.ReachDeps{Open: open, RootCAs: e.RootCAs, Timeout: e.ReachTimeout,
			Exits: func() []string { return envcheck.ClaudeExits(d.currentRules(), exitUp) }}),
		envcheck.ClaudeProcEnv(procEnvs),
		envcheck.IPStability(exit, &envcheck.History{Dir: d.opts.StateDir, OnEvent: func(k, detail string) { d.events.Add("envcheck", k, detail) }}),
		envcheck.NetworkType(exit),
		envcheck.ClaudeClientLang(hd),
		envcheck.BrowserLang(hd),
		envcheck.TerminalLang(procEnvs, target),
		envcheck.Takeover(d.takeoverView),
		envcheck.IPv6(envcheck.IPv6Deps{Snapshot: s.IPv6.Snapshot, DialV6: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, envProbeTimeout)
			defer cancel()
			return e.DialV6(ctx)
		}}),
		envcheck.DNS(func(ctx context.Context, name string) ([]netip.Addr, error) {
			ctx, cancel := context.WithTimeout(ctx, envProbeTimeout)
			defer cancel()
			return e.Resolve(ctx, name)
		}, e.Echo),
		envcheck.ClaudeRoute(envcheck.RouteDeps{Config: d.currentRules, ExitUp: exitUp}),
		envcheck.Conflicts(e.Conflicts),
	}
	return append(items, e.Extra...)
}

// setupEnv 组装环境检测（Start 里调用；System.Env 为 nil 时什么都不做）。
func (d *Daemon) setupEnv() {
	e := d.opts.Sys.Env
	if e == nil {
		return
	}
	d.envSettings = &envcheck.Settings{Path: filepath.Join(d.opts.StateDir, envcheck.SettingsFileName)}
	d.envItemsList = d.envItems(e)
	d.env = &envcheck.Monitor{
		BeforeEnable: d.measureRealIP, // 启用时测一次本机真实公网 IP（只在内存）
		Runner:       &envcheck.Runner{Items: d.envItemsList, Disabled: d.envSettings.Disabled, Clock: envClock{d.opts.Clock}},
		Gate:         d.gate,
		Enabled:      func() bool { return d.fileState() == state.Enabled },
		AfterPass:    d.re.Kick, // 全部通过：补一轮出口复查重新放行
		Clock:        d.opts.Clock,
		Interval:     d.opts.EnvInterval,
		OnEvent:      func(kind, detail string) { d.events.Add("envcheck", kind, detail) },
	}
	d.m.D.Env = d.env
}

func (d *Daemon) envNow() time.Time {
	if d.opts.Clock != nil {
		return d.opts.Clock.Now()
	}
	return time.Now()
}

// measureRealIP 经直连拨号模块测本机真实公网 IP（启用时、网络变化时；结果只在内存里）。
func (d *Daemon) measureRealIP(ctx context.Context) {
	if d.realIP == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, envProbeTimeout)
	defer cancel()
	d.realIP.Measure(ctx)
}

// netWatch 包着监护的网络快照：直连网段或虚拟机网桥变了（换了网络）就重测本机真实 IP、补一轮环境检测（决定 12）。
type netWatch struct {
	inner pf.NetSource
	d     *Daemon
	mu    sync.Mutex
	last  *pf.NetSnapshot
}

func (w *netWatch) Snapshot(ctx context.Context) (pf.NetSnapshot, error) {
	snap, err := w.inner.Snapshot(ctx)
	if err != nil {
		return snap, err
	}
	w.mu.Lock()
	changed := w.last != nil && (!slices.Equal(w.last.Direct, snap.Direct) || !slices.Equal(w.last.VMIfs, snap.VMIfs))
	c := snap
	w.last = &c
	w.mu.Unlock()
	if changed && w.d.env != nil {
		w.d.events.Add("envcheck", "network_changed", "网络变了：重测本机真实公网 IP、补一轮环境检测")
		go func() {
			w.d.measureRealIP(context.Background())
			w.d.envKick()
		}()
	}
	return snap, nil
}

// envKick 补一轮环境检测（没有配置时什么都不做）。
func (d *Daemon) envKick() {
	if d.env != nil {
		d.env.Kick()
	}
}

// claudeRouteKey 是当前规则下 Claude 的去向摘要（换规则时比较）。
func (d *Daemon) claudeRouteKey(c *rules.Config) string {
	up := func(string) bool { return false }
	if d.opts.Sys.Data != nil {
		up = d.opts.Sys.Data.ExitUp
	}
	return envcheck.ClaudeRouteKey(c, up)
}

// envStatus 是 status 里的环境检测（最近一轮每项的结果、说明、修复办法；不含地址）。
func (d *Daemon) envStatus() *envcheck.Report {
	if d.env == nil {
		return nil
	}
	rep, ok := d.env.Last()
	if !ok {
		return nil
	}
	return &rep
}

// handleEnvGet 是 GET /v1/envcheck。
func (d *Daemon) handleEnvGet(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	if d.env == nil {
		writeJSON(w, http.StatusNotImplemented, errBody{"这个模式没有环境检测"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"last": d.envStatus(), "disabled": d.envSettings.Disabled()})
}

// handleEnvRun 是 POST /v1/envcheck/run：立即补跑一轮（只在启用状态下有意义）。
func (d *Daemon) handleEnvRun(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	if d.env == nil {
		writeJSON(w, http.StatusNotImplemented, errBody{"这个模式没有环境检测"})
		return
	}
	if d.fileState() != state.Enabled {
		writeJSON(w, http.StatusConflict, errBody{"只有在启用状态下才跑环境检测"})
		return
	}
	rep := d.env.RunOnce(context.WithoutCancel(r.Context()))
	writeJSON(w, http.StatusOK, rep)
}

// EnvSettingsRequest 是 PUT /v1/envcheck/settings 的请求体。
type EnvSettingsRequest struct {
	Disabled []string `json:"disabled"`
}

// handleEnvSettings 是 PUT /v1/envcheck/settings：关闭或打开警告项；硬性项关不掉（422）。属主才能调用（本地接口已校验对端）。
func (d *Daemon) handleEnvSettings(w http.ResponseWriter, r *http.Request) {
	if d.env == nil {
		writeJSON(w, http.StatusNotImplemented, errBody{"这个模式没有环境检测"})
		return
	}
	var req EnvSettingsRequest
	b, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, errBody{"请求体超过 64 KiB"})
		return
	}
	if err := json.Unmarshal(b, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody{"请求体不是合法的 JSON"})
		return
	}
	if err := d.envSettings.SetDisabled(req.Disabled, d.envItemsList); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, envcheck.ErrHardItem) {
			code = http.StatusUnprocessableEntity
		}
		writeJSON(w, code, errBody{usererror.Wrap("修改环境检测的设置", err, "只能关闭警告项，硬性项不能关闭。").Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"disabled": d.envSettings.Disabled()})
}

var _ = exitcheck.Pass

// envClock 让环境检测的重试等待用注入的时钟（nil 时用真实时间）。
type envClock struct{ c gatesm.Clock }

func (e envClock) Now() time.Time {
	if e.c == nil {
		return time.Now()
	}
	return e.c.Now()
}

func (e envClock) Sleep(ctx context.Context, d time.Duration) error {
	if e.c == nil {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
	ch := make(chan struct{})
	t := e.c.AfterFunc(d, func() { close(ch) })
	select {
	case <-ctx.Done():
		t.Stop()
		return ctx.Err()
	case <-ch:
		return nil
	}
}
