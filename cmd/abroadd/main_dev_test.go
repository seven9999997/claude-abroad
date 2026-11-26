//go:build abroaddev

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 开发构建才有的假系统模式和开发参数（M6-1）。

func TestDevFlagsPresent(t *testing.T) {
	var stderr bytes.Buffer
	dir := filepath.Join(t.TempDir(), "st")
	if rc := run([]string{"--fake-system"}, &stderr, nil); rc == 0 {
		t.Fatal("没有 --state-dir 应失败")
	}
	if rc := run([]string{"--fake-system", "--state-dir", dir, "extra"}, &stderr, nil); rc == 0 {
		t.Fatal("多余参数应失败")
	}
	if os.Geteuid() != 0 {
		stderr.Reset()
		args := []string{"--state-dir", dir, "--vm-dir", dir, "--main-proxy-store", dir, "--exit-check-host", "x.test", "--dev-skip-claude-kill", "--dev-skip-region", "--dev-resolver-test-domain"}
		if rc := run(args, &stderr, nil); rc == 0 || !strings.Contains(stderr.String(), "root") || strings.Contains(stderr.String(), "not defined") {
			t.Fatalf("开发构建认识开发参数，走到真实模式的 root 检查：%d %s", rc, stderr.String())
		}
	}
}

// 带 --fake-system：启动、查状态、收到 SIGTERM 后走退出流程，socket 删除。
func TestFakeSystemRun(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "st")
	var stderr bytes.Buffer
	var sock string
	rc := run([]string{"--fake-system", "--state-dir", dir}, &stderr, func(s string, stop func()) {
		sock = s
		c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", s)
		}}}
		resp, err := c.Get("http://abroadd/v1/status")
		if err != nil {
			t.Error(err)
		} else {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || !strings.Contains(string(b), `"state":"RESTORED"`) {
				t.Errorf("%d %s", resp.StatusCode, b)
			}
		}
		stop()
	})
	if rc != 0 {
		t.Fatalf("退出码 %d：%s", rc, stderr.String())
	}
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("socket 应已删除")
	}
}

// M5-8：--fake-main-proxy 接上假的出口检查和假虚拟机：应用主代理后引导的“主代理”一步完成；
// 不和 --fake-system 一起用时拒绝。
func TestFakeMainProxyRun(t *testing.T) {
	var stderr bytes.Buffer
	if rc := run([]string{"--fake-main-proxy", "--state-dir", filepath.Join(t.TempDir(), "x")}, &stderr, nil); rc != 2 || !strings.Contains(stderr.String(), "--fake-system") {
		t.Fatalf("单独用应拒绝：%d %s", rc, stderr.String())
	}
	dir := filepath.Join(t.TempDir(), "st")
	rc := run([]string{"--fake-system", "--fake-main-proxy", "--state-dir", dir}, &stderr, func(s string, stop func()) {
		defer stop()
		c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", s)
		}}}
		do := func(method, path string) (int, string) {
			req, _ := http.NewRequest(method, "http://abroadd"+path, nil)
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, string(b)
		}
		if _, b := do("GET", "/v1/setup"); !strings.Contains(b, `{"id":"main_proxy","done":false}`) {
			t.Errorf("应用主代理之前：%s", b)
		}
		if code, b := do("POST", "/v1/main-proxy/apply"); code != 200 {
			t.Errorf("应用主代理：%d %s", code, b)
		}
		if _, b := do("GET", "/v1/setup"); !strings.Contains(b, `{"id":"main_proxy","done":true}`) {
			t.Errorf("应用主代理之后：%s", b)
		}
		if code, b := do("POST", "/v1/enable"); code != 200 && code != 409 {
			t.Errorf("启用：%d %s", code, b)
		}
	})
	if rc != 0 {
		t.Fatalf("退出码 %d：%s", rc, stderr.String())
	}
}

// M6-4b-1 返工：--dev-fake-claude-dir 的组合检查（在取生产执行器之前就按参数错退出）。
func TestFakeClaudeDirChecks(t *testing.T) {
	dir := t.TempDir()
	for _, c := range [][]string{{"--dev-fake-claude-dir", "rel"}, {"--dev-fake-claude-dir", "/"}, {"--dev-fake-claude-dir", "/fake", "--dev-skip-claude-kill"}} {
		var stderr bytes.Buffer
		if rc := run(append([]string{"--state-dir", dir}, c...), &stderr, nil); rc != 2 || !strings.Contains(stderr.String(), "--dev-fake-claude-dir") {
			t.Errorf("%v：应按参数错退出：%d %s", c, rc, stderr.String())
		}
	}
}
