# M4 真机验证脚本的公共函数（M4-9）。用 source 引入，不单独执行。
# 通用部分在 scripts/lib/common.sh（死人开关、还原 trap、空跑、按 PID 文件结束进程、测试用户、脱敏、自测护栏）；
# 地区的快照、还原、读回在 scripts/m4/region.sh；这里只放 M4 的名字和参数（m4-plan v2.13、v2.14）。

# 状态目录放在 /var/run（重启即清空）：abroadd 自己的状态目录在它下面，重启后 abroadd 不会按旧状态重新接管。
M4_STATE=/var/run/claude-abroad-m4
M4_SNAP="/Library/Application Support/claude-abroad-m4-snap"
M4_INSTALL="/Library/Application Support/claude-abroad-m4"
M4_USER=_abroadm4
M4_LABEL=com.claude-abroad.m4.abroadd
M4_PLIST="/Library/LaunchDaemons/${M4_LABEL}.plist"
M4_ANCHOR=com.apple/250.abroad
M4_DNS_KEY=State:/Network/Service/claude-abroad/DNS
M4_RESTORED=0

AB_P=M4
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)/common.sh"
ab_apply_test_state # 只在不用 root 的自测里覆盖 M4_STATE、M4_SNAP（scripts/m4/selftest.sh）
ab_wrap m4

# 由状态目录派生的路径（自测覆盖状态目录之后再算）。
M4_VMDIR="${M4_STATE}/vm"
M4_SECRET="${M4_STATE}/secret"                                   # 0700：本机真实公网 IP（用完即删）
M4_FAKE_ROOT="${M4_STATE}/m4fake"                                # 假 Claude 进程（v2.14 必须改 2）
M4_FAKE_DIR="${M4_FAKE_ROOT}/.local/share/claude/versions/0.0.0-m4fake" # 带 Claude Code 安装位置特征
M4_FAKE_BIN="${M4_FAKE_DIR}/claude"
