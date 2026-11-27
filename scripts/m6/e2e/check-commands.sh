#!/usr/bin/env bash
# 核对 M6-9 端到端脚本的空跑输出：
#   1. 每一条 CHANGE: 都在 commands.txt 的清单里；没有 pfctl、networksetup、route、ifconfig、pkill、killall、open
#      （网络由 abroadd 和卸载脚本负责；应用由你自己打开）；kill 只对死人开关；
#   2. 第一段：死人开关之前只有建状态目录；地区快照在安装之前；安装在第一个检查点之前；本机代理端口的核对（R23）在启用之后、回国之前；
#      最后请你重启，并且取消了死人开关；
#   3. 第二段：先启动死人开关，再核对 guard；应急演练先停后台服务、再执行应急程序、再注册回来；卸载在回国检查点之后；最后执行 restore.sh（地区按快照还原、删状态目录）。
# 用法：scripts/m6/e2e/check-commands.sh <第一段空跑输出> <第二段空跑输出>
set -uo pipefail
cd "$(dirname "$0")/../../.." || exit 1
ROOT=$(pwd)
[ $# -ge 3 ] || { echo "用法：$0 <第一段空跑输出> <第二段空跑输出> <第三段空跑输出>" >&2; exit 2; }
fails=0
bad() { echo "✗ $*"; fails=$((fails + 1)); }
PAT=$(mktemp "${TMPDIR:-/tmp}/m6ecc.XXXXXX"); trap 'rm -f "${PAT:?}"' EXIT
grep -v '^#' scripts/m6/e2e/commands.txt | grep . >"$PAT"
BAN='(^|[ /])(pfctl|networksetup|route|ifconfig|pkill|killall|open)( |$)'
# 唯一允许的 pf 改动：只清测试段的连接状态（M6-11b，pfcheck.sh）
PFOK='^pfctl -k (0\.0\.0\.0/0 -k 192\.0\.2\.0/24|::/0 -k 2001:db8:abd::/48)$'
for f in "$1" "$2" "$3"; do
  c=$(sed -e "s|${ROOT}|<仓库>|g" "$f" | sed -n 's/^CHANGE: //p')
  [ -n "$c" ] || bad "$(basename "$f")：没有任何改动命令"
  while IFS= read -r l; do [ -n "$l" ] && bad "不在清单里：$l"; done < <(printf '%s\n' "$c" | grep -vE -f "$PAT")
  while IFS= read -r l; do [ -n "$l" ] && bad "禁止的命令：$l"; done < <(printf '%s\n' "$c" | grep -E -- "$BAN" | grep -vE -- "$PFOK")
  sed -n 's/^QUERY: //p' "$f" | grep -vE '^pfctl -a com\.apple/250\.abroad -s rules$' | grep -E -- "$BAN" | while IFS= read -r l; do echo "✗ 禁止的查询：$l"; done >>"$PAT.q"
done
awk '
  /^CHANGE: 死人开关/ { dm = NR; next }
  /^CHANGE: / && !dm && $0 !~ /claude-abroad-m6e$/ { print "死人开关之前不能有：" $0 }
  /^STATE: snap\/region/ { rg = NR }
  /^CHANGE: scripts\/m6\/install\/install\.sh$/ { if (!rg) print "地区快照应在安装之前"; inst = NR }
  /^STEP: 检查点 P1/ { if (!inst) print "安装应在第一个检查点之前"; p1 = NR }
  /^STEP: 检查点 P2/ { p2 = NR }
  /^STEP: 核对 CC1/ { if (!p2) print "本机代理端口的核对（R23）应在启用之后"; cc1 = NR }
  /^STEP: 检查点 P3/ { if (!cc1) print "回国之前应先核对本机代理端口（R23）" }
  /^CHANGE: kill <死人开关的 PID>$/ { stop = NR }
  /^WAIT: 重启电脑/ { if (!stop) print "请你重启之前应先取消死人开关"; rb = NR }
  END { if (!dm) print "第一段没有死人开关"; if (!p1) print "第一段没有检查点"; if (!cc1) print "第一段没有核对本机代理端口（R23）"; if (!rb) print "第一段最后没有请你重启" }' "$1" |
  while IFS= read -r l; do echo "✗ 第一段：$l"; done >"$PAT.o"
awk '
  /^CHANGE: 死人开关/ { dm = NR }
  /^STEP: 核对 B1/ { if (!dm) print "核对 guard 之前应先启动死人开关"; b1 = NR }
  /^CHANGE: launchctl bootout / { bo = NR }
  /^CHANGE: \/Library\/Application Support\/claude-abroad\/bin\/abroad-rescue$/ { if (!bo) print "应急演练应先停掉后台服务"; rs = NR }
  /^CHANGE: launchctl bootstrap / { if (!rs) print "应急程序之后才把后台服务注册回来"; bs = NR }
  /^STEP: 检查点 P5/ { p5 = NR }
  /^CHANGE: kill <死人开关的 PID>$/ { if (!p5) print "取消死人开关应在回国检查点之后"; stop = NR }
  /^WAIT: 再重启一次电脑/ { if (!stop) print "请你再重启之前应先取消死人开关"; rb = NR }
  /^CHANGE: scripts\/m6\/install\/uninstall\.sh$/ { print "卸载应在第三段" }
  END { if (!rs || !bs) print "第二段没有应急演练"; if (!b1) print "第二段没有核对 guard"; if (!rb) print "第二段最后没有请你再重启" }' "$2" |
  while IFS= read -r l; do echo "✗ 第二段：$l"; done >>"$PAT.o"
awk '
  /^CHANGE: 死人开关/ { dm = NR }
  /^STEP: 核对 B3/ { if (!dm) print "核对 guard 之前应先启动死人开关"; b3 = NR }
  /^CHANGE: scripts\/m6\/install\/uninstall\.sh$/ { if (!un) { if (!b3) print "卸载应在核对回国拦截之后"; un = NR } }
  /^CHANGE: \/usr\/sbin\/systemsetup -settimezone/ { if (!un) print "地区还原应在卸载之后"; rg = NR }
  /^CHANGE: rm -rf \/Library\/Application Support\/claude-abroad-m6e$/ { if (!rg) print "删状态目录之前应先还原地区"; rm = NR }
  END { if (!b3) print "第三段没有核对开机时的回国拦截"; if (!un) print "第三段没有卸载"; if (!rm) print "第三段最后没有执行 restore.sh" }' "$3" |
  while IFS= read -r l; do echo "✗ 第三段：$l"; done >>"$PAT.o"
[ -s "$PAT.q" ] && cat "$PAT.q" >>"$PAT.o"; rm -f "${PAT:?}.q"
if [ -s "$PAT.o" ]; then cat "$PAT.o"; fails=$((fails + $(wc -l <"$PAT.o"))); fi
rm -f "${PAT:?}.o"
[ $fails = 0 ] && echo "✓ 命令核对通过" || echo "命令核对：${fails} 处不符"
exit $((fails > 0))
