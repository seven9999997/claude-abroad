# M5 第二级真机走查脚本的公共函数（M5-11，m5-plan v2.11 决定 1）。用 source 引入，不单独执行。
# 通用部分在 scripts/lib/common.sh（死人开关、还原 trap、空跑、按 PID 文件结束进程、测试用户、脱敏、自测护栏）；
# 地区的快照、还原、读回复用 scripts/m4/region.sh（它调用 m4_do、m4_log，下面转接到 m5 的同名函数）。

# 状态目录放在 /var/run（重启即清空）：abroadd 自己的状态目录在它下面，重启后 abroadd 不会按旧状态重新接管。
M5_STATE=/var/run/claude-abroad-m5
M5_SNAP="/Library/Application Support/claude-abroad-m5-snap"
M5_INSTALL="/Library/Application Support/claude-abroad-m5"
M5_USER=_abroadm5
M5_LABEL=com.claude-abroad.m5.abroadd
M5_PLIST="/Library/LaunchDaemons/${M5_LABEL}.plist"
M5_ANCHOR=com.apple/250.abroad
M5_DNS_KEY=State:/Network/Service/claude-abroad/DNS
# 本地接口的共享 socket 目录：用正式位置，应用和命令行不用另外指定（design 11.1；root 所有、0755，socket 0666）。
M5_SOCKDIR=/var/run/claude-abroad
# 命令行：终端退路给的命令是 sudo abroad …，运行期间在这里放一个指向安装位置的符号链接（原来没有时才放，restore 只删自己放的）。
M5_CLI_LINK=/usr/local/bin/abroad
M5_RESTORED=0

AB_P=M5
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)/common.sh"
ab_apply_test_state # 只在不用 root 的自测里覆盖 M5_STATE、M5_SNAP（scripts/m5/selftest.sh）
ab_wrap m5

# 由状态目录派生的路径（自测覆盖状态目录之后再算）。
M5_VMDIR="${M5_STATE}/vm"
M5_STORE="${M5_STATE}/store" # abroadd 的主代理存储（在应用里添加来源、选择节点；一开始是空的）

# scripts/m4/region.sh 用的函数名。
m4_do() { m5_do "$@"; }
m4_log() { m5_log "$@"; }
M4_CMD_SYSTEMSETUP="${M5_CMD_SYSTEMSETUP:-}"
M4_CMD_DEFAULTS="${M5_CMD_DEFAULTS:-}"
M4_CMD_LAUNCHCTL="${M5_CMD_LAUNCHCTL:-}"
[ -n "${M4_CMD_SYSTEMSETUP}" ] || unset M4_CMD_SYSTEMSETUP
[ -n "${M4_CMD_DEFAULTS}" ] || unset M4_CMD_DEFAULTS
[ -n "${M4_CMD_LAUNCHCTL}" ] || unset M4_CMD_LAUNCHCTL
