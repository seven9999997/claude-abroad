# 第四层真机实测（R21、R22、R23，fallback-plan 第 6 节实测 1–4）的公共部分。用 source 引入，不单独执行。
M6L_STATE="/Library/Application Support/claude-abroad-m6l"
M6L_SNAP="/Library/Application Support/claude-abroad-m6l-snap" # common.sh 的约定；不用
M6L_CC_DIR="/Library/Application Support/ClaudeCode/managed-settings.d"
M6L_FRAG="${M6L_CC_DIR}/claude-abroad.json"
M6L_SOCK=/var/run/claude-abroad
M6L_PORT=47821
M6L_DECOY=47899  # 实测 1：启动环境里的“另一个代理”
M6L_DEAD=47898   # 实测 2：没人监听
M6L_DENY=47897   # 实测 2：一律 403
M6L_RESTORED=0

AB_P=M6L
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/../../lib" && pwd)/common.sh"
ab_apply_test_state
ab_wrap m6l
# 自测：托管设置目录换到临时目录（只能配 M6L_TEST_STATE，不用 root）。
if [ -n "${M6L_TEST_CC_DIR:-}" ]; then
  [ -n "${M6L_TEST_STATE:-}" ] && [ "$(id -u)" != 0 ] || { echo "M6L_TEST_CC_DIR 只能在不用 root 的自测里配 M6L_TEST_STATE 用" >&2; exit 1; }
  M6L_CC_DIR="${M6L_TEST_CC_DIR}"
  M6L_FRAG="${M6L_CC_DIR}/claude-abroad.json"
fi

# 托管设置分片：只删内容和本脚本写入时一致的（记录在状态目录 frag.content）。
m6l_frag_remove() {
  if [ "$DRY_RUN" = 1 ]; then printf 'CHANGE: rm -f %s（内容和本脚本写入的一致才删）\n' "${M6L_FRAG}"; return 0; fi
  [ -f "${M6L_FRAG}" ] || return 0
  if [ -f "${M6L_STATE}/frag.content" ] && cmp -s "${M6L_FRAG}" "${M6L_STATE}/frag.content"; then
    m6l_do rm -f "${M6L_FRAG:?}"
    if [ "$(m6l_state_get created_dir)" = 1 ] && [ -d "${M6L_CC_DIR}" ] && [ -z "$(ls -A "${M6L_CC_DIR}")" ]; then
      m6l_do rmdir "${M6L_CC_DIR:?}"
      local parent; parent=$(dirname "${M6L_CC_DIR}")
      if [ "$(m6l_state_get created_parent)" = 1 ] && [ -d "${parent}" ] && [ -z "$(ls -A "${parent}")" ]; then
        m6l_do rmdir "${parent:?}"
      fi
    fi
    return 0
  fi
  m6l_log "${M6L_FRAG} 的内容不是本脚本写的（或没有记录），不删：请打开看一眼，确认是 Claude 出国的再手动删除"
  return 1
}
