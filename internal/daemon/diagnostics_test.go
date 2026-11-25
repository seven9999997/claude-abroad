package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"

	"claude-abroad/internal/envcheck"
)

// M6-6a：设备体检只返回结果（接口级别 Normal），字体目录由系统依赖给；诊断包整段脱敏。
func TestDeviceCheckAPI(t *testing.T) {
	e := newEnv(t)
	o := e.opts()
	o.Sys.DeviceRoot = fstest.MapFS{"Library/Fonts/SimSun.ttc": {}, "Library/Fonts/Arial.ttf": {}}
	o.Sys.DeviceHome = fstest.MapFS{"Library/Fonts/msyh.ttc": {}}
	d := e.start(o)
	code, raw := call(t, d, "POST", "/v1/envcheck/device", nil)
	var rep envcheck.DeviceReport
	if code != 200 || json.Unmarshal(raw, &rep) != nil || len(rep.Findings) != 2 {
		t.Fatalf("%d %s", code, raw)
	}
	if s := status(t, d); s.GateOpen {
		t.Fatal("体检不应改变闸门")
	}
}

func TestDiagnosticsRedacted(t *testing.T) {
	e := newEnv(t)
	d := e.start(e.opts())
	post(t, d, "/v1/enable", 200)
	secrets := []string{"203.0.113.77", "198.51.100.9", "/sub/abc", "SECRET123", "hunter2", "2001:db8::5"}
	d.events.Add("test", "leak", "出口 203.0.113.77 订阅 https://sub.example.com/sub/abc?token=SECRET123 password=hunter2 节点 198.51.100.9:443 v6 2001:db8::5")
	code, raw := call(t, d, "GET", "/v1/diagnostics", nil)
	if code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	for _, s := range secrets {
		if strings.Contains(string(raw), s) {
			t.Fatalf("诊断包里不应有 %q", s)
		}
	}
	var dg Diagnostics
	if json.Unmarshal(raw, &dg) != nil || dg.Version.Go == "" || dg.Status.State != "ENABLED" || len(dg.LaunchGuard.Layers) != 4 {
		t.Fatalf("诊断包内容不全：%s", raw)
	}
}
