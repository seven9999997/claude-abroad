#!/usr/bin/env bash
# 解析锁真机脚本的还原（M6-3，R9、R18）：删掉 run.sh 自己写的锁文件。不依赖 run.sh 还在运行，不依赖 Go，可以重复执行。
#   - 只看状态目录里的记录（lock.<域名>，就是写入的内容）：/etc/resolver/<域名> 的内容和记录一致才删；
#     不一致（被别的程序改过）或不是普通文件就不动，报告出来，退出码 1；
#   - /etc/resolver 目录是 run.sh 建的、并且删完已经空了，才删掉这个空目录；
#   - 全部删干净后清掉状态目录。
# 不改网络设置，不发信号，不启动、不结束任何进程。
#
#   sudo scripts/m6/resolver/restore.sh
#   DRY_RUN=1 scripts/m6/resolver/restore.sh     # 空跑：只打印会执行的命令
#   （--deadman <轮次> 只给死人开关用：轮次不是当前这一轮就什么都不做）
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "${ROOT}" || exit 1
DM_GEN=""
case "${1:-}" in
  --deadman) DM_GEN=${2:-}; [ -n "${DM_GEN}" ] || { echo "用法：$0 [--deadman <轮次>]" >&2; exit 2; } ;;
  "") ;;
  *) echo "用法：sudo $0" >&2; exit 2 ;;
esac
. scripts/m6/resolver/lib.sh

if [ -n "${DM_GEN}" ]; then
  # 死人开关到点：状态目录不在（已经还原过）或轮次已经换了（取消了），什么都不做。
  [ -d "${M6R_STATE}" ] || exit 0
  [ "$(ab_state_get deadman_gen)" = "${DM_GEN}" ] || { echo "死人开关第 ${DM_GEN} 轮已经取消，不做任何事"; exit 0; }
fi
m6r_require_mode

TS="$(date +%Y%m%d-%H%M%S)"
case "${M6R_MODE}" in
  real) OUT="${ROOT}/scripts/m6/resolver/out" ;;
  test) OUT="${M6R_TEST_DIR}/out" ;;
  dry) OUT="" ;;
esac
if [ -n "${OUT}" ]; then
  mkdir -p "${OUT}" 2>/dev/null || true
  M6R_LOG="${M6R_LOG:-${OUT}/restore-${TS}.log}"
fi
m6r_log "还原开始$([ -n "${DM_GEN}" ] && echo "（死人开关第 ${DM_GEN} 轮到点）")"

FAILS=0
if [ "$DRY_RUN" = 1 ]; then
  printf 'CHANGE: rm -f %s/<记录里的域名>（只删内容和记录一致的）\n' "${M6R_RESOLVER_DIR}"
  printf 'CHANGE: rm -f %s/lock.<记录里的域名>\n' "${M6R_STATE}"
  printf 'CHANGE: rmdir %s（是 run.sh 建的、并且已经空了才删）\n' "${M6R_RESOLVER_DIR}"
  printf 'CHANGE: rm -f %s/deadman_gen\n' "${M6R_STATE}"
  printf 'CHANGE: rm -f %s/created_dir\n' "${M6R_STATE}"
  printf 'CHANGE: rm -f %s/deadman.log\n' "${M6R_STATE}"
  printf 'CHANGE: rmdir %s\n' "${M6R_STATE}"
  m6r_log "（空跑）还原结束"
  exit 0
fi

if [ ! -d "${M6R_STATE}" ]; then
  m6r_log "没有状态目录：run.sh 没有写过锁文件，或者已经还原过。没有要做的事。"
  m6r_log "如果 ${M6R_RESOLVER_DIR} 下还有 claude.ai、claude.com、anthropic.com、lock-test.claude-abroad.invalid，见 scripts/m6/resolver/README.md 的“应急”一节手动核对。"
  exit 0
fi

for d in $(m6r_records); do
  m6r_remove_lock "$d" || FAILS=$((FAILS + 1))
done

# run.sh 建的 /etc/resolver：删完空了才删这个目录。
if [ "$(ab_state_get created_dir)" = 1 ] && [ -z "$(m6r_records)" ]; then
  if [ -d "${M6R_RESOLVER_DIR}" ] && [ ! -L "${M6R_RESOLVER_DIR}" ] && [ -z "$(ls -A "${M6R_RESOLVER_DIR}" 2>/dev/null)" ]; then
    rmdir -- "${M6R_RESOLVER_DIR:?}" && m6r_log "已删除 run.sh 建的空目录 ${M6R_RESOLVER_DIR}"
  elif [ -d "${M6R_RESOLVER_DIR}" ]; then
    m6r_log "${M6R_RESOLVER_DIR} 里还有别的文件，目录留着"
  fi
  rm -f -- "${M6R_STATE:?}/created_dir"
fi

if [ "${FAILS}" -gt 0 ]; then
  m6r_log "还原结束，但有 ${FAILS} 个锁文件没有删（见上面的原因）。记录留在状态目录，核对后可以再执行一次 restore.sh。"
  exit 1
fi
rm -f -- "${M6R_STATE:?}/deadman_gen"
if [ -z "${DM_GEN}" ]; then
  # 正常还原（不是死人开关触发的）：状态目录清掉。死人开关触发时留着，让还在运行的 run.sh 看到“已经被还原”。
  rm -f -- "${M6R_STATE:?}/deadman.log"
  rmdir -- "${M6R_STATE:?}" 2>/dev/null || m6r_log "状态目录里还有别的文件，留着：${M6R_STATE}"
fi
m6r_log "还原结束：这次写入的锁文件都已删除"
exit 0
