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

// v0.71c（顾问 2026-11-27 拍板）：只读的卸载预览和卸载第一步 409 应答里的 preview 内容一致；调用预览不签发确认码、
// 状态文件和管控都不变；路由级别只读（另由 TestRoutesNeverLaunchWhenNotEnabled 按路由表核对不拉起虚拟机）。
func TestUninstallPreviewMatchesFirstStep(t *testing.T) {
	e := newUninstallEnv(t, false)
	addClaude(e.env, claudeproc.ExitOnTerm)
	stPath := filepath.Join(e.dir, state.FileName)
	before, _ := os.ReadFile(stPath)
	signals := len(e.sys.Procs.SignalLog())
	for _, kill := range []bool{false, true} {
		path := "/v1/uninstall/preview"
		if kill {
			path += "?kill_browsers=true"
		}
		n0 := pendingConfirms(e.d)
		code, b := call(t, e.d, "GET", path, nil)
		var pv UninstallPreviewView
		if code != http.StatusOK || json.Unmarshal(b, &pv) != nil || pv.Note != UninstallPreviewRecheckNote || len(pv.Processes) == 0 || len(pv.Removes) == 0 {
			t.Fatalf("预览：%d %s", code, b)
		}
		if n := pendingConfirms(e.d); n != n0 {
			t.Fatalf("预览不应签发确认码：%d", n)
		}
		after, _ := os.ReadFile(stPath)
		if !bytes.Equal(before, after) || !e.lock.Check().Written || e.d.uninstalled() || len(e.sys.Procs.SignalLog()) != signals {
			t.Fatal("预览不应有任何改动")
		}
		body, _ := json.Marshal(ConfirmRequest{KillBrowsers: kill})
		code, raw := call(t, e.d, "POST", "/v1/actions/uninstall", body)
		var cn struct {
			Preview UninstallPreview `json:"preview"`
		}
		if code != http.StatusConflict || json.Unmarshal(raw, &cn) != nil {
			t.Fatalf("第一步：%d %s", code, raw)
		}
		if !jsonEq(t, pv.UninstallPreview, cn.Preview) {
			t.Fatalf("预览和卸载第一步不一致（kill_browsers=%v）：\n%s\n%s", kill, b, raw)
		}
	}
	for _, rt := range RouteTable() {
		if rt.ID == "uninstall-preview" && (rt.Level != api.Read || rt.Method != "GET") {
			t.Fatalf("卸载预览应是只读的 GET：%+v", rt)
		}
	}
}
