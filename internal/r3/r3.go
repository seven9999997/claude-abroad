// Package r3 是风险 R3（查连接表识别进程的准确率）的测量工具（m3-plan M3-3 验收 g）。
//
// 在本机回环上起一个 HTTP 服务，并发发起 N 条连接；服务端每接受一条连接就识别一次它属于哪个进程，
// 统计识别率、认错率、识别不到的比例和单次识别耗时。只连本机回环，不改宿主机网络。
//
// 两种模式：
//   - curl：并发启动 N 个 curl 进程。普通用户在连接表里只能看到本进程的套接字（macOS 27 实测），
//     所以这个模式要 root（M3-8 交给用户执行）；
//   - self：本进程起 N 条并发连接，普通用户也能跑，测并发下的识别和耗时。
package r3

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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"claude-abroad/internal/procinfo"
)

// Options 是一次测量的参数。
type Options struct {
	Mode     string // "curl" 或 "self"
	N        int
	CurlPath string // 默认 /usr/bin/curl
	Ident    *procinfo.Identifier
	Timeout  time.Duration // 整体时限，默认 60 秒
	Batch    int           // 每批并发数；0 表示 N 条一次全部并发
	// 目标：认错必须为 0；识别率不低于 MinRate（百分比）；MaxP99 为 0 时不看耗时。
	MinRate float64
	MaxP99  time.Duration
}

// Report 是测量结果。
type Report struct {
	Mode                  string
	N, Accepted           int
	Right, Wrong, Unknown int
	SpawnFailed           int
	P50, P99, Max         time.Duration
	SnapshotReads         int64
	DistinctSrc           int // 服务端看到的不重复来源地址数
	Duplicates            int // 内核把同一个四元组交给 accept 不止一次的次数（监听队列溢出时出现，不计入统计）
	AcceptErrors          int
	ClientOK              int // 客户端完整收到应答的条数
	WrongPaths            map[string]int
	Batch                 int
	MinRate               float64
	MaxP99                time.Duration
}

// Pass 按目标判定（architect 审 M3-3 时定）：认错必须为 0；识别率不低于 MinRate；MaxP99 非零时 99 分位不超过它。
func (r Report) Pass() bool {
	return r.Accepted > 0 && r.Wrong == 0 && pct(r.Right, r.N) >= r.MinRate && (r.MaxP99 == 0 || r.P99 <= r.MaxP99)
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "模式 %s：发起 %d，服务端接受 %d 条不同的连接（内核重复交付 %d 次，不计入；accept 出错 %d），客户端收到应答 %d，启动失败 %d\n",
		r.Mode, r.N, r.Accepted, r.Duplicates, r.AcceptErrors, r.ClientOK, r.SpawnFailed)
	fmt.Fprintf(&b, "识别正确 %d（%.1f%%），认错 %d（%.1f%%），识别不到 %d（%.1f%%）\n",
		r.Right, pct(r.Right, r.N), r.Wrong, pct(r.Wrong, r.N), r.Unknown, pct(r.Unknown, r.N))
	fmt.Fprintf(&b, "单次识别耗时：中位数 %v，99 分位 %v，最长 %v；读连接表 %d 次\n", r.P50, r.P99, r.Max, r.SnapshotReads)
	if len(r.WrongPaths) > 0 {
		fmt.Fprintf(&b, "认错成：%v\n", r.WrongPaths)
	}
	verdict := "未达标"
	if r.Pass() {
		verdict = "达标"
	}
	p99 := "不看"
	if r.MaxP99 > 0 {
		p99 = "≤" + r.MaxP99.String()
	}
	batch := "一次全部并发"
	if r.Batch > 0 {
		batch = fmt.Sprintf("每批并发 %d", r.Batch)
	}
	fmt.Fprintf(&b, "测法：%s。目标（认错 0、识别率 ≥%.0f%%、99 分位 %s）：%s\n", batch, r.MinRate, p99, verdict)
	return b.String()
}

// countingTable 统计读连接表的次数。
type countingTable struct {
	procinfo.Table
	n atomic.Int64
}

func (c *countingTable) Snapshot(network string) ([]procinfo.Entry, error) {
	c.n.Add(1)
	return c.Table.Snapshot(network)
}

// Percentile 取已排序耗时的 p 分位（0–100）。
func Percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p / 100 * float64(len(sorted)-1))
	return sorted[i]
}

func resolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// Run 跑一次测量。
func Run(ctx context.Context, o Options) (Report, error) {
	if o.N <= 0 || o.Ident == nil {
		return Report{}, errors.New("参数不对")
	}
	if o.Timeout <= 0 {
		o.Timeout = 60 * time.Second
	}
	if o.CurlPath == "" {
		o.CurlPath = "/usr/bin/curl"
	}
	var want string
	switch o.Mode {
	case "curl":
		want = resolve(o.CurlPath)
	case "self":
		exe, err := os.Executable()
		if err != nil {
			return Report{}, err
		}
		want = resolve(exe)
	default:
		return Report{}, fmt.Errorf("不认识的模式 %q", o.Mode)
	}
	ct := &countingTable{Table: o.Ident.Table}
	id := &procinfo.Identifier{ // 同样的配置，换上计数的连接表
		Table: ct, Procs: o.Ident.Procs, Bundle: o.Ident.Bundle, InheritParent: o.Ident.InheritParent,
		Retries: o.Ident.Retries, RetryDelay: o.Ident.RetryDelay, ShareFor: o.Ident.ShareFor,
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Report{}, err
	}
	defer ln.Close()
	lnAddr := netip.MustParseAddrPort(ln.Addr().String())

	rep := Report{Mode: o.Mode, N: o.N, WrongPaths: map[string]int{}, Batch: o.Batch, MinRate: o.MinRate, MaxP99: o.MaxP99}
	var mu sync.Mutex
	var times []time.Duration
	var served sync.WaitGroup
	srcs := map[netip.AddrPort]bool{}
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			c, err := ln.Accept()
			if err != nil {
				if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
					return
				}
				mu.Lock()
				rep.AcceptErrors++
				mu.Unlock()
				time.Sleep(time.Millisecond)
				continue
			}
			served.Add(1)
			go func() {
				defer served.Done()
				defer c.Close()
				src := netip.MustParseAddrPort(c.RemoteAddr().String())
				start := time.Now()
				r := id.Identify(ctx, "tcp", src, lnAddr)
				d := time.Since(start)
				mu.Lock()
				if srcs[src] {
					// 本机内核在监听队列溢出（并发数远超 somaxconn）时，偶尔会把同一个四元组交给 accept 两次
					// （2026-11-19 实测：客户端只拨了一次）。只统计第一次，重复的单独计数。
					rep.Duplicates++
					mu.Unlock()
					_, _ = io.WriteString(c, "HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n")
					return
				}
				rep.Accepted++
				srcs[src] = true
				times = append(times, d)
				switch {
				case r.Path == "":
					rep.Unknown++
				case resolve(r.Path) == want:
					rep.Right++
				default:
					rep.Wrong++
					rep.WrongPaths[filepath.Base(r.Path)]++
				}
				mu.Unlock()
				_, _ = io.WriteString(c, "HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n")
			}()
		}
	}()

	url := "http://" + ln.Addr().String() + "/"
	var clients sync.WaitGroup
	var spawnFailed, clientOK atomic.Int64
	batch := o.Batch
	if batch <= 0 || batch > o.N {
		batch = o.N
	}
	for done := 0; done < o.N; done += batch {
		gate := make(chan struct{})
		for i := done; i < min(done+batch, o.N); i++ {
			clients.Add(1)
			go func() {
				defer clients.Done()
				<-gate
				switch o.Mode {
				case "curl":
					cmd := exec.CommandContext(ctx, o.CurlPath, "-s", "-o", "/dev/null", "--noproxy", "*", "--max-time", "20", url)
					err := cmd.Run()
					switch {
					case err == nil:
						clientOK.Add(1)
					case cmd.ProcessState == nil:
						spawnFailed.Add(1)
					}
				case "self":
					c, err := net.DialTimeout("tcp", ln.Addr().String(), 20*time.Second)
					if err != nil {
						spawnFailed.Add(1)
						return
					}
					_ = c.SetDeadline(time.Now().Add(20 * time.Second))
					_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: r3\r\n\r\n")
					b, _ := io.ReadAll(c)
					c.Close()
					if strings.HasPrefix(string(b), "HTTP/1.1 204") {
						clientOK.Add(1)
					}
				}
			}()
		}
		close(gate)
		clients.Wait() // 一批全部结束再起下一批
	}
	ln.Close()
	<-acceptDone // accept 循环退出后再等处理完，避免 served.Add 与 Wait 竞争
	served.Wait()
	if ctx.Err() != nil {
		return rep, errors.New("超过整体时限")
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	rep.P50, rep.P99 = Percentile(times, 50), Percentile(times, 99)
	if len(times) > 0 {
		rep.Max = times[len(times)-1]
	}
	rep.SpawnFailed = int(spawnFailed.Load())
	rep.SnapshotReads = ct.n.Load()
	rep.DistinctSrc = len(srcs)
	rep.ClientOK = int(clientOK.Load())
	return rep, nil
}
