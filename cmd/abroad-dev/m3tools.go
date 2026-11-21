package main

// M3-8 真机脚本用到的工具（m3-plan v2.10）：
//   - ident-sample：读一份连接表快照，逐条按进程号映射应用 ID，统计各类计数（系统进程的识别抽查）；
//   - direct-probe 的批量模式（--count、--concurrency）：经直连拨号模块拨 N 次，核对收到的 fd；
//   - upstream 的 --pid-file 在 main.go 里。
// 输出都不含地址、路径（path: 应用只计数，不列出：路径里可能有用户名）。

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"claude-abroad/internal/directdial"
	"claude-abroad/internal/procinfo"
	"claude-abroad/internal/rules"
)

// IdentSample 是 ident-sample 的报告。
type IdentSample struct {
	Entries    int            `json:"entries"`    // 连接表里的条目（tcp + udp）
	WithPID    int            `json:"with_pid"`   // 有归属进程号的
	Identified int            `json:"identified"` // 映射出应用 ID 的（不是 unknown）
	Kinds      map[string]int `json:"kinds"`      // system、path、bundle、unknown
	SystemIDs  map[string]int `json:"system_ids"` // system: 开头的应用 ID → 条目数
	BundleIDs  int            `json:"bundle_ids"` // 不同的 Bundle ID 个数（不列出）
	RatePct    float64        `json:"rate_pct"`   // Identified / WithPID
	Errors     []string       `json:"errors,omitempty"`
	ReadOnly   bool           `json:"read_only"`   // 恒为 true：只读 sysctl 和进程信息
	RootNeeded bool           `json:"root_needed"` // 不是 root 时为 true（只能看到本进程的套接字）
}

// identSampleRun 对一份连接表做识别统计（纯函数，便于测试）。
func identSampleRun(table procinfo.Table, procs procinfo.Procs, bundle procinfo.BundleReader) IdentSample {
	rep := IdentSample{Kinds: map[string]int{}, SystemIDs: map[string]int{}, ReadOnly: true}
	bundles := map[rules.AppID]bool{}
	for _, network := range []string{"tcp", "udp"} {
		entries, err := table.Snapshot(network)
		if err != nil {
			rep.Errors = append(rep.Errors, network+"："+err.Error())
			continue
		}
		cache := map[int]rules.AppID{}
		for _, e := range entries {
			rep.Entries++
			if e.PID <= 0 {
				continue
			}
			rep.WithPID++
			app, ok := cache[e.PID]
			if !ok {
				app = rules.Unknown
				if p, err := procs.Info(e.PID); err == nil && p.Path != "" {
					app = procinfo.AppIDForPath(p.Path, bundle)
				}
				cache[e.PID] = app
			}
			switch s := string(app); {
			case app == rules.Unknown:
				rep.Kinds["unknown"]++
			case strings.HasPrefix(s, "system:"):
				rep.Kinds["system"]++
				rep.SystemIDs[s]++
			case strings.HasPrefix(s, "path:"):
				rep.Kinds["path"]++
			default:
				rep.Kinds["bundle"]++
				bundles[app] = true
			}
			if app != rules.Unknown {
				rep.Identified++
			}
		}
	}
	rep.BundleIDs = len(bundles)
	if rep.WithPID > 0 {
		rep.RatePct = float64(rep.Identified) * 100 / float64(rep.WithPID)
	}
	return rep
}

// identSample：ident-sample 子命令。退出码：0 完成；2 参数错。
func identSample(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ident-sample", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "用法：sudo abroad-dev ident-sample")
		return 2
	}
	rep := identSampleRun(procinfo.SysTable{}, procinfo.SysProcs{}, (&procinfo.FileBundles{}).Read)
	rep.RootNeeded = os.Geteuid() != 0
	b, _ := json.Marshal(rep)
	fmt.Fprintln(stdout, string(b))
	return 0
}

// BatchReport 是 direct-probe 批量模式的报告（不含地址）。
type BatchReport struct {
	Count          int            `json:"count"`
	OK             int            `json:"ok"`               // 拨号成功、收到的 fd 核对无误、TLS 握手成功
	DialFail       int            `json:"dial_fail"`        // 直连拨号模块回错误，或连不上它
	HandshakeFail  int            `json:"handshake_fail"`   // 拿到了连接，但 TLS 握手失败（含读到 EOF：在途 fd 被冲掉的表现）
	Codes          map[string]int `json:"codes"`            // 拨号失败的应答码（other：连接、身份等其他错误）
	Interfaces     map[string]int `json:"interfaces"`       // 收到的连接的本端在哪块网卡上
	RemoteMismatch int            `json:"remote_mismatch"`  // 收到的 fd 的对端不是请求的目标（计入 dial_fail）
	NotTCP         int            `json:"not_tcp"`          // 收到的不是 TCP 连接（计入 dial_fail）
	MaxInflight    int            `json:"max_inflight"`     // 实际的最大并发
	P99Ms          int64          `json:"p99_ms"`           // 拨号加握手的 99 分位
	Errors         map[string]int `json:"errors,omitempty"` // other、握手失败的错误种类（不含地址）
}

// MaxBatchConcurrency 是批量模式的并发上限：避免对公网目标造成突发（m3-plan v2.11）。
const MaxBatchConcurrency = 10

// batchDialer 是批量模式要的直连客户端（directdial.Client 满足；测试注入）。
type batchDialer interface {
	Dial(ctx context.Context, network, host string, ip netip.Addr, port uint16) (net.Conn, error)
}

// runBatch 拨 count 次（TCP，按 IP，并发不超过 conc），逐个核对收到的连接，再做一次完整的 TLS 握手
// （tlsConf 给出服务器名和信任的根证书；证书照常校验），然后关闭。握手成功才算这一次成功：
// 在途 fd 被冲掉的表现是连接建立了、读的时候拿到 EOF，只看对端地址验证不到（m3-plan v2.11）。
//
// wantRemote 是收到的 fd 应有的对端地址：生产里就是 target；测试里直连拨号模块被接到本机的假服务器上。
func runBatch(ctx context.Context, d batchDialer, target, wantRemote netip.AddrPort, count, conc int, timeout time.Duration,
	tlsConf *tls.Config, ifaceOf func(net.Addr) string) BatchReport {
	rep := BatchReport{Count: count, Codes: map[string]int{}, Interfaces: map[string]int{}, Errors: map[string]int{}}
	conc = max(1, min(conc, MaxBatchConcurrency))
	var mu sync.Mutex
	var lat []time.Duration
	inflight := 0
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	one := func() {
		c, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		start := time.Now()
		var kind, code, ekind, iface string
		conn, err := d.Dial(c, "tcp", "", target.Addr(), target.Port())
		switch {
		case err != nil:
			kind = "dial"
			var ce *directdial.CodeError
			if errors.As(err, &ce) {
				code = ce.Code
			} else {
				code, ekind = "other", errKind(err)
			}
		default:
			defer conn.Close()
			tc, ok := conn.(*net.TCPConn)
			if !ok {
				kind, code = "dial", "not_tcp"
				break
			}
			ra, perr := netip.ParseAddrPort(tc.RemoteAddr().String())
			if perr != nil || ra.Addr().Unmap() != wantRemote.Addr() || ra.Port() != wantRemote.Port() {
				kind, code = "dial", "remote_mismatch"
				break
			}
			if ifaceOf != nil {
				iface = ifaceOf(tc.LocalAddr())
			}
			_ = tc.SetDeadline(time.Now().Add(timeout))
			tl := tls.Client(tc, tlsConf.Clone())
			if herr := tl.HandshakeContext(c); herr != nil {
				kind, ekind = "handshake", "handshake_"+errKind(herr)
				break
			}
			kind = "ok"
		}
		el := time.Since(start)
		mu.Lock()
		defer mu.Unlock()
		lat = append(lat, el)
		if ekind != "" {
			rep.Errors[ekind]++
		}
		switch kind {
		case "ok":
			rep.OK++
			rep.Interfaces[iface]++
		case "handshake":
			rep.HandshakeFail++
			rep.Interfaces[iface]++
		default:
			rep.DialFail++
			switch code {
			case "not_tcp":
				rep.NotTCP++
			case "remote_mismatch":
				rep.RemoteMismatch++
			default:
				rep.Codes[code]++
			}
		}
	}
	for i := 0; i < count && ctx.Err() == nil; i++ {
		wg.Add(1)
		sem <- struct{}{}
		mu.Lock()
		inflight++
		rep.MaxInflight = max(rep.MaxInflight, inflight)
		mu.Unlock()
		go func() {
			defer wg.Done()
			defer func() { mu.Lock(); inflight--; mu.Unlock(); <-sem }()
			one()
		}()
	}
	wg.Wait()
	if len(lat) > 0 {
		slices.Sort(lat)
		rep.P99Ms = lat[(len(lat)*99+99)/100-1].Milliseconds()
	}
	return rep
}

// errKind 把错误归成不含地址的几类。
func errKind(err error) string {
	s := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, directdial.ErrWrongServer):
		return "wrong_server"
	case strings.Contains(s, "连不上直连拨号模块"):
		return "no_server"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), strings.Contains(s, "EOF"):
		return "eof"
	case strings.Contains(s, "certificate"):
		return "cert"
	case strings.Contains(s, "connection reset"):
		return "reset"
	}
	return "other"
}

// ownConfig 是 --own-config 用的配置来源：很大的代数、不带 DNS 和置顶追加名单（只按 IP 拨号）。
// 直连拨号模块回 stale_config 时客户端下发它；之后 abroadd 发起的直连会收到 stale_config 并补发自己的配置
// （m3-plan v2.9 的自愈路径）。
func ownConfig() directdial.Config { return directdial.Config{Gen: 1 << 40} }
