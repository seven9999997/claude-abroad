#!/usr/bin/env bash
# pf 规则的语法检查（M2-6a、M2-6b）：只运行四种写死的形式（另有 -n -v -o none -f <文件>）：
#   /sbin/pfctl -n -f <文件>、/sbin/pfctl -n -v -f <文件>（采集 pf 打印的规范形式，给 Matcher 做样本）、
#   /sbin/pfctl -n -f -（规则从 stdin 读，实测 Load 能否经 stdin）。
#   -n 只解析不加载；非 root 也打不开 /dev/pf，所以这个脚本不会改动 pf 的任何状态，更不会断网。
#   不带 sudo；以 root 身份运行时拒绝（root 下的真实检查放在 M2-9 交给用户的脚本里）。
#   不接受任何参数，也不执行其他 pfctl 命令。
# 规则由 abroad-dev pf-render 生成（本机没有 _abroad 用户，用 nobody）。结果写进 scripts/dev/out/pf-syntax-<时间>.txt。
set -euo pipefail

if [[ $# -ne 0 ]]; then
  echo "用法：scripts/dev/pf-syntax.sh（不接受参数）" >&2
  exit 2
fi
if [[ "$(id -u)" -eq 0 ]]; then
  echo "拒绝以 root 运行：这个脚本只做非 root 的语法检查（pfctl -n）" >&2
  exit 2
fi

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
OUT="scripts/dev/out"
BIN="${OUT}/bin"
TS="$(date +%Y%m%d-%H%M%S)"
mkdir -p "${BIN}"
EVID="${OUT}/pf-syntax-${TS}.txt"
go build -o "${BIN}/abroad-dev" ./cmd/abroad-dev

rc_all=0
check() { # check <名字> <pf-render 参数…>
  local name=$1
  shift
  local f="${OUT}/pf-${name}-${TS}.conf"
  "${BIN}/abroad-dev" pf-render "$@" >"${f}"
  {
    echo "==== ${name}：/sbin/pfctl -n -f ${f}"
    cat "${f}"
    echo "---- 输出："
  } >>"${EVID}"
  local rc=0
  /sbin/pfctl -n -f "${f}" >>"${EVID}" 2>&1 || rc=$?
  echo "---- 退出码 ${rc}" >>"${EVID}"
  echo "${name}：pfctl -n 退出码 ${rc}"
  if [[ ${rc} -ne 0 ]]; then rc_all=1; fi
  # pf 打印的规范形式（-n -v）：单独存一份，Matcher 的测试用它当样本。
  local vf="${OUT}/pf-${name}-${TS}.nvf.txt"
  local vrc=0
  /sbin/pfctl -n -v -f "${f}" >"${vf}" 2>&1 || vrc=$?
  { echo "---- -n -v -f 的输出（退出码 ${vrc}）："; cat "${vf}"; } >>"${EVID}"
  echo "${name}：pfctl -n -v 退出码 ${vrc}（输出 ${vf}）"
  if [[ ${vrc} -ne 0 ]]; then rc_all=1; fi
  # 关闭优化器时的打印形式（-n -v -o none）：Load 用 -o none，Matcher 和 FakePF 以这份为准。
  local of="${OUT}/pf-${name}-${TS}.nvf-onone.txt"
  local orc=0
  /sbin/pfctl -n -v -o none -f "${f}" >"${of}" 2>&1 || orc=$?
  { echo "---- -n -v -o none -f 的输出（退出码 ${orc}）："; cat "${of}"; } >>"${EVID}"
  echo "${name}：pfctl -n -v -o none 退出码 ${orc}（输出 ${of}）"
  if [[ ${orc} -ne 0 ]]; then rc_all=1; fi
}
check full --utun utun9 --lan 192.168.1.0/24 --vmif bridge100 --vmif vmenet0
check boot --lan 192.168.1.0/24 --vmif bridge100
check nolan --utun utun9 --vmif bridge100
# 反例：故意写错的规则必须解析失败，证明非 root 下 pfctl -n 确实做了语法检查（不是什么都不查就返回 0）。
BAD="${OUT}/pf-bad-${TS}.conf"
printf 'pass out quick on lo0 all\npass out quick bogus-keyword all\n' >"${BAD}"
{
  echo "==== 反例：/sbin/pfctl -n -f ${BAD}（应失败）"
  cat "${BAD}"
  echo "---- 输出："
} >>"${EVID}"
brc=0
/sbin/pfctl -n -f "${BAD}" >>"${EVID}" 2>&1 || brc=$?
echo "---- 退出码 ${brc}" >>"${EVID}"
echo "反例：pfctl -n 退出码 ${brc}（应非 0）"
if [[ ${brc} -eq 0 ]]; then rc_all=1; fi
# 经 stdin 送规则（-n -f -）：正例应退出 0，反例应非 0。两者都成立，Load 才用 stdin。
src=0
/sbin/pfctl -n -f - <"${OUT}/pf-full-${TS}.conf" >>"${EVID}" 2>&1 || src=$?
echo "---- stdin 正例（-n -f - < full）退出码 ${src}" >>"${EVID}"
sbrc=0
/sbin/pfctl -n -f - <"${BAD}" >>"${EVID}" 2>&1 || sbrc=$?
echo "---- stdin 反例（-n -f - < bad）退出码 ${sbrc}" >>"${EVID}"
echo "stdin：正例退出码 ${src}，反例退出码 ${sbrc}"
if [[ ${src} -eq 0 && ${sbrc} -ne 0 ]]; then
  echo "结论：pfctl 能经 stdin 读规则（Load 用 -f -）" | tee -a "${EVID}"
else
  echo "结论：pfctl 不能可靠地经 stdin 读规则（Load 写临时文件）" | tee -a "${EVID}"
fi
echo "证据：${EVID}"
exit "${rc_all}"
