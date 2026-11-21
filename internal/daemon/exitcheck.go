package daemon

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"claude-abroad/internal/guestabi"
	"claude-abroad/internal/stack"
	"claude-abroad/internal/usererror"
	"claude-abroad/internal/wire"
)

// DefaultExitCheckTimeout 是出口检查的时限。
const DefaultExitCheckTimeout = 10 * time.Second

// ExitCheck 是出口检查的最小版（M2-9b-2 决定 5，design 10）：经数据通道、出口 main，对 Host 发 GET /generate_204，
// Timeout 内拿到 204 才算通过。它不经过闸门（检查时闸门是关着的）。
type ExitCheck struct {
	Opener  stack.Opener
	Host    string
	Timeout time.Duration // 0 用默认 10 秒
}

// Check 实现 state.ExitCheck。
func (e ExitCheck) Check(ctx context.Context) error {
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = DefaultExitCheckTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	fail := func(why string, err error) error {
		return usererror.New("出口检查", why, "请确认主代理可用、虚拟机在运行，再试一次。", err)
	}
	conn, err := e.Opener.Open(ctx, wire.Header{V: wire.Version, Exit: guestabi.ExitMain, Net: wire.TCP, Host: e.Host, Port: 80})
	if err != nil {
		return fail("经主代理连不上检查地址", err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if _, err := io.WriteString(conn, "GET /generate_204 HTTP/1.1\r\nHost: "+e.Host+"\r\nConnection: close\r\nUser-Agent: abroadd\r\n\r\n"); err != nil {
		return fail("发请求失败", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return fail("读应答失败（超时或连接断开）", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		// 收到了应答但不是预期的：明确不通过（复查里关闸门）；连不上、超时在复查里按无法判断。
		return fail(fmt.Sprintf("检查地址返回 %d，应为 204", resp.StatusCode), ErrExitCheckFailed)
	}
	return nil
}
