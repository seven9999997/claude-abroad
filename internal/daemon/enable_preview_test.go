package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"claude-abroad/internal/api"
	"claude-abroad/internal/claudeproc"
	"claude-abroad/internal/state"
)

func enablePreview(t *testing.T, d *Daemon) EnablePreview {
	t.Helper()
	code, b := call(t, d, "GET", "/v1/enable/preview", nil)
	var pv EnablePreview
	if code != http.StatusOK || json.Unmarshal(b, &pv) != nil {
		t.Fatalf("启用预览：%d %s", code, b)
	}
	return pv
}

func pendingConfirms(d *Daemon) int {
	d.confirms.mu.Lock()
	defer d.confirms.mu.Unlock()
	return len(d.confirms.m)
}

func jsonEq(t *testing.T, a, b any) bool {
	t.Helper()
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// v0.71b（顾问必须改）：只读的启用预览和启用第一步的 409 应答内容一致（地区变、不变各一例）；调用预览不签发确认码、
// 状态文件不变、什么都不改。
func TestEnablePreviewMatchesFirstStep(t *testing.T) {
	e, _, o := confirmEnv(t)
	d := e.start(o)
	if code, b := putRegion(t, d, `{"mode":"manual","timezone":"Asia/Tokyo","lang":"ja-JP"}`); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	addClaude(e, claudeproc.ExitOnTerm)
	stPath := filepath.Join(e.dir, state.FileName)
	before, _ := os.ReadFile(stPath)
	signals := len(e.sys.Procs.SignalLog())

	// 地区会变
	pv := enablePreview(t, d)
	if !pv.CanEnable || pv.Region == nil || pv.Preview == nil || pv.Note != EnablePreviewRecheckNote {
		t.Fatalf("地区会变时预览应有地区变化和进程清单：%+v", pv)
	}
	if n := pendingConfirms(d); n != 0 {
		t.Fatalf("预览不应签发确认码：%d 个", n)
	}
	after, _ := os.ReadFile(stPath)
	if !bytes.Equal(before, after) || !e.sys.Net.Snapshot().Equal(e.init) || len(e.sys.Procs.SignalLog()) != signals {
		t.Fatal("预览不应改动状态文件、网络、进程")
	}
	code, b := call(t, d, "POST", "/v1/enable", nil)
	if code != http.StatusConflict {
		t.Fatalf("%d %s", code, b)
	}
	cn := confirmOf(t, b)
	if !jsonEq(t, pv.Region, cn.Region) || !jsonEq(t, *pv.Preview, cn.Preview) {
		t.Fatalf("预览和启用第一步不一致：\n%+v %+v\n%+v %+v", pv.Region, pv.Preview, cn.Region, cn.Preview)
	}

	// 地区不变：目标就是此刻的设置
	if code, b := putRegion(t, d, `{"mode":"manual","timezone":"Europe/Berlin","lang":"zh-Hans-CN"}`); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	pv = enablePreview(t, d)
	if !pv.CanEnable || pv.Region != nil || pv.Preview != nil {
		t.Fatalf("地区不变时预览不应有地区变化：%+v", pv)
	}
	if code, b := call(t, d, "POST", "/v1/enable", nil); code != 200 {
		t.Fatalf("地区不变时启用一步完成：%d %s", code, b)
	}
	if pv = enablePreview(t, d); !pv.Enabled || pv.Region != nil {
		t.Fatalf("已启用：%+v", pv)
	}
}

// 现在不能启用时给原因代码：已卸载、正在进行别的操作；路由级别只读。
func TestEnablePreviewBlockers(t *testing.T) {
	e := newEnv(t)
	d := e.start(e.opts())
	if pv := enablePreview(t, d); !pv.CanEnable || pv.Blocker != "" {
		t.Fatalf("%+v", pv)
	}
	if err := d.m.Exclusive(func() error {
		if pv := enablePreview(t, d); pv.CanEnable || pv.Blocker != api.CodeBusy {
			t.Errorf("忙时：%+v", pv)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(e.dir, UninstalledFile), []byte("x\n"), 0o600)
	if pv := enablePreview(t, d); pv.CanEnable || pv.Blocker != api.CodeUninstalled {
		t.Fatalf("已卸载：%+v", pv)
	}
	for _, rt := range RouteTable() {
		if rt.ID == "enable-preview" && (rt.Level != api.Read || rt.Method != "GET") {
			t.Fatalf("启用预览应是只读的 GET：%+v", rt)
		}
	}
}

// 没有出口基准时给 no_baseline（abroadd 持有出口基准服务时）。
func TestEnablePreviewNoBaseline(t *testing.T) {
	e := newEnv(t)
	o := e.opts()
	o.Sys.Baseline = NewBaselineService(FakeProber{}, e.dir)
	d := e.start(o)
	if pv := enablePreview(t, d); pv.CanEnable || pv.Blocker != api.CodeNoBaseline || pv.Reason == "" {
		t.Fatalf("没有出口基准：%+v", pv)
	}
}
