package agent

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claude-abroad/internal/guestabi"
)

// i. 开发用多出口的隔离（m3-plan v2.8 决定 7）。
func TestParseDevCmdline(t *testing.T) {
	for in, want := range map[string]bool{
		"console=hvc0 abroad.dev=1": true,
		"abroad.dev=1 panic=0":      true,
		"console=hvc0":              false,
		"abroad.dev=0":              false,
		"abroad.dev=1x":             false,
		"xabroad.dev=1":             false,
		"abroad.dev":                false,
	} {
		if ParseDevCmdline(in) != want {
			t.Errorf("%q：应为 %v", in, want)
		}
	}
}

func TestDevExitsNotRegisteredByDefault(t *testing.T) {
	h := (&API{State: &State{}}).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/dev-exits", strings.NewReader(`{"exits":[]}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("没有开发标记时应是 404：%d", rec.Code)
	}
}

func TestDevExitsHandler(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "sing-box.json")
	dev := &DevExits{Set: &SingBoxSet{Path: cfg, Password: strings.Repeat("p", 32), Base: context.Background()}}
	h := (&API{State: &State{}, DevExits: dev}).Handler()
	put := func(body string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/dev-exits", strings.NewReader(body)))
		return rec.Code
	}
	if c := put(`{"exits":[{"Tag":"wg-a","Port":40001,"Server":"` + guestabi.HostLoopbackIP + `"},{"Tag":"clash-b","Port":40002,"Server":"127.0.0.1"}]}`); c != 200 {
		t.Fatalf("合法的开发出口：%d", c)
	}
	b, _ := os.ReadFile(cfg)
	tags, err := SingBoxConfigCheck(b)
	if err != nil || strings.Join(tags, ",") != "main,wg-a,clash-b" || !bytes.Contains(b, []byte(guestabi.HostLoopbackIP)) {
		t.Fatalf("sing-box 配置：%v %v", tags, err)
	}
	if !dev.has("wg-a") || dev.has("other") {
		t.Fatal("出口表应认开发出口")
	}
	for name, body := range map[string]string{
		"非回环的服务器": `{"exits":[{"Tag":"x","Port":40001,"Server":"203.0.113.5"}]}`,
		"局域网地址":   `{"exits":[{"Tag":"x","Port":40001,"Server":"192.168.1.5"}]}`,
		"出口网段":    `{"exits":[{"Tag":"x","Port":40001,"Server":"10.200.1.2"}]}`,
		"叫 main":  `{"exits":[{"Tag":"main","Port":40001}]}`,
		"未知字段":    `{"exits":[],"x":1}`,
	} {
		if c := put(body); c == 200 {
			t.Errorf("%s：应被拒", name)
		}
	}
}

func TestSingBoxServerOnlyLoopback(t *testing.T) {
	pw := strings.Repeat("p", 32)
	if _, err := SingBoxConfig([]SingBoxExit{{Tag: "main", Port: guestabi.MainProxyPort}, {Tag: "x", Port: 40001, Server: "8.8.8.8"}}, pw); err == nil {
		t.Fatal("非回环的 Server 应被拒")
	}
	if _, err := SingBoxConfig([]SingBoxExit{{Tag: "main", Port: guestabi.MainProxyPort}, {Tag: "x", Port: 40001, Server: guestabi.HostLoopbackIP}}, pw); err != nil {
		t.Fatalf("网关的回环映射地址应允许：%v", err)
	}
}

// M4-7：出口命名空间的 veth 网段可以做 socks 出站的服务器，网段外的私网地址不行。
func TestSingBoxServerExitNet(t *testing.T) {
	pw := strings.Repeat("p", 32)
	if _, err := SingBoxConfig([]SingBoxExit{{Tag: "main", Port: guestabi.MainProxyPort}, {Tag: "x", Port: 1080, Server: "10.200.3.2"}}, pw); err != nil {
		t.Fatalf("出口网段应允许：%v", err)
	}
	for _, bad := range []string{"10.201.0.2", "10.0.2.2", "fd00::1", "host.example"} {
		if _, err := SingBoxConfig([]SingBoxExit{{Tag: "main", Port: guestabi.MainProxyPort}, {Tag: "x", Port: 1080, Server: bad}}, pw); err == nil {
			t.Fatalf("%s 应被拒", bad)
		}
	}
}

// SingBoxSet：开发出口和分流出口各改各的，配置里是 main 加两者；冲突时不改。
func TestSingBoxSetMerges(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "sing-box.json")
	s := &SingBoxSet{Path: cfg, Password: strings.Repeat("p", 32), Base: context.Background()}
	tags := func() string {
		b, _ := os.ReadFile(cfg)
		ts, err := SingBoxConfigCheck(b)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(ts, ",")
	}
	if err := s.SetDev([]SingBoxExit{{Tag: "d1", Port: 40001}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetExits([]SingBoxExit{{Tag: "hk", Port: 1080, Server: "10.200.1.2"}}); err != nil {
		t.Fatal(err)
	}
	if got := tags(); got != "main,d1,hk" {
		t.Fatal(got)
	}
	if err := s.SetExits([]SingBoxExit{{Tag: "d1", Port: 1080, Server: "10.200.1.2"}}); err == nil {
		t.Fatal("和开发出口重名应被拒")
	}
	if got := tags(); got != "main,d1,hk" {
		t.Fatalf("被拒后不应改：%s", got)
	}
	if err := s.SetDev(nil); err != nil {
		t.Fatal(err)
	}
	if got := tags(); got != "main,hk" {
		t.Fatal(got)
	}
}
