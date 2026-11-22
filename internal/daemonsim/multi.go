package daemonsim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"claude-abroad/internal/daemon"
	"claude-abroad/internal/directdial"
	"claude-abroad/internal/redact"
	"claude-abroad/internal/rules"
)

// MultiConfig 是多出口多应用的阶段（m3-plan M3-7 验收 g）：三个应用（假识别器，按源端口段）经真实虚拟机的
// 出口 A、出口 B（开发上游，/whoami 回各自的名字）和真实的直连拨号模块。
type MultiConfig struct {
	Config
	DirectSock string // abroad-vm 的 direct.sock
	// DirectTarget 是直连应用连的地址（默认 1.1.1.1:443）。连上后先发几个非 TLS 的字节：裸 IP 加 443 而客户端不先发数据时，
	// daemon 会按“看不清去向”改走主代理（M3 定下的行为）；本机不能经物理网卡到公网时用局域网目标（端口不是 443）。
	DirectTarget netip.AddrPort
	PinnedName   string // 置顶域名（默认 claude.ai）
}

// Check 是一项检查的结果（不含地址、域名）。
type Check struct {
	Name   string `json:"name"`
	Want   string `json:"want"`
	Got    string `json:"got"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// MultiReport 是多出口多应用阶段的报告。
type MultiReport struct {
	OK      bool    `json:"ok"`
	Checks  []Check `json:"checks"`
	LogLeak bool    `json:"log_leak"`
	Err     string  `json:"err,omitempty"`
}

// 应用按源端口段区分（与网络栈、daemon 的测试约定一致）。
const (
	portA = 20000 // com.app.a
	portB = 21000 // com.app.b
	portC = 22000 // com.app.c
)

type portIdent struct{}

func (portIdent) Identify(_ context.Context, _ string, src, _ netip.AddrPort) (rules.AppID, bool) {
	switch p := src.Port(); {
	case p >= portA && p < portA+1000:
		return "com.app.a", false
	case p >= portB && p < portB+1000:
		return "com.app.b", false
	case p >= portC && p < portC+1000:
		return "com.app.c", false
	}
	return "com.app.other", false
}

// MultiRules 是阶段一的规则：A 走出口 wg-a，B 走出口 clash-b，C 直连。
const MultiRules = `
rules:
  - {app: com.app.a, action: "proxy:wg-a"}
  - {app: com.app.b, action: "proxy:clash-b"}
  - {app: com.app.c, action: direct}
`

// RunMulti 跑多出口多应用的阶段：启用 → 写规则 → 三个应用各访问一次 → 把 A 改成直连后访问置顶域名（应经 main）
// → 恢复。直连拨号模块的拨号计数从 abroad-vm 的 host.sock 读。
func RunMulti(ctx context.Context, cfg MultiConfig) (rep MultiReport) {
	cfg.Config.DevExits = []string{"wg-a", "clash-b"} // 阶段一的出口是开发接口建的（vm-up 用 /v1/dev-exits）
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if !cfg.DirectTarget.IsValid() {
		cfg.DirectTarget = netip.MustParseAddrPort("1.1.1.1:443")
	}
	if cfg.PinnedName == "" {
		cfg.PinnedName = "claude.ai"
	}
	fail := func(err error) MultiReport { rep.Err = redact.String(err.Error()); return rep }
	add := func(c Check) { c.OK = c.Got == c.Want; rep.Checks = append(rep.Checks, c) }
	hs, err := newHarness(ctx, cfg.Config, func(dp *daemon.DataPlane) {
		dp.SetIdent(portIdent{})
		dp.SetDirect(&directdial.Client{Path: cfg.DirectSock, ServerUID: uint32(os.Getuid())})
	})
	if err != nil {
		return fail(err)
	}
	defer hs.close()

	code, _ := hs.op("/v1/enable")
	add(Check{Name: "启用", Want: "200", Got: fmt.Sprint(code)})
	if code != 200 {
		return finish(rep, hs)
	}
	add(Check{Name: "写规则", Want: "200", Got: fmt.Sprint(putRules(hs, MultiRules))})
	cl, err := hs.client()
	if err != nil {
		return fail(err)
	}
	dns := netip.MustParseAddrPort("198.18.0.2:53")
	lookup := func(name string) (netip.Addr, error) {
		c, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
		return cl.LookupA(c, name, dns)
	}
	whoami := func(port uint16, name string) string {
		a, err := lookup(name)
		if err != nil {
			return "dns_failed"
		}
		c, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
		conn, err := cl.DialTCPFrom(c, port, netip.AddrPortFrom(a, 80))
		if err != nil {
			return "dial_failed"
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(cfg.Timeout))
		_, _ = io.WriteString(conn, "GET /whoami HTTP/1.1\r\nHost: "+name+"\r\nConnection: close\r\n\r\n")
		b, _ := io.ReadAll(conn)
		if i := bytes.Index(b, []byte("\r\n\r\n")); i >= 0 {
			return string(b[i+4:])
		}
		return "no_reply"
	}
	add(Check{Name: "应用 A 经出口 wg-a", Want: "wg-a", Got: whoami(portA+1, "whoami.test")})
	add(Check{Name: "应用 B 经出口 clash-b", Want: "clash-b", Got: whoami(portB+1, "whoami.test")})
	// 应用 C 直连：经真实的直连拨号模块连 DirectTarget（只看连接是否建成、直连拨号计数是否增长）。
	// 每轮用不同的源端口：上一轮关掉的连接还在 TIME_WAIT，同一个源端口绑不上。
	directRound := func(round string, src uint16) {
		d0 := directOK(cfg.VMDir)
		c, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
		conn, err := cl.DialTCPFrom(c, src, cfg.DirectTarget)
		got := "connected"
		if err != nil {
			got = "failed"
		} else {
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			_, _ = conn.Write([]byte("abroad-9c\r\n")) // 先发非 TLS 的字节，daemon 才看得清去向
			// 连接被接受之后才发起直连；等直连拨号模块的计数增长。
			for i := 0; i < 50 && directOK(cfg.VMDir) == d0; i++ {
				time.Sleep(100 * time.Millisecond)
			}
			if directOK(cfg.VMDir) == d0 {
				got = "no_direct_dial"
			}
			conn.Close()
		}
		add(Check{Name: "应用 C 经直连拨号模块" + round, Want: "connected", Got: got})
	}
	directRound("", portC+1)
	// 把 A 改成直连后访问置顶域名：经 main 到达（main 的开发上游回 main），直连拨号计数不变。
	aDirect := strings.Replace(MultiRules, `"proxy:wg-a"`, "direct", 1)
	add(Check{Name: "把应用 A 改成直连", Want: "200", Got: fmt.Sprint(putRules(hs, aDirect))})
	d1 := directOK(cfg.VMDir)
	add(Check{Name: "设成直连的应用 A 访问置顶域名经 main", Want: "main", Got: whoami(portA+2, cfg.PinnedName)})
	add(Check{Name: "直连拨号计数不变", Want: fmt.Sprint(d1), Got: fmt.Sprint(directOK(cfg.VMDir))})
	// 改规则之后再走一轮：B 仍经 clash-b，C 仍经直连拨号模块。
	add(Check{Name: "改规则后 应用 B 经出口 clash-b", Want: "clash-b", Got: whoami(portB+2, "whoami.test")})
	directRound("（改规则后）", portC+2)
	code, st := hs.op("/v1/restore")
	add(Check{Name: "恢复", Want: "200 RESTORED", Got: fmt.Sprint(code, " ", st.State)})
	return finish(rep, hs)
}

func finish(rep MultiReport, hs *harness) MultiReport {
	log := hs.lb.String()
	for _, s := range []string{"whoami.test", "com.app", "claude.ai", "1.1.1.1"} {
		if strings.Contains(log, s) {
			rep.LogLeak = true
		}
	}
	rep.OK = rep.Err == "" && !rep.LogLeak && len(rep.Checks) > 0
	for _, c := range rep.Checks {
		rep.OK = rep.OK && c.OK
	}
	return rep
}

// putRules 经本地接口 PUT 规则，返回状态码（忙时重试）。
func putRules(hs *harness, y string) int {
	hc := unixHTTP(hs.d.SocketPath())
	for i := 0; i < 100; i++ {
		req, _ := http.NewRequest(http.MethodPut, "http://abroadd/v1/rules?drop_affected=1", strings.NewReader(y))
		resp, err := hc.Do(req)
		if err != nil {
			return 0
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			return resp.StatusCode
		}
		time.Sleep(100 * time.Millisecond)
	}
	return http.StatusConflict
}

// directOK 读 abroad-vm 的 host.sock 里直连拨号模块的成功计数（读不到时 -1）。
func directOK(vmDir string) int64 {
	resp, err := unixHTTP(vmDir + "/host.sock").Get("http://abroad-vm/v1/netstats")
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	var st struct {
		Direct map[string]int64 `json:"direct"`
	}
	if json.NewDecoder(resp.Body).Decode(&st) != nil {
		return -1
	}
	return st.Direct["ok"]
}
