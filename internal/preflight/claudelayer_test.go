package preflight

import "testing"

// v0.68：第四道关着（默认）时，和它有关的两项（别的托管设置定义了代理变量、本机代理端口被占用）都不出现；打开时照旧阻断。
func TestClaudeLayerGate(t *testing.T) {
	c := &Checker{
		ClaudeSettingsConflicts: func() []string { return []string{"corp.json：HTTPS_PROXY"} },
		ClaudeProxyDown:         func() bool { return true },
	}
	if got := c.checkClaudeCode(); len(got) != 2 {
		t.Fatalf("没有开关（老行为）时应有两项：%+v", got)
	}
	on := false
	c.ClaudeLayerOn = func() bool { return on }
	if got := c.checkClaudeCode(); len(got) != 0 {
		t.Fatalf("关着时不应有：%+v", got)
	}
	on = true
	got := c.checkClaudeCode()
	if len(got) != 2 || got[0].ID != "claude_settings" || got[1].ID != "claude_proxy_port" || got[0].Severity != Block || got[1].Severity != Block {
		t.Fatalf("打开时应照旧阻断：%+v", got)
	}
}
