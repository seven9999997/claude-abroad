package rules

import (
	"path"
	"strings"
)

// Claude 的应用清单（design 10.1）。procinfo 用同一份清单判定 Who.Claude。
// M3 只按 Bundle ID、路径和命令行识别，代码签名校验在 M6（design 5.3）。

// ClaudeBundleIDs 是 Claude 桌面版的 Bundle ID。
var ClaudeBundleIDs = []string{"com.anthropic.claudefordesktop"}

// claudePathMarks 是 Claude Code 的安装位置特征（路径里含有即算）。
var claudePathMarks = []string{"/@anthropic-ai/claude-code/", "/.claude/local/"}

// IsClaudeApp 判断应用 ID 是否属于 Claude。宁可多算：
//   - Bundle ID 在清单里，或以 com.anthropic. 开头；
//   - path: 的文件名是 claude，或路径带 Claude Code 的安装位置特征。
func IsClaudeApp(id AppID) bool {
	s := string(id)
	if p, ok := strings.CutPrefix(s, "path:"); ok {
		if path.Base(p) == "claude" {
			return true
		}
		for _, m := range claudePathMarks {
			if strings.Contains(p+"/", m) {
				return true
			}
		}
		return false
	}
	if strings.Contains(s, ":") {
		return false
	}
	ls := strings.ToLower(s)
	for _, b := range ClaudeBundleIDs {
		if ls == b {
			return true
		}
	}
	return strings.HasPrefix(ls, "com.anthropic.")
}
