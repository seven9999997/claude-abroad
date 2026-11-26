#!/usr/bin/env bash
# 核对第四层实测脚本的空跑输出：
#   1. 每一条 CHANGE: 都在 commands.txt 里；没有 pfctl、networksetup、route、ifconfig、pkill、killall、open、launchctl；
#   2. 顺序：死人开关在写托管设置之前；第一次写的是 47821；T2 之后、收尾之前写回 47821；最后删分片、取消死人开关；
#      每一次启动 Claude Code（CLAUDE:）都在写入分片之后、删除之前。
# 用法：scripts/m6/layer4/check-commands.sh <run 空跑输出>
set -uo pipefail
cd "$(dirname "$0")/../../.." || exit 1
ROOT=$(pwd)
[ $# -ge 1 ] || { echo "用法：$0 <run 空跑输出>" >&2; exit 2; }
fails=0
bad() { echo "✗ $*"; fails=$((fails + 1)); }
PAT=$(mktemp "${TMPDIR:-/tmp}/m6lcc.XXXXXX"); trap 'rm -f "${PAT:?}" "${PAT:?}.o"' EXIT
grep -v '^#' scripts/m6/layer4/commands.txt | grep . >"$PAT"
c=$(sed -e "s|${ROOT}|<仓库>|g" "$1" | sed -n 's/^CHANGE: //p')
[ -n "$c" ] || bad "没有任何改动命令"
while IFS= read -r l; do [ -n "$l" ] && bad "不在清单里：$l"; done < <(printf '%s\n' "$c" | grep -vE -f "$PAT")
while IFS= read -r l; do [ -n "$l" ] && bad "禁止的命令：$l"; done < <(printf '%s\n' "$c" | grep -E '(^|[ /])(pfctl|networksetup|route|ifconfig|pkill|killall|open|launchctl)( |$)')
awk '
  /^CHANGE: 死人开关/ { dm = NR }
  /^CHANGE: install .*claude-abroad\.json$/ {
    if (!dm) print "写托管设置之前没有死人开关"
    n++; if (n == 1 && $0 !~ /--port 47821>/) print "第一次写的应是 47821"
    if ($0 ~ /--port 4789[78]>/) t2 = 1; else if (t2) back = 1
    written = 1; next
  }
  /^CLAUDE: / { if (!written || removed) print "启动 Claude Code 时没有写入分片：" $0 }
  /^STEP: 收尾/ { if (t2 && !back) print "T2 之后没有写回 47821" }
  /^CHANGE: rm -f .*claude-abroad\.json/ { removed = 1 }
  /^CHANGE: kill <死人开关的 PID>$/ { if (!removed) print "取消死人开关之前没有删分片"; stop = 1 }
  END { if (!t2) print "没有 T2 的两种端口"; if (!removed) print "最后没有删分片"; if (!stop) print "最后没有取消死人开关" }' "$1" >"$PAT.o"
while IFS= read -r l; do bad "顺序：$l"; done <"$PAT.o"
[ $fails = 0 ] && echo "✓ 命令核对通过" || echo "命令核对：${fails} 处不符"
exit $((fails > 0))
