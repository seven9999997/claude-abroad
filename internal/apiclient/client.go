// Package apiclient 是 abroadd 本地接口的小客户端（M5-6，m5-plan v2.4）：命令行用它连接、请求、把连不上的情况分类、
// 读事件流。与平台无关（Windows 上 W 系列换成命名管道）；Mac 应用用的是 Swift 那一套，不共用。
package apiclient

import (
	"bufio"
	"bytes"
	"claude-abroad/internal/api"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"
)

// DefaultSocket 是真实模式本地接口的 socket（design 11.1）。
const DefaultSocket = "/var/run/claude-abroad/api.sock"

// 连不上 abroadd 的三种情况（文字不同、都可读，不打印原始的连接错误）。
var (
	ErrNotRunning = errors.New("后台服务没有运行（找不到它的本地接口）")
	ErrNoListener = errors.New("后台服务没有在监听（本地接口还在，但没有进程在听；可能刚退出或正在重启）")
	ErrRejected   = errors.New("后台服务拒绝了这个连接（只接受当前登录的用户和 root）")
)

// Client 连接 abroadd 的本地接口。
type Client struct {
	Socket  string
	Timeout time.Duration // 单个请求的时限（0 用 5 分钟：启用、换主代理可能要一阵）
	// Requests 记下发出的请求数（测试用：本地检查拦下时应为 0）。
	Requests int
}

// Response 是一次请求的应答。
type Response struct {
	Code int
	Body []byte
}

func (c *Client) httpClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", c.Socket)
		},
		DisableKeepAlives: true,
	}}
}

// classify 把连接错误分成三种情况；其余原样返回。
func (c *Client) classify(err error) error {
	if _, serr := os.Stat(c.Socket); errors.Is(serr, fs.ErrNotExist) {
		return ErrNotRunning
	}
	switch {
	case isRefused(err):
		return ErrNoListener
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF),
		strings.Contains(err.Error(), "connection reset"), strings.Contains(err.Error(), "EOF"), strings.Contains(err.Error(), "broken pipe"),
		strings.Contains(err.Error(), "server closed idle connection"):
		return ErrRejected
	case errors.Is(err, syscall.EACCES), errors.Is(err, fs.ErrPermission):
		return ErrRejected
	// 还没写完请求就被后台断开（architect 判 M6-3：以前漏了这一类，提示时对时错）：
	// 连接已被关闭、套接字没有连接、管道已断、连接被中止。客户端只连本机的 unix socket，这些都只能是后台断开的。
	case errors.Is(err, net.ErrClosed), errors.Is(err, syscall.ENOTCONN), errors.Is(err, syscall.EPIPE), errors.Is(err, syscall.ECONNABORTED),
		strings.Contains(err.Error(), "use of closed network connection"), strings.Contains(err.Error(), "socket is not connected"):
		return ErrRejected
	}
	return err
}

// Do 发一个请求。body 为 nil 表示没有请求体；不是 []byte 时按 JSON 编码。
func (c *Client) Do(ctx context.Context, method, path string, body any) (Response, error) {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		j, err := json.Marshal(b)
		if err != nil {
			return Response{}, err
		}
		rd = bytes.NewReader(j)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://abroadd"+path, rd)
	if err != nil {
		return Response{}, err
	}
	t := c.Timeout
	if t <= 0 {
		t = 5 * time.Minute
	}
	c.Requests++
	resp, err := c.httpClient(t).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, c.classify(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return Response{}, c.classify(err)
	}
	return Response{Code: resp.StatusCode, Body: b}, nil
}

// ErrorText 取应答里的 {"error": "…"}（没有时为空）。
func ErrorText(b []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(b, &e)
	return e.Error
}

// ErrorCodeOf 取应答里的错误代码（{"code": "…"}；没有时为空）。分支只看它，不看文字（m5-plan v2.7 决定 0）。
func ErrorCodeOf(b []byte) api.ErrorCode {
	var e struct {
		Code api.ErrorCode `json:"code"`
	}
	_ = json.Unmarshal(b, &e)
	return e.Code
}

// Event 是事件流里的一条。
type Event struct {
	ID    string
	Event string
	Data  string
}

// ErrStreamClosed 是事件流被后台服务断开。
var ErrStreamClosed = errors.New("和后台服务的连接断开了")

// Events 订阅事件流，每条事件调用 fn，直到 ctx 结束（返回 nil）或连接断开（返回 ErrStreamClosed 或连不上的错误）。
func (c *Client) Events(ctx context.Context, fn func(Event)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://abroadd/v1/events", nil)
	if err != nil {
		return err
	}
	c.Requests++
	resp, err := c.httpClient(0).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return c.classify(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return &StatusError{Code: resp.StatusCode, Text: ErrorText(b)}
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var ev Event
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if ev.Data != "" || ev.Event != "" {
				fn(ev)
			}
			ev = Event{}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "id: "):
			ev.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			ev.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			ev.Data = strings.TrimPrefix(line, "data: ")
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return ErrStreamClosed
}

// StatusError 是非 200 的应答。
type StatusError struct {
	Code int
	Text string
}

func (e *StatusError) Error() string { return e.Text }
