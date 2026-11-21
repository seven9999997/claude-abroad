package agent

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"claude-abroad/internal/kmod"
)

func TestHealthOverListener(t *testing.T) {
	st := NewState("6.12.115-0-virt")
	base := st.start
	st.now = func() time.Time { return base.Add(42 * time.Second) }
	st.AddModules([]kmod.Result{{Name: "vsock", OK: true}})
	st.SetNet(nil)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	srv := NewServer(Handler(st, func() { close(stopped) }))
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	url := "http://" + l.Addr().String()

	resp, err := http.Get(url + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	var h Health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	select {
	case <-stopped:
		t.Fatal("health 不应触发关机")
	default:
	}
	if resp.StatusCode != 200 || !h.OK || h.Kernel != "6.12.115-0-virt" || h.UptimeS != 42 || h.Version == "" || h.Engines == nil {
		t.Fatalf("health 不对：%d %+v", resp.StatusCode, h)
	}

	for _, c := range []struct {
		method, path string
		code         int
	}{
		{"POST", "/v1/health", 405},
		{"GET", "/v1/nope", 404},
		{"GET", "/", 404},
		{"GET", "/v1/shutdown", 405},
	} {
		req, _ := http.NewRequest(c.method, url+c.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.code {
			t.Errorf("%s %s：应为 %d，实际 %d", c.method, c.path, c.code, resp.StatusCode)
		}
	}
}

func TestShutdownEndpoint(t *testing.T) {
	var called atomic.Int32
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(Handler(NewState("k"), func() { called.Add(1) }))
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	resp, err := http.Post("http://"+l.Addr().String()+"/v1/shutdown", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 202 || called.Load() != 1 {
		t.Fatalf("shutdown：%d，调用 %d 次", resp.StatusCode, called.Load())
	}
}

func TestHealthNotOK(t *testing.T) {
	cases := map[string]func(*State){
		"模块失败": func(s *State) { s.SetNet(nil); s.AddModules([]kmod.Result{{Name: "virtio_net", Error: "x"}}) },
		"网卡失败": func(s *State) { s.SetNet(errors.New("no eth0")) },
		"有问题":  func(s *State) { s.SetNet(nil); s.AddProblem("p") },
	}
	for name, f := range cases {
		s := NewState("k")
		f(s)
		if s.Snapshot().OK {
			t.Errorf("%s：ok 应为 false", name)
		}
	}
}

func TestBootID(t *testing.T) {
	a, b := NewState("k"), NewState("k")
	id := a.Snapshot().BootID
	if len(id) != 32 || id != a.Snapshot().BootID {
		t.Fatalf("boot_id 应为 32 位 hex 且一次开机内不变：%q", id)
	}
	if id == b.Snapshot().BootID {
		t.Fatal("两次开机的 boot_id 应不同")
	}
}
