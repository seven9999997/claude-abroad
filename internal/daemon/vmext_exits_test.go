package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claude-abroad/internal/exitconf"
)

// ExternalVM 的分流出口操作：启动标识、端点集合（host.sock）、出口（ctl.sock，含 agent 回 422 时的部分状态）。
func TestExternalVMExits(t *testing.T) {
	dir, err := os.MkdirTemp("", "vx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	var gotEps []ExitEndpoint
	var gotExits []StoredExit
	partial := false
	ctl := http.NewServeMux()
	ctl.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"boot_id":"b-123"}`)
	})
	ctl.HandleFunc("PUT /v1/exits", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Exits []StoredExit }
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotExits = req.Exits
		if partial {
			w.WriteHeader(422)
			_, _ = io.WriteString(w, `{"error":"出口 b：解析不出服务器的地址","exits":[{"tag":"a","up":true}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"exits":[{"tag":"a","up":true},{"tag":"b","up":false,"reason":"隧道设备不在"}]}`)
	})
	ctl.HandleFunc("GET /v1/exits", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"exits":[{"tag":"a","up":false,"reason":"3 分钟内没有握手"}]}`)
	})
	host := http.NewServeMux()
	host.HandleFunc("PUT /v1/exit-endpoints", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Endpoints []ExitEndpoint }
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotEps = req.Endpoints
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	serveUnix(t, filepath.Join(dir, "ctl.sock"), ctl.ServeHTTP)
	serveUnix(t, filepath.Join(dir, "host.sock"), host.ServeHTTP)
	v := ExternalVM{Dir: dir}
	ctx := context.Background()
	if b, err := v.BootID(ctx); err != nil || b != "b-123" {
		t.Fatalf("%q %v", b, err)
	}
	if err := v.PushEndpoints(ctx, []ExitEndpoint{{Host: "h.example", Proto: "udp", Port: 51820}}); err != nil || len(gotEps) != 1 || gotEps[0].Port != 51820 {
		t.Fatalf("%v %+v", err, gotEps)
	}
	st, err := v.PushExits(ctx, []StoredExit{{Tag: "a", Kind: exitconf.WireGuard, Conf: "secret-conf"}, {Tag: "b"}})
	if err != nil || len(st) != 2 || st[1].Up || st[1].Reason != "隧道设备不在" || len(gotExits) != 2 || gotExits[0].Conf != "secret-conf" {
		t.Fatalf("%+v %v %+v", st, err, gotExits)
	}
	partial = true
	st, err = v.PushExits(ctx, []StoredExit{{Tag: "a"}, {Tag: "b"}})
	if err == nil || len(st) != 1 || !strings.Contains(err.Error(), "422") {
		t.Fatalf("agent 回 422 时仍返回各出口的状态：%+v %v", st, err)
	}
	if strings.Contains(err.Error(), "secret-conf") {
		t.Fatal("错误里不应带配置")
	}
	if st, err := v.AgentExits(ctx); err != nil || len(st) != 1 || st[0].Up {
		t.Fatalf("%+v %v", st, err)
	}
	if _, err := (ExternalVM{Dir: filepath.Join(dir, "nope")}).BootID(ctx); err == nil {
		t.Fatal("连不上时应报没有就绪")
	}
}
