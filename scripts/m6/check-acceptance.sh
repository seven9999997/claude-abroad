#!/usr/bin/env bash
# 核对 docs/m6-acceptance.md（M6-10 验收 b）：
#   go:<包目录>#<测试名>      → 该目录下的 *_test.go 里有 func <测试名>(
#   swift:<测试函数名>        → app/Tests 下有 func <测试函数名>(
#   script:<路径>#<步骤编号>  → 文件存在，并且里面出现这个步骤编号
#   file:<路径>               → 文件存在
# 另核对：R1–R23（含 R9.1）、P1–P8、G1–G10（含 G4.1、G9.1、G9.2）每一行都在；第 1 节五条完成标准都在；
# 没有“待定”；没有把发布构建验证写成“达到”。只读，不改任何东西。
set -uo pipefail
cd "$(dirname "$0")/../.."
DOC="${M6_ACCEPT_DOC:-docs/m6-acceptance.md}" # 自测用反例时覆盖
fails=0; n=0
bad() { echo "✗ $*"; fails=$((fails + 1)); }
[ -f "$DOC" ] || { echo "✗ 没有 $DOC"; exit 1; }
for ref in $(grep -oE 'go:[a-zA-Z0-9/_.-]+#Test[A-Za-z0-9_]+' "$DOC" | sort -u); do
  n=$((n + 1)); dir=${ref#go:}; dir=${dir%%#*}; name=${ref##*#}
  grep -lq "^func ${name}(" "${dir}"/*_test.go 2>/dev/null || bad "找不到 ${ref}"
done
for ref in $(grep -oE 'swift:[a-zA-Z0-9_]+' "$DOC" | sort -u); do
  n=$((n + 1)); name=${ref#swift:}
  grep -rqE "func ${name}\(" app/Tests || bad "找不到 ${ref}"
done
for ref in $(grep -oE 'script:[a-zA-Z0-9/_.-]+#[A-Za-z0-9_-]+' "$DOC" | sort -u); do
  n=$((n + 1)); p=${ref#script:}; p=${p%%#*}; step=${ref##*#}
  [ -f "$p" ] || { bad "脚本不存在：${ref}"; continue; }
  grep -qF -- "$step" "$p" || bad "脚本里没有这一步：${ref}"
done
for ref in $(grep -oE 'file:[a-zA-Z0-9/_.-]+' "$DOC" | sort -u); do
  n=$((n + 1)); p=${ref#file:}
  [ -e "$p" ] || bad "文件不存在：${ref}"
done
for id in R1 R2 R3 R4 R5 R6 R7 R8 R9 R9.1 R10 R11 R12 R13 R14 R15 R16 R17 R18 R19 R20 R21 R22 R23 \
  P1 P2 P3 P4 P5 P6 P7 P8 G1 G2 G3 G4 G4.1 G5 G6 G7 G8 G9 G9.1 G9.2 G10; do
  grep -qE "^\| ${id//./\\.} \|" "$DOC" || bad "清单里没有 ${id} 这一行"
done
for k in 1 2 3 4 5; do
  sec1=$(sed -n '/^## 1\./,/^## 2\./p' "$DOC")
  printf '%s\n' "$sec1" | grep -qE "^\| ${k} \|" || bad "第 1 节缺第 ${k} 条完成标准"
done
# 风险表里写着“没有做”的行：引号里的功能名必须在第 7 节有对应的条目（不能只藏在风险表的半句话里）
sec5=$(sed -n '/^## 5\./,/^## 6\./p' "$DOC"); sec7=$(sed -n '/^## 7\./,/^## 8\./p' "$DOC")
while IFS= read -r row; do
  [ -n "$row" ] || continue
  id=$(printf '%s' "$row" | awk -F'|' '{gsub(/ /,"",$2); print $2}')
  names=$(printf '%s' "$row" | grep -oE '“[^”]+”[^|“]{0,6}没有做' | sed -E 's/^“([^”]+)”.*/\1/')
  total=$(printf '%s' "$row" | grep -o '没有做' | wc -l | tr -d ' ')
  named=$(printf '%s\n' "$names" | grep -c . | tr -d ' ')
  [ -n "$names" ] && [ "$total" = "$named" ] || { bad "${id} 写着“没有做”，但没有用引号写出是什么功能"; continue; }
  while IFS= read -r nm; do
    printf '%s\n' "$sec7" | grep -qF -- "$nm" || bad "${id} 里的“${nm}”没有做，但第 7 节没有对应的条目"
  done <<<"$names"
done < <(printf '%s\n' "$sec5" | grep -E '^\| R' | grep '没有做')
grep -q '待定' "$DOC" && bad "清单里还有“待定”"
grep -qE '^\| 用发布构建验证' "$DOC" || bad "“没有做”里缺用发布构建验证真实效果那一行"
grep -E '^\| [^|]*发布构建验证' "$DOC" | grep -qE '\*\*达到' && bad "发布构建验证不能写成“达到”"
[ $fails = 0 ] && echo "最终验收清单：引用的 ${n} 处测试、脚本步骤和文件都存在，各行齐全" || exit 1
