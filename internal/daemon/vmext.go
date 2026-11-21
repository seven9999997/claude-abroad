package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"claude-abroad/internal/usererror"
)

// ExternalVM 是 M2 阶段由外部管理的虚拟机（M2-9b-2 决定 4，design 3.1）：abroad-vm 由脚本或 launchd 单独启动，
// abroadd 不负责拉起和关闭它。Start 只确认它就绪且出站已收紧；Stop 什么都不做。
type ExternalVM struct {
	Dir     string        // abroad-vm 的状态目录（ctl.sock、host.sock、data.sock）
	Timeout time.Duration // 0 用默认 60 秒
	Poll    time.Duration // 0 用默认 500 毫秒
}

// DefaultVMTimeout 是等虚拟机就绪的默认时限。
const DefaultVMTimeout = 60 * time.Second

// 错误。
var (
	ErrVMNotRunning = errors.New("虚拟机没有启动")
	ErrVMNotReady   = errors.New("虚拟机没有就绪")
	ErrVMOpen       = errors.New("虚拟机没有收紧出站")
)

// unixClient 是只拨 unix socket 的 HTTP 客户端（不用 net/http 的默认客户端，不走任何 TCP）。
func unixClient(sock string, timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
}

func getJSON(ctx context.Context, sock, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := unixClient(sock, 5*time.Second).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(b, v)
}

// Start 实现 state.VM：在时限内等到控制面 health 通过，并且 host.sock 的 netstats 里 outbound_mode 是 first_hop。
func (v ExternalVM) Start(ctx context.Context) error {
	timeout, poll := v.Timeout, v.Poll
	if timeout <= 0 {
		timeout = DefaultVMTimeout
	}
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	ctl, host := filepath.Join(v.Dir, "ctl.sock"), filepath.Join(v.Dir, "host.sock")
	deadline := time.Now().Add(timeout)
	var last error
	for {
		last = v.check(ctx, ctl, host)
		if last == nil || errors.Is(last, ErrVMOpen) {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
	switch {
	case last == nil:
		return nil
	case errors.Is(last, ErrVMOpen):
		return usererror.New("确认虚拟机", "虚拟机的出站没有收紧到主代理第一跳（outbound_mode 不是 first_hop）", "请不带 --outbound-open 重新启动 abroad-vm。", last)
	case errors.Is(last, ErrVMNotRunning):
		return usererror.New("确认虚拟机", "虚拟机没有启动（找不到它的控制接口）", "请先启动 abroad-vm，再启用。", last)
	}
	return usererror.New("确认虚拟机", fmt.Sprintf("%s 内虚拟机没有就绪", timeout), "请检查 abroad-vm 的日志后再试。", last)
}

func (v ExternalVM) check(ctx context.Context, ctl, host string) error {
	for _, s := range []string{ctl, host} {
		if _, err := os.Stat(s); err != nil {
			return fmt.Errorf("%w：%v", ErrVMNotRunning, err)
		}
	}
	var h map[string]any
	if err := getJSON(ctx, ctl, "http://abroad-agent/v1/health", &h); err != nil {
		return fmt.Errorf("%w：health：%v", ErrVMNotReady, err)
	}
	var ns struct {
		OutboundMode string `json:"outbound_mode"`
	}
	if err := getJSON(ctx, host, "http://abroad-vm/v1/netstats", &ns); err != nil {
		return fmt.Errorf("%w：netstats：%v", ErrVMNotReady, err)
	}
	if ns.OutboundMode != "first_hop" {
		return fmt.Errorf("%w：%q", ErrVMOpen, ns.OutboundMode)
	}
	return nil
}

// Stop 实现 state.VM：M2 阶段什么都不做（虚拟机由外部管理）。
func (ExternalVM) Stop(context.Context) error { return nil }
