#!/usr/bin/env bash
# M6-9 端到端脚本的自测（不用 root，不改网络和地区，不写系统路径）：
#   a. 两段的空跑和 restore.sh 的空跑退出码 0，没有写状态目录；
#   b. 命令核对通过；反例（碰网络、打开应用、清单外的命令、没有死人开关、卸载早于回国、重启前没取消死人开关、
#      应急程序早于停掉后台服务、没有应急演练）都被拦下；
#   c. --check-only 在没有确认 M5 通过、没有主代理节点时拒绝，并说明原因；
#   d. 源码检查：run.sh、restore.sh、lib.sh 里没有 pfctl、networksetup、route、ifconfig、pkill、killall、open 命令；
#   e. 开发安装脚本的自测、命令核对也通过（端到端依赖它们）；
#   f. 最终验收清单（docs/m6-acceptance.md）引用的测试、脚本步骤、文件都存在，反例被拦下（M6-10）。
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
DRY_RUN=1 scripts/m6/e2e/run.sh --after-reboot-2 >"${TMP}/e3" 2>/dev/null && ok "a. 第三段空跑退出码 0" || bad "a. 第三段空跑退出码非零"
DRY_RUN=1 scripts/m6/e2e/restore.sh >"${TMP}/r" 2>/dev/null && ok "a. restore.sh 空跑退出码 0" || bad "a. restore.sh 空跑退出码非零"
[ -e "$ST" ] && bad "a. 空跑写了状态目录" || ok "a. 空跑没有写状态目录"

# ---- b. 命令核对和反例 ----
scripts/m6/e2e/check-commands.sh "${TMP}/e1" "${TMP}/e2" "${TMP}/e3" >"${TMP}/cc" && ok "b. 命令核对通过" || bad "b. 命令核对不通过：$(cat "${TMP}/cc")"
neg() { if scripts/m6/e2e/check-commands.sh "$2" "$3" "${4:-${TMP}/e3}" >/dev/null 2>&1; then bad "b. 反例没被拦下：$1"; else ok "b. 反例被拦下：$1"; fi; }
{ cat "${TMP}/e1"; echo 'CHANGE: pfctl -d'; } >"${TMP}/n1"; neg "碰 pf" "${TMP}/n1" "${TMP}/e2"
{ cat "${TMP}/e1"; echo 'CHANGE: open /Applications/ClaudeAbroad.app'; } >"${TMP}/n2"; neg "打开应用" "${TMP}/n2" "${TMP}/e2"
{ cat "${TMP}/e2"; echo 'CHANGE: rm -rf /Users'; } >"${TMP}/n3"; neg "清单外的命令" "${TMP}/e1" "${TMP}/n3"
grep -v '死人开关 ' "${TMP}/e1" >"${TMP}/n4"; neg "第一段没有死人开关" "${TMP}/n4" "${TMP}/e2"
awk '/^STEP: 核对 B3/ { hold = $0; next } { print } /^CHANGE: scripts\/m6\/install\/uninstall\.sh$/ && hold { print hold; hold = "" }' "${TMP}/e3" >"${TMP}/n5"
neg "卸载早于核对开机时的回国拦截" "${TMP}/e1" "${TMP}/e2" "${TMP}/n5"
{ cat "${TMP}/e2"; echo 'CHANGE: scripts/m6/install/uninstall.sh'; } >"${TMP}/n9"; neg "第二段就卸载" "${TMP}/e1" "${TMP}/n9"
{ cat "${TMP}/e1"; echo 'CHANGE: pfctl -k 0.0.0.0/0 -k 160.79.104.0/21'; } >"${TMP}/n10"; neg "定向清理碰真实的地址段" "${TMP}/n10" "${TMP}/e2"
{ cat "${TMP}/e1"; echo 'CHANGE: pfctl -F states'; } >"${TMP}/n11"; neg "清全机的状态" "${TMP}/n11" "${TMP}/e2"
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

# M6-11b：pf 只在 pfcheck.sh 里，而且只有固定的三行；三段里都不出现真实的 Anthropic 地址段
p=$(grep -nE '(^|[[:space:]])pfctl[[:space:]]' scripts/m6/e2e/pfcheck.sh | grep -v '^[0-9]*:#' | sed 's/^[0-9]*://; s/^[[:space:]]*//')
want='rules) run QUERY pfctl -a com.apple/250.abroad -s rules ;;
run CHANGE pfctl -k 0.0.0.0/0 -k 192.0.2.0/24 || exit 1
run CHANGE pfctl -k ::/0 -k 2001:db8:abd::/48 ;;'
[ "$p" = "$want" ] && ok "d. pfcheck.sh 里只有固定的三种 pf 命令" || bad "d. pfcheck.sh 里的 pf 命令不对：$p"
bash scripts/m6/e2e/pfcheck.sh bogus >/dev/null 2>&1 && bad "d. pfcheck.sh 接受了别的参数" || ok "d. pfcheck.sh 不接受别的参数"
if cat scripts/m6/e2e/run.sh scripts/m6/e2e/pfcheck.sh scripts/m6/e2e/commands.txt scripts/m6/e2e/README.md "${TMP}/e1" "${TMP}/e2" "${TMP}/e3" | grep -qE '160\.79\.104|2607:6bc0'; then
  bad "d. 端到端里出现了真实的 Anthropic 地址段（开发构建只用测试段）"
else
  ok "d. 端到端里没有真实的 Anthropic 地址段"
fi

# ---- f. 最终验收清单的引用（M6-10）----
scripts/m6/check-acceptance.sh >"${TMP}/acc" 2>&1 && ok "f. $(tail -1 "${TMP}/acc")" || bad "f. 验收清单核对不通过：$(cat "${TMP}/acc")"
accneg() { if M6_ACCEPT_DOC="$2" scripts/m6/check-acceptance.sh >/dev/null 2>&1; then bad "f. 反例没被拦下：$1"; else ok "f. 反例被拦下：$1"; fi; }
{ cat docs/m6-acceptance.md; echo '| x | `go:internal/state#TestNoSuchTest` |'; } >"${TMP}/acc1"; accneg "引用不存在的测试" "${TMP}/acc1"
{ cat docs/m6-acceptance.md; echo '`script:scripts/m6/e2e/run.sh#ZZ9`'; } >"${TMP}/acc2"; accneg "引用不存在的脚本步骤" "${TMP}/acc2"
grep -v '^| R17 |' docs/m6-acceptance.md >"${TMP}/acc3"; accneg "缺一条风险" "${TMP}/acc3"
{ cat docs/m6-acceptance.md; echo '| 用发布构建验证管控的真实效果 | **达到** |'; } >"${TMP}/acc4"; accneg "发布构建验证写成达到" "${TMP}/acc4"
{ cat docs/m6-acceptance.md; echo '待定'; } >"${TMP}/acc5"; accneg "还有待定" "${TMP}/acc5"
{ cat docs/m6-acceptance.md; echo '`swift:noSuchSwiftTest`'; } >"${TMP}/acc6"; accneg "引用不存在的应用测试" "${TMP}/acc6"

# ---- e. 开发安装脚本 ----
scripts/m6/install/selftest.sh >"${TMP}/inst" 2>&1 && ok "e. 开发安装脚本的自测通过（$(grep -c '^✓' "${TMP}/inst") 项）" || bad "e. 开发安装脚本的自测不通过：$(grep '^✗' "${TMP}/inst" | head -3)"

echo
[ $fails = 0 ] && echo "自测全部通过" || echo "自测：${fails} 处不通过"
exit $((fails > 0))
