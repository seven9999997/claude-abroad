# M2 真机验证脚本的公共函数（M2-9c）。用 source 引入，不单独执行。
# 通用部分在 scripts/lib/common.sh（M3-8 抽出，m3-plan v2.10 决定 1）；这里只放 M2 的名字和参数。
#
# 约定（docs/m2-plan.md 的 M2-9c 决定）：
# - 状态记在 root 所有的 ${M2_STATE}，restore.sh 只读这里和脚本自己拍的快照，不依赖 abroadd；
# - 改系统之前先启动死人开关（独立进程，到点执行 restore.sh）；只有“建状态目录”可以在它之前；
# - DRY_RUN=1 时：改动类命令只打印 "CHANGE: …"，pfctl 的查询只打印 "QUERY: …"，都不执行；
# - 只按程序自己写的 PID 文件结束进程（先核对命令行）；不按名字结束进程，不执行 pfctl -d，不碰用户的 Clash。

# 状态目录放在 /var/run（重启即清空）是有意的：abroadd 自己的状态目录在它下面，重启后 abroadd 读到的是 RESTORED，
# 不会在没有虚拟机的情况下重新接管、把用户断在网外。IPv6 快照另存一份到重启不丢的 M2_SNAP（IPv6 关闭是持久的）。
M2_STATE=/var/run/claude-abroad-m2
M2_SNAP="/Library/Application Support/claude-abroad-m2-snap"
M2_INSTALL="/Library/Application Support/claude-abroad-m2"
M2_USER=_abroadm2
M2_LABEL=com.claude-abroad.m2.abroadd
M2_PLIST="/Library/LaunchDaemons/${M2_LABEL}.plist"
M2_ANCHOR=com.apple/250.abroad
M2_DNS_KEY=State:/Network/Service/claude-abroad/DNS
M2_RESTORED=0

AB_P=M2
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)/common.sh"
ab_apply_test_state # 只在不用 root 的自测里覆盖 M2_STATE、M2_SNAP（scripts/m2/selftest.sh）
ab_wrap m2
