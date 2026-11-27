package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claude-abroad/internal/buildinfo"
)

// M2-9b-2 i. 非 root 运行真实模式：非零退出，提示需要 root，状态目录没有被创建（不论其他参数）。
func TestRealModeNeedsRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 运行时不测")
	}
	dir := filepath.Join(t.TempDir(), "st")
	var stderr bytes.Buffer
	args := []string{"--state-dir", dir, "--vm-dir", dir, "--main-proxy-store", dir, "--exit-check-host", "x.test"}
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
	if rc := run([]string{"--state-dir", dir, "extra"}, &stderr, nil); rc != 2 {
		t.Fatal("多余参数应失败")
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
	if v := regionVM(realFlags{vmDir: "/x", skipRegion: true}, lg); v != nil || !strings.Contains(buf.String(), "不同步时区和语言") {
		t.Fatalf("%v %q", v, buf.String())
	}
	if v := regionVM(realFlags{vmDir: "/x"}, lg); v == nil {
		t.Fatal("不带参数时应接虚拟机一侧")
	}
}

// 原地升级（v0.71）：--version 打印 JSON（提交号、构建时间、是否有未提交改动、状态格式版本号），不碰状态目录、不要 root。
func TestVersionFlag(t *testing.T) {
	var out, stderr bytes.Buffer
	old := stdout
	stdout = &out
	defer func() { stdout = old }()
	dir := filepath.Join(t.TempDir(), "st")
	if rc := run([]string{"--version", "--state-dir", dir}, &stderr, nil); rc != 0 {
		t.Fatalf("退出码 %d：%s", rc, stderr.String())
	}
	var v struct {
		Commit      string `json:"commit"`
		BuildTime   string `json:"build_time"`
		Dirty       *bool  `json:"dirty"`
		StateFormat int    `json:"state_format"`
	}
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("不是 JSON：%q", out.String())
	}
	if v.Commit != "unknown" || v.BuildTime != "unknown" || v.Dirty == nil || v.StateFormat != buildinfo.StateFormat {
		t.Fatalf("内容不对：%s", out.String())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("--version 不应创建状态目录")
	}
}
