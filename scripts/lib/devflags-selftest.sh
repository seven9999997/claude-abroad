#!/usr/bin/env bash
# 各真机脚本自测共用（M6-1）：“二进制认识要传的开发参数”的检查。
#   1. 真编出两份 abroadd：正式构建（不带标签）和开发构建（-tags abroaddev）；
#   2. 正式构建：<前缀>_dev_flags_missing 报缺、返回非零；开发构建：返回 0；
#   3. run.sh 里这条检查在前提检查里，排在建状态目录（任何改动）之前。
# 用法：scripts/lib/devflags-selftest.sh <m2|m3|m4|m5> <要传的开发参数…>；输出 ✓/✗ 行，有 ✗ 时退出码 1。
set -uo pipefail
cd "$(dirname "$0")/../.."
tag=$1; shift
P=$(printf '%s' "$tag" | tr '[:lower:]' '[:upper:]')
TMP=$(mktemp -d "${TMPDIR:-/tmp}/devflags.XXXXXX")
trap 'rm -rf "${TMP}"' EXIT
fails=0
ok() { echo "✓ $*"; }
bad() { echo "✗ $*"; fails=$((fails + 1)); }
go build -o "${TMP}/abroadd-release" ./cmd/abroadd 2>"${TMP}/b1" || bad "编不出正式构建：$(cat "${TMP}/b1")"
go build -tags abroaddev -o "${TMP}/abroadd-dev" ./cmd/abroadd 2>"${TMP}/b2" || bad "编不出开发构建：$(cat "${TMP}/b2")"
out=$(
  export "${P}_TEST_STATE=${TMP}/st" DRY_RUN=1
  . "scripts/${tag}/lib.sh"
  miss=$("${tag}_dev_flags_missing" "${TMP}/abroadd-release" "$@"); echo "release rc=$? miss=$(echo ${miss})"
  miss=$("${tag}_dev_flags_missing" "${TMP}/abroadd-dev" "$@"); echo "dev rc=$? miss=$(echo ${miss})"
  "${tag}_dev_flags_missing" "${TMP}/nope" "$@" >/dev/null; echo "nobin rc=$?"
)
printf '%s\n' "$out" | grep -q '^release rc=1 miss=.*--' && ok "${tag}：正式构建的 abroadd 认不出 $*（报缺：$(printf '%s\n' "$out" | sed -n 's/^release rc=1 miss=//p')）" || bad "${tag}：正式构建没被拦下：$out"
printf '%s\n' "$out" | grep -q '^dev rc=0 miss=$' && ok "${tag}：开发构建的 abroadd 认识 $*" || bad "${tag}：开发构建被误拦：$out"
printf '%s\n' "$out" | grep -q '^nobin rc=2' && ok "${tag}：程序不存在时返回 2" || bad "${tag}：程序不存在时：$out"
chk=$(grep -n "${tag}_dev_flags_missing" "scripts/${tag}/run.sh" | head -1 | cut -d: -f1)
pre=$(grep -n '^precheck() {' "scripts/${tag}/run.sh" | cut -d: -f1)
init=$(grep -n "^${tag}_state_init" "scripts/${tag}/run.sh" | head -1 | cut -d: -f1)
[ -n "$chk" ] && [ -n "$pre" ] && [ -n "$init" ] && [ "$pre" -lt "$chk" ] && [ "$chk" -lt "$init" ] &&
  ok "${tag}：检查在前提检查里（第 ${chk} 行），在建状态目录（第 ${init} 行）之前" || bad "${tag}：检查的位置不对（precheck ${pre}、检查 ${chk}、建状态目录 ${init}）"
exit $((fails > 0))
