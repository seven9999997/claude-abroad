package main

import (
	"bytes"
	"strings"
	"testing"
)

// g. 非 root 在做任何事之前退出；参数不对、Windows 上各自退出；没有任何开发参数。
func TestEntry(t *testing.T) {
	var w bytes.Buffer
	if c := run([]string{"--state-dir", "/x", "--run-dir", "/y"}, &w, false, "darwin"); c != ExitNotRoot || !strings.Contains(w.String(), "root") {
		t.Fatalf("非 root：%d %s", c, w.String())
	}
	for _, args := range [][]string{
		{}, {"--state-dir", "/x"}, {"--run-dir", "/y"}, {"--state-dir", "x", "--run-dir", "/y"},
		{"--state-dir", "/x", "--run-dir", "/y", "extra"}, {"--state-dir", "/x", "--run-dir", "/y", "--fake-system"},
		{"--state-dir", "/x", "--run-dir", "/y", "--dev-skip"},
	} {
		w.Reset()
		if c := run(args, &w, true, "darwin"); c != 2 {
			t.Errorf("%v 应按参数不对退出：%d", args, c)
		}
	}
	w.Reset()
	if c := run([]string{"--state-dir", "/x", "--run-dir", "/y"}, &w, true, "windows"); c == 0 || !strings.Contains(w.String(), "macOS") {
		t.Fatalf("Windows：%d %s", c, w.String())
	}
	// 帮助里只有两个参数
	w.Reset()
	_, _ = parse([]string{"-h"}, &w)
	if strings.Contains(w.String(), "dev") || strings.Contains(w.String(), "fake") {
		t.Fatalf("不应有开发参数：%s", w.String())
	}
}
