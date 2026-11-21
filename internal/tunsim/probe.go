package tunsim

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"claude-abroad/internal/redact"
	"claude-abroad/internal/stack"
)

// ProbeResult 是一轮探测的结果（只有步骤结果，没有地址；Fake 只给调用方比对，不进报告）。
type ProbeResult struct {
	Steps []Step     `json:"steps"`
	Fake  netip.Addr `json:"-"`
}

// OK 报告所有步骤都通过。
func (r ProbeResult) OK() bool {
	for _, s := range r.Steps {
		if !s.OK {
			return false
		}
	}
	return len(r.Steps) > 0
}

// Failed 报告所有步骤都失败（停用后应如此）。
func (r ProbeResult) Failed() bool {
	for _, s := range r.Steps {
		if s.OK {
			return false
		}
	}
	return len(r.Steps) > 0
}

// Probe 只用客户端跑一轮探测（M2-9b-2 决定 9）：网络栈和闸门在外面（abroadd 的数据面），这里不碰它们。
//   - dns：经 198.18.0.2:53 查 cfg.Name 的 A，拿到 fake 段的地址；
//   - tcp_fake：连 假地址:80，GET /generate_204 → 204；
//   - udp：发一个数据报，要求原样回显（cfg.UDPTarget 非零时发往它，否则发往 假地址:UDPPort）；
//   - tcp_realip：连 cfg.RealIP，GET /generate_204 → 204。
//
// knownFake 有效时跳过 dns，直接用它（utun 重建后不重新解析，证明映射还在）。
func Probe(ctx context.Context, cl *Client, cfg Config, knownFake netip.Addr) ProbeResult {
	cfg = cfg.withDefaults()
	var r ProbeResult
	add := func(name string, err error) {
		st := Step{Name: name, OK: err == nil}
		if err != nil {
			st.Err = redact.String(err.Error())
		}
		r.Steps = append(r.Steps, st)
	}
	step := func() (context.Context, context.CancelFunc) { return context.WithTimeout(ctx, cfg.Timeout) }

	fake := knownFake
	if !fake.IsValid() {
		c, cancel := step()
		addr, err := cl.LookupA(c, cfg.Name, stack.DefaultDNSAddr)
		cancel()
		if err == nil && (!fakeNet.Contains(addr) || reservedNet.Contains(addr)) {
			err = errors.New("拿到的地址不在 fake 段的可分配范围")
		}
		fake = addr
		add("dns", err)
	}
	r.Fake = fake

	tcp := func(name string, dst netip.AddrPort, host string) {
		if !dst.Addr().IsValid() {
			add(name, errors.New("没有目标地址"))
			return
		}
		c, cancel := step()
		defer cancel()
		conn, err := cl.DialTCP(c, dst)
		if err != nil {
			add(name, fmt.Errorf("连接：%w", err))
			return
		}
		defer conn.Close()
		add(name, get204(conn, host, cfg.Timeout))
	}
	tcp("tcp_fake", netip.AddrPortFrom(fake, 80), cfg.Name)

	func() {
		dst := cfg.UDPTarget
		if !dst.IsValid() {
			dst = netip.AddrPortFrom(fake, uint16(cfg.UDPPort))
		}
		u, err := cl.DialUDP()
		if err != nil {
			add("udp", err)
			return
		}
		defer u.Close()
		msg := []byte("abroadd-probe")
		if _, err := u.WriteTo(msg, net.UDPAddrFromAddrPort(dst)); err != nil {
			add("udp", err)
			return
		}
		wait := cfg.Timeout
		if wait > 5*time.Second {
			wait = 5 * time.Second
		}
		_ = u.SetReadDeadline(time.Now().Add(wait))
		buf := make([]byte, 2048)
		n, _, err := u.ReadFrom(buf)
		if err == nil && string(buf[:n]) != string(msg) {
			err = errors.New("回显的内容不对")
		}
		add("udp", err)
	}()

	tcp("tcp_realip", cfg.RealIP, "realip.test")
	return r
}
