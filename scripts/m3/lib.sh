# M3 真机验证脚本的公共函数（M3-8）。用 source 引入，不单独执行。
# 通用部分在 scripts/lib/common.sh（死人开关、还原 trap、空跑、按 PID 文件结束进程、测试用户、脱敏、自测护栏）；
# 这里只放 M3 的名字和参数（m3-plan v2.10 决定 1）。约定同 M2（docs/m2-plan.md 的 M2-9c 决定）。

# 状态目录放在 /var/run（重启即清空）：abroadd 自己的状态目录在它下面，重启后 abroadd 不会按旧状态重新接管。
M3_STATE=/var/run/claude-abroad-m3
M3_SNAP="/Library/Application Support/claude-abroad-m3-snap"
M3_INSTALL="/Library/Application Support/claude-abroad-m3"
M3_USER=_abroadm3
M3_LABEL=com.claude-abroad.m3.abroadd
M3_PLIST="/Library/LaunchDaemons/${M3_LABEL}.plist"
M3_ANCHOR=com.apple/250.abroad
M3_DNS_KEY=State:/Network/Service/claude-abroad/DNS
M3_RESTORED=0

AB_P=M3
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)/common.sh"
ab_apply_test_state # 只在不用 root 的自测里覆盖 M3_STATE、M3_SNAP（scripts/m3/selftest.sh）
ab_wrap m3

# 由状态目录派生的路径（自测覆盖状态目录之后再算）。
M3_UPDIR="${M3_STATE}/up"         # 两个开发上游的日志和 PID 文件（属主是测试用户，700）
M3_CHROME_DIR="${M3_STATE}/chrome" # Chrome 的临时配置目录（属主是运行 sudo 的用户，700）
M3_VMDIR="${M3_STATE}/vm"
