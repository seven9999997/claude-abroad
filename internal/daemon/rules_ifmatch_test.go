package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"claude-abroad/internal/api"
	"claude-abroad/internal/resolverlock"
	"claude-abroad/internal/rules"
)

// doRules 发规则请求，可带 If-Match；返回状态码、应答体和 ETag。
func doRules(t *testing.T, d *Daemon, method, path, ifMatch string, body []byte) (int, []byte, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, method, "http://abroadd"+path, strings.NewReader(string(body)))
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", d.SocketPath())
	}, DisableKeepAlives: true}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header.Get("ETag")
}

// 补齐一（architect 2026-11-27）验收 j：规则的条件写入。
func TestRulesConditionalWrite(t *testing.T) {
	var c callers
	e := newEnv(t)
	o := e.opts()
	c.opts(&o)
	d := e.start(o)
	c.as(os.Getuid())
	_, y, tagY := doRules(t, d, "GET", "/v1/rules", "", nil)
	_, _, tagJ := doRules(t, d, "GET", "/v1/rules?format=json", "", nil)
	if tagY == "" || tagY != tagJ {
		t.Fatalf("YAML 和 JSON 应给同一个标识：%q %q", tagY, tagJ)
	}
	tighten := []byte("default: proxy:main\nrules:\n  - {app: com.example.a, action: block}\n")
	// 带当前标识：通过，标识变了
	if code, b, _ := doRules(t, d, "PUT", "/v1/rules", tagY, tighten); code != 200 {
		t.Fatalf("带当前标识应通过：%d %s", code, b)
	}
	_, after, tag2 := doRules(t, d, "GET", "/v1/rules", "", nil)
	if tag2 == tagY {
		t.Fatal("内容变了标识应变")
	}
	// 同样内容再提交：标识不变
	if code, _, _ := doRules(t, d, "PUT", "/v1/rules", tag2, tighten); code != 200 {
		t.Fatal("同样内容带当前标识应通过")
	}
	if _, _, tag3 := doRules(t, d, "GET", "/v1/rules", "", nil); tag3 != tag2 {
		t.Fatalf("同样内容提交两次标识应不变：%s %s", tag2, tag3)
	}
	fileBefore, _ := os.ReadFile(filepath.Join(e.dir, RulesFile))
	// 带旧标识：412 rules_changed，文件和内存都不变
	other := []byte("default: proxy:main\nrules:\n  - {app: com.example.b, action: block}\n")
	code, b, _ := doRules(t, d, "PUT", "/v1/rules", tagY, other)
	if code != http.StatusPreconditionFailed || codeOf(t, b).Code != api.CodeRulesChanged {
		t.Fatalf("旧标识应 412 rules_changed：%d %s", code, b)
	}
	if _, now, _ := doRules(t, d, "GET", "/v1/rules", "", nil); string(now) != string(after) {
		t.Fatal("冲突时内存里的规则不应变")
	}
	if fb, _ := os.ReadFile(filepath.Join(e.dir, RulesFile)); string(fb) != string(fileBefore) {
		t.Fatal("冲突时规则文件不应变")
	}
	// 带旧标识、内容是放松、调用方没有授权：得到的是“规则已变”，不是“需要授权”
	c.as(consoleTestUID)
	relax := []byte("default: proxy:main\nlocal_domains: [\"*.corp.example\"]\n")
	if code, b, _ := doRules(t, d, "PUT", "/v1/rules", tagY, relax); code != http.StatusPreconditionFailed || codeOf(t, b).Code != api.CodeRulesChanged {
		t.Fatalf("旧标识的放松应先判冲突：%d %s", code, b)
	}
	// 当前标识、放松、没有授权：需要授权（分级照旧）
	if code, b, _ := doRules(t, d, "PUT", "/v1/rules", tag2, relax); code != http.StatusForbidden || codeOf(t, b).Code != api.CodeNeedsAuthorization {
		t.Fatalf("放松应要授权：%d %s", code, b)
	}
	// 不带标识：行为和以前相同（收紧照常通过）
	if code, b, _ := doRules(t, d, "PUT", "/v1/rules", "", other); code != 200 {
		t.Fatalf("不带 If-Match 应照旧：%d %s", code, b)
	}
	// 弱标识、带引号的形式都认
	_, _, cur := doRules(t, d, "GET", "/v1/rules", "", nil)
	if code, b, _ := doRules(t, d, "PUT", "/v1/rules", "W/"+cur, other); code != 200 {
		t.Fatalf("W/ 形式应认：%d %s", code, b)
	}
	_ = y
}

// 验收 m：状态里的内置置顶名单和规则引擎、解析锁是同一个来源。
func TestStatusBuiltinPinned(t *testing.T) {
	e := newEnv(t)
	d := e.start(e.opts())
	_, b := call(t, d, "GET", "/v1/status", nil)
	var st struct {
		BuiltinPinned []string `json:"builtin_pinned"`
	}
	if json.Unmarshal(b, &st) != nil || len(st.BuiltinPinned) == 0 {
		t.Fatalf("%s", b)
	}
	if !slices.Equal(st.BuiltinPinned, rules.BuiltinPinnedBases()) || !slices.Equal(st.BuiltinPinned, resolverlock.Domains()) {
		t.Fatalf("应和规则引擎、解析锁同一个来源：%v", st.BuiltinPinned)
	}
}
