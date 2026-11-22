package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"claude-abroad/internal/usernet"
)

// m. host.sock 的替换操作：合法时替换（主代理第一跳不受影响），不合法 422 指出第几项且不改，开放模式 409。
func TestHostExitEndpoints(t *testing.T) {
	out := usernet.NewOutbound(nil, []usernet.FirstHop{{Host: "203.0.113.7", Ports: []usernet.PortRange{{Lo: 443, Hi: 443}}}}, nil)
	u := &userNet{out: out, mode: outboundFirstHop}
	h := u.hostHandler()
	put := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/v1/exit-endpoints", strings.NewReader(body)))
		return w
	}
	if w := put(`{"endpoints":[{"host":"203.0.113.5","proto":"udp","port":51820}]}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if out.BeforeDial("udp", netip.MustParseAddrPort("203.0.113.5:51820")) != nil || out.BeforeDial("tcp", netip.MustParseAddrPort("203.0.113.7:443")) != nil {
		t.Fatal("新端点可达，第一跳照旧")
	}
	if w := put(`{"endpoints":[{"host":"203.0.113.6","proto":"udp","port":1},{"host":"127.0.0.1","proto":"udp","port":53}]}`); w.Code != 422 || !strings.Contains(w.Body.String(), "第 2 个端点") {
		t.Fatal(w.Code, w.Body.String())
	}
	if out.BeforeDial("udp", netip.MustParseAddrPort("203.0.113.5:51820")) != nil || out.BeforeDial("udp", netip.MustParseAddrPort("203.0.113.6:1")) == nil {
		t.Fatal("被拒的替换不改任何东西")
	}
	if w := put(`{"endpoints":[],"x":1}`); w.Code != 400 {
		t.Fatal("未知字段 400")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/exit-endpoints", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatal("没有读操作")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/netstats", nil))
	if !strings.Contains(w.Body.String(), `"exit_endpoint_count":1`) || strings.Contains(w.Body.String(), "203.0.113") {
		t.Fatal(w.Body.String())
	}
	if w := put(`{"endpoints":[]}`); w.Code != 200 || out.BeforeDial("udp", netip.MustParseAddrPort("203.0.113.5:51820")) == nil {
		t.Fatal("清空后不可达")
	}
	open := (&userNet{mode: outboundOpen}).hostHandler()
	w = httptest.NewRecorder()
	open.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/v1/exit-endpoints", strings.NewReader(`{"endpoints":[]}`)))
	if w.Code != http.StatusConflict {
		t.Fatal("开放模式 409")
	}
}

// m. host.sock 只接受 abroad-vm 自己的用户和 root：别的 uid、取不到身份的连接被断开（读写都一样）。
func TestHostPeerCheck(t *testing.T) {
	dir, err := os.MkdirTemp("", "hp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "h.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var uid atomic.Int64
	var rejects atomic.Int32
	pl := newPeerListener(l)
	pl.peerUID = func(*net.UnixConn) (uint32, error) {
		v := uid.Load()
		if v < 0 {
			return 0, errors.New("取不到")
		}
		return uint32(v), nil
	}
	pl.onReject = func() { rejects.Add(1) }
	u := &userNet{mode: outboundFirstHop, out: usernet.NewOutbound(nil, nil, nil)}
	srv := &http.Server{Handler: u.hostHandler()}
	go func() { _ = srv.Serve(pl) }()
	t.Cleanup(func() { srv.Close() })
	cl := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}, DisableKeepAlives: true}}
	try := func(method, path, body string) (int, error) {
		req, _ := http.NewRequest(method, "http://abroad-vm"+path, strings.NewReader(body))
		resp, err := cl.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		_, _ = io.ReadAll(resp.Body)
		return resp.StatusCode, nil
	}
	for _, c := range []struct {
		uid  int64
		pass bool
	}{{int64(os.Getuid()), true}, {0, true}, {int64(os.Getuid()) + 1, false}, {-1, false}} {
		uid.Store(c.uid)
		for _, op := range [][3]string{{"GET", "/v1/netstats", ""}, {"PUT", "/v1/exit-endpoints", `{"endpoints":[]}`}} {
			code, err := try(op[0], op[1], op[2])
			if c.pass && (err != nil || code != 200) {
				t.Fatalf("uid %d 的 %s 应能用：%d %v", c.uid, op[1], code, err)
			}
			if !c.pass && err == nil {
				t.Fatalf("uid %d 的 %s 应被断开", c.uid, op[1])
			}
		}
	}
	if rejects.Load() != 4 {
		t.Fatalf("应断开 4 次：%d", rejects.Load())
	}
}

// M5-4 验收 e（abroad-vm 一侧）：PUT /v1/main-hops 合法时整体替换主代理那一组（分流出口那一组不受影响），
// 不合法 422 且不改，局域网节点不带确认被拒，开放模式 409，只能 PUT。
func TestHostMainHops(t *testing.T) {
	out := usernet.NewOutbound(nil, []usernet.FirstHop{{Host: "9.9.9.7", Ports: []usernet.PortRange{{Lo: 443, Hi: 443}}}}, nil)
	u := &userNet{out: out, mode: outboundFirstHop}
	h := u.hostHandler()
	put := func(path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, path, strings.NewReader(body)))
		return w
	}
	if w := put("/v1/exit-endpoints", `{"endpoints":[{"host":"203.0.113.5","proto":"udp","port":51820}]}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := put("/v1/main-hops", `{"hops":["8.8.4.9:8443"],"lan":[{"addr":"192.168.1.20:7890","confirmed":true}]}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	dial := func(n, dst string) bool { return out.BeforeDial(n, netip.MustParseAddrPort(dst)) == nil }
	if dial("tcp", "9.9.9.7:443") || !dial("tcp", "8.8.4.9:8443") || !dial("tcp", "192.168.1.20:7890") || !dial("udp", "203.0.113.5:51820") {
		t.Fatal("旧第一跳不可达、新的可达，分流出口照旧")
	}
	for _, body := range []string{
		`{"hops":["127.0.0.1:7890"]}`,
		`{"lan":[{"addr":"192.168.1.30:7890"}]}`,
		`{"lan":[{"addr":"127.0.0.1:7890","confirmed":true}]}`,
	} {
		if w := put("/v1/main-hops", body); w.Code != 422 {
			t.Errorf("%s：应 422，得到 %d", body, w.Code)
		}
	}
	if !dial("tcp", "8.8.4.9:8443") || dial("tcp", "192.168.1.30:7890") {
		t.Fatal("被拒的替换不改任何东西")
	}
	if w := put("/v1/main-hops", `{"hops":[],"x":1}`); w.Code != 400 {
		t.Fatal("未知字段 400")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/main-hops", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatal("没有读操作")
	}
	open := (&userNet{mode: outboundOpen}).hostHandler()
	w = httptest.NewRecorder()
	open.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/v1/main-hops", strings.NewReader(`{"hops":[]}`)))
	if w.Code != http.StatusConflict {
		t.Fatal("开放模式 409")
	}
}
