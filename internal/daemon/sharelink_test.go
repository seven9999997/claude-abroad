package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claude-abroad/internal/exitconf"
)

// M6-6c：分享链接导入成分流出口：存成订阅出口（Clash），原始链接不保存；不合法的 422 不回显链接内容；应答里没有密码。
func TestShareLinkExit(t *testing.T) {
	var dir string
	e := newExitEnv(t, func(d string) { dir = d })
	link := "trojan://TrSecret77@tr.example.com:443?sni=tr.example.com#我的节点"
	code, msg := putExit(t, e.d, "tr", exitconf.ShareLink, link)
	if code != 200 || strings.Contains(msg, "TrSecret77") {
		t.Fatalf("%d %s", code, msg)
	}
	b, err := os.ReadFile(filepath.Join(dir, SecretsDir, "exit-tr.json"))
	if err != nil {
		t.Fatal(err)
	}
	var x StoredExit
	if json.Unmarshal(b, &x) != nil || x.Kind != exitconf.Clash || strings.Contains(x.Conf, "trojan://") {
		t.Fatalf("应存成订阅出口、不存原始链接：%s", b)
	}
	if _, err := exitconf.ParseClash([]byte(x.Conf)); err != nil {
		t.Fatalf("存下的配置应能通过订阅出口的校验：%v", err)
	}
	_, raw := call(t, e.d, "GET", "/v1/exits/tr", nil)
	if strings.Contains(string(raw), "TrSecret77") {
		t.Fatalf("应答里不应有密码：%s", raw)
	}
	code, msg = putExit(t, e.d, "bad", exitconf.ShareLink, "trojan://BadSecret88@tr.example.com#x")
	if code != 422 || strings.Contains(msg, "BadSecret88") || !strings.Contains(msg, "第 1 行") {
		t.Fatalf("不合法应 422、指出行号、不回显：%d %s", code, msg)
	}
}
