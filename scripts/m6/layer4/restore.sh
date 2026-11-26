#!/usr/bin/env bash
# 第四层实测的还原（死人开关到点、中途退出、手动执行）：删本脚本写的托管设置分片（内容一致才删），结束本脚本起的
# 诱饵监听（按 PID 文件，核对命令行），取消死人开关。不动 abroadd、不改网络。可以重复执行。
#
#   sudo scripts/m6/layer4/restore.sh
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "${ROOT}" || exit 1
. scripts/m6/layer4/lib.sh
[ $# -eq 0 ] || { echo "用法：sudo $0" >&2; exit 2; }
m6l_test_guard
m6l_require_root
M6L_LOG="${M6L_LOG:-${ROOT}/scripts/m6/layer4/out/m6l-restore-$(date +%Y%m%d-%H%M%S).log}"
mkdir -p "$(dirname "${M6L_LOG}")" 2>/dev/null || true
ok=1
m6l_frag_remove || ok=0
for n in decoy deny; do m6l_kill_pidfile "${M6L_STATE}/${n}.pid" "m6l-listener"; done
m6l_deadman_stop
if [ "$DRY_RUN" = 1 ] || [ "$ok" = 1 ]; then m6l_do rm -rf "${M6L_STATE:?}"; fi
m6l_log "还原结束：本机的 Claude Code 不再被指向本机代理端口"
