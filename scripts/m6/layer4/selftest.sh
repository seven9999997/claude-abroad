#!/usr/bin/env bash
# 第四层实测脚本的自测（不用 root，不写系统路径，不启动 Claude Code）：
#   a. 空跑和 restore.sh 的空跑退出码 0，没有写状态目录和托管设置目录；命令核对通过；
#   b. 反例（碰网络、清单外的命令、第一次写的不是 47821、T2 之后没写回、没删分片就取消死人开关、删了分片还启动 Claude Code）都被拦下；
#   c. 只删自己写的分片（临时目录）：内容和记录一致才删；自己建的空目录一并删；被改过的留下并报告；别人的文件不动；
#   d. 自测用的目录变量离开自测被拒绝；
#   e. 源码检查：没有 pfctl、networksetup、route、ifconfig、pkill、killall、launchctl，不打开应用；
#   f. 生成内容的唯一来源：abroad-dev claude-settings content 和 abroadd 写的逐字相同（Go 测试 TestClaudeSettingsCmd）。
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "${ROOT}" || exit 1
[ "$(id -u)" != 0 ] || { echo "不要以 root 运行" >&2; exit 1; }
TMP=$(mktemp -d "${TMPDIR:-/tmp}/m6lst.XXXXXX")
trap 'rm -rf "${TMP:?}"' EXIT
fails=0
ok() { echo "✓ $*"; }
bad() { echo "✗ $*"; fails=$((fails + 1)); }

# ---- a ----
DRY_RUN=1 scripts/m6/layer4/run.sh >"${TMP}/l1" 2>/dev/null && ok "a. 空跑退出码 0" || bad "a. 空跑退出码非零"
DRY_RUN=1 scripts/m6/layer4/restore.sh >/dev/null 2>&1 && ok "a. restore.sh 空跑退出码 0" || bad "a. restore.sh 空跑退出码非零"
[ -e "/Library/Application Support/claude-abroad-m6l" ] && bad "a. 空跑写了状态目录" || ok "a. 空跑没有写状态目录"
[ -e "/Library/Application Support/ClaudeCode/managed-settings.d/claude-abroad.json" ] && bad "a. 托管设置分片存在（不该在开发机上出现）" || ok "a. 空跑没有写托管设置"
scripts/m6/layer4/check-commands.sh "${TMP}/l1" >"${TMP}/cc" && ok "a. 命令核对通过" || bad "a. 命令核对不通过：$(cat "${TMP}/cc")"

# ---- b ----
neg() { if scripts/m6/layer4/check-commands.sh "$2" >/dev/null 2>&1; then bad "b. 反例没被拦下：$1"; else ok "b. 反例被拦下：$1"; fi; }
{ cat "${TMP}/l1"; echo 'CHANGE: pfctl -d'; } >"${TMP}/n1"; neg "碰 pf" "${TMP}/n1"
{ cat "${TMP}/l1"; echo 'CHANGE: rm -rf /Users'; } >"${TMP}/n2"; neg "清单外的命令" "${TMP}/n2"
awk '!done && /--port 47821>/ { sub(/--port 47821>/, "--port 47898>"); done = 1 } { print }' "${TMP}/l1" >"${TMP}/n3"; neg "第一次写的不是 47821" "${TMP}/n3"
awk '/--port 47821>/ { n++; if (n == 2) next } { print }' "${TMP}/l1" >"${TMP}/n4"; neg "T2 之后没写回 47821" "${TMP}/n4"
grep -v 'rm -f .*claude-abroad\.json' "${TMP}/l1" >"${TMP}/n5"; neg "没删分片就取消死人开关" "${TMP}/n5"
awk '/^CHANGE: rm -f .*claude-abroad\.json/ { print; print "CLAUDE: X env  claude -p"; next } { print }' "${TMP}/l1" >"${TMP}/n6"; neg "删了分片还启动 Claude Code" "${TMP}/n6"

# ---- c ----
frag() { # frag <场景> <准备> ：在临时目录里跑 m6l_frag_remove，打印返回码
  local d="${TMP}/$1"; mkdir -p "$d/st"
  ( export M6L_TEST_STATE="$d/st" M6L_TEST_CC_DIR="$d/ClaudeCode/managed-settings.d" DRY_RUN=0
    . scripts/m6/layer4/lib.sh
    eval "$2"
    m6l_frag_remove; echo "rc=$?" ) 2>/dev/null
}
CONTENT='{"env":{"https_proxy":"http://127.0.0.1:47821"}}'
out=$(frag c1 'mkdir -p "$M6L_CC_DIR"; printf "%s" "$CONTENT" >"$M6L_FRAG"; cp "$M6L_FRAG" "$M6L_STATE/frag.content"; m6l_state_set created_dir 1; m6l_state_set created_parent 1')
[ "$out" = rc=0 ] && [ ! -e "${TMP}/c1/ClaudeCode" ] && ok "c. 自己写的分片删了，自己建的空目录一并删了" || bad "c. 自己写的分片：$out，$(ls -R "${TMP}/c1/ClaudeCode" 2>/dev/null | head -3)"
out=$(frag c2 'mkdir -p "$M6L_CC_DIR"; printf "%s" "$CONTENT" >"$M6L_FRAG"; cp "$M6L_FRAG" "$M6L_STATE/frag.content"; echo changed >>"$M6L_FRAG"')
[ "$out" = rc=1 ] && [ -e "${TMP}/c2/ClaudeCode/managed-settings.d/claude-abroad.json" ] && ok "c. 被改过的分片留下并报告" || bad "c. 被改过的分片：$out"
out=$(frag c3 'mkdir -p "$M6L_CC_DIR"; printf "%s" "$CONTENT" >"$M6L_FRAG"; cp "$M6L_FRAG" "$M6L_STATE/frag.content"; echo "{}" >"$M6L_CC_DIR/other.json"; m6l_state_set created_dir 1')
[ "$out" = rc=0 ] && [ -e "${TMP}/c3/ClaudeCode/managed-settings.d/other.json" ] && [ ! -e "${TMP}/c3/ClaudeCode/managed-settings.d/claude-abroad.json" ] && ok "c. 别人的分片不动，目录不空时不删" || bad "c. 别人的分片：$out"
out=$(frag c4 'mkdir -p "$M6L_CC_DIR"; printf "%s" "$CONTENT" >"$M6L_FRAG"')
[ "$out" = rc=1 ] && [ -e "${TMP}/c4/ClaudeCode/managed-settings.d/claude-abroad.json" ] && ok "c. 没有记录的分片不删" || bad "c. 没有记录的分片：$out"

# ---- d ----
( M6L_TEST_CC_DIR=/tmp/x . scripts/m6/layer4/lib.sh ) >/dev/null 2>&1 && bad "d. 自测目录变量离开自测没被拒绝" || ok "d. 自测目录变量离开自测被拒绝"

# ---- e ----
for s in run.sh restore.sh lib.sh; do
  sed -e 's/^[[:space:]]*#.*$//' -e 's/[[:space:]]#[[:space:]].*$//' "scripts/m6/layer4/$s" |
    grep -nE '(^|[;&|(][[:space:]]*|then[[:space:]]+|do[[:space:]]+|^[[:space:]]+)(pfctl|networksetup|route|ifconfig|pkill|killall|launchctl|open)[[:space:]]' >"${TMP}/e" &&
    bad "e. $s 里有不允许的命令：$(cat "${TMP}/e")" || ok "e. $s 没有碰网络、服务和应用"
done

# ---- f ----
go test ./cmd/abroad-dev -run TestClaudeSettingsCmd >/dev/null 2>&1 && ok "f. 托管设置内容和 abroadd 写的逐字相同" || bad "f. TestClaudeSettingsCmd 不通过"

echo
[ $fails = 0 ] && echo "自测全部通过" || echo "自测：${fails} 处不通过"
exit $((fails > 0))
