package claudeproc

import "testing"

// M4-9：按路径分类与扫描一致——安装位置特征算 Claude Code（不看签名）；只是文件名叫 claude 不算；
// 带 Anthropic 签名的 claude 算；签名查不了时列为“没能确认”。
func TestClassifyPath(t *testing.T) {
	sig := &FakeSigner{Teams: map[string]string{"/usr/local/bin/claude": AnthropicTeamID}, Err: map[string]bool{"/opt/x/claude": true}}
	s := &Scanner{Signer: sig}
	for _, c := range []struct {
		path        string
		match       bool
		reason, unv string
	}{
		{"/var/run/claude-abroad-m4/m4fake/.local/share/claude/versions/0.0.0-m4fake/claude", true, "安装位置", ""},
		{"/Users/u/.vscode/extensions/anthropic.claude-code-1.0.0/resources/native-binary/claude", true, "安装位置", ""},
		{"/usr/local/bin/claude", true, "签名", ""},
		{"/tmp/claude", false, "", ""},
		{"/opt/x/claude", false, "", noteUnsigned},
		{"/bin/sleep", false, "", ""},
	} {
		v := s.ClassifyPath(c.path)
		if v.Match != c.match || v.Reason != c.reason || v.Unverified != c.unv {
			t.Errorf("%s：%+v", c.path, v)
		}
	}
	if len(InstallMarkers()) < 5 {
		t.Fatal("安装位置特征")
	}
}
