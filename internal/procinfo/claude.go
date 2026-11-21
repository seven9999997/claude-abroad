package procinfo

import (
	"path"
	"strings"

	"claude-abroad/internal/rules"
)

// MaxAncestors 是判定 Claude 和“继承父进程”时往上找的最多级数（m3-plan v2.4 补充 5、6）。
const MaxAncestors = 16

const claudeCodePkg = "@anthropic-ai/claude-code"

// isClaudeProc 判断单个进程是否属于 Claude（design 10.1）：应用 ID 命中 rules.IsClaudeApp，
// 或者可执行文件是 node、bun，并且命令行参数里有 @anthropic-ai/claude-code，或有文件名是 claude 的参数
// （npm 全局安装时进程是 node，参数是 /usr/local/bin/claude 这个符号链接）。
func isClaudeProc(p Proc, app rules.AppID) bool {
	if rules.IsClaudeApp(app) || rules.IsClaudeApp(rules.AppID("path:"+p.Path)) {
		return true
	}
	switch path.Base(p.Path) {
	case "node", "bun":
		for _, a := range p.Args {
			if strings.Contains(a, claudeCodePkg) || strings.HasPrefix(a, "/") && path.Base(a) == "claude" {
				return true
			}
		}
	}
	return false
}
