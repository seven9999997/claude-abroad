package envcheck

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"fmt"
	"go/ast"
	"math/big"
	mrand "math/rand"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/exitpolicy"
	"claude-abroad/internal/procinfo"
	"claude-abroad/internal/region"
	"claude-abroad/internal/srccheck"
	"claude-abroad/internal/sysnet"
	"claude-abroad/internal/wire"
)

// ---- 02、03、13、14：从本机看出口 ----

func rep(items map[string]exitcheck.Result, ip string, country, org string) exitcheck.Report {
	r := exitcheck.Report{Country: country}
	if ip != "" {
		r.IP = netip.MustParseAddr(ip)
		r.Geo = &exitpolicy.Geo{IP: r.IP, Country: country, Org: org}
	}
	for _, n := range []string{"https", "ip", "mainland", "geo", "rule", "doh"} {
		res, ok := items[n]
		if !ok {
			res = exitcheck.Pass
		}
		r.Items = append(r.Items, exitcheck.Item{Name: n, Result: res, Cause: "x"})
	}
	return r
}

func probeOf(r exitcheck.Report, target *region.Target) *ExitProbe {
	return &ExitProbe{Run: func(context.Context, *exitcheck.Baseline) exitcheck.Report { return r },
		Target: func() (region.Target, bool) {
			if target == nil {
				return region.Target{}, false
			}
			return *target, true
		}}
}

var jp = region.Target{Timezone: "Asia/Tokyo", Lang: "ja-JP", Locale: "ja_JP", Country: "JP", Source: region.SourceExit}

// b、c、d. 02 出口 IP、03 出口地区。
func TestExitItems(t *testing.T) {
	ok := rep(nil, "203.0.113.7", "JP", "Example ISP")
	want(t, "02 通过", res(t, ExitIP(probeOf(ok, &jp))), exitcheck.Pass)
	want(t, "02 不符合规则", res(t, ExitIP(probeOf(rep(map[string]exitcheck.Result{"rule": exitcheck.Fail}, "203.0.113.8", "JP", ""), &jp))), exitcheck.Fail)
	want(t, "02 取不到 IP", res(t, ExitIP(probeOf(rep(map[string]exitcheck.Result{"ip": exitcheck.Unknown, "rule": exitcheck.Unknown}, "", "", ""), &jp))), exitcheck.Unknown)
	want(t, "03 通过", res(t, ExitRegion(probeOf(ok, &jp))), exitcheck.Pass)
	want(t, "03 国家不同", res(t, ExitRegion(probeOf(rep(nil, "203.0.113.7", "US", ""), &jp))), exitcheck.Fail)
	want(t, "03 大陆", res(t, ExitRegion(probeOf(rep(map[string]exitcheck.Result{"mainland": exitcheck.Fail}, "1.0.1.1", "CN", ""), &jp))), exitcheck.Fail)
	want(t, "03 地区没接时只判大陆", res(t, ExitRegion(probeOf(rep(nil, "203.0.113.7", "US", ""), nil))), exitcheck.Pass)
	want(t, "03 查不到", res(t, ExitRegion(probeOf(rep(map[string]exitcheck.Result{"mainland": exitcheck.Unknown}, "", "", ""), &jp))), exitcheck.Unknown)
	want(t, "03 查不到国家", res(t, ExitRegion(probeOf(rep(nil, "203.0.113.7", "", ""), &jp))), exitcheck.Unknown)
	want(t, "14 住宅", res(t, NetworkType(probeOf(ok, &jp))), exitcheck.Pass)
	want(t, "14 机房", res(t, NetworkType(probeOf(rep(nil, "203.0.113.7", "JP", "Example Hosting LLC"), &jp))), exitcheck.Fail)
	want(t, "14 无字段", res(t, NetworkType(probeOf(rep(nil, "203.0.113.7", "JP", ""), &jp))), exitcheck.Unknown)
}

// d. 取 IP 和位置的源全都不可用（真实的 exitcheck 检查器经本机探测出口，拨号全失败）：02、03 是无法判断。
func TestExitItemsAllSourcesDown(t *testing.T) {
	c := &exitcheck.Checker{Opener: ProbeOpener{Dial: func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	}}, Total: 2 * time.Second}
	p := &ExitProbe{Checker: c, Target: func() (region.Target, bool) { return jp, true }}
	for _, it := range []Item{ExitIP(p), ExitRegion(p)} {
		if o := it.Run(bg); o.Result != exitcheck.Unknown {
			t.Fatalf("%s：%s", it.ID, o.Result)
		}
	}
}

// i. 13 出口变化记录：文件 0600、里面没有 IP；48 小时窗口；变化 3 次以上给警告；文件损坏时从空记录开始并发事件。
func TestIPHistory(t *testing.T) {
	dir := t.TempDir()
	var evs []string
	h := &History{Dir: dir, OnEvent: func(k, _ string) { evs = append(evs, k) }}
	now := time.Unix(1_900_000_000, 0)
	ips := []string{"203.0.113.7", "203.0.113.8", "203.0.113.7", "203.0.113.9"}
	var n int
	for i, ip := range ips {
		var err error
		n, err = h.Record(netip.MustParseAddr(ip), now.Add(time.Duration(i)*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
	}
	if n != 3 {
		t.Fatalf("变化次数：%d", n)
	}
	for _, f := range []string{HistoryFileName, HistoryKeyName} {
		fi, err := os.Stat(filepath.Join(dir, f))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s 应 0600：%v", f, err)
		}
		b, _ := os.ReadFile(filepath.Join(dir, f))
		if strings.Contains(string(b), "203.0.113") {
			t.Fatalf("%s 里不应有 IP", f)
		}
	}
	// 48 小时后：旧记录滚出窗口。
	n, _ = h.Record(netip.MustParseAddr("203.0.113.9"), now.Add(52*time.Hour))
	if n != 0 {
		t.Fatalf("48 小时窗口：%d", n)
	}
	// 警告项：变化 3 次以上不通过（警告）。
	clock := now
	p := &ExitProbe{Now: func() time.Time { return clock }, TTL: time.Nanosecond}
	cur := "203.0.113.20"
	p.Run = func(context.Context, *exitcheck.Baseline) exitcheck.Report { return rep(nil, cur, "JP", "") }
	h2 := &History{Dir: t.TempDir()}
	it := IPStability(p, h2)
	for i, ip := range []string{"203.0.113.20", "203.0.113.21", "203.0.113.22"} {
		cur, clock = ip, now.Add(time.Duration(i)*time.Minute)
		want(t, "13 变化少", res(t, it), exitcheck.Pass)
	}
	cur, clock = "203.0.113.23", now.Add(time.Hour)
	want(t, "13 变化多", res(t, it), exitcheck.Fail)
	p.Run = func(context.Context, *exitcheck.Baseline) exitcheck.Report { return rep(nil, "", "", "") }
	clock = clock.Add(time.Minute) // 下一轮（不复用上一轮的结果）
	want(t, "13 取不到", res(t, it), exitcheck.Unknown)
	// 损坏。
	os.WriteFile(filepath.Join(dir, HistoryFileName), []byte("{broken"), 0o600)
	h3 := &History{Dir: dir, OnEvent: func(k, _ string) { evs = append(evs, k) }}
	if n, err := h3.Record(netip.MustParseAddr("203.0.113.7"), now); err != nil || n != 0 || len(evs) == 0 {
		t.Fatalf("损坏时从空记录开始并发事件：%d %v %v", n, err, evs)
	}
}

// ---- 08 STUN ----

func stunReply(req []byte, ap netip.AddrPort) []byte {
	var tx [12]byte
	copy(tx[:], req[8:20])
	attr := make([]byte, 12)
	binary.BigEndian.PutUint16(attr[0:], 0x0020)
	binary.BigEndian.PutUint16(attr[2:], 8)
	attr[5] = 0x01
	binary.BigEndian.PutUint16(attr[6:], ap.Port()^uint16(stunMagic>>16))
	a := ap.Addr().As4()
	binary.BigEndian.PutUint32(attr[8:], binary.BigEndian.Uint32(a[:])^stunMagic)
	b := make([]byte, 20)
	binary.BigEndian.PutUint16(b[0:], 0x0101)
	binary.BigEndian.PutUint16(b[2:], uint16(len(attr)))
	binary.BigEndian.PutUint32(b[4:], stunMagic)
	copy(b[8:], tx[:])
	return append(b, attr...)
}

// fakeStun 是本地的假 STUN 服务器：回报 addr（无效时不应答）。
func fakeStun(t *testing.T, addr string) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if addr == "" {
				continue
			}
			_, _ = pc.WriteTo(stunReply(buf[:n], netip.MustParseAddrPort(addr)), from)
		}
	}()
	return pc.LocalAddr().String()
}

// localDial 把 STUN 服务器名映射到本地假服务（测试里没有对真实域名的拨号）。
func localDial(m map[string]string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		local, ok := m[addr]
		if !ok {
			return nil, errors.New("测试里不拨真实地址：" + addr)
		}
		var d net.Dialer
		return d.DialContext(ctx, network, local)
	}
}

func TestStunCodec(t *testing.T) {
	req, tx := StunRequest()
	ap := netip.MustParseAddrPort("203.0.113.9:4500")
	got, err := ParseStunResponse(stunReply(req, ap), tx)
	if err != nil || got != ap {
		t.Fatalf("%v %v", got, err)
	}
	var other [12]byte
	if _, err := ParseStunResponse(stunReply(req, ap), other); err == nil {
		t.Fatal("事务号不对应报错")
	}
	for _, bad := range [][]byte{nil, make([]byte, 19), append(stunReply(req, ap)[:20], 0, 0x20, 0, 8)} {
		if _, err := ParseStunResponse(bad, tx); err == nil {
			t.Fatalf("应报错：%v", bad)
		}
	}
}

// b、c、d、g. 08：真实 IP、大陆、国家不同各检出；同国不同 IP 通过；主代理不支持 UDP 时全部无响应通过；
// 测不到真实 IP 时无法判断。
func TestStunItem(t *testing.T) {
	realSrv := fakeStun(t, "198.51.100.77:5000") // 直连拨号模块测得的“本机真实 IP”
	real := &RealIP{Dial: func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", realSrv)
	}}
	real.Measure(bg)
	if fmt.Sprintf("%v %+v %#v", real, real, real) != "RealIP(不显示) RealIP(不显示) RealIP(不显示)" {
		t.Fatal("RealIP 的输出不应带地址")
	}
	exit := probeOf(rep(nil, "203.0.113.7", "US", ""), nil)
	country := map[string]string{"203.0.113.9": "US", "203.0.113.66": "JP", "198.51.100.77": "US"}
	exit.LocateFn = func(_ context.Context, ip netip.Addr) (string, error) {
		c, ok := country[ip.String()]
		if !ok {
			return "", errors.New("查不到")
		}
		return c, nil
	}
	run := func(addr string, udpOff bool, r *RealIP) Outcome {
		srv := fakeStun(t, addr)
		return STUN(STUNDeps{Servers: []string{"s1:3478"}, Dial: localDial(map[string]string{"s1:3478": srv}), Real: r, Exit: exit,
			UDPUnsupported: func() bool { return udpOff }, Timeout: 300 * time.Millisecond}).Run(bg)
	}
	want(t, "同国不同 IP", run("203.0.113.9:1", false, real), exitcheck.Pass)
	want(t, "真实 IP", run("198.51.100.77:9", false, real), exitcheck.Fail)
	want(t, "大陆", run("1.0.1.1:9", false, real), exitcheck.Fail)
	want(t, "国家不同", run("203.0.113.66:9", false, real), exitcheck.Fail)
	want(t, "主代理不支持 UDP", run("", true, real), exitcheck.Pass)
	want(t, "支持 UDP 却全无响应", run("", false, real), exitcheck.Unknown)
	unknownReal := &RealIP{}
	unknownReal.Measure(bg)
	want(t, "测不到真实 IP", run("203.0.113.9:1", false, unknownReal), exitcheck.Unknown)
	want(t, "测不到真实 IP、回报的就是出口 IP", run("203.0.113.7:1", false, unknownReal), exitcheck.Pass)
	want(t, "回报出口 IP、真实 IP 已测到", run("203.0.113.7:1", false, real), exitcheck.Pass)
	o := run("198.51.100.77:9", false, real)
	if strings.Contains(o.Detail+o.Fix, "198.51.100.77") {
		t.Fatal("说明里不应有地址")
	}
}

// ---- 10 Claude 可达性 ----

type tlsWorld struct {
	ca     *x509.CertPool
	good   string // 正常的假服务
	bad    string // 证书不对
	hang   string // 握手后不回应
	exits  []string
	mu     sync.Mutex
	opened []string
}

func newTLSWorld(t *testing.T) *tlsWorld {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	leaf := func(signer *x509.Certificate, key *ecdsa.PrivateKey) tls.Certificate {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tpl := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{claudeHost, apiHost}, NotBefore: time.Now().Add(-time.Hour),
			NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, _ := x509.CreateCertificate(rand.Reader, tpl, signer, &k.PublicKey, key)
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
	}
	serve := func(cert tls.Certificate, hang bool) string {
		l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if hang {
				<-r.Context().Done()
				return
			}
			w.WriteHeader(http.StatusForbidden) // 任意状态码都算到达
		})}
		go srv.Serve(l)
		t.Cleanup(func() { srv.Close() })
		return l.Addr().String()
	}
	selfKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	selfTpl := *caTpl
	selfDER, _ := x509.CreateCertificate(rand.Reader, &selfTpl, &selfTpl, &selfKey.PublicKey, selfKey)
	selfCert, _ := x509.ParseCertificate(selfDER)
	w := &tlsWorld{ca: x509.NewCertPool()}
	w.ca.AddCert(caCert)
	w.good, w.bad, w.hang = serve(leaf(caCert, caKey), false), serve(leaf(selfCert, selfKey), false), serve(leaf(caCert, caKey), true)
	return w
}

// open 是假的“按出口开流”：全部指向本地假服务；记下用的出口和主机。
func (w *tlsWorld) open(to string) func(context.Context, wire.Header) (net.Conn, error) {
	return func(ctx context.Context, h wire.Header) (net.Conn, error) {
		w.mu.Lock()
		w.opened = append(w.opened, h.Exit+" "+h.Host)
		w.mu.Unlock()
		if to == "" {
			return nil, errors.New("连不上")
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", to)
	}
}

// f. 10：握手成功加任意状态码通过；证书不对不通过；超时、连不上无法判断；开流的出口等于规则里 Claude 的出口，多个出口各一次。
func TestClaudeReach(t *testing.T) {
	w := newTLSWorld(t)
	exits := func() []string { return []string{"main", "wg-a"} }
	d := func(to string, timeout time.Duration) ReachDeps {
		return ReachDeps{Open: w.open(to), Exits: exits, RootCAs: w.ca, Timeout: timeout}
	}
	want(t, "到达", res(t, ClaudeReach(d(w.good, 3*time.Second))), exitcheck.Pass)
	if got := strings.Join(w.opened, ","); got != "main "+claudeHost+",main "+apiHost+",wg-a "+claudeHost+",wg-a "+apiHost {
		t.Fatalf("开流：%s", got)
	}
	want(t, "证书不对", res(t, ClaudeReach(d(w.bad, 3*time.Second))), exitcheck.Fail)
	want(t, "超时", res(t, ClaudeReach(d(w.hang, 300*time.Millisecond))), exitcheck.Unknown)
	want(t, "连不上", res(t, ClaudeReach(d("", time.Second))), exitcheck.Unknown)
}

// ---- 04、05：自动改回 ----

type fakeMac struct {
	mu       sync.Mutex
	cur      sysnet.MacLocale
	reapply  func(*sysnet.MacLocale) error
	reads    int
	reapplys int
}

func (m *fakeMac) Read(context.Context) (sysnet.MacLocale, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads++
	return m.cur, nil
}

func (m *fakeMac) Reapply(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reapplys++
	if m.reapply == nil {
		return nil
	}
	return m.reapply(&m.cur)
}

func good() sysnet.MacLocale {
	return sysnet.MacLocale{Timezone: "Asia/Tokyo", AutoTZ: sysnet.Some("0"), User: "alice",
		UserLangs: sysnet.SomeList("ja-JP", "en-US"), UserLocale: sysnet.Some("ja_JP")}
}

// b、c、d、j. 04、05：一致通过；被改了能改回通过并记一次；改不回不通过；连续 3 轮都要改回有提示；忙时无法判断；
// 地区没接时已跳过且不读设置。
func TestRegionFixer(t *testing.T) {
	m := &fakeMac{cur: good()}
	fix := func() { m.cur = good() }
	f := &RegionFixer{D: RegionDeps{Read: m.Read, Reapply: m.Reapply, Target: func() (region.Target, bool) { return jp, true }}}
	want(t, "04 一致", res(t, Timezone(f)), exitcheck.Pass)
	want(t, "05 一致", res(t, Language(f)), exitcheck.Pass)
	m.reapply = func(l *sysnet.MacLocale) error { fix(); return nil }
	for i := 1; i <= 3; i++ {
		m.cur.AutoTZ = sysnet.Some("1") // 自动时区被重新打开
		o := res(t, Timezone(f))
		want(t, "04 改回", o, exitcheck.Pass)
		if f.Fixed["04"] != i || (i == 3) != strings.Contains(o.Detail, "别的软件") {
			t.Fatalf("第 %d 轮：%+v %s", i, f.Fixed, o.Detail)
		}
	}
	m.reapply = func(*sysnet.MacLocale) error { return nil } // 改不回
	m.cur.Timezone = "Europe/Paris"
	want(t, "04 改不回", res(t, Timezone(f)), exitcheck.Fail)
	m.cur = good()
	m.cur.UserLangs = sysnet.SomeList("en-US")
	want(t, "05 改不回", res(t, Language(f)), exitcheck.Fail)
	m.reapply = func(*sysnet.MacLocale) error { return fmt.Errorf("%w", ErrBusy) }
	want(t, "05 忙", res(t, Language(f)), exitcheck.Unknown)
	m.cur.User = ""
	want(t, "05 没人登录", res(t, Language(f)), exitcheck.Pass)
	off := &RegionFixer{D: RegionDeps{Read: m.Read, Reapply: m.Reapply, Target: func() (region.Target, bool) { return region.Target{}, false }}}
	reads := m.reads
	if o := Timezone(off).Run(bg); o.Result != Skipped || m.reads != reads {
		t.Fatal("地区没接时已跳过，不读设置")
	}
	if o := Language(off).Run(bg); o.Result != Skipped {
		t.Fatal("已跳过")
	}
}

// ---- 15、16、12、17、07 后半 ----

func writeHome(t *testing.T, files map[string]string) string {
	t.Helper()
	home := t.TempDir()
	for p, c := range files {
		full := filepath.Join(home, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(c), 0o600)
	}
	return home
}

func homeDeps(home string, on bool) HomeDeps {
	return HomeDeps{Home: func() (string, error) { return home, nil }, Target: func() (region.Target, bool) { return jp, on }}
}

func TestClientAndBrowserLang(t *testing.T) {
	// R10 的样本（脱敏）：顶层 locale，形如 xx-XX。
	want(t, "15 一致", res(t, ClaudeClientLang(homeDeps(writeHome(t, map[string]string{ClaudeDesktopConfig: `{"locale":"ja-JP","updaterLastSeenVersion":"0.0.0"}`}), true))), exitcheck.Pass)
	want(t, "15 不一致", res(t, ClaudeClientLang(homeDeps(writeHome(t, map[string]string{ClaudeDesktopConfig: `{"locale":"en-US"}`}), true))), exitcheck.Fail)
	want(t, "15 读不出", res(t, ClaudeClientLang(homeDeps(writeHome(t, map[string]string{ClaudeDesktopConfig: `{broken`}), true))), exitcheck.Unknown)
	if o := ClaudeClientLang(homeDeps(writeHome(t, nil), true)).Run(bg); o.Result != Skipped {
		t.Fatal("没装桌面版时已跳过")
	}
	chrome := browserPrefs[0].path
	want(t, "16 一致", res(t, BrowserLang(homeDeps(writeHome(t, map[string]string{chrome: `{"intl":{"accept_languages":"ja-JP,ja,en"}}`}), true))), exitcheck.Pass)
	want(t, "16 不一致", res(t, BrowserLang(homeDeps(writeHome(t, map[string]string{chrome: `{"intl":{"accept_languages":"zh-CN,zh"}}`}), true))), exitcheck.Fail)
	want(t, "16 读不出", res(t, BrowserLang(homeDeps(writeHome(t, map[string]string{chrome: `not json`}), true))), exitcheck.Unknown)
	if o := BrowserLang(homeDeps(writeHome(t, nil), true)).Run(bg); o.Result != Skipped {
		t.Fatal("没有浏览器时已跳过")
	}
}

func procs(ps ...ProcEnv) ProcEnvs {
	return func(context.Context) ([]ProcEnv, error) { return ps, nil }
}

// c. 12：Claude 进程带代理变量（大小写各一例）能检出，只报变量名；17：终端语言。
func TestProcEnvItems(t *testing.T) {
	want(t, "12 干净", res(t, ClaudeProcEnv(procs(ProcEnv{App: "Claude Code", Env: procinfo.EnvInfo{Lang: "ja_JP.UTF-8"}}))), exitcheck.Pass)
	for _, v := range []string{"HTTPS_PROXY", "all_proxy"} {
		o := res(t, ClaudeProcEnv(procs(ProcEnv{App: "Claude Code", Env: procinfo.EnvInfo{ProxyVars: []string{v}}})))
		want(t, "12 "+v, o, exitcheck.Fail)
		if !strings.Contains(o.Detail, v) {
			t.Fatal("应报变量名")
		}
	}
	want(t, "12 读不出", res(t, ClaudeProcEnv(func(context.Context) ([]ProcEnv, error) { return nil, errX })), exitcheck.Unknown)
	tgt := func() (region.Target, bool) { return jp, true }
	want(t, "17 一致", res(t, TerminalLang(procs(ProcEnv{App: "Claude Code", Env: procinfo.EnvInfo{Lang: "ja_JP.UTF-8"}}), tgt)), exitcheck.Pass)
	want(t, "17 LC_ALL 优先", res(t, TerminalLang(procs(ProcEnv{App: "Claude Code", Env: procinfo.EnvInfo{Lang: "ja_JP.UTF-8", LcAll: "zh_CN.UTF-8"}}), tgt)), exitcheck.Fail)
	want(t, "17 读不出", res(t, TerminalLang(func(context.Context) ([]ProcEnv, error) { return nil, errX }, tgt)), exitcheck.Unknown)
}

// c. 07 后半：解析器的回显地址在大陆能检出；查询失败无法判断。
func TestDNSEcho(t *testing.T) {
	fake := func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("198.18.0.9")}, nil
	}
	echo := func(a string, err error) EchoResolver {
		return func(context.Context) (netip.Addr, error) {
			if err != nil {
				return netip.Addr{}, err
			}
			return netip.MustParseAddr(a), nil
		}
	}
	want(t, "上游在海外", res(t, DNS(fake, echo("203.0.113.53", nil))), exitcheck.Pass)
	want(t, "上游在大陆", res(t, DNS(fake, echo("1.0.1.1", nil))), exitcheck.Fail)
	want(t, "查询失败", res(t, DNS(fake, echo("", errX))), exitcheck.Unknown)
}

// ---- e. 探测出口函数 ----

// envcheck 里只有 probe.go 可以直接拨号；它不设置任何绑定网卡或代理的选项。
func TestProbeDialOnly(t *testing.T) {
	srccheck.NoDial(t, ".", map[string][]string{"probe.go": {"net.Dialer"}})
	af := srccheck.Parse(t, "probe.go")
	ast.Inspect(af, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CompositeLit:
			if sel, ok := v.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Dialer" && len(v.Elts) > 0 {
				t.Error("net.Dialer 不能带任何选项")
			}
		case *ast.AssignStmt:
			for _, l := range v.Lhs {
				if sel, ok := l.(*ast.SelectorExpr); ok {
					switch sel.Sel.Name {
					case "LocalAddr", "Control", "ControlContext", "Resolver", "Proxy":
						t.Errorf("不能设置 %s", sel.Sel.Name)
					}
				}
			}
		case *ast.BasicLit:
			if strings.Contains(strings.ToLower(v.Value), "proxy") || strings.Contains(v.Value, "en0") {
				t.Errorf("不应出现 %s", v.Value)
			}
		}
		return true
	})
}

// e. 01 不通过时，探测出口一次都不调用（网络项都经注入的拨号函数发探测）。
func TestProbeNotCalledWhenTakeoverFails(t *testing.T) {
	var dials atomic.Int32
	dial := func(context.Context, string, string) (net.Conn, error) { dials.Add(1); return nil, errX }
	exit := &ExitProbe{Checker: &exitcheck.Checker{Opener: ProbeOpener{Dial: dial}, Total: time.Second}}
	items := []Item{
		Takeover(func(context.Context) ([]string, error) { return []string{"断网保护没有生效"}, nil }),
		ExitIP(exit), ExitRegion(exit), NetworkType(exit),
		STUN(STUNDeps{Servers: []string{"s:1"}, Dial: dial, Exit: exit}),
		DNS(func(ctx context.Context, _ string) ([]netip.Addr, error) { dials.Add(1); return nil, errX }),
	}
	(&Runner{Items: items}).Round(bg)
	if dials.Load() != 0 {
		t.Fatalf("01 不通过时探测出口被调用了 %d 次", dials.Load())
	}
}

// ---- d. 性质测试：只注入网络类故障时，没有任何硬性项变成不通过 ----

func TestNetworkFaultsNeverFailHard(t *testing.T) {
	netErrs := []error{
		&net.OpError{Op: "dial", Err: errors.New("connection refused")},
		&net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded},
		context.DeadlineExceeded, errors.New("EOF"), &net.DNSError{Err: "no such host", IsTimeout: true},
	}
	w := newTLSWorld(t)
	silent := fakeStun(t, "")
	for seed := int64(0); seed < 2000; seed++ {
		r := mrand.New(mrand.NewSource(seed))
		var rmu sync.Mutex // exitcheck 会并行开流
		intn := func(n int) int { rmu.Lock(); defer rmu.Unlock(); return r.Intn(n) }
		pick := func() error { return netErrs[intn(len(netErrs))] }
		failing := func(context.Context, string, string) (net.Conn, error) { return nil, pick() }
		exit := &ExitProbe{Checker: &exitcheck.Checker{Opener: ProbeOpener{Dial: failing}, Total: time.Second},
			Target: func() (region.Target, bool) { return jp, true }}
		stunDial := failing
		if intn(2) == 0 { // 或者连得上但没有应答
			stunDial = localDial(map[string]string{"s:1": silent})
		}
		var reachOpen func(context.Context, wire.Header) (net.Conn, error)
		switch intn(3) {
		case 0:
			reachOpen = func(context.Context, wire.Header) (net.Conn, error) { return nil, pick() }
		case 1:
			reachOpen = w.open(w.hang)
		default:
			reachOpen = func(ctx context.Context, h wire.Header) (net.Conn, error) { // 握手中途断开
				a, b := net.Pipe()
				go func() { buf := make([]byte, 10); b.Read(buf); b.Close() }()
				return a, nil
			}
		}
		items := []Item{
			ExitIP(exit), ExitRegion(exit),
			DNS(func(context.Context, string) ([]netip.Addr, error) { return nil, pick() },
				func(context.Context) (netip.Addr, error) { return netip.Addr{}, pick() }),
			STUN(STUNDeps{Servers: []string{"s:1"}, Dial: stunDial, Exit: exit, UDPUnsupported: func() bool { return intn(2) == 0 },
				Real: &RealIP{}, Timeout: 5 * time.Millisecond}),
			ClaudeReach(ReachDeps{Open: reachOpen, Exits: func() []string { return []string{"main"} }, RootCAs: w.ca, Timeout: 30 * time.Millisecond}),
		}
		for _, it := range items {
			if o := it.Run(bg); o.Result == exitcheck.Fail {
				t.Fatalf("种子 %d：%s 在只有网络故障时不通过：%s", seed, it.ID, o.Detail)
			}
		}
	}
}

// 2026-11-27 真机：链式主代理上目标是域名的 UDP 没有应答。设了 Resolve 时 08 先把服务器解析成真实地址、按 IP 拨；
// 解析失败的服务器跳过（全部跳过时无法判断）。
func TestSTUNResolvesToIP(t *testing.T) {
	real := &RealIP{Dial: func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", fakeStun(t, "198.51.100.77:9"))
	}}
	real.Measure(bg)
	exit := probeOf(rep(nil, "203.0.113.7", "US", ""), nil)
	exit.LocateFn = func(context.Context, netip.Addr) (string, error) { return "US", nil }
	srv := fakeStun(t, "203.0.113.9:1")
	dial := localDial(map[string]string{"192.0.2.5:3478": srv}) // 只认解析后的 IP
	resolve := func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "s1" {
			return []netip.Addr{netip.MustParseAddr("192.0.2.5")}, nil
		}
		return nil, errors.New("查不到")
	}
	o := STUN(STUNDeps{Servers: []string{"s1:3478"}, Dial: dial, Real: real, Exit: exit, Resolve: resolve, Timeout: 300 * time.Millisecond}).Run(bg)
	want(t, "解析成 IP 后拨", o, exitcheck.Pass)
	o = STUN(STUNDeps{Servers: []string{"s1:3478"}, Dial: dial, Real: real, Exit: exit, Timeout: 300 * time.Millisecond}).Run(bg)
	want(t, "不解析时按域名拨（测试里拨不通）", o, exitcheck.Unknown)
	o = STUN(STUNDeps{Servers: []string{"s2:3478"}, Dial: dial, Real: real, Exit: exit, Resolve: resolve, Timeout: 300 * time.Millisecond}).Run(bg)
	want(t, "解析失败跳过", o, exitcheck.Unknown)
}

// 全部没有应答时：请求确实经主代理发出（开流计数涨了）判通过；没发出去（计数没涨）仍是无法判断。
func TestSTUNSilentButProxied(t *testing.T) {
	exit := probeOf(rep(nil, "203.0.113.7", "US", ""), nil)
	var opens atomic.Int64
	silent := func(ctx context.Context, network, addr string) (net.Conn, error) {
		opens.Add(1) // 网络栈经主代理开了一条 UDP 流，但没有应答
		var d net.Dialer
		return d.DialContext(ctx, "udp", "127.0.0.1:9") // discard：不回
	}
	o := STUN(STUNDeps{Servers: []string{"s:1"}, Dial: silent, Exit: exit, ProxiedUDP: opens.Load, Timeout: 200 * time.Millisecond}).Run(bg)
	want(t, "经主代理发出、没有应答", o, exitcheck.Pass)
	var none atomic.Int64
	o = STUN(STUNDeps{Servers: []string{"s:1"}, Dial: silent, Exit: exit, ProxiedUDP: none.Load, Timeout: 200 * time.Millisecond}).Run(bg)
	want(t, "没有经主代理发出", o, exitcheck.Unknown)
}

// 2026-11-28 真机：测真实 IP 时第一个服务器没有应答（国内直连问谷歌），要接着试下一个，测到为止。
func TestRealIPTriesServersInOrder(t *testing.T) {
	silent := fakeStun(t, "")
	good := fakeStun(t, "198.51.100.77:5000")
	addrs := map[string]string{"a.example": silent, "b.example": good}
	var asked []string
	real := &RealIP{Servers: []string{"a.example:3478", "bad", "b.example:3478", "c.example:3478"},
		DialServer: func(ctx context.Context, host string, port uint16) (net.Conn, error) {
			asked = append(asked, host)
			if port != 3478 {
				t.Errorf("端口 %d", port)
			}
			var d net.Dialer
			return d.DialContext(ctx, "udp", addrs[host])
		}}
	real.Measure(bg)
	if eq, known := real.Equal(netip.MustParseAddr("198.51.100.77")); !eq || !known {
		t.Fatalf("应测到真实 IP：%v %v", eq, known)
	}
	if strings.Join(asked, ",") != "a.example,b.example" {
		t.Fatalf("试过的服务器：%v", asked)
	}
	if RealStunServers[0] != "stun.miwifi.com:3478" {
		t.Fatal("国内的服务器应排在前面")
	}
}

// 2026-11-28 真机：这一轮的出口结果没有位置时，14 按出口 IP 再查一次运营商；查不到时写清原因，不带地址。
func TestNetworkTypeOrgFallback(t *testing.T) {
	noGeo := exitcheck.Report{IP: netip.MustParseAddr("203.0.113.7"), Country: "US"}
	p := probeOf(noGeo, nil)
	p.OrgFn = func(context.Context, netip.Addr) (string, error) { return "Example Residential Broadband", nil }
	want(t, "补查到住宅运营商", NetworkType(p).Run(bg), exitcheck.Pass)
	p = probeOf(noGeo, nil)
	p.OrgFn = func(context.Context, netip.Addr) (string, error) { return "Example Cloud Hosting", nil }
	want(t, "补查到机房", NetworkType(p).Run(bg), exitcheck.Fail)
	p = probeOf(noGeo, nil)
	p.OrgFn = func(context.Context, netip.Addr) (string, error) {
		return "", errors.New(`Get "https://ipwho.is/203.0.113.7": context deadline exceeded`)
	}
	o := NetworkType(p).Run(bg)
	if o.Result != exitcheck.Unknown || strings.Contains(o.Detail, "203.0.113") || !strings.Contains(o.Detail, "：") {
		t.Fatalf("查不到：%+v", o)
	}
}

// 2026-11-28 真机：macOS 不让读 Chrome 的配置（Operation not permitted）时 16 是“已跳过”并说明，不是“无法判断”。
func TestBrowserLangProtected(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 读得到 000 权限的文件")
	}
	chrome := browserPrefs[0].path
	home := writeHome(t, map[string]string{chrome: `{"intl":{"accept_languages":"ja-JP"}}`})
	f := filepath.Join(home, chrome)
	if err := os.Chmod(f, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f, 0o600) })
	o := BrowserLang(homeDeps(home, true)).Run(bg)
	if o.Result != Skipped || !strings.Contains(o.Detail, "不允许读取 Chrome") || o.Fix == "" {
		t.Fatalf("%+v", o)
	}
}
