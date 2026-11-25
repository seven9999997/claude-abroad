//go:build !abroaddev

package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// i. 默认（发布）构建里没有任何能打开开发接口的参数；开发标记只出现在带 abroaddev 构建标签的文件里。
func TestNoDevSwitchInDefaultBuild(t *testing.T) {
	for _, f := range []string{"--dev-agent", "--outbound-open"} {
		if _, err := parseFlags([]string{"--state-dir", "x", f}, io.Discard); err == nil {
			t.Fatalf("默认构建不应有 %s", f)
		}
	}
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		dev := strings.Contains(string(b), "//go:build abroaddev")
		if strings.Contains(string(b), "devAgent = true") || strings.Contains(string(b), "outboundOpen = true") ||
			(!dev && (strings.Contains(string(b), `"dev-agent"`) || strings.Contains(string(b), `"outbound-open"`) || strings.Contains(string(b), "--outbound-open"))) {
			t.Errorf("%s：开发开关只能出现在带 abroaddev 构建标签的文件里", f)
		}
	}
	// abroadd 的参数里同样没有。
	b, _ := os.ReadFile("../abroadd/main.go")
	if strings.Contains(string(b), "dev-agent") || strings.Contains(string(b), "CmdlineDev") || strings.Contains(string(b), "dev-exits") {
		t.Fatal("abroadd 不应有打开开发接口的参数")
	}
}
