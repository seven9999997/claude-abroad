package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"claude-abroad/internal/claudeproc"
)

// M4-9 验收 n：只读识别子命令认假 Claude 进程的路径（安装位置特征），不认只是叫 claude 的文件；输出把主目录换成 ~。
func TestClaudeFacts(t *testing.T) {
	sc := &claudeproc.Scanner{Signer: &claudeproc.FakeSigner{Teams: map[string]string{"/Users/u/bin/claude": claudeproc.AnthropicTeamID}}}
	var out, errb bytes.Buffer
	fake := "/var/run/claude-abroad-m4/m4fake/.local/share/claude/versions/0.0.0-m4fake/claude"
	if code := claudeFactsRun([]string{"--path", fake, "--path", "/tmp/claude", "--path", "/Users/u/bin/claude"}, sc, "/Users/u", &out, &errb); code != 0 {
		t.Fatalf("%d %s", code, errb.String())
	}
	var got []PathFact
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got) != 3 {
		t.Fatalf("%v %s", err, out.String())
	}
	if !got[0].Match || got[0].App != "Claude Code" || got[0].Reason != "安装位置" {
		t.Errorf("假 Claude 进程的路径应算 Claude Code（依据安装位置）：%+v", got[0])
	}
	if got[1].Match {
		t.Errorf("只是叫 claude 的文件不算：%+v", got[1])
	}
	if !got[2].Match || got[2].Path != "~/bin/claude" || got[2].Reason != "签名" {
		t.Errorf("主目录换成 ~，签名对的算：%+v", got[2])
	}
	out.Reset()
	if code := claudeFactsRun([]string{"--facts"}, sc, "", &out, &errb); code != 0 || !strings.Contains(out.String(), claudeproc.AnthropicTeamID) || !strings.Contains(out.String(), "/.local/share/claude/versions/") {
		t.Fatalf("%d %s", code, out.String())
	}
	out.Reset()
	if code := claudeFactsRun([]string{"--team-of", "/Users/u/bin/claude"}, sc, "/Users/u", &out, &errb); code != 0 || !strings.Contains(out.String(), `"path":"~/bin/claude"`) {
		t.Fatalf("%d %s", code, out.String())
	}
	for _, bad := range [][]string{nil, {"--facts", "--count"}, {"--path"}, {"x"}} {
		if code := claudeFactsRun(bad, sc, "", &out, &errb); code != 2 {
			t.Errorf("%v：应是参数错（%d）", bad, code)
		}
	}
}
