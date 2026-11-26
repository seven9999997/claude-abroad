#!/usr/bin/env bash
# M6-9 端到端脚本的自测（不用 root，不改网络和地区，不写系统路径）：
#   a. 两段的空跑和 restore.sh 的空跑退出码 0，没有写状态目录；
#   b. 命令核对通过；反例（碰网络、打开应用、清单外的命令、没有死人开关、卸载早于回国、重启前没取消死人开关、
#      应急程序早于停掉后台服务、没有应急演练）都被拦下；
#   c. --check-only 在没有确认 M5 通过、没有主代理节点时拒绝，并说明原因；
#   d. 源码检查：run.sh、restore.sh、lib.sh 里没有 pfctl、networksetup、route、ifconfig、pkill、killall、open 命令；
#   e. 开发安装脚本的自测、命令核对也通过（端到端依赖它们）。
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "${ROOT}" || exit 1
[ "$(id -u)" != 0 ] || { echo "不要以 root 运行" >&2; exit 1; }
TMP=$(mktemp -d "${TMPDIR:-/tmp}/m6est.XXXXXX")
trap 'rm -rf "${TMP:?}"' EXIT
fails=0
ok() { echo "✓ $*"; }
bad() { echo "✗ $*"; fails=$((fails + 1)); }
ST="/Library/Application Support/claude-abroad-m6e"

# ---- a. 空跑 ----
[ -e "$ST" ] && bad "a. 运行前状态目录就存在"
DRY_RUN=1 scripts/m6/e2e/run.sh >"${TMP}/e1" 2>/dev/null && ok "a. 第一段空跑退出码 0" || bad "a. 第一段空跑退出码非零"
DRY_RUN=1 scripts/m6/e2e/run.sh --after-reboot >"${TMP}/e2" 2>/dev/null && ok "a. 第二段空跑退出码 0" || bad "a. 第二段空跑退出码非零"
DRY_RUN=1 scripts/m6/e2e/restore.sh >"${TMP}/r" 2>/dev/null && ok "a. restore.sh 空跑退出码 0" || bad "a. restore.sh 空跑退出码非零"
[ -e "$ST" ] && bad "a. 空跑写了状态目录" || ok "a. 空跑没有写状态目录"

# ---- b. 命令核对和反例 ----
scripts/m6/e2e/check-commands.sh "${TMP}/e1" "${TMP}/e2" >"${TMP}/cc" && ok "b. 命令核对通过" || bad "b. 命令核对不通过：$(cat "${TMP}/cc")"
neg() { if scripts/m6/e2e/check-commands.sh "$2" "$3" >/dev/null 2>&1; then bad "b. 反例没被拦下：$1"; else ok "b. 反例被拦下：$1"; fi; }
{ cat "${TMP}/e1"; echo 'CHANGE: pfctl -d'; } >"${TMP}/n1"; neg "碰 pf" "${TMP}/n1" "${TMP}/e2"
{ cat "${TMP}/e1"; echo 'CHANGE: open /Applications/ClaudeAbroad.app'; } >"${TMP}/n2"; neg "打开应用" "${TMP}/n2" "${TMP}/e2"
{ cat "${TMP}/e2"; echo 'CHANGE: rm -rf /Users'; } >"${TMP}/n3"; neg "清单外的命令" "${TMP}/e1" "${TMP}/n3"
grep -v '死人开关 ' "${TMP}/e1" >"${TMP}/n4"; neg "第一段没有死人开关" "${TMP}/n4" "${TMP}/e2"
awk '/^STEP: 检查点 P5/ { hold = $0; next } { print } /^CHANGE: scripts\/m6\/install\/uninstall\.sh$/ && hold { print hold; hold = "" }' "${TMP}/e2" >"${TMP}/n5"
neg "卸载早于回国检查点" "${TMP}/e1" "${TMP}/n5"
grep -v '^CHANGE: kill <死人开关的 PID>$' "${TMP}/e1" >"${TMP}/n6"; neg "重启前没取消死人开关" "${TMP}/n6" "${TMP}/e2"
awk '/^CHANGE: launchctl bootout / { hold = $0; next } { print } /bin\/abroad-rescue$/ && hold { print hold; hold = "" }' "${TMP}/e2" >"${TMP}/n7"
neg "应急程序早于停掉后台服务" "${TMP}/e1" "${TMP}/n7"
grep -v 'bin/abroad-rescue$' "${TMP}/e2" >"${TMP}/n8"; neg "第二段没有应急演练" "${TMP}/e1" "${TMP}/n8"

# ---- c. 前提检查 ----
env -u M6E_M5_PASSED -u M6E_NODES scripts/m6/e2e/run.sh --check-only >/dev/null 2>"${TMP}/c1"; rc=$?
[ $rc != 0 ] && grep -q 'M6E_M5_PASSED' "${TMP}/c1" && grep -q 'M6E_NODES' "${TMP}/c1" && ok "c. --check-only 没确认 M5、没有节点时拒绝并说明" || bad "c. --check-only 应拒绝：rc=$rc"

# ---- d. 源码检查 ----
for s in run.sh restore.sh lib.sh; do
  sed -e 's/^[[:space:]]*#.*$//' -e 's/[[:space:]]#[[:space:]].*$//' "scripts/m6/e2e/$s" |
    grep -nE '(^|[;&|(][[:space:]]*|then[[:space:]]+|do[[:space:]]+|^[[:space:]]+)(pfctl|networksetup|route|ifconfig|pkill|killall|open)[[:space:]]' >"${TMP}/d" &&
    bad "d. $s 里有不允许的命令：$(cat "${TMP}/d")" || ok "d. $s 没有直接碰网络、打开应用、按名字结束进程"
done

# ---- e. 开发安装脚本 ----
scripts/m6/install/selftest.sh >"${TMP}/inst" 2>&1 && ok "e. 开发安装脚本的自测通过（$(grep -c '^✓' "${TMP}/inst") 项）" || bad "e. 开发安装脚本的自测不通过：$(grep '^✗' "${TMP}/inst" | head -3)"

echo
[ $fails = 0 ] && echo "自测全部通过" || echo "自测：${fails} 处不通过"
exit $((fails > 0))
