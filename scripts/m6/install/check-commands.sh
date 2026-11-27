#!/usr/bin/env bash
# 核对开发安装、卸载脚本（scripts/m6/install）会执行的命令（M6-8b）：
#   1. 空跑输出里每一条 CHANGE:、QUERY: 都在 commands.txt 的清单里（仓库路径先换成 <仓库>）；
#   2. 安装的空跑里没有 pfctl、networksetup、scutil、route、ifconfig、kill（安装不碰网络）；
#      两份空跑里都没有 pfctl -d/-e/-f、route、ifconfig、kill、pkill、killall；
#   3. 顺序：安装先建快照目录、拍快照，再做别的改动；guard 先于 abroadd 注册；
#      卸载先走 abroad uninstall，再停后台服务，再兜底，再删 plist、用户、目录，快照目录最后删；
#   4. 源码检查（install.sh、uninstall.sh、lib.sh 去掉注释后）：没有 kill、route、ifconfig、pfctl -d/-e/-f；
#      rm -rf 的目标都经 P "…" 或带 ${VAR:?} 保护；
#   5. 原地升级（第三份空跑，v0.71）：每条命令都在清单里；没有 pfctl、networksetup、scutil、dscl、dscacheutil（升级不碰网络、
#      不动专用用户），不碰状态目录 state/、日志目录；顺序：先只读查状态，再停 abroadd，再把旧版搬进 prev，再放新文件，
#      最后启动 abroadd、核对，成功后才删 prev。
# 用法：scripts/m6/install/check-commands.sh <install 空跑输出> <uninstall 空跑输出> [<install --upgrade 空跑输出>]
#      （M6I_CC_SRC_DIR 只给自测的反例用：换一个目录做源码检查）
set -uo pipefail
cd "$(dirname "$0")/../../.." || exit 1
ROOT=$(pwd)
[ $# -ge 2 ] || { echo "用法：$0 <install 空跑输出> <uninstall 空跑输出> [<install --upgrade 空跑输出>]" >&2; exit 2; }
fails=0
bad() { echo "✗ $*"; fails=$((fails + 1)); }
LIST=scripts/m6/install/commands.txt
SRC="${M6I_CC_SRC_DIR:-scripts/m6/install}"
TMPD=$(mktemp -d "${TMPDIR:-/tmp}/m6icc.XXXXXX")
trap 'rm -rf "${TMPD:?}"' EXIT
sed -n 's/^CHANGE //p' "$LIST" >"$TMPD/change.pat"
sed -n 's/^QUERY //p' "$LIST" >"$TMPD/query.pat"
BAN='(^|[ /])(route|ifconfig|kill|pkill|killall)( |$)|pfctl .*-[def]( |$)'
NONET='(^|[ /])(pfctl|networksetup|scutil)( |$)'
for f in "$1" "$2"; do
  sed -e "s|${ROOT}|<仓库>|g" "$f" >"$TMPD/norm"
  sed -n 's/^CHANGE: //p' "$TMPD/norm" >"$TMPD/c"
  sed -n 's/^QUERY: //p' "$TMPD/norm" >"$TMPD/q"
  [ -s "$TMPD/c" ] || bad "$(basename "$f")：空跑里没有任何改动命令"
  while IFS= read -r c; do bad "不在清单里的改动：$c"; done < <(grep -vE -f "$TMPD/change.pat" "$TMPD/c")
  while IFS= read -r c; do bad "不在清单里的查询：$c"; done < <(grep -vE -f "$TMPD/query.pat" "$TMPD/q")
  while IFS= read -r c; do bad "禁止的命令：$c"; done < <(cat "$TMPD/c" "$TMPD/q" | grep -E -- "$BAN")
done
while IFS= read -r c; do bad "安装里不该碰网络：$c"; done < <(sed -n 's/^CHANGE: //p' "$1" | grep -E -- "$NONET")

# ---- 原地升级 ----
if [ $# -ge 3 ]; then
  sed -e "s|${ROOT}|<仓库>|g" "$3" >"$TMPD/unorm"
  sed -n 's/^CHANGE: //p' "$TMPD/unorm" >"$TMPD/uc"
  sed -n 's/^QUERY: //p' "$TMPD/unorm" >"$TMPD/uq"
  [ -s "$TMPD/uc" ] || bad "升级：空跑里没有任何改动命令"
  while IFS= read -r c; do bad "升级：不在清单里的改动：$c"; done < <(grep -vE -f "$TMPD/change.pat" "$TMPD/uc")
  while IFS= read -r c; do bad "升级：不在清单里的查询：$c"; done < <(grep -vE -f "$TMPD/query.pat" "$TMPD/uq")
  while IFS= read -r c; do bad "升级里不该碰网络或专用用户：$c"; done < <(cat "$TMPD/uc" "$TMPD/uq" | grep -E -- '(^|[ /])(pfctl|networksetup|scutil|dscl|dscacheutil|route|ifconfig|kill|pkill|killall)( |$)')
  while IFS= read -r c; do bad "升级里碰了状态目录或日志目录：$c"; done < <(grep -E -- '/claude-abroad/state(/| |$)|/var/log/claude-abroad' "$TMPD/uc")
  awk '
    /^QUERY: .*v1\/status/ && !q { q = NR }
    /^CHANGE: / && !q { print "改动之前应先只读查状态：" $0 }
    /^CHANGE: launchctl bootout system\/com\.claude-abroad\.dev\.abroadd$/ && !stop { stop = NR }
    /^CHANGE: mv .*\/claude-abroad\/bin .*\/prev\/bin$/ { if (!stop) print "搬旧版之前应先停 abroadd"; mv = NR }
    /^CHANGE: cp .*\/bin\/abroadd .*\/claude-abroad\/bin\/abroadd$/ { if (!mv) print "放新文件之前应先把旧版搬进 prev"; place = NR }
    /^CHANGE: launchctl bootstrap system .*abroadd\.plist$/ { if (!place) print "启动 abroadd 之前应先放好新文件"; start = NR }
    /^CHANGE: rm -rf .*\/claude-abroad\/prev$/ { if (!start) print "核对之前就删了 prev"; done = NR }
    END { if (!stop) print "升级的空跑里没有停 abroadd"; if (!start) print "升级的空跑里没有启动 abroadd"; if (!done) print "升级的空跑里没有删 prev" }' "$TMPD/unorm" >"$TMPD/o3"
  while IFS= read -r l; do bad "升级的顺序：$l"; done <"$TMPD/o3"
fi

# ---- 顺序 ----
awk '
  /^CHANGE: / { n++ }
  n == 1 && !/^CHANGE: mkdir -p \/Library\/Application Support\/claude-abroad-install-snap$/ && /^CHANGE: / { print "第一条改动应是建快照目录：" $0 }
  /^STATE: snap\/files/ { snap = 1; next }
  /^CHANGE: / && n > 2 && !snap { print "拍快照之前就有改动：" $0 }
  /^CHANGE: launchctl bootstrap .*guard/ { g = 1 }
  /^CHANGE: launchctl bootstrap .*abroadd/ { if (!g) print "abroadd 应在 guard 之后注册"; d = 1 }
  END { if (!snap) print "安装的空跑里没有拍快照"; if (!g || !d) print "安装的空跑里没有注册两个后台服务" }' "$1" >"$TMPD/o1"
awk '
  /^CHANGE: .*\/bin\/abroad( --yes)? uninstall( --kill-browsers)?$/ { u = NR }
  /^CHANGE: launchctl bootout / { if (!u) print "停后台服务之前应先走 abroad uninstall"; b = NR }
  /^CHANGE: pfctl / { if (!b) print "兜底应在停掉后台服务之后"; p = NR }
  /^CHANGE: rm -f \/Library\/LaunchDaemons\// { if (!b) print "删 plist 之前应先停后台服务"; pl = NR }
  /^CHANGE: dscl \. -delete / { if (!b) print "删用户之前应先停后台服务" }
  /^CHANGE: rm -rf \/Library\/Application Support\/claude-abroad$/ { inst = NR }
  /^CHANGE: rm -rf \/Library\/Application Support\/claude-abroad-install-snap/ { if (!inst) print "快照目录应在安装目录之后删"; s = NR }
  /^CHANGE: / { last = $0 }
  END {
    if (!u) print "卸载的空跑里没有 abroad uninstall"
    if (!p) print "卸载的空跑里没有兜底步骤"
    if (!s) print "卸载的空跑里没有删快照目录"
    if (last !~ /claude-abroad-install-snap（对照一致时）$/) print "最后一条改动应是删快照目录：" last
  }' "$2" >"$TMPD/o2"
while IFS= read -r l; do bad "顺序：$l"; done < <(cat "$TMPD/o1" "$TMPD/o2")

# ---- 源码检查 ----
for s in install.sh uninstall.sh lib.sh; do
  [ -f "$SRC/$s" ] || { bad "源码检查：缺少 $SRC/$s"; continue; }
  sed -e 's/^[[:space:]]*#.*$//' -e 's/[[:space:]]#[[:space:]].*$//' "$SRC/$s" >"$TMPD/src"
  grep -nE '(^|[^A-Za-z0-9_./-])(kill|pkill|killall|route|ifconfig)([[:space:]]|$)' "$TMPD/src" |
    while IFS= read -r l; do echo "✗ 源码检查 $s：不允许的命令：$l"; done >"$TMPD/s1"
  grep -nE 'pfctl[^|;]*[[:space:]]-[def]([[:space:]]|$)' "$TMPD/src" |
    while IFS= read -r l; do echo "✗ 源码检查 $s：不允许的 pfctl：$l"; done >>"$TMPD/s1"
  grep -nE '(^|[^A-Za-z0-9_-])rm -rf[[:space:]]' "$TMPD/src" | grep -v 'printf' | grep -vE '\$\(P "|:\?\}' |
    while IFS= read -r l; do echo "✗ 源码检查 $s：rm -rf 的目标要经 P 或带 \${VAR:?}：$l"; done >>"$TMPD/s1"
  if [ -s "$TMPD/s1" ]; then cat "$TMPD/s1"; fails=$((fails + $(wc -l <"$TMPD/s1"))); fi
done

[ $fails = 0 ] && echo "✓ 命令核对通过" || echo "命令核对：${fails} 处不符"
exit $((fails > 0))
