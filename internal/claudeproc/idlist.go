package claudeproc

import "strings"

// Anthropic 的签名和桌面版的 Bundle ID 清单（m6-plan v2.6 决定 4）：回国用的判定（Scanner）和启动管控用的
// 自动结束判定（AutoJudge）共用这一份，有测试防止两边不一致。

// AnthropicTeamID 是 Anthropic 的代码签名 Team ID（2026-11-20 在本机对原生安装的 Claude Code 只读执行
// codesign -dv 得到，design 15 R17）。
const AnthropicTeamID = "Q6L2SF6YDW"

// DesktopBundleID 是 Claude 桌面版的 Bundle ID（design 10.1）。
const DesktopBundleID = "com.anthropic.claudefordesktop"

// TeamIDs 是 Anthropic 的代码签名 Team ID。
var TeamIDs = []string{AnthropicTeamID}

// DesktopBundleIDs 是 Claude 桌面版的 Bundle ID（Helper 在桌面版的 .app 里，按外层的 .app 取 Bundle ID）。
var DesktopBundleIDs = []string{DesktopBundleID}

// IsAnthropicTeam 判断 Team ID 是不是 Anthropic 的。
func IsAnthropicTeam(id string) bool {
	for _, t := range TeamIDs {
		if id == t {
			return true
		}
	}
	return false
}

// IsDesktopBundle 判断 Bundle ID 是不是桌面版的（不区分大小写，procinfo 取到的已是小写）。
func IsDesktopBundle(id string) bool {
	for _, b := range DesktopBundleIDs {
		if strings.EqualFold(id, b) {
			return true
		}
	}
	return false
}
