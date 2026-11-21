#!/bin/bash
# R2 还原：清空测试锚点、释放 pf 启用引用、删除临时用户和安装目录。
# 只读 /var/run/claude-abroad-m0 里记下的状态，可重复执行。
# 用法：sudo scripts/m0/r2/restore.sh
set -u
here="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../lib.sh
. "$here/../lib.sh"

[ "$(id -u)" = 0 ] || m0_die "需要用 sudo 运行"

m0_log "R2 还原开始${M0_FROM_DEADMAN:+（由死人开关触发）}"
pkill -f "$M0_INSTALL/m0-r2-probe" 2>/dev/null || true
m0_pf_restore
m0_users_delete
rm -rf "$M0_INSTALL" "$M0_STATE/sock"
m0_deadman_stop
m0_check_network
m0_log "R2 还原完成"
