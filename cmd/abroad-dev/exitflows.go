package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"claude-abroad/internal/wire"
)

// exit-flows：M4-7 的虚拟机端到端用。经 data.sock 对一个出口并发开 N 条流，每条发一段不同的数据，
// 核对应答是 “<期望标记>:<原样数据>”（开发用对端的回显带标记，标记说明流量从哪条隧道到达）。
//
//	abroad-dev exit-flows --data-sock <路径> --exit wg --net tcp|udp --target 10.77.0.1:7 --want-tag wg [--n 50]
//
// --hold 3s：核对完回显后，每条流再每 100 毫秒发一段数据，持续这么久（发送失败就停，不算不符合）；
// 用来在长连接还在发数据时断开隧道，看旧连接的包会不会外泄（M4-7 v2.9 验收 o）。
//
// 向标准输出打一行 JSON：{"n":N,"good":N,"codes":{"exit_down":3},"bad":["…"]}。
// 退出码：0 全部符合；1 有不符合的或连不上；2 参数错。

// FlowsResult 是 exit-flows 的报告。
type FlowsResult struct {
	N     int            `json:"n"`
	Good  int            `json:"good"`
	Codes map[string]int `json:"codes"`
	Bad   []string       `json:"bad,omitempty"`
}

func exitFlows(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("exit-flows", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sock := fs.String("data-sock", "", "abroad-vm 的 data.sock")
	exit := fs.String("exit", "", "出口标签")
	netw := fs.String("net", "tcp", "tcp 或 udp")
	target := fs.String("target", "", "目标 host:port")
	want := fs.String("want-tag", "", "回显应带的标记（空表示只看回应帧）")
	n := fs.Int("n", 1, "并发流数（1–200）")
	hold := fs.Duration("hold", 0, "核对完后继续发数据的时长（0 表示不继续）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	h, _, err := buildProbe(*sock, *exit, *netw, *target, "", "abroad-dev-flows")
	if err != nil || *n < 1 || *n > 200 {
		fmt.Fprintln(stderr, "abroad-dev exit-flows: 参数不对：", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second+*hold)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", *sock)
	if err != nil {
		fmt.Fprintln(stderr, "abroad-dev exit-flows: 连 data.sock：", err)
		return 1
	}
	defer c.Close()
	cfg := wire.DefaultSessionConfig()
	cfg.KeepAliveInterval = -1
	sess, err := wire.Client(c, cfg)
	if err != nil {
		fmt.Fprintln(stderr, "abroad-dev exit-flows: 建会话：", err)
		return 1
	}
	defer sess.Close()
	res := runFlows(func() (io.ReadWriteCloser, error) { return sess.Open() }, h, *want, *n, *hold)
	b, _ := json.Marshal(res)
	fmt.Fprintln(stdout, string(b))
	if res.Good != res.N {
		return 1
	}
	return 0
}

// runFlows 并发开 n 条流（open 由调用方给，测试注入假的）。
func runFlows(open func() (io.ReadWriteCloser, error), h wire.Header, want string, n int, hold time.Duration) FlowsResult {
	res := FlowsResult{N: n, Codes: map[string]int{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, bad := oneFlow(open, h, want, i, hold)
			mu.Lock()
			defer mu.Unlock()
			if code != "" {
				res.Codes[code]++
			}
			if bad == "" && code == "" {
				res.Good++
			} else if bad != "" && len(res.Bad) < 10 {
				res.Bad = append(res.Bad, bad)
			}
		}(i)
	}
	wg.Wait()
	return res
}

type deadliner interface{ SetDeadline(time.Time) error }

// oneFlow 返回 (回应帧的失败码, 不符合的说明)；两者都空表示这条流符合。
func oneFlow(open func() (io.ReadWriteCloser, error), h wire.Header, want string, i int, hold time.Duration) (string, string) {
	st, err := open()
	if err != nil {
		return "", "开流：" + err.Error()
	}
	defer st.Close()
	if dl, ok := st.(deadliner); ok {
		_ = dl.SetDeadline(time.Now().Add(20*time.Second + hold))
	}
	if err := wire.WriteHeader(st, h); err != nil {
		return "", "写帧头：" + err.Error()
	}
	rep, err := wire.ReadReply(st)
	if err != nil {
		return "", "读回应帧：" + err.Error()
	}
	if !rep.OK {
		return string(rep.Code), ""
	}
	payload := []byte(fmt.Sprintf("flow-%03d", i))
	expect := append([]byte(want+":"), payload...)
	var got []byte
	if h.Net == wire.UDP {
		dc := wire.NewDatagramConn(st)
		if err := dc.WriteDatagram(payload); err != nil {
			return "", "发数据报：" + err.Error()
		}
		buf := make([]byte, wire.MaxDatagram)
		n, err := dc.ReadDatagram(buf)
		if err != nil {
			return "", "收数据报：" + err.Error()
		}
		got = buf[:n]
	} else {
		if _, err := st.Write(payload); err != nil {
			return "", "发数据：" + err.Error()
		}
		got = make([]byte, len(expect))
		if _, err := io.ReadFull(st, got); err != nil {
			return "", "收数据：" + err.Error()
		}
	}
	if want != "" && !bytes.Equal(got, expect) {
		return "", fmt.Sprintf("回显不对：%q", got)
	}
	keepSending(st, h, payload, hold)
	return "", ""
}

// keepSending 每 100 毫秒发一段数据，直到 hold 用完或发送失败。TCP 的回显不读（对端回显堆在缓冲里无妨），
// UDP 按数据报发。
func keepSending(st io.ReadWriteCloser, h wire.Header, payload []byte, hold time.Duration) {
	if hold <= 0 {
		return
	}
	if h.Net != wire.UDP { // 读掉回显，免得缓冲满了卡住写
		go func() { _, _ = io.Copy(io.Discard, st) }()
	}
	dc := wire.NewDatagramConn(st)
	end := time.Now().Add(hold)
	for time.Now().Before(end) {
		var err error
		if h.Net == wire.UDP {
			err = dc.WriteDatagram(payload)
		} else {
			_, err = st.Write(payload)
		}
		if err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
