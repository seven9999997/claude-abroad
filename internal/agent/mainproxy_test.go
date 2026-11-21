package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const sentinel = "SENTINEL-7f3a9c"

type mpFixture struct {
	mp     *MainProxy
	runner *fakeRunner
	logs   *syncBuffer
	srv    *httptest.Server
	cfg    string
	check  func(file string) ([]byte, error)
	active atomic.Int32
	maxAct atomic.Int32
}

func newMP(t *testing.T) *mpFixture {
	dir := t.TempDir()
	f := &mpFixture{runner: newFakeRunner(), logs: &syncBuffer{}, cfg: filepath.Join(dir, "main.yaml")}
	f.runner.ready.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logf := func(format string, a ...any) { _, _ = f.logs.Write([]byte(Redact(sprintf(format, a...)) + "\n")) }
	eng := &Engine{Name: "mihomo", Run: []string{"mihomo"}, ReadyAddr: "127.0.0.1:7890",
		Log: NewRingLog(RingLogBytes), Runner: f.runner, Timing: testEngineTiming, Logf: logf,
		DialReady: func(context.Context, string) error {
			if f.runner.ready.Load() {
				return nil
			}
			return errors.New("no")
		}}
	f.mp = &MainProxy{Path: f.cfg, Engine: eng, Base: ctx, Logf: logf, Wait: 200 * time.Millisecond,
		Check: func(_ context.Context, file string) ([]byte, error) {
			n := f.active.Add(1)
			defer f.active.Add(-1)
			for {
				m := f.maxAct.Load()
				if n <= m || f.maxAct.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
				return nil, errors.New("临时文件应为 0600")
			}
			if f.check != nil {
				return f.check(file)
			}
			return nil, nil
		}}
	f.srv = httptest.NewServer(f.mp)
	t.Cleanup(f.srv.Close)
	return f
}

func sprintf(format string, a ...any) string { return strings.TrimSpace(fmtSprintf(format, a...)) }

func (f *mpFixture) put(t *testing.T, body []byte) (int, string) {
	req, _ := http.NewRequest(http.MethodPut, f.srv.URL, bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return resp.StatusCode, b.String()
}

func (f *mpFixture) startCount() int {
	f.runner.mu.Lock()
	defer f.runner.mu.Unlock()
	return len(f.runner.starts)
}

// yamlWith 返回一份能通过二次校验的配置，节点口令是 secret。
func yamlWith(secret string) []byte {
	return []byte(`mixed-port: 7890
bind-address: 127.0.0.1
allow-lan: false
mode: rule
ipv6: false
dns:
  enable: true
  ipv6: false
  use-hosts: false
  use-system-hosts: false
  default-nameserver: ["192.168.127.1"]
  proxy-server-nameserver: ["192.168.127.1"]
  nameserver: ["https://1.1.1.1/dns-query#CAGE"]
proxies:
  - name: n
    type: socks5
    server: 203.0.113.9
    port: 1080
    password: ` + secret + `
proxy-groups:
  - {name: CAGE, type: select, proxies: [n]}
rules: ["MATCH,CAGE", "MATCH,REJECT"]
`)
}

func TestMainProxySuccess(t *testing.T) {
	f := newMP(t)
	code, body := f.put(t, yamlWith(sentinel))
	if code != 200 || strings.Contains(body, sentinel) {
		t.Fatalf("应成功：%d %s", code, body)
	}
	fi, err := os.Stat(f.cfg)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("main.yaml 应为 0600：%v", err)
	}
	if b, _ := os.ReadFile(f.cfg); !bytes.Equal(b, yamlWith(sentinel)) {
		t.Fatal("main.yaml 内容不对")
	}
	ents, _ := os.ReadDir(filepath.Dir(f.cfg))
	if len(ents) != 1 {
		t.Fatalf("不应留下临时文件：%v", ents)
	}
	if !f.mp.Configured() || f.startCount() != 1 {
		t.Fatal("应标记已配置并启动 mihomo")
	}
	// 再下发一次：替换并重启。
	if code, _ := f.put(t, yamlWith("other")); code != 200 || f.startCount() != 2 {
		t.Fatalf("第二次下发：%d，启动 %d 次", code, f.startCount())
	}
	f.assertNoSentinel(t)
}

func TestMainProxyCheckFailKeepsOld(t *testing.T) {
	f := newMP(t)
	if code, _ := f.put(t, yamlWith("old")); code != 200 {
		t.Fatal("先放一份好配置")
	}
	f.check = func(string) ([]byte, error) {
		// mihomo -t 的报错可能带出配置片段。
		return []byte("parse config error: line 5: password: " + sentinel + " server 203.0.113.9"), errors.New("exit status 1")
	}
	code, body := f.put(t, yamlWith(sentinel))
	if code != http.StatusUnprocessableEntity || strings.Contains(body, sentinel) || strings.Contains(body, "203.0.113.9") {
		t.Fatalf("校验失败应回 422 且原因已脱敏：%d %s", code, body)
	}
	if b, _ := os.ReadFile(f.cfg); !bytes.Equal(b, yamlWith("old")) {
		t.Fatal("校验失败时旧配置不应改变")
	}
	if f.startCount() != 1 {
		t.Fatal("校验失败时不应重启 mihomo")
	}
	f.assertNoSentinel(t)
}

func TestMainProxyLimitsAndTimeout(t *testing.T) {
	f := newMP(t)
	if code, _ := f.put(t, make([]byte, MaxMainProxyBody+1)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超过 4 MiB 应回 413：%d", code)
	}
	if code, _ := f.put(t, nil); code != http.StatusBadRequest {
		t.Fatalf("空配置应回 400：%d", code)
	}
	req, _ := http.NewRequest(http.MethodGet, f.srv.URL, nil)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 405 {
		t.Fatalf("GET 应回 405：%d", resp.StatusCode)
	}
	f.runner.ready.Store(false)
	if code, body := f.put(t, yamlWith("x")); code != http.StatusGatewayTimeout {
		t.Fatalf("端口一直不通应回 504：%d %s", code, body)
	}
}

func TestMainProxySerialized(t *testing.T) {
	f := newMP(t)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); f.put(t, yamlWith("x")) }()
	}
	wg.Wait()
	if f.maxAct.Load() != 1 {
		t.Fatalf("并发下发应串行：最多同时 %d 个", f.maxAct.Load())
	}
}

func (f *mpFixture) assertNoSentinel(t *testing.T) {
	t.Helper()
	if strings.Contains(f.logs.String(), sentinel) {
		t.Error("日志里出现了配置内容")
	}
	lines, _ := f.mp.Engine.Log.Tail(0)
	if strings.Contains(strings.Join(lines, "\n"), sentinel) {
		t.Error("引擎日志缓冲里出现了配置内容")
	}
}

func TestLogsEndpoint(t *testing.T) {
	l := NewRingLog(RingLogBytes)
	for i := 0; i < 10; i++ {
		_, _ = l.Write([]byte("line " + string(rune('0'+i)) + " peer 198.51.100.1\n"))
	}
	srv := httptest.NewServer((&API{State: NewState("k"), Shutdown: func() {}, Logs: map[string]*RingLog{"sing-box": l}}).Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/logs?engine=sing-box&tail=3")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Engine  string   `json:"engine"`
		Lines   []string `json:"lines"`
		Dropped int64    `json:"dropped"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(out.Lines) != 3 || out.Lines[2] != "line 9 peer <已脱敏>" {
		t.Fatalf("logs：%d %+v", resp.StatusCode, out)
	}
	for q, code := range map[string]int{"?engine=nope": 404, "?engine=sing-box&tail=-1": 400, "?engine=sing-box&tail=x": 400} {
		resp, _ := http.Get(srv.URL + "/v1/logs" + q)
		resp.Body.Close()
		if resp.StatusCode != code {
			t.Errorf("%s：应为 %d，实际 %d", q, code, resp.StatusCode)
		}
	}
}

var fmtSprintf = fmt.Sprintf

// 二次校验不通过：不建临时文件、不调 mihomo -t、不重启，原文件和 configured 不变，应答和日志不含配置内容。
func TestMainProxySecondaryValidation(t *testing.T) {
	f := newMP(t)
	if code, _ := f.put(t, yamlWith("old")); code != 200 {
		t.Fatal("先放一份好配置")
	}
	var checks atomic.Int32
	f.check = func(string) ([]byte, error) { checks.Add(1); return nil, nil }
	for name, body := range map[string]string{
		"MATCH,DIRECT":  strings.Replace(string(yamlWith(sentinel)), `rules: ["MATCH,CAGE", "MATCH,REJECT"]`, `rules: ["MATCH,DIRECT", "MATCH,REJECT"]`, 1),
		"mode direct":   strings.Replace(string(yamlWith(sentinel)), "mode: rule", "mode: direct", 1),
		"nameserver 直连": strings.Replace(string(yamlWith(sentinel)), "#CAGE", "", 1),
	} {
		code, resp := f.put(t, []byte(body))
		if code != http.StatusUnprocessableEntity || strings.Contains(resp, sentinel) || strings.Contains(resp, "203.0.113.9") {
			t.Errorf("%s：应回 422 且不回显配置内容：%d %s", name, code, resp)
		}
	}
	if checks.Load() != 0 {
		t.Fatal("二次校验不通过时不应调用 mihomo -t")
	}
	if b, _ := os.ReadFile(f.cfg); !bytes.Equal(b, yamlWith("old")) {
		t.Fatal("原配置应不变")
	}
	if f.startCount() != 1 || !f.mp.Configured() {
		t.Fatal("不应重启 mihomo，configured 不变")
	}
	ents, _ := os.ReadDir(filepath.Dir(f.cfg))
	if len(ents) != 1 {
		t.Fatalf("不应留下临时文件：%v", ents)
	}
	f.assertNoSentinel(t)
}
