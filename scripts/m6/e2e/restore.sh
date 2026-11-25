#!/usr/bin/env bash
# M6-9 的还原（死人开关到点、中途退出、或用户手动执行）：卸载开发安装（scripts/m6/install/uninstall.sh：先试 abroad uninstall，
# 没有终端或走不通时按应急步骤兜底），再按本脚本自己的快照还原地区（逐项读回），最后取消死人开关。可以重复执行。
# 不执行 pfctl -d，不按名字结束进程，不碰用户的 Clash。
#
#   sudo scripts/m6/e2e/restore.sh
#   DRY_RUN=1 scripts/m6/e2e/restore.sh
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "${ROOT}" || exit 1
. scripts/m6/e2e/lib.sh
. scripts/m4/region.sh
[ $# -eq 0 ] || { echo "用法：sudo $0" >&2; exit 2; }
m6e_test_guard
m6e_require_root
M6E_LOG="${M6E_LOG:-${ROOT}/scripts/m6/e2e/out/m6e-restore-$(date +%Y%m%d-%H%M%S).log}"
mkdir -p "$(dirname "${M6E_LOG}")" 2>/dev/null || true
m6e_log "还原开始"

# ---- 1. 卸载开发安装（它自己按安装前的快照对照）----
if [ "$DRY_RUN" = 1 ]; then
  printf 'CHANGE: scripts/m6/install/uninstall.sh\n'
  UN_OK=1
elif M6I_YES=1 bash scripts/m6/install/uninstall.sh; then
  UN_OK=1
else
  UN_OK=0
  m6e_log "卸载没有完全还原（见上面的差异）：可以再执行一次 sudo scripts/m6/install/uninstall.sh，或按 docs/emergency.md 处理"
fi

# ---- 2. 地区：按本脚本的快照还原，逐项读回 ----
RG_OK=1
if [ "$DRY_RUN" = 1 ]; then rg_restore "${M6E_STATE}/region"; else
  bad=$(rg_restore "${M6E_STATE}/region"); rc=$?
  case $rc in
    0) m6e_log "地区已按快照还原，读回与开始前一致" ;;
    3) ;;
    *) RG_OK=0; m6e_log "地区还原后读回与快照不一致的项：$(printf '%s' "$bad" | tr '\n' ' ')（请手动核对：系统设置 → 通用 → 日期与时间、语言与地区）" ;;
  esac
fi

# ---- 3. 取消死人开关；都还原了才删状态目录 ----
m6e_deadman_stop
if [ "$DRY_RUN" = 1 ] || { [ "${UN_OK}" = 1 ] && [ "${RG_OK}" = 1 ]; }; then
  m6e_do rm -rf "${M6E_STATE:?}"
else
  m6e_log "没有全部还原，状态目录 ${M6E_STATE} 留着（里面有地区快照），处理后再执行一次 restore.sh"
fi
m6e_log "还原结束"
