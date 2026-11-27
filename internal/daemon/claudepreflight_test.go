package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-abroad/internal/preflight"
	"claude-abroad/internal/state"
)

// cleanRunner 是没有任何冲突的只读查询样本（不开代理、路由干净、没有代理软件）。
type cleanRunner struct{ t *testing.T }

func (r cleanRunner) Run(_ context.Context, p string, args []string, _ []byte) ([]byte, []byte, error) {
	files := map[string]string{
		"netstat -rn -f inet": "netstat-clean.constructed.txt", "scutil --proxy": "proxy-off.txt",
		"scutil --nwi": "nwi-v4.txt", "ps -axo comm": "ps-clean.constructed.txt", "route -n get default": "route-default-en0.txt",
		"route -n get 1.1.1.1": "route-en0.constructed.txt", "route -n get 129.0.0.1": "route-en0.constructed.txt",
		"route -n get 198.18.0.2": "route-en0.constructed.txt",
	}
	key := filepath.Base(p) + " " + strings.Join(args, " ")
	b, err := os.ReadFile(filepath.Join("..", "preflight", "testdata", files[key]))
	if err != nil {
		r.t.Fatalf("%s：%v", key, err)
	}
	return b, nil, nil
}

// 必须改 2（daemon 级）：第四层的两项“必须处理”阻断启用；没配置第四层时启用不受影响；端口释放、重新监听上之后启用通过。
func TestClaudeLayerBlocksEnable(t *testing.T) {
	// 没配置第四层：启用照常（冲突检查里没有这两项）
	e0 := newEnv(t)
	chk0 := &preflight.Checker{R: cleanRunner{t}}
	e0.sys.Pre = PreflightHints{C: chk0}
	d0 := e0.start(e0.opts())
	if code, b := callOp(t, d0, "/v1/enable"); code != 200 {
		t.Fatalf("没配置第四层时启用应通过：%d %s", code, b)
	}
	for _, f := range chk0.Last().Findings {
		if f.ID == "claude_settings" || f.ID == "claude_proxy_port" {
			t.Fatalf("没配置时不应有第四层的检查项：%+v", f)
		}
	}

	// 端口没在听：启用被拒，应答里有这一项，状态不变
	occ, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	chk := &preflight.Checker{R: cleanRunner{t}}
	e.sys.Pre = PreflightHints{C: chk}
	o := e.opts()
	o.ClaudeProxyAddr = occ.Addr().String()
	o.ClaudeProxyRetry = 20 * time.Millisecond
	presetClaudeProxyOn(t, o.StateDir) // 第四道打开着（v0.68：默认关）
	d := e.start(o)
	code, b := callOp(t, d, "/v1/enable")
	if code != 422 || !strings.Contains(string(b), "本机代理端口被别的程序占着") {
		t.Fatalf("端口没在听时启用应被拒：%d %s", code, b)
	}
	if f, err := e.view.Load(); err != nil || f.State != state.Restored {
		t.Fatalf("状态不应变化：%+v %v", f, err)
	}
	// 托管设置有冲突：被拒，列出文件和变量
	chk.ClaudeSettingsConflicts = func() []string { return []string{"managed-settings.json：HTTPS_PROXY"} }
	code, b = callOp(t, d, "/v1/enable")
	if code != 422 || !strings.Contains(string(b), "managed-settings.json：HTTPS_PROXY") {
		t.Fatalf("托管设置冲突时启用应被拒：%d %s", code, b)
	}
	chk.ClaudeSettingsConflicts = nil
	// 端口释放、重新监听上：启用通过
	occ.Close()
	deadline := time.Now().Add(5 * time.Second)
	for d.claudeProxyDown() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if code, b := callOp(t, d, "/v1/enable"); code != 200 {
		t.Fatalf("端口恢复后启用应通过：%d %s", code, b)
	}
}
