package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"claude-abroad/internal/daemon"
	"claude-abroad/internal/wire"
)

// toServer 是假的数据通道：不管目标是什么，都连到本地的测试服务器，并记下请求头（出口、目标）。
type toServer struct {
	addr string
	seen []wire.Header
}

func (o *toServer) Open(ctx context.Context, h wire.Header) (net.Conn, error) {
	o.seen = append(o.seen, h)
	var d net.Dialer
	return d.DialContext(ctx, "tcp", o.addr)
}

// M5-4 验收 c（真实实现）：启用时经数据通道按出口 main 开流（域名交给出口解析）；超过上限、状态码不对、跳转到别的协议都失败。
func TestRealFetch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("proxies: []")) })
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", daemon.SourceFetchMax+10)))
	})
	mux.HandleFunc("/500", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 500) })
	mux.HandleFunc("/redir", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://other.example.com/x", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	op := &toServer{addr: strings.TrimPrefix(srv.URL, "http://")}
	f := realFetch(op, nil)
	b, err := f(context.Background(), "http://sub.example.com/ok", false)
	if err != nil || string(b) != "proxies: []" {
		t.Fatalf("%q %v", b, err)
	}
	if len(op.seen) != 1 || op.seen[0].Exit != "main" || op.seen[0].Host != "sub.example.com" || op.seen[0].IP != "" || op.seen[0].Port != 80 {
		t.Fatalf("应经数据通道按出口 main、带域名开流：%+v", op.seen)
	}
	for path, want := range map[string]error{"/big": daemon.ErrFetchTooLarge, "/500": daemon.ErrFetchStatus, "/redir": daemon.ErrFetchRedirect} {
		if _, err := f(context.Background(), "http://sub.example.com"+path, false); !errors.Is(err, want) {
			t.Errorf("%s：%v，应为 %v", path, err, want)
		}
	}
	// 经本机网络但没有直连拨号模块：失败，不会退回别的路。
	if _, err := f(context.Background(), "http://sub.example.com/ok", true); err == nil {
		t.Fatal("没有直连拨号模块时经本机网络应失败")
	}
	if len(op.seen) != 4 {
		t.Fatalf("经本机网络时不能走数据通道：%d", len(op.seen))
	}
}
