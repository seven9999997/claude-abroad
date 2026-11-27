#!/usr/bin/env bash
# scripts/dev/reinstall.sh 的自测（只用空跑，不调用真的 sudo，不碰系统路径）：
#   a. 装过时：构建、组装 payload、打包、原地升级（不卸载）、以用户身份把应用放进“应用程序”；结尾写清版本从哪到哪；只剩快照时：构建三步、卸载、检查、安装；没装过时跳过卸载；
#   b. 命令核对：只允许清单里的行（三个构建、检查、安装、升级、卸载、放应用），反例（多出一条、--yes、pfctl、networksetup、
#      route、ifconfig、launchctl）都被拦下；源码里也没有这些；
#   c. 以 root 运行被拒；测试变量离开空跑被拒；
#   d. 每一步注入失败都会停下，后面的步骤不执行，退出码非零，给出下一步；升级被拒、失败的各个退出码给出对应的下一步，
#      且都不放应用；
#   e. 放置逻辑只有一份（在 build-app.sh 里，reinstall.sh 只调用 --place-only）。
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
[ "$(id -u)" != 0 ] || { echo "不要以 root 运行" >&2; exit 1; }
TMP=$(mktemp -d "${TMPDIR:-/tmp}/reinst.XXXXXX")
trap 'rm -rf "${TMP:?}"' EXIT
fails=0
ok() { echo "✓ $*"; }
bad() { echo "✗ $*"; fails=$((fails + 1)); }
S=scripts/dev/reinstall.sh
dry() { env DRY_RUN=1 "$@" bash "$S" 2>"${TMP}/err"; }

ALLOW='^RUN: (scripts/dev/integrity-build\.sh --dev|scripts/dev/payload\.sh|scripts/dev/app-bundle\.sh --payload scripts/dev/out/payload|sudo scripts/m6/install/uninstall\.sh|scripts/m6/install/install\.sh --check-only|sudo scripts/m6/install/install\.sh|sudo scripts/m6/install/install\.sh --upgrade|scripts/dev/build-app\.sh --place-only)$'
BAN='(--yes|pfctl|networksetup|route|ifconfig|launchctl)'
cmdcheck() { # 只允许清单里的行，不许出现禁止的词
  grep -vE "$ALLOW" "$1" | grep -q . && return 1
  grep -qE "$BAN" "$1" && return 1
  return 0
}

# ---- a. 顺序 ----
printf '{"commit":"aaaaaaa11111","build_time":"20261127T010000Z","dirty":false,"state_format":1}\n' >"${TMP}/old.json"
printf '{"commit":"bbbbbbb22222","build_time":"20261127T020000Z","dirty":false,"state_format":1}\n' >"${TMP}/new.json"
dry REINSTALL_TEST_INSTALLED=1 REINSTALL_TEST_OLD_BUILD="${TMP}/old.json" REINSTALL_TEST_NEW_BUILD="${TMP}/new.json" >"${TMP}/i1"; rc=$?
want1='RUN: scripts/dev/integrity-build.sh --dev
RUN: scripts/dev/payload.sh
RUN: scripts/dev/app-bundle.sh --payload scripts/dev/out/payload
RUN: sudo scripts/m6/install/install.sh --upgrade
RUN: scripts/dev/build-app.sh --place-only'
[ $rc = 0 ] && [ "$(cat "${TMP}/i1")" = "$want1" ] && ok "a. 装过时：构建、组装 payload、打包、原地升级、放进应用程序（构建 → 升级 → 放置），不卸载" || bad "a. 装过时的顺序不对：$(cat "${TMP}/i1")"
grep -q '主代理和设置都保留' "${TMP}/err" && ! grep -q '清空' "${TMP}/err" && ok "a. 装过时结尾提示主代理和设置都保留" || bad "a. 装过时的结尾提示不对：$(tail -1 "${TMP}/err")"
grep -q '从 aaaaaaa11111（构建于 20261127T010000Z） 升到 bbbbbbb22222（构建于 20261127T020000Z）' "${TMP}/err" && grep -q '请退出并重新打开应用' "${TMP}/err" &&
  ok "a. 装过时结尾写清后台从哪个版本升到哪个版本、应用已更新请重新打开" || bad "a. 版本提示不对：$(tail -2 "${TMP}/err")"
dry REINSTALL_TEST_INSTALLED=1 REINSTALL_TEST_OLD_BUILD="${TMP}/new.json" REINSTALL_TEST_NEW_BUILD="${TMP}/new.json" >"${TMP}/i1s"; rc=$?
[ $rc = 0 ] && [ "$(cat "${TMP}/i1s")" = "$want1" ] && grep -q '已经是 bbbbbbb22222' "${TMP}/err" && ok "a. 版本一样（升级退出码 0）：照样放应用，提示没有变" || bad "a. 版本一样时不对：$(tail -2 "${TMP}/err")"
dry REINSTALL_TEST_INSTALLED=0 >"${TMP}/i0"; rc=$?
want0='RUN: scripts/dev/integrity-build.sh --dev
RUN: scripts/dev/payload.sh
RUN: scripts/dev/app-bundle.sh --payload scripts/dev/out/payload
RUN: scripts/m6/install/install.sh --check-only
RUN: sudo scripts/m6/install/install.sh'
[ $rc = 0 ] && [ "$(cat "${TMP}/i0")" = "$want0" ] && grep -q '没装过，跳过卸载' "${TMP}/err" && ok "a. 没装过时跳过卸载，检查后安装" || bad "a. 没装过时不对：$(cat "${TMP}/i0")"
grep -q '请添加主代理来源' "${TMP}/err" && ! grep -q '已随卸载清空' "${TMP}/err" && ok "a. 没装过时结尾提示添加主代理、不提卸载" || bad "a. 没装过时的结尾提示不对"
dry REINSTALL_TEST_INSTALLED=0 REINSTALL_TEST_SNAP_ONLY=1 >"${TMP}/is"; rc=$?
wants=$(printf '%s\n' "$want0" | sed 's#^RUN: scripts/m6/install/install.sh --check-only$#RUN: sudo scripts/m6/install/uninstall.sh\n&#')
[ $rc = 0 ] && [ "$(cat "${TMP}/is")" = "$wants" ] && ok "a. 只剩快照目录时：先走卸载（只重做对照、删快照），再检查、安装" || bad "a. 只剩快照目录时不对：$(cat "${TMP}/is")"

# ---- b. 命令核对 ----
cmdcheck "${TMP}/i1" && cmdcheck "${TMP}/i0" && cmdcheck "${TMP}/is" && ok "b. 命令核对通过（只有允许的调用）" || bad "b. 命令核对不通过"
neg() { if cmdcheck "$2"; then bad "b. 反例没被拦下：$1"; else ok "b. 反例被拦下：$1"; fi; }
{ cat "${TMP}/i1"; echo 'RUN: rm -rf /Users'; } >"${TMP}/n1"; neg "多出一条命令" "${TMP}/n1"
sed 's#sudo scripts/m6/install/install.sh --upgrade$#sudo scripts/m6/install/install.sh --upgrade --yes#' "${TMP}/i1" >"${TMP}/n2"; neg "--yes" "${TMP}/n2"
{ cat "${TMP}/i1"; echo 'RUN: sudo scripts/m6/install/uninstall.sh'; } >"${TMP}/n4"; cmdcheck "${TMP}/n4" && grep -q uninstall "${TMP}/i1" && bad "b. 装过时出现了卸载" || ok "b. 装过时的调用里没有卸载"
for w in 'sudo pfctl -d' 'sudo networksetup -setdnsservers Wi-Fi Empty' 'sudo route -n delete default' 'sudo ifconfig en0 down' 'sudo launchctl bootout system/x'; do
  { cat "${TMP}/i1"; echo "RUN: $w"; } >"${TMP}/n3"; neg "${w%% *} ${w#* }" "${TMP}/n3"
done
code=$(sed -e 's/^[[:space:]]*#.*$//' "$S")
printf '%s\n' "$code" | grep -nE '(^|[^a-z-])(pfctl|networksetup|route|ifconfig|launchctl)([^a-z]|$)|--yes' >"${TMP}/src" &&
  bad "b. 源码里有不允许的命令：$(cat "${TMP}/src")" || ok "b. 源码里没有 pfctl、networksetup、route、ifconfig、launchctl、--yes"

# ---- c. 以 root 运行、测试变量 ----
dry REINSTALL_TEST_EUID=0 >"${TMP}/r"; rc=$?
[ $rc != 0 ] && [ ! -s "${TMP}/r" ] && grep -q '请不带 sudo 运行' "${TMP}/err" && ok "c. 以 root 运行被拒（退出码 ${rc}，什么都没调用）" || bad "c. 以 root 运行没被拒"
env REINSTALL_TEST_INSTALLED=1 bash "$S" >/dev/null 2>"${TMP}/err"; rc=$?
[ $rc != 0 ] && grep -q '只能在空跑' "${TMP}/err" && ok "c. 测试变量离开空跑被拒" || bad "c. 测试变量离开空跑没被拒"

# ---- d. 每一步失败都停下 ----
for at in 1 2 3 4 5; do
  dry REINSTALL_TEST_INSTALLED=1 REINSTALL_TEST_FAIL_AT=$at >"${TMP}/f"; rc=$?
  got=$(grep -c '^RUN:' "${TMP}/f")
  if [ $rc != 0 ] && [ "$got" = "$at" ] && grep -q "停在第 ${at} 步" "${TMP}/err" && grep -q 'emergency.md' "${TMP}/err" && ! grep -q '完成' "${TMP}/err"; then
    ok "d. 装过时第 ${at} 步失败：停下，后面的步骤没有调用，给出下一步"
  else
    bad "d. 装过时第 ${at} 步失败时不对：rc=$rc，调用了 ${got} 步"
  fi
done
dry REINSTALL_TEST_INSTALLED=1 REINSTALL_TEST_FAIL_AT=5 >/dev/null
grep -q '后台服务已经升级好了，只是应用没有更新' "${TMP}/err" && ok "d. 放应用失败：说明后台已升级、应用没有更新" || bad "d. 放应用失败的提示不对：$(tail -3 "${TMP}/err")"
for at in 1 2 3 5 6; do # 第 4 步是“没装过，跳过卸载”（不调用脚本）
  dry REINSTALL_TEST_INSTALLED=0 REINSTALL_TEST_FAIL_AT=$at >"${TMP}/f"; rc=$?
  got=$(grep -c '^RUN:' "${TMP}/f"); want=$at; [ $at -gt 4 ] && want=$((at - 1))
  if [ $rc != 0 ] && [ "$got" = "$want" ] && grep -q "停在第 ${at} 步" "${TMP}/err"; then
    ok "d. 没装过时第 ${at} 步失败：停下"
  else
    bad "d. 没装过时第 ${at} 步失败时不对：rc=$rc，调用了 ${got} 步"
  fi
done
for c in "10:已启用时先 sudo abroad down" "11:不降级" "13:先卸载" "14:--accept-unknown-version" "20:已经退回旧版本" "21:docs/emergency.md"; do
  code=${c%%:*} hint=${c#*:}
  dry REINSTALL_TEST_INSTALLED=1 REINSTALL_TEST_FAIL_AT=4 REINSTALL_TEST_RC=$code >"${TMP}/f"; rc=$?
  [ $rc = "$code" ] && grep -q -- "$hint" "${TMP}/err" && ! grep -q 'build-app' "${TMP}/f" && ok "d. 升级返回 ${code}：照原样退出，提示“${hint}”，没有放应用" || bad "d. 升级返回 ${code} 时不对：rc=$rc $(tail -1 "${TMP}/err")"
done

# ---- e. 放置逻辑只有一份 ----
# 放置的特征（临时名、包标识核对、复制应用包）只出现在 build-app.sh；reinstall.sh 里没有 cp、mv、rm，只调用 --place-only
hits=$(grep -rlE '\.ClaudeAbroad\.app\.(new|old)|bundle_id\(\)' scripts --include='*.sh' | grep -vE '^scripts/dev/(build-app|build-app-selftest|reinstall-selftest)\.sh$')
[ -z "$hits" ] && ok "e. 放置逻辑只在 build-app.sh 里" || bad "e. 别处也有放置逻辑：$hits"
printf '%s\n' "$(sed -e 's/^[[:space:]]*#.*$//' "$S")" | grep -nE '(^|[^a-z-])(cp|mv|rm|ditto)[[:space:]]' >"${TMP}/src" &&
  bad "e. reinstall.sh 自己复制或移动文件：$(cat "${TMP}/src")" || ok "e. reinstall.sh 没有自己复制、移动文件"
grep -q 'scripts/dev/build-app.sh --place-only' "$S" && ok "e. reinstall.sh 调用 build-app.sh --place-only" || bad "e. reinstall.sh 没调用 --place-only"

echo
[ $fails = 0 ] && echo "自测全部通过" || echo "自测：${fails} 处不通过"
exit $((fails > 0))
