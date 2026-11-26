package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claude-abroad/internal/claudedomains"
)

// M6-12：已启用时，经网络栈（这里是本机代理端口那条路，和接管流量同一个判定入口）识别为 Claude 的连接，
// 不在内置后缀之下的域名被记下；内置后缀、别的应用不记。文件 0600、不含地址；诊断包默认只有条数，显式选择才有清单；
// 清空之后为空（卸载之后也能清空）。
func TestClaudeDomainsRecorded(t *testing.T) {
	c := newCPEnv(t)
	for _, x := range []struct {
		base   int
		target string
	}{
		{24000, "www.example.org:443"},               // Claude、普通域名：记
		{24000, "claude.ai:443"},                     // Claude、内置后缀：不记
		{24000, "a.frame.claudeusercontent.com:443"}, // 新追加的内置后缀：不记
		{22000, "other.example.net:443"},             // 别的应用：不记
	} {
		code, conn := c.connect(t, x.base, x.target)
		if code != 200 || !echoOK(conn) {
			t.Fatalf("%s：%d", x.target, code)
		}
		conn.Close()
	}
	_, raw := call(t, c.d, "GET", "/v1/claude-domains", nil)
	var v ClaudeDomainsView
	if json.Unmarshal(raw, &v) != nil || v.Count != 1 || v.Domains[0].Domain != "www.example.org" || v.Note == "" {
		t.Fatalf("只应记 Claude 访问的普通域名：%s", raw)
	}
	fi, err := os.Stat(filepath.Join(c.dir, claudedomains.FileName))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("新域名应马上落盘、0600：%v %v", fi, err)
	}
	// 诊断包：默认只有条数；显式选择才有清单
	_, raw = call(t, c.d, "GET", "/v1/diagnostics", nil)
	if !strings.Contains(string(raw), `"claude_domains": {`) || strings.Contains(string(raw), "www.example.org") {
		t.Fatalf("诊断包默认不应带清单：%s", raw)
	}
	_, raw = call(t, c.d, "GET", "/v1/diagnostics?include_domains=1", nil)
	if !strings.Contains(string(raw), "www.example.org") {
		t.Fatalf("显式选择时应带清单")
	}
	// 清空
	if code, raw := call(t, c.d, "DELETE", "/v1/claude-domains", nil); code != 200 || !strings.Contains(string(raw), `"count":0`) {
		t.Fatalf("清空：%d %s", code, raw)
	}
	if _, err := os.Stat(filepath.Join(c.dir, claudedomains.FileName)); !os.IsNotExist(err) {
		t.Fatal("清空后文件应删掉")
	}
	if !AllowedAfterUninstall["claude-domains-clear"] {
		t.Fatal("卸载之后也应能清空")
	}
}

// 没有数据面（假系统的基础环境）：接口照常回空清单。
func TestClaudeDomainsWithoutDataPlane(t *testing.T) {
	e := newEnv(t)
	d := e.start(e.opts())
	_, raw := call(t, d, "GET", "/v1/claude-domains", nil)
	if !strings.Contains(string(raw), `"count":0`) || !strings.Contains(string(raw), `"domains":[]`) {
		t.Fatalf("%s", raw)
	}
}
