package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"claude-abroad/internal/claudecfg"
)

func TestClaudeSettingsCmd(t *testing.T) {
	var so, se bytes.Buffer
	if code := claudeSettingsCmd([]string{"content"}, &so, &se); code != 0 {
		t.Fatalf("code %d: %s", code, se.String())
	}
	want, _ := claudecfg.Content(claudecfg.DefaultPort)
	if !bytes.Equal(so.Bytes(), want) {
		t.Fatal("应和 abroadd 写入的逐字相同")
	}
	so.Reset()
	if code := claudeSettingsCmd([]string{"content", "--port", "47822"}, &so, &se); code != 0 || !strings.Contains(so.String(), "127.0.0.1:47822") || !json.Valid(so.Bytes()) {
		t.Fatalf("换端口：%d %s", code, so.String())
	}
	for _, bad := range [][]string{nil, {"x"}, {"content", "--port", "0"}, {"content", "--port", "70000"}} {
		if code := claudeSettingsCmd(bad, &so, &se); code != 2 {
			t.Errorf("%v：退出码 %d，应为 2", bad, code)
		}
	}
}
