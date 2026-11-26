package daemon

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"claude-abroad/internal/exitcheck"
)

// M6-6a 补审返工：按真实流程走一遍，诊断包里搜不到订阅的主机名、节点的服务器域名、分享链接的服务器域名、局域网节点的
// 地址（以及密码一类）。流程：添加来源（本地配置、订阅抓取失败）、刷新失败、应用主代理成功和失败、出口检查不通过、
// 导入分享链接出口、出口检查不通过。
func TestDiagnosticsNoHostnamesRealFlows(t *testing.T) {
	m := newMainEnv(t, false)
	id := m.addYAML(t, "本地", mainNodesYAML) // srvmark.example.com、relay.example.com、局域网 192.168.1.20、密码标记
	// 订阅：抓取失败（添加、刷新各一次）
	m.fetch.mu.Lock()
	m.fetch.err = errors.New("拉取 https://subhostmark.example.net/api/v1/client/subscribe?token=SUBTOKENMARK 失败：连接 198.51.100.44:443 超时")
	m.fetch.mu.Unlock()
	sb, _ := json.Marshal(AddSourceRequest{Kind: "url", Name: "订阅", URL: "https://subhostmark.example.net/api/v1/client/subscribe?token=SUBTOKENMARK", ViaHost: true})
	_, _ = call(t, m.d, "POST", "/v1/main-proxy/sources", sb)
	rb, _ := json.Marshal(RefreshRequest{ViaHost: true})
	_, _ = call(t, m.d, "POST", "/v1/main-proxy/sources/"+id+"/refresh", rb)
	// 应用主代理成功、启用
	m.enableWith(t, "exit2")
	// 换主代理失败（检查不通过）
	m.pr.mu.Lock()
	r := passReport()
	r.Items[2] = exitcheck.Item{Name: "mainland", Result: exitcheck.Fail, Cause: exitcheck.CauseMainland}
	m.pr.rep = r
	m.pr.mu.Unlock()
	_, _ = m.sel(t, MainSelectRequest{Selected: m.nodeID(t, "relay")})
	// 导入分享链接出口，单独检查一次（不通过）
	link := "trojan://SLPWDMARK@slhostmark.example.org:443?sni=slhostmark.example.org#hk"
	eb, _ := json.Marshal(exitPutReq{Kind: "sharelink", Conf: link})
	if code, raw := call(t, m.d, "PUT", "/v1/exits/hk", eb); code != 200 {
		t.Fatalf("导入分享链接：%d %s", code, raw)
	}
	_, _ = call(t, m.d, "POST", "/v1/exits/hk/check", nil)
	time.Sleep(100 * time.Millisecond)
	_, raw := call(t, m.d, "GET", "/v1/diagnostics", nil)
	diag := string(raw)
	// 流程确实走到了：有分享链接出口、有订阅抓取失败和换主代理失败的事件（否则断言“搜不到”没有意义）
	var dg Diagnostics
	if json.Unmarshal(raw, &dg) != nil {
		t.Fatalf("%s", raw)
	}
	evs, _ := json.Marshal(dg.Status.Events)
	t.Logf("诊断包里的事件：%s", evs)
	hasExit := false
	for _, x := range dg.Status.Exits {
		hasExit = hasExit || x.Tag == "hk"
	}
	if !hasExit || len(dg.Status.Events) < 5 {
		t.Fatalf("流程没走到：出口 hk %v，事件 %d 条", hasExit, len(dg.Status.Events))
	}
	for _, mark := range append(mainMarkers, "subhostmark", "SUBTOKENMARK", "198.51.100.44", "slhostmark", "SLPWDMARK",
		"relay.example.com", "192.168.1.20", "srvmark.example.com") {
		if strings.Contains(diag, mark) {
			i := strings.Index(diag, mark)
			lo, hi := max(0, i-120), min(len(diag), i+80)
			t.Errorf("诊断包里有 %q：…%s…", mark, diag[lo:hi])
		}
	}
}
