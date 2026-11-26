//go:build !abroaddev

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M6-1 验收 a：默认（发布）构建里没有假系统模式和任何开发参数：帮助里没有，传入时按“不认识的参数”退出（退出码 2）。
func TestNoDevFlagsInReleaseBuild(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "st")
	for _, f := range []string{"--fake-system", "--fake-main-proxy", "--dev-skip-claude-kill", "--dev-skip-region", "--dev-resolver-test-domain", "--dev-fake-claude-dir"} {
		var stderr bytes.Buffer
		if rc := run([]string{"--state-dir", dir, f}, &stderr, nil); rc != 2 || !strings.Contains(stderr.String(), "flag provided but not defined") {
			t.Errorf("%s：应按不认识的参数退出，得到 %d %s", f, rc, stderr.String())
		}
	}
	var help bytes.Buffer
	run([]string{"-h"}, &help, nil)
	for _, f := range []string{"fake-system", "fake-main-proxy", "dev-skip", "dev-"} {
		if strings.Contains(help.String(), f) {
			t.Errorf("帮助里出现了 %s：%s", f, help.String())
		}
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("不应创建状态目录")
	}
}
