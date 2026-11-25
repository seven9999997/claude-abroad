# M6-9 开发安装的端到端（真机）的公共部分。用 source 引入，不单独执行。
# 通用部分在 scripts/lib/common.sh（死人开关、还原 trap、空跑、自测护栏）；地区的快照、还原、读回复用 scripts/m4/region.sh。
# 安装、卸载调用 scripts/m6/install（它们有自己的快照和对照）。

# 状态目录跨重启保留（第二段在重启之后）。
M6E_STATE="/Library/Application Support/claude-abroad-m6e"
M6E_SNAP="/Library/Application Support/claude-abroad-m6e-snap" # common.sh 的约定；本脚本不用
M6E_SOCK=/var/run/claude-abroad
M6E_ANCHOR=com.apple/250.abroad
M6E_GUARD_LOG=/var/log/claude-abroad/guard.log
M6E_RESTORED=0

AB_P=M6E
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/../../lib" && pwd)/common.sh"
ab_apply_test_state # 只在不用 root 的自测里覆盖 M6E_STATE
ab_wrap m6e

m4_do() { m6e_do "$@"; }
m4_log() { m6e_log "$@"; }
M4_CMD_SYSTEMSETUP="${M6E_CMD_SYSTEMSETUP:-}"
M4_CMD_DEFAULTS="${M6E_CMD_DEFAULTS:-}"
M4_CMD_LAUNCHCTL="${M6E_CMD_LAUNCHCTL:-}"
[ -n "${M4_CMD_SYSTEMSETUP}" ] || unset M4_CMD_SYSTEMSETUP
[ -n "${M4_CMD_DEFAULTS}" ] || unset M4_CMD_DEFAULTS
[ -n "${M4_CMD_LAUNCHCTL}" ] || unset M4_CMD_LAUNCHCTL
