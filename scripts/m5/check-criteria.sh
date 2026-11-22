#!/usr/bin/env bash
# 核对 docs/m5-walkthrough.md 完成标准对照表里引用的自动测试真实存在（M5-11 验收 d）：
#   go:<包目录>#<测试名>  → 该目录下的 *_test.go 里有 func <测试名>(
#   swift:<测试函数名>    → app/Tests 下有 func <测试函数名>(
# 另核对：C1–C13、D1–D6 每一行都有着落（第一级、第二级、自动测试、推迟至少一项不是“—”）。
set -uo pipefail
cd "$(dirname "$0")/../.."
DOC="${M5_CRITERIA_DOC:-docs/m5-walkthrough.md}" # 自测用反例时覆盖
fails=0; n=0
bad() { echo "✗ $*"; fails=$((fails + 1)); }
table=$(sed -n '/^## 完成标准对照表/,/^## 结果回填/p' "$DOC")
for ref in $(printf '%s\n' "$table" | grep -oE 'go:[a-zA-Z0-9/_-]+#Test[A-Za-z0-9_]+' | sort -u); do
  n=$((n + 1)); dir=${ref#go:}; dir=${dir%%#*}; name=${ref##*#}
  grep -lq "^func ${name}(" "${dir}"/*_test.go 2>/dev/null || bad "找不到 ${ref}"
done
for ref in $(printf '%s\n' "$table" | grep -oE 'swift:[a-zA-Z0-9_]+' | sort -u); do
  n=$((n + 1)); name=${ref#swift:}
  grep -rqE "func ${name}\(" app/Tests || bad "找不到 ${ref}"
done
for id in C1 C2 C3 C4 C5 C6 C7 C8 C9 C10 C11 C12 C13 D1 D2 D3 D4 D5 D6; do
  row=$(printf '%s\n' "$table" | grep -E "^\| ${id} \|")
  [ -n "$row" ] || { bad "对照表里没有 ${id}"; continue; }
  cells=$(printf '%s' "$row" | awk -F'|' '{print $4"|"$5"|"$6"|"$7}')
  printf '%s' "$cells" | tr '|' '\n' | grep -qvE '^[[:space:]]*—?[[:space:]]*$' || bad "${id} 没有着落"
done
[ $fails = 0 ] && echo "完成标准对照表：19 条都有着落，引用的 ${n} 个自动测试都存在" || exit 1
