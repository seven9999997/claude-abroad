package apiclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sockPath 给一个短的 socket 路径（unix socket 路径有长度上限，t.TempDir() 带测试名会超长）。
func sockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ac")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

func serve(t *testing.T, h http.Handler) string {
	t.Helper()
	p := sockPath(t)
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Skipf("这个系统不支持 unix socket：%v", err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return p
}

func TestDoJSONAndRaw(t *testing.T) {
	var got []string
	p := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.String()+" "+string(b))
		w.WriteHeader(http.StatusTeapot)
		fmt.Fprint(w, `{"error":"出错了"}`)
	}))
	c := &Client{Socket: p}
	resp, err := c.Do(context.Background(), "PUT", "/v1/x?a=1", map[string]any{"k": "v"})
	if err != nil || resp.Code != http.StatusTeapot || ErrorText(resp.Body) != "出错了" {
		t.Fatalf("%v %+v", err, resp)
	}
	if _, err := c.Do(context.Background(), "POST", "/v1/y", []byte("rules: []\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(context.Background(), "GET", "/v1/z", nil); err != nil {
		t.Fatal(err)
	}
	want := []string{`PUT /v1/x?a=1 {"k":"v"}`, "POST /v1/y rules: []\n", "GET /v1/z "}
	if strings.Join(got, "|") != strings.Join(want, "|") || c.Requests != 3 {
		t.Fatalf("%q（%d 次）", got, c.Requests)
	}
	if ErrorText([]byte("not json")) != "" {
		t.Fatal("不是 JSON 时为空")
	}
}

// 三种连不上的情况分得开：socket 不存在、文件在但没有进程在听、连上后被断开。
func TestClassify(t *testing.T) {
	ctx := context.Background()
	c := &Client{Socket: filepath.Join(sockPath(t), "nope")}
	if _, err := c.Do(ctx, "GET", "/v1/status", nil); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("不存在：%v", err)
	}
	p := sockPath(t)
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Skipf("这个系统不支持 unix socket：%v", err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if _, err := os.Stat(p); err != nil {
		t.Skipf("关闭后 socket 文件没有留下：%v", err)
	}
	c = &Client{Socket: p}
	if _, err := c.Do(ctx, "GET", "/v1/status", nil); !errors.Is(err, ErrNoListener) {
		t.Fatalf("没有在听：%v", err)
	}
	p2 := sockPath(t)
	l2, err := net.Listen("unix", p2)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	go func() {
		for {
			conn, err := l2.Accept()
			if err != nil {
				return
			}
			conn.Close() // 像 abroadd 对不认识的 uid 那样直接断开
		}
	}()
	c = &Client{Socket: p2}
	if _, err := c.Do(ctx, "GET", "/v1/status", nil); !errors.Is(err, ErrRejected) {
		t.Fatalf("被断开：%v", err)
	}
	for _, e := range []error{ErrNotRunning, ErrNoListener, ErrRejected} {
		if strings.Contains(e.Error(), "dial") || strings.Contains(e.Error(), "unix") {
			t.Errorf("提示里不该有底层错误：%s", e)
		}
	}
}

func TestDoCanceled(t *testing.T) {
	p := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := (&Client{Socket: p}).Do(ctx, "GET", "/", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
}

func TestEvents(t *testing.T) {
	p := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/events" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": hello\n\nid: 1\nevent: state\ndata: {\"source\":\"state\",\"kind\":\"enabled\"}\n\nid: 2\nevent: pf\ndata: {\"kind\":\"x\"}\n\n")
		w.(http.Flusher).Flush()
		// 写完就结束：客户端应报告“断开”。
	}))
	var evs []Event
	err := (&Client{Socket: p}).Events(context.Background(), func(e Event) { evs = append(evs, e) })
	if !errors.Is(err, ErrStreamClosed) || len(evs) != 2 || evs[0].ID != "1" || evs[0].Event != "state" || !strings.Contains(evs[0].Data, "enabled") || evs[1].Event != "pf" {
		t.Fatalf("%v %+v", err, evs)
	}
	// ctx 结束：干净返回 nil。
	p2 := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if err := (&Client{Socket: p2}).Events(ctx, func(Event) {}); err != nil {
		t.Fatalf("Ctrl+C 应干净返回：%v", err)
	}
	// 非 200：带上应答里的错误文字。
	p3 := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":"订阅的人太多"}`)
	}))
	var se *StatusError
	if err := (&Client{Socket: p3}).Events(context.Background(), func(Event) {}); !errors.As(err, &se) || se.Code != 503 || se.Text != "订阅的人太多" {
		t.Fatalf("%v", err)
	}
}
