package wire

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func validHeader() Header {
	return Header{V: Version, Exit: "main", Net: TCP, Host: "example.com", Port: 443, App: "com.google.Chrome"}
}

func rawFrame(body string) []byte {
	b := make([]byte, 2+len(body))
	binary.BigEndian.PutUint16(b, uint16(len(body)))
	copy(b[2:], body)
	return b
}

func TestHeaderRoundTrip(t *testing.T) {
	for _, h := range []Header{
		validHeader(),
		{V: 1, Exit: "wg-us", Net: UDP, IP: "1.2.3.4", Port: 53},
		{V: 1, Exit: "clash_jp", Net: TCP, Host: "a.b", IP: "2001:db8::1", Port: 65535},
	} {
		var buf bytes.Buffer
		if err := WriteHeader(&buf, h); err != nil {
			t.Fatalf("%+v: %v", h, err)
		}
		got, err := ReadHeader(&buf)
		if err != nil || got != h {
			t.Fatalf("往返不一致: %+v → %+v, %v", h, got, err)
		}
		if buf.Len() != 0 {
			t.Fatal("读完帧头后不应有剩余数据")
		}
	}
}

func TestHeaderValidation(t *testing.T) {
	mod := func(f func(*Header)) Header { h := validHeader(); f(&h); return h }
	cases := map[string]Header{
		"版本":         mod(func(h *Header) { h.V = 2 }),
		"出口为空":       mod(func(h *Header) { h.Exit = "" }),
		"出口含空格":      mod(func(h *Header) { h.Exit = "a b" }),
		"出口过长":       mod(func(h *Header) { h.Exit = strings.Repeat("a", 65) }),
		"net":        mod(func(h *Header) { h.Net = "icmp" }),
		"端口 0":       mod(func(h *Header) { h.Port = 0 }),
		"端口越界":       mod(func(h *Header) { h.Port = 65536 }),
		"端口为负":       mod(func(h *Header) { h.Port = -1 }),
		"无目标":        mod(func(h *Header) { h.Host = ""; h.IP = "" }),
		"域名非法":       mod(func(h *Header) { h.Host = "a b.com" }),
		"域名连续点":      mod(func(h *Header) { h.Host = "a..com" }),
		"域名以点开头":     mod(func(h *Header) { h.Host = ".a.com" }),
		"域名过长":       mod(func(h *Header) { h.Host = strings.Repeat("a", 254) }),
		"IP 非法":      mod(func(h *Header) { h.IP = "1.2.3" }),
		"IP 带区域":     mod(func(h *Header) { h.IP = "fe80::1%en0" }),
		"域名是 IPv4":   mod(func(h *Header) { h.Host = "127.0.0.1" }),
		"域名是十进制 IP":  mod(func(h *Header) { h.Host = "2130706433" }),
		"域名末段全数字":    mod(func(h *Header) { h.Host = "foo.123" }),
		"域名段超 63 字节": mod(func(h *Header) { h.Host = strings.Repeat("a", 64) + ".com" }),
		"应用含换行":      mod(func(h *Header) { h.App = "a\nb" }),
		"应用含 DEL":    mod(func(h *Header) { h.App = "a\x7fb" }),
		"应用过长":       mod(func(h *Header) { h.App = strings.Repeat("x", maxAppLen+1) }),
	}
	for name, h := range cases {
		if err := h.Validate(); !errors.Is(err, ErrBadFrame) {
			t.Errorf("%s: 应拒绝，得到 %v", name, err)
		}
		if err := WriteHeader(io.Discard, h); err == nil {
			t.Errorf("%s: WriteHeader 不应写出非法帧头", name)
		}
	}
}

func TestReadHeaderRejects(t *testing.T) {
	cases := map[string][]byte{
		"未知版本":   rawFrame(`{"v":2,"exit":"main","net":"tcp","host":"a.com","port":1,"future":true}`),
		"缺版本":    rawFrame(`{"exit":"main","net":"tcp","host":"a.com","port":1}`),
		"未知字段":   rawFrame(`{"v":1,"exit":"main","net":"tcp","host":"a.com","port":1,"extra":1}`),
		"非 JSON": rawFrame(`hello`),
		"尾部多余":   rawFrame(`{"v":1,"exit":"main","net":"tcp","host":"a.com","port":1}{}`),
		"类型错":    rawFrame(`{"v":1,"exit":"main","net":"tcp","host":"a.com","port":"443"}`),
		"长度为 0":  {0, 0},
		"长度超上限":  func() []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, MaxFrame+1); return b }(),
		"字段值非法":  rawFrame(`{"v":1,"exit":"main","net":"tcp","host":"a.com","port":0}`),
	}
	for name, b := range cases {
		_, err := ReadHeader(bytes.NewReader(b))
		if !errors.Is(err, ErrBadFrame) {
			t.Errorf("%s: 应返回 ErrBadFrame，得到 %v", name, err)
		}
	}
	// 未知版本报的是版本，而不是未知字段。
	_, err := ReadHeader(bytes.NewReader(cases["未知版本"]))
	if err == nil || !strings.Contains(err.Error(), "版本") {
		t.Errorf("未知版本应报版本错误，得到 %v", err)
	}
}

func TestReadHeaderTruncated(t *testing.T) {
	full := rawFrame(`{"v":1,"exit":"main","net":"tcp","host":"a.com","port":1}`)
	if _, err := ReadHeader(bytes.NewReader(nil)); err != io.EOF {
		t.Errorf("空输入应为 io.EOF，得到 %v", err)
	}
	for _, n := range []int{1, 2, 10, len(full) - 1} {
		if _, err := ReadHeader(bytes.NewReader(full[:n])); err != io.ErrUnexpectedEOF {
			t.Errorf("截断到 %d 字节应为 io.ErrUnexpectedEOF，得到 %v", n, err)
		}
	}
}

func TestHeaderTooLarge(t *testing.T) {
	// 每个字段都合法，但整体超过 4 KiB：用很长的应用 ID 拼不出来（上限 512），
	// 所以直接验证 writeJSON 的上限检查。
	big := map[string]string{"x": strings.Repeat("a", MaxFrame)}
	if err := writeJSON(io.Discard, big); !errors.Is(err, ErrBadFrame) {
		t.Fatalf("超长帧应拒绝，得到 %v", err)
	}
}

func TestReplyRoundTripAndCodes(t *testing.T) {
	all := []Reply{{OK: true}}
	for c := range knownCodes {
		all = append(all, Reply{Code: c})
	}
	for _, r := range all {
		var buf bytes.Buffer
		if err := WriteReply(&buf, r); err != nil {
			t.Fatalf("%+v: %v", r, err)
		}
		got, err := ReadReply(&buf)
		if err != nil || got != r {
			t.Fatalf("往返不一致: %+v → %+v, %v", r, got, err)
		}
		var re *ReplyError
		if r.OK != (got.Err() == nil) || (!r.OK && (!errors.As(got.Err(), &re) || re.Code != r.Code)) {
			t.Fatalf("Err() 不对: %+v", got)
		}
	}
	for name, r := range map[string]Reply{
		"成功带码": {OK: true, Code: CodeDialFailed},
		"未知码":  {Code: "weird"},
		"失败无码": {},
	} {
		if err := WriteReply(io.Discard, r); !errors.Is(err, ErrBadFrame) {
			t.Errorf("%s: 应拒绝，得到 %v", name, err)
		}
	}
	if _, err := ReadReply(bytes.NewReader(rawFrame(`{"ok":true,"extra":1}`))); !errors.Is(err, ErrBadFrame) {
		t.Errorf("回应帧未知字段应拒绝，得到 %v", err)
	}
}

func TestDatagrams(t *testing.T) {
	var buf bytes.Buffer
	c := NewDatagramConn(&buf)
	big := bytes.Repeat([]byte{7}, MaxDatagram)
	for _, p := range [][]byte{{}, []byte("hello"), big} {
		if err := c.WriteDatagram(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.WriteDatagram(make([]byte, MaxDatagram+1)); !errors.Is(err, ErrBadFrame) {
		t.Fatalf("超长数据报应拒绝，得到 %v", err)
	}
	p := make([]byte, MaxDatagram)
	for _, want := range [][]byte{{}, []byte("hello"), big} {
		n, err := c.ReadDatagram(p)
		if err != nil || !bytes.Equal(p[:n], want) {
			t.Fatalf("读到 %d 字节, %v；期望 %d 字节", n, err, len(want))
		}
	}
	if _, err := c.ReadDatagram(p); err != io.EOF {
		t.Fatalf("数据报之间结束应为 io.EOF，得到 %v", err)
	}
}

func TestDatagramSmallBufferKeepsAlignment(t *testing.T) {
	var buf bytes.Buffer
	c := NewDatagramConn(&buf)
	_ = c.WriteDatagram([]byte("0123456789"))
	_ = c.WriteDatagram([]byte("ok"))
	small := make([]byte, 4)
	if _, err := c.ReadDatagram(small); !errors.Is(err, ErrDatagramTooLarge) {
		t.Fatalf("缓冲区不够应返回 ErrDatagramTooLarge，得到 %v", err)
	}
	n, err := c.ReadDatagram(small)
	if err != nil || string(small[:n]) != "ok" {
		t.Fatalf("丢弃超长数据报后应能读到下一个: %q %v", small[:n], err)
	}
}

func TestDatagramTruncated(t *testing.T) {
	var buf bytes.Buffer
	_ = NewDatagramConn(&buf).WriteDatagram([]byte("hello"))
	b := buf.Bytes()
	for _, n := range []int{1, 3, len(b) - 1} {
		_, err := NewDatagramConn(bytes.NewBuffer(append([]byte(nil), b[:n]...))).ReadDatagram(make([]byte, 16))
		if err != io.ErrUnexpectedEOF {
			t.Errorf("截断到 %d 字节应为 io.ErrUnexpectedEOF，得到 %v", n, err)
		}
	}
}

// fakeAgent 在 net.Pipe 的一端跑 yamux 服务端：
// 出口 main 的 TCP 流回显直到对方半关闭，再写 "bye"；UDP 流把每个数据报原样回送；
// 其他出口回 unknown_exit；端口 1 回 dial_failed。
func fakeAgent(t *testing.T, conn net.Conn) {
	t.Helper()
	sess, err := Server(conn, SessionConfig{})
	if err != nil {
		t.Error(err)
		return
	}
	go func() {
		defer sess.Close()
		for {
			st, err := sess.AcceptStream()
			if err != nil {
				return
			}
			go func(st net.Conn) {
				defer st.Close()
				h, err := ReadHeader(st)
				switch {
				case err != nil:
					_ = WriteReply(st, Reply{Code: CodeBadRequest})
					return
				case h.Exit != "main":
					_ = WriteReply(st, Reply{Code: CodeUnknownExit})
					return
				case h.Port == 1:
					_ = WriteReply(st, Reply{Code: CodeDialFailed})
					return
				}
				_ = WriteReply(st, Reply{OK: true})
				if h.Net == UDP {
					dc := NewDatagramConn(st)
					p := make([]byte, MaxDatagram)
					for {
						n, err := dc.ReadDatagram(p)
						if err != nil {
							return
						}
						_ = dc.WriteDatagram(p[:n])
					}
				}
				data, _ := io.ReadAll(st) // 读到对方半关闭
				_, _ = st.Write(append(data, []byte("bye")...))
			}(st)
		}
	}()
}

func pipeDialer(t *testing.T) (*Dialer, *int) {
	dials := 0
	var mu sync.Mutex
	d := &Dialer{Dial: func(context.Context) (net.Conn, error) {
		mu.Lock()
		dials++
		mu.Unlock()
		a, b := net.Pipe()
		fakeAgent(t, b)
		return a, nil
	}}
	t.Cleanup(func() { d.Close() })
	return d, &dials
}

func TestSessionConcurrentStreamsAndHalfClose(t *testing.T) {
	d, _ := pipeDialer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := d.Open(ctx, validHeader())
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			msg := fmt.Sprintf("stream-%d|", i)
			if _, err := c.Write([]byte(msg)); err != nil {
				errs <- err
				return
			}
			// yamux 的 Close 是半关闭：发 FIN 后仍能读到对方的回应。
			if err := c.Close(); err != nil {
				errs <- err
				return
			}
			got, err := io.ReadAll(c)
			if err != nil || string(got) != msg+"bye" {
				errs <- fmt.Errorf("流 %d 读到 %q, %v", i, got, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestSessionUDP(t *testing.T) {
	d, _ := pipeDialer(t)
	ctx := context.Background()
	c, err := d.Open(ctx, Header{V: 1, Exit: "main", Net: UDP, IP: "8.8.8.8", Port: 53})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	dc := NewDatagramConn(c)
	p := make([]byte, MaxDatagram)
	for _, want := range [][]byte{{}, []byte("q1"), bytes.Repeat([]byte{1}, MaxDatagram)} {
		if err := dc.WriteDatagram(want); err != nil {
			t.Fatal(err)
		}
		n, err := dc.ReadDatagram(p)
		if err != nil || !bytes.Equal(p[:n], want) {
			t.Fatalf("回送不一致: %d 字节, %v", n, err)
		}
	}
}

func TestSessionErrorReplies(t *testing.T) {
	d, dials := pipeDialer(t)
	ctx := context.Background()
	h := validHeader()
	h.Exit = "wg-unknown"
	_, err := d.Open(ctx, h)
	var re *ReplyError
	if !errors.As(err, &re) || re.Code != CodeUnknownExit {
		t.Fatalf("未知出口应回 unknown_exit，得到 %v", err)
	}
	h = validHeader()
	h.Port = 1
	if _, err := d.Open(ctx, h); !errors.As(err, &re) || re.Code != CodeDialFailed {
		t.Fatalf("应回 dial_failed，得到 %v", err)
	}
	h.Port = 0
	if _, err := d.Open(ctx, h); !errors.Is(err, ErrBadFrame) {
		t.Fatalf("非法帧头应在本机就被拒绝，得到 %v", err)
	}
	// agent 回错误帧不应让本机丢掉正常的会话。
	if *dials != 1 {
		t.Fatalf("错误回应后不应重连，实际拨号 %d 次", *dials)
	}
}

// 第一个会话在握手中途断开（agent 收下流后整个会话关掉），Open 应自动重连一次并成功。
func TestDialerReconnectsMidHandshake(t *testing.T) {
	dials := 0
	d := &Dialer{Dial: func(context.Context) (net.Conn, error) {
		dials++
		a, b := net.Pipe()
		if dials == 1 {
			go func() {
				s, err := Server(b, SessionConfig{})
				if err != nil {
					return
				}
				if st, err := s.AcceptStream(); err == nil {
					_, _ = ReadHeader(st)
				}
				s.Close()
			}()
		} else {
			fakeAgent(t, b)
		}
		return a, nil
	}}
	defer d.Close()
	c, err := d.Open(context.Background(), validHeader())
	if err != nil {
		t.Fatalf("握手中途断开后应自动重连: %v", err)
	}
	c.Close()
	if dials != 2 {
		t.Fatalf("应拨号 2 次，实际 %d 次", dials)
	}
}

// 没有 deadline 的 ctx 被取消：走 context.AfterFunc 路径，Open 应及时返回 context.Canceled。
func TestOpenCancelWithoutDeadline(t *testing.T) {
	d := &Dialer{Dial: func(context.Context) (net.Conn, error) {
		a, b := net.Pipe()
		go func() {
			s, err := Server(b, SessionConfig{})
			if err != nil {
				return
			}
			for {
				if _, err := s.AcceptStream(); err != nil {
					return
				}
			}
		}()
		return a, nil
	}}
	defer d.Close()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := d.Open(ctx, validHeader())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应返回 context.Canceled，得到 %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("取消后没有及时返回")
	}
}

// 握手成功后流必须可用：反复在"刚好握手完"附近取消 ctx，
// 返回成功的流不能带着过去的 deadline（之前的竞态）。
func TestOpenSuccessNotPoisonedByLateCancel(t *testing.T) {
	d, _ := pipeDialer(t)
	for i := 0; i < 200; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		go cancel()
		c, err := d.Open(ctx, validHeader())
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("第 %d 次: 失败只能是 context.Canceled，得到 %v", i, err)
			}
			continue
		}
		if _, err := c.Write([]byte("x")); err != nil {
			t.Fatalf("第 %d 次: 成功返回的流不可写: %v", i, err)
		}
		c.Close()
		if got, err := io.ReadAll(c); err != nil || string(got) != "xbye" {
			t.Fatalf("第 %d 次: 成功返回的流不可读: %q %v", i, got, err)
		}
	}
}

func TestDialerReconnects(t *testing.T) {
	d, dials := pipeDialer(t)
	ctx := context.Background()
	c, err := d.Open(ctx, validHeader())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	// 模拟虚拟机重启：会话被关掉。
	d.mu.Lock()
	_ = d.sess.Close()
	d.mu.Unlock()
	c, err = d.Open(ctx, validHeader())
	if err != nil {
		t.Fatalf("会话断开后应自动重连: %v", err)
	}
	c.Close()
	if *dials != 2 {
		t.Fatalf("应拨号 2 次，实际 %d 次", *dials)
	}
}

func TestDialerDialFailure(t *testing.T) {
	d := &Dialer{Dial: func(context.Context) (net.Conn, error) { return nil, errors.New("data.sock 不存在") }}
	if _, err := d.Open(context.Background(), validHeader()); err == nil {
		t.Fatal("拨号失败应返回错误")
	}
}

func TestOpenRespectsContext(t *testing.T) {
	// agent 收下流但永不回应：Open 应在 ctx 到期时返回。
	d := &Dialer{Dial: func(context.Context) (net.Conn, error) {
		a, b := net.Pipe()
		go func() {
			s, err := Server(b, SessionConfig{})
			if err != nil {
				return
			}
			for {
				if _, err := s.AcceptStream(); err != nil {
					return
				}
			}
		}()
		return a, nil
	}}
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := d.Open(ctx, validHeader()); err == nil {
		t.Fatal("没有回应时应失败")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("Open 没有按 ctx 及时返回")
	}
}

func TestSessionConfigDefaults(t *testing.T) {
	y := SessionConfig{KeepAliveInterval: -1}.yamux()
	if y.EnableKeepAlive {
		t.Error("负的心跳间隔应关闭心跳")
	}
	if y.MaxStreamWindowSize != 1<<20 || y.AcceptBacklog != 256 ||
		y.StreamOpenTimeout != 75*time.Second || y.StreamCloseTimeout != 5*time.Minute {
		t.Errorf("默认值不对: %+v", y)
	}
	y = SessionConfig{MaxStreamWindow: 4 << 20, KeepAliveInterval: time.Second}.yamux()
	if y.MaxStreamWindowSize != 4<<20 || !y.EnableKeepAlive || y.KeepAliveInterval != time.Second {
		t.Errorf("自定义值没生效: %+v", y)
	}
}
