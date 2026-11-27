#!/usr/bin/env bash
# scripts/dev/reinstall.sh 的自测（只用空跑，不调用真的 sudo，不碰系统路径）：
#   a. 装过 / 没装过时按顺序调用这几个脚本，没装过时跳过卸载；
#   b. 命令核对：只允许五行（三个构建、检查、安装脚本加一个卸载脚本），反例（多出一条、--yes、pfctl、networksetup、
#      route、ifconfig、launchctl）都被拦下；源码里也没有这些；
#   c. 以 root 运行被拒；测试变量离开空跑被拒；
#   d. 每一步注入失败都会停下，后面的步骤不执行，退出码非零，给出下一步。
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

ALLOW='^RUN: (scripts/dev/integrity-build\.sh --dev|scripts/dev/app-bundle\.sh|sudo scripts/m6/install/uninstall\.sh|scripts/m6/install/install\.sh --check-only|sudo scripts/m6/install/install\.sh)$'
BAN='(--yes|pfctl|networksetup|route|ifconfig|launchctl)'
cmdcheck() { # 只允许清单里的行，不许出现禁止的词
  grep -vE "$ALLOW" "$1" | grep -q . && return 1
  grep -qE "$BAN" "$1" && return 1
  return 0
}

# ---- a. 顺序 ----
dry REINSTALL_TEST_INSTALLED=1 >"${TMP}/i1"; rc=$?
want1='RUN: scripts/dev/integrity-build.sh --dev
RUN: scripts/dev/app-bundle.sh
RUN: sudo scripts/m6/install/uninstall.sh
RUN: scripts/m6/install/install.sh --check-only
RUN: sudo scripts/m6/install/install.sh'
[ $rc = 0 ] && [ "$(cat "${TMP}/i1")" = "$want1" ] && ok "a. 装过时：构建、打包、卸载、检查、安装，顺序对" || bad "a. 装过时的顺序不对：$(cat "${TMP}/i1")"
grep -q '主代理配置已随卸载清空' "${TMP}/err" && ok "a. 结尾提示重新添加主代理" || bad "a. 结尾没有提示重新添加主代理"
dry REINSTALL_TEST_INSTALLED=0 >"${TMP}/i0"; rc=$?
want0=$(printf '%s\n' "$want1" | grep -v uninstall)
[ $rc = 0 ] && [ "$(cat "${TMP}/i0")" = "$want0" ] && grep -q '没装过，跳过卸载' "${TMP}/err" && ok "a. 没装过时跳过卸载" || bad "a. 没装过时不对：$(cat "${TMP}/i0")"

# ---- b. 命令核对 ----
cmdcheck "${TMP}/i1" && cmdcheck "${TMP}/i0" && ok "b. 命令核对通过（只有五行允许的调用）" || bad "b. 命令核对不通过"
neg() { if cmdcheck "$2"; then bad "b. 反例没被拦下：$1"; else ok "b. 反例被拦下：$1"; fi; }
{ cat "${TMP}/i1"; echo 'RUN: rm -rf /Users'; } >"${TMP}/n1"; neg "多出一条命令" "${TMP}/n1"
sed 's#sudo scripts/m6/install/install.sh$#sudo scripts/m6/install/install.sh --yes#' "${TMP}/i1" >"${TMP}/n2"; neg "--yes" "${TMP}/n2"
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
  if [ $rc != 0 ] && [ "$got" = "$at" ] && grep -q "停在第 ${at} 步" "${TMP}/err" && grep -q 'emergency.md' "${TMP}/err"; then
    ok "d. 第 ${at} 步失败：停下，后面的步骤没有调用，给出下一步"
  else
    bad "d. 第 ${at} 步失败时不对：rc=$rc，调用了 ${got} 步"
  fi
done

echo
[ $fails = 0 ] && echo "自测全部通过" || echo "自测：${fails} 处不通过"
exit $((fails > 0))
