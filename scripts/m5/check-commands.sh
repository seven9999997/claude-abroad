#!/usr/bin/env bash
# 核对 DRY_RUN=1 空跑的输出（M5-11）：
#   - 每一条 CHANGE: 都落在 scripts/m5/commands.txt 的清单里；
#   - 禁止项沿用 M3、M4：pfctl -d/-e、不带锚点的 pfctl -f 和 -F all、pkill、killall、networksetup -setdnsservers、对默认路由的
#     route delete；另加：脚本自己不改语言（只能 restore 按快照写回）、不发信号给本脚本记下的 PID 文件和死人开关以外的进程、
#     不打开应用（open）、不按名字结束进程；
#   - run.sh：死人开关之前只有建状态目录；快照（网络、地区、持久副本）在安装程序、建用户之前；
#   - restore.sh：停服务 → 清网络 → 还原地区 → 清 pf → 删 socket 目录和命令行链接 → 删用户 → 取消死人开关 → 删安装的程序。
# 用法：scripts/m5/check-commands.sh <run 空跑输出> <restore 空跑输出>
set -uo pipefail
cd "$(dirname "$0")/../.."
[ $# -ge 2 ] || { echo "用法：$0 <run 空跑输出> <restore 空跑输出>" >&2; exit 2; }
fails=0
bad() { echo "✗ $*"; fails=$((fails + 1)); }
BAN='pfctl (-d|-e)( |$)|pfctl( -o none)? -f |pfctl -F all|pkill|killall|networksetup -setdnsservers|route .*delete.*default|route .*delete -net 0\.0\.0\.0/0|(^| )open( |$)'
PATF=$(mktemp "${TMPDIR:-/tmp}/m5cc.XXXXXX"); CMDF=$(mktemp "${TMPDIR:-/tmp}/m5cc.XXXXXX")
trap 'rm -f "$PATF" "$CMDF"' EXIT
grep -v '^#' scripts/m5/commands.txt | grep . >"$PATF"
for f in "$1" "$2"; do
  grep '^CHANGE: ' "$f" | sed 's/^CHANGE: //' >"$CMDF"
  while IFS= read -r cmd; do bad "不在清单里：$cmd"; done < <(grep -vE -f "$PATF" "$CMDF")
  while IFS= read -r cmd; do bad "禁止的命令：$cmd"; done < <(grep -E -- "$BAN" "$CMDF")
  while IFS= read -r cmd; do bad "脚本不能自己改语言（只能 restore 按快照写回）：$cmd"; done < <(grep -E -- '(write|delete).*(AppleLanguages|AppleLocale)' "$CMDF" | grep -vF -- '快照里的')
  while IFS= read -r cmd; do bad "只能向本脚本记下的 PID 文件发信号：$cmd"; done < <(grep -E -- '(^| )kill( |$)' "$CMDF" | grep -vE -- '^kill( -TERM| -KILL)? <abroad-vm\.pid 里的 PID>$|^kill <死人开关的 PID>$')
done
# run.sh 的顺序：死人开关之前只有建状态目录；快照在安装之前。
R=$(grep '^CHANGE: ' "$1" | sed 's/^CHANGE: //')
first_dm=$(printf '%s\n' "$R" | grep -n '^死人开关' | head -1 | cut -d: -f1)
[ -n "$first_dm" ] || bad "run.sh 没有启动死人开关"
if [ -n "$first_dm" ]; then
  printf '%s\n' "$R" | head -n $((first_dm - 1)) | grep -vE '^(mkdir -p|chmod 755) /var/run/claude-abroad-m5$' | while IFS= read -r c; do echo "✗ 死人开关之前不能有：$c"; done | grep . && fails=$((fails + 1))
fi
snap=$(printf '%s\n' "$R" | grep -n 'claude-abroad-m5-snap/$' | head -1 | cut -d: -f1)
inst=$(printf '%s\n' "$R" | grep -n '^mkdir -p /Library/Application Support/claude-abroad-m5/bin' | head -1 | cut -d: -f1)
usr=$(printf '%s\n' "$R" | grep -n '^dscl \. -create' | head -1 | cut -d: -f1)
[ -n "$snap" ] && [ -n "$inst" ] && [ "$snap" -lt "$inst" ] && [ "$snap" -lt "${usr:-0}" ] || bad "快照（持久副本）要在安装程序、建用户之前"
sockdir=$(printf '%s\n' "$R" | grep -n '^mkdir -p /var/run/claude-abroad$' | head -1 | cut -d: -f1)
boot=$(printf '%s\n' "$R" | grep -n '^launchctl bootstrap' | head -1 | cut -d: -f1)
[ -n "$sockdir" ] && [ -n "$boot" ] && [ "$sockdir" -lt "$boot" ] || bad "共享 socket 目录要在启动 abroadd 之前建好"
# restore.sh 的顺序。
S=$(grep '^CHANGE: ' "$2" | sed 's/^CHANGE: //')
pos() { printf '%s\n' "$S" | grep -nE -- "$1" | head -1 | cut -d: -f1; }
order=("launchctl bootout" "route -n delete|scutil <<remove|networksetup -setv6" "systemsetup -settimezone" "pfctl -a com\.apple/250\.abroad -F all" "rm -rf /var/run/claude-abroad$" "dscl \. -delete" "rm -rf /Library/Application Support/claude-abroad-m5$")
last=0
for o in "${order[@]}"; do
  p=$(pos "$o")
  if [ -z "$p" ]; then bad "restore.sh 缺少：$o"; continue; fi
  [ "$p" -gt "$last" ] || bad "restore.sh 的顺序不对：$o"
  last=$p
done
[ $fails = 0 ] && echo "✓ 命令核对通过" || echo "命令核对：${fails} 处不符"
exit $((fails > 0))
