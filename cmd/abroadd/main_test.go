package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M2-9b-2 i. 非 root 运行不带 --fake-system：非零退出，提示需要 root，状态目录没有被创建（不论其他参数）。
func TestRealModeNeedsRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 运行时不测")
	}
	dir := filepath.Join(t.TempDir(), "st")
	var stderr bytes.Buffer
	args := []string{"--state-dir", dir, "--vm-dir", dir, "--main-proxy-store", dir, "--exit-check-host", "x.test", "--dev-skip-claude-kill"}
	if rc := run(args, &stderr, nil); rc == 0 {
		t.Fatal("应以非零退出")
	}
	if !strings.Contains(stderr.String(), "root") {
		t.Fatalf("%s", stderr.String())
	}
	stderr.Reset()
	if rc := run([]string{"--state-dir", dir}, &stderr, nil); rc == 0 || !strings.Contains(stderr.String(), "root") {
		t.Fatalf("%d %s", rc, stderr.String())
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("不应创建状态目录")
	}
	if rc := run([]string{"--fake-system"}, &stderr, nil); rc == 0 {
		t.Fatal("没有 --state-dir 应失败")
	}
	if rc := run([]string{"--fake-system", "--state-dir", dir, "extra"}, &stderr, nil); rc == 0 {
		t.Fatal("多余参数应失败")
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

// M2-9b-2 i. 真实模式参数检查（纯函数）：缺必填参数拒绝。M4-4 起不再要求 --dev-skip-claude-kill（有了真实实现）。
func TestCheckRealFlags(t *testing.T) {
	ok := realFlags{vmDir: "v", mainStore: "m", exitHost: "h", pfUser: "_abroad", skipKill: true}
	if err := checkRealFlags(ok); err != nil {
		t.Fatal(err)
	}
	noSkip := ok
	noSkip.skipKill = false
	if err := checkRealFlags(noSkip); err != nil {
		t.Fatalf("不带跳过参数也应通过参数检查：%v", err)
	}
	bad := map[string]func(*realFlags){
		"缺 vm-dir": func(f *realFlags) { f.vmDir = "" },
		"缺主代理存储":   func(f *realFlags) { f.mainStore = "" },
		"缺检查主机":    func(f *realFlags) { f.exitHost = "" },
		"缺放行用户":    func(f *realFlags) { f.pfUser = "" },
	}
	for name, mut := range bad {
		f := ok
		mut(&f)
		err := checkRealFlags(f)
		if err == nil {
			t.Errorf("%s 应被拒绝", name)
		}
	}
}

// v2.5 建议改 3：带 --dev-skip-region 时不接地区同步（虚拟机一侧为 nil，NewRealSystem 因此不接 Mac 一侧），有醒目日志。
func TestRegionVMSkip(t *testing.T) {
	var buf strings.Builder
	lg := log.New(&buf, "", 0)
	if v := regionVM(realFlags{vmDir: "/x", skipRegion: true}, lg); v != nil || !strings.Contains(buf.String(), "--dev-skip-region") {
		t.Fatalf("%v %q", v, buf.String())
	}
	if v := regionVM(realFlags{vmDir: "/x"}, lg); v == nil {
		t.Fatal("不带参数时应接虚拟机一侧")
	}
}
