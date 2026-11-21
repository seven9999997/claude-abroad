package sysnet

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"sync"
)

// FakeState 是 Fake 的全部状态。
type FakeState struct {
	Default string                  // 默认路由的接口（例如 en0；Via 查不到更具体的路由时用它）
	Routes  map[netip.Prefix]string // 目的网段 → 接口名
	DNS     netip.Addr              // 我们设的 DNS（零值表示没有）
	IPv6    map[string]IPv6Setting  // 网络服务 → IPv6 设置
	Net     NetInfo
	SysDNS  []netip.Addr // 系统当前首选解析器的服务器（System 返回它；只读，不参与 Equal）
	// Locale 是 Mac 的地区设置（M4-5；User 字段不用，登录用户看 Console）。参与 Equal：恢复后要等于初始值。
	Locale MacLocale
	// Console 是当前登录用户（/dev/console 的属主；空表示没人登录）。不参与 Equal。
	Console string
}

func (s FakeState) clone() FakeState {
	return FakeState{
		Default: s.Default,
		Routes:  maps.Clone(s.Routes),
		DNS:     s.DNS,
		IPv6:    maps.Clone(s.IPv6),
		Net:     NetInfo{Direct: slices.Clone(s.Net.Direct), VMIfs: slices.Clone(s.Net.VMIfs)},
		SysDNS:  slices.Clone(s.SysDNS),
		Locale:  s.Locale.clone(),
		Console: s.Console,
	}
}

// Equal 报告两个状态是否完全相等（nil 与空 map、空切片视为相等）。
func (s FakeState) Equal(o FakeState) bool {
	return s.Default == o.Default && maps.Equal(s.Routes, o.Routes) && s.DNS == o.DNS && maps.Equal(s.IPv6, o.IPv6) &&
		slices.Equal(s.Net.Direct, o.Net.Direct) && slices.Equal(s.Net.VMIfs, o.Net.VMIfs) && s.Locale.Equal(o.Locale)
}

// Fake 是内存里的本机网络（7b 的状态机测试用）：
//   - Calls 记录每次调用（"routes.add 198.18.0.0/15 utun7" 这种形式，失败的也记）；
//   - FailNext(op, err) 让下一次该操作返回 err，且不改状态；
//   - OnChange 在每次成功的改动调用之后恰好回调一次（不持锁；可在里面调用 Snapshot）。
type Fake struct {
	OnChange func(op string)

	mu    sync.Mutex
	st    FakeState
	calls []string
	fail  map[string]error
}

// 操作名（FailNext 和 OnChange 用）。
const (
	OpRoutesAdd     = "routes.add"
	OpRoutesDelete  = "routes.delete"
	OpRoutesPresent = "routes.present"
	OpRoutesVia     = "routes.via"
	OpRoutesConf    = "routes.conflicts"
	OpDNSSet        = "dns.set"
	OpDNSRemove     = "dns.remove"
	OpDNSPresent    = "dns.present"
	OpDNSSystem     = "dns.system"
	OpIPv6Snapshot  = "ipv6.snapshot"
	OpIPv6Off       = "ipv6.off"
	OpIPv6Restore   = "ipv6.restore"
	OpInterfaces    = "interfaces"
)

// NewFake 用初始状态建一个 Fake（深拷贝）。
func NewFake(initial FakeState) *Fake {
	st := initial.clone()
	if st.Routes == nil {
		st.Routes = map[netip.Prefix]string{}
	}
	if st.IPv6 == nil {
		st.IPv6 = map[string]IPv6Setting{}
	}
	return &Fake{st: st, fail: map[string]error{}}
}

// Snapshot 返回当前状态的深拷贝。
func (f *Fake) Snapshot() FakeState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st.clone()
}

// Calls 返回调用记录的副本。
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// FailNext 让下一次 op 返回 err（不改状态）。
func (f *Fake) FailNext(op string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[op] = err
}

// SetNet 改只读的网卡信息（模拟换网络；不算改动，不回调）。
func (f *Fake) SetNet(n NetInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st.Net = NetInfo{Direct: slices.Clone(n.Direct), VMIfs: slices.Clone(n.VMIfs)}
}

// RemoveService 模拟用户删掉了一个网络服务（不算我们的改动，不回调）。
func (f *Fake) RemoveService(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.st.IPv6, name)
}

// RemoveRoutesVia 模拟 utun 消失：经它的路由随之消失（测试辅助，不回调）。
func (f *Fake) RemoveRoutesVia(ifName string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for dst, via := range f.st.Routes {
		if via == ifName {
			delete(f.st.Routes, dst)
		}
	}
}

// SetForeignRoute 模拟别的软件插入或改写一条路由（测试辅助，不回调）。
func (f *Fake) SetForeignRoute(dst netip.Prefix, ifName string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st.Routes[dst.Masked()] = ifName
}

// DeleteRoute 模拟别的软件删掉一条路由（测试辅助，不回调）。
func (f *Fake) DeleteRoute(dst netip.Prefix) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.st.Routes, dst.Masked())
}

// RemoveDNS 模拟别的软件删掉我们的 DNS 设置（测试辅助，不回调）。
func (f *Fake) RemoveDNS() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st.DNS = netip.Addr{}
}

// ClearRoutesAndDNS 模拟重启：我们加的路由和 DNS 设置都不跨重启保留（测试辅助，不回调）。
func (f *Fake) ClearRoutesAndDNS() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st.Routes = map[netip.Prefix]string{}
	f.st.DNS = netip.Addr{}
}

// do 记录调用、处理 FailNext，再在锁里执行 fn；mutating 且成功时在锁外回调 OnChange。
func (f *Fake) do(op, detail string, mutating bool, fn func() error) error {
	f.mu.Lock()
	call := op
	if detail != "" {
		call += " " + detail
	}
	f.calls = append(f.calls, call)
	if err, ok := f.fail[op]; ok {
		delete(f.fail, op)
		f.mu.Unlock()
		return err
	}
	err := fn()
	f.mu.Unlock()
	if err == nil && mutating && f.OnChange != nil {
		f.OnChange(op)
	}
	return err
}

// ErrNotFound 是要删除或还原的东西不存在。
var ErrNotFound = errors.New("不存在")

// ErrExists 是要添加的路由或 DNS 设置已经存在（调用方按成功处理）。
var ErrExists = errors.New("已存在")

// Routes 返回 Fake 的 Routes 视图。
func (f *Fake) Routes() Routes { return fakeRoutes{f} }

// DNS 返回 Fake 的 DNS 视图。
func (f *Fake) DNS() DNS { return fakeDNS{f} }

// IPv6 返回 Fake 的 IPv6 视图。
func (f *Fake) IPv6() IPv6 { return fakeIPv6{f} }

// Interfaces 实现 Interfaces。
func (f *Fake) Interfaces(ctx context.Context) (NetInfo, error) {
	var out NetInfo
	err := f.do(OpInterfaces, "", false, func() error {
		out = NetInfo{Direct: slices.Clone(f.st.Net.Direct), VMIfs: slices.Clone(f.st.Net.VMIfs)}
		return nil
	})
	return out, err
}

type fakeRoutes struct{ f *Fake }

func (r fakeRoutes) Add(ctx context.Context, dst netip.Prefix, ifName string) error {
	return r.f.do(OpRoutesAdd, dst.String()+" "+ifName, true, func() error {
		if !dst.IsValid() || ifName == "" {
			return errors.New("参数不合法")
		}
		if cur, ok := r.f.st.Routes[dst.Masked()]; ok && cur == ifName {
			return fmt.Errorf("路由 %s：%w", dst, ErrExists)
		}
		r.f.st.Routes[dst.Masked()] = ifName // 不存在就加，接口不同就改写
		return nil
	})
}

func (r fakeRoutes) Delete(ctx context.Context, dst netip.Prefix) error {
	return r.f.do(OpRoutesDelete, dst.String(), true, func() error {
		if _, ok := r.f.st.Routes[dst.Masked()]; !ok {
			return fmt.Errorf("路由 %s：%w", dst, ErrNotFound)
		}
		delete(r.f.st.Routes, dst.Masked())
		return nil
	})
}

func (r fakeRoutes) Present(ctx context.Context) (bool, error) {
	var ok bool
	err := r.f.do(OpRoutesPresent, "", false, func() error { ok = len(r.f.st.Routes) > 0; return nil })
	return ok, err
}

func (r fakeRoutes) Via(ctx context.Context, addr netip.Addr) (string, error) {
	var out string
	err := r.f.do(OpRoutesVia, addr.String(), false, func() error {
		best := -1
		for dst, via := range r.f.st.Routes {
			if dst.Contains(addr) && dst.Bits() > best {
				best, out = dst.Bits(), via
			}
		}
		if best < 0 {
			if r.f.st.Default == "" {
				return fmt.Errorf("到 %s 的路由：%w", addr, ErrNotFound)
			}
			out = r.f.st.Default
		}
		return nil
	})
	return out, err
}

func (r fakeRoutes) Conflicts(ctx context.Context, ours string) ([]string, error) {
	var out []string
	err := r.f.do(OpRoutesConf, ours, false, func() error {
		rows := []RouteRow{}
		if r.f.st.Default != "" {
			rows = append(rows, RouteRow{Dest: "default", Default: true, OK: true, Iface: r.f.st.Default})
		}
		dsts := slices.Collect(maps.Keys(r.f.st.Routes))
		slices.SortFunc(dsts, func(a, b netip.Prefix) int { return a.Addr().Compare(b.Addr()) })
		for _, d := range dsts {
			rows = append(rows, RouteRow{Dest: d.String(), Prefix: d, OK: true, Iface: r.f.st.Routes[d]})
		}
		out = ConflictIfaces(rows, ours)
		return nil
	})
	return out, err
}

type fakeDNS struct{ f *Fake }

func (d fakeDNS) Set(ctx context.Context, server netip.Addr) error {
	return d.f.do(OpDNSSet, server.String(), true, func() error {
		if !server.IsValid() {
			return errors.New("参数不合法")
		}
		if d.f.st.DNS == server {
			return fmt.Errorf("DNS 设置：%w", ErrExists)
		}
		d.f.st.DNS = server
		return nil
	})
}

func (d fakeDNS) Remove(ctx context.Context) error {
	return d.f.do(OpDNSRemove, "", true, func() error {
		if !d.f.st.DNS.IsValid() {
			return fmt.Errorf("DNS 设置：%w", ErrNotFound)
		}
		d.f.st.DNS = netip.Addr{}
		return nil
	})
}

func (d fakeDNS) Present(ctx context.Context) (bool, error) {
	var ok bool
	err := d.f.do(OpDNSPresent, "", false, func() error { ok = d.f.st.DNS.IsValid(); return nil })
	return ok, err
}

type fakeIPv6 struct{ f *Fake }

func (v fakeIPv6) Snapshot(ctx context.Context) (map[string]IPv6Setting, error) {
	var out map[string]IPv6Setting
	err := v.f.do(OpIPv6Snapshot, "", false, func() error { out = maps.Clone(v.f.st.IPv6); return nil })
	return out, err
}

func (v fakeIPv6) Off(ctx context.Context, service string) error {
	return v.f.do(OpIPv6Off, service, true, func() error {
		if _, ok := v.f.st.IPv6[service]; !ok {
			return fmt.Errorf("网络服务：%w", ErrNotFound)
		}
		v.f.st.IPv6[service] = IPv6Setting{Mode: IPv6Off}
		return nil
	})
}

func (v fakeIPv6) Restore(ctx context.Context, service string, s IPv6Setting) error {
	return v.f.do(OpIPv6Restore, service+" "+string(s.Mode), true, func() error {
		if _, ok := v.f.st.IPv6[service]; !ok {
			return fmt.Errorf("网络服务：%w", ErrNotFound)
		}
		if !s.Valid() {
			return errors.New("IPv6 设置不合法")
		}
		v.f.st.IPv6[service] = s
		return nil
	})
}

func (d fakeDNS) System(ctx context.Context) ([]netip.Addr, error) {
	var out []netip.Addr
	err := d.f.do(OpDNSSystem, "", false, func() error { out = slices.Clone(d.f.st.SysDNS); return nil })
	return out, err
}

// SetSysDNSForTest 改系统当前首选解析器（测试用）。
func (f *Fake) SetSysDNSForTest(addrs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st.SysDNS = nil
	for _, a := range addrs {
		f.st.SysDNS = append(f.st.SysDNS, netip.MustParseAddr(a))
	}
}
