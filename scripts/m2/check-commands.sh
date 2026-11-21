#!/usr/bin/env bash
# 核对 DRY_RUN=1 空跑的输出（M2-9c 验收 d、e）：
#   - 每一条 CHANGE: 都落在 scripts/m2/commands.txt 的清单里；
#   - 没有禁止的命令：pfctl -d/-e、不带锚点的 pfctl -f 和 -F all、pkill、killall、networksetup -setdnsservers、
#     对默认路由的 route delete；
#   - run.sh：死人开关是第一条改动命令；restore.sh：先停进程和服务，再清网络，最后清 pf，然后删用户。
# 用法：scripts/m2/check-commands.sh <run 空跑输出> <restore 空跑输出>
set -uo pipefail
cd "$(dirname "$0")/../.."
[ $# -eq 2 ] || { echo "用法：$0 <run 空跑输出> <restore 空跑输出>" >&2; exit 2; }
fails=0
bad() { echo "✗ $*"; fails=$((fails + 1)); }
pats=$(grep -v '^#' scripts/m2/commands.txt | grep .)
BAN='pfctl (-d|-e)( |$)|pfctl( -o none)? -f |pfctl -F all|pkill|killall|networksetup -setdnsservers|route .*delete.*default|route .*delete -net 0\.0\.0\.0/0'
for f in "$1" "$2"; do
  n=0
  while IFS= read -r line; do
    cmd=${line#CHANGE: }
    n=$((n + 1))
    ok=0
    while IFS= read -r p; do
      if printf '%s\n' "$cmd" | grep -qE -- "$p"; then ok=1; break; fi
    done <<<"$pats"
    [ $ok = 1 ] || bad "不在清单里：$cmd"
    if printf '%s\n' "$cmd" | grep -qE -- "$BAN"; then bad "禁止的命令：$cmd"; fi
  done < <(grep '^CHANGE: ' "$f")
  echo "$(basename "$f")：核对了 $n 条改动命令"
done
# 死人开关之前只允许建状态目录（死人开关的日志和 PID 写在里面），其余改动都要在它之后。
pre=$(awk '/^CHANGE: 死人开关/{exit} /^CHANGE: /{print}' "$1")
if ! grep -q '^CHANGE: 死人开关' "$1"; then
  bad "run.sh 的空跑里没有死人开关"
elif printf '%s\n' "$pre" | grep -vE '^CHANGE: (mkdir -p|chmod 755) /var/run/claude-abroad-m2$' | grep -q .; then
  bad "死人开关之前有建状态目录以外的改动：$(printf '%s' "$pre" | tr '\n' ' ')"
else
  echo "✓ 死人开关之前只有建状态目录（$(printf '%s\n' "$pre" | grep -c .) 条），其余改动都在它之后"
fi
line_of() { grep -n -m1 -E "$1" "$2" | cut -d: -f1; }
a=$(line_of 'CHANGE: launchctl bootout' "$2"); b=$(line_of 'CHANGE: kill -TERM <abroad-vm' "$2")
c=$(line_of 'CHANGE: route -n delete' "$2"); d=$(line_of 'CHANGE: scutil <<remove' "$2"); e=$(line_of 'CHANGE: networksetup -setv6' "$2")
g=$(line_of 'CHANGE: /sbin/pfctl -a .* -F all' "$2"); h=$(line_of 'CHANGE: dscl . -delete' "$2"); k=$(line_of 'CHANGE: kill <死人开关' "$2")
m=$(line_of 'CHANGE: mv /var/run/claude-abroad-m2/abroadd ' "$2")
for v in a b c d e g h k m; do [ -n "${!v}" ] || bad "restore.sh 的空跑里缺了一步（${v}）"; done
if [ -n "$a$b$c$d$e$g$h$k" ] && [ "$a" -lt "$c" ] && [ "$b" -lt "$c" ] && [ "$c" -lt "$g" ] && [ "$d" -lt "$g" ] && [ "$e" -lt "$g" ] && [ "$g" -lt "$m" ] && [ "$m" -lt "$h" ] && [ "$h" -lt "$k" ]; then
  echo "✓ restore.sh 的顺序：停服务（${a}）、停虚拟机（${b}）→ 清网络（路由 ${c}、DNS ${d}、IPv6 ${e}）→ 清 pf（${g}）→ 改名备份 abroadd 状态（${m}）→ 删用户（${h}）→ 取消死人开关（${k}）"
else
  bad "restore.sh 的顺序不对：$a $b $c $d $e $g $h $k"
fi
[ $fails = 0 ] && echo "✓ 命令核对通过" || echo "✗ 命令核对没通过（$fails 处）"
exit $((fails > 0))
