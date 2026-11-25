#!/usr/bin/env bash
# 核对 M6-9 端到端脚本的空跑输出：
#   1. 每一条 CHANGE: 都在 commands.txt 的清单里；没有 pfctl、networksetup、route、ifconfig、pkill、killall、open
#      （网络由 abroadd 和卸载脚本负责；应用由你自己打开）；kill 只对死人开关；
#   2. 第一段：死人开关之前只有建状态目录；地区快照在安装之前；安装在第一个检查点之前；最后请你重启，并且取消了死人开关；
#   3. 第二段：先启动死人开关，再核对 guard；卸载在回国检查点之后；最后执行 restore.sh（地区按快照还原、删状态目录）。
# 用法：scripts/m6/e2e/check-commands.sh <第一段空跑输出> <第二段空跑输出>
set -uo pipefail
cd "$(dirname "$0")/../../.." || exit 1
ROOT=$(pwd)
[ $# -ge 2 ] || { echo "用法：$0 <第一段空跑输出> <第二段空跑输出>" >&2; exit 2; }
fails=0
bad() { echo "✗ $*"; fails=$((fails + 1)); }
PAT=$(mktemp "${TMPDIR:-/tmp}/m6ecc.XXXXXX"); trap 'rm -f "${PAT:?}"' EXIT
grep -v '^#' scripts/m6/e2e/commands.txt | grep . >"$PAT"
BAN='(^|[ /])(pfctl|networksetup|route|ifconfig|pkill|killall|open)( |$)'
for f in "$1" "$2"; do
  c=$(sed -e "s|${ROOT}|<仓库>|g" "$f" | sed -n 's/^CHANGE: //p')
  [ -n "$c" ] || bad "$(basename "$f")：没有任何改动命令"
  while IFS= read -r l; do [ -n "$l" ] && bad "不在清单里：$l"; done < <(printf '%s\n' "$c" | grep -vE -f "$PAT")
  while IFS= read -r l; do [ -n "$l" ] && bad "禁止的命令：$l"; done < <(printf '%s\n' "$c" | grep -E -- "$BAN")
done
awk '
  /^CHANGE: 死人开关/ { dm = NR; next }
  /^CHANGE: / && !dm && $0 !~ /claude-abroad-m6e$/ { print "死人开关之前不能有：" $0 }
  /^STATE: snap\/region/ { rg = NR }
  /^CHANGE: scripts\/m6\/install\/install\.sh$/ { if (!rg) print "地区快照应在安装之前"; inst = NR }
  /^STEP: 检查点 P1/ { if (!inst) print "安装应在第一个检查点之前"; p1 = NR }
  /^CHANGE: kill <死人开关的 PID>$/ { stop = NR }
  /^WAIT: 重启电脑/ { if (!stop) print "请你重启之前应先取消死人开关"; rb = NR }
  END { if (!dm) print "第一段没有死人开关"; if (!p1) print "第一段没有检查点"; if (!rb) print "第一段最后没有请你重启" }' "$1" |
  while IFS= read -r l; do echo "✗ 第一段：$l"; done >"$PAT.o"
awk '
  /^CHANGE: 死人开关/ { dm = NR }
  /^STEP: 核对 B1/ { if (!dm) print "核对 guard 之前应先启动死人开关"; b1 = NR }
  /^STEP: 检查点 P5/ { p5 = NR }
  /^CHANGE: scripts\/m6\/install\/uninstall\.sh$/ { if (!un) { if (!p5) print "卸载应在回国检查点之后"; un = NR } }
  /^CHANGE: \/usr\/sbin\/systemsetup -settimezone/ { if (!un) print "地区还原应在卸载之后"; rg = NR }
  /^CHANGE: rm -rf \/Library\/Application Support\/claude-abroad-m6e$/ { if (!rg) print "删状态目录之前应先还原地区"; rm = NR }
  END { if (!b1) print "第二段没有核对 guard"; if (!un) print "第二段没有卸载"; if (!rm) print "第二段最后没有执行 restore.sh" }' "$2" |
  while IFS= read -r l; do echo "✗ 第二段：$l"; done >>"$PAT.o"
if [ -s "$PAT.o" ]; then cat "$PAT.o"; fails=$((fails + $(wc -l <"$PAT.o"))); fi
rm -f "${PAT:?}.o"
[ $fails = 0 ] && echo "✓ 命令核对通过" || echo "命令核对：${fails} 处不符"
exit $((fails > 0))
