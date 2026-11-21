//go:build darwin

package claudeproc

import (
	"os"
	"path/filepath"
	"testing"
)

// c. 真实的信号发送器：非 root 构造时返回错误（不发任何信号）。这是测试代码里唯一调用它的地方（源码检查白名单）。
func TestRealSignalerNeedsRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 运行时不测（会话里不用 sudo）")
	}
	if s, err := NewRealSignaler(); err == nil || s != nil {
		t.Fatal("非 root 应返回错误")
	}
}

// c. 真实的签名查询：只读执行 codesign，对系统自带程序和已安装的 Claude（有的话）各查一次。
func TestCodesignReadOnly(t *testing.T) {
	var s CodesignSigner
	if id, err := s.TeamID("/bin/ls"); err != nil || id != "" {
		t.Fatalf("/bin/ls 是 Apple 平台签名，没有 Team ID：%q %v", id, err)
	}
	if _, err := s.TeamID("relative/path"); err == nil {
		t.Fatal("相对路径应拒绝")
	}
	if _, err := s.TeamID("/nonexistent/claude-abroad-test"); err == nil {
		t.Fatal("文件不存在应返回错误，不当成匹配")
	}
	home, _ := os.UserHomeDir()
	link := filepath.Join(home, ".local", "bin", "claude")
	exe, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Skip("本机没有原生安装的 Claude Code")
	}
	if id, err := s.TeamID(exe); err != nil || id != AnthropicTeamID {
		t.Fatalf("已安装的 Claude Code 应是 Anthropic 签名：%q %v", id, err)
	}
}
