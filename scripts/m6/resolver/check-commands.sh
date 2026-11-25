#!/usr/bin/env bash
# 核对解析锁真机脚本（scripts/m6/resolver）会执行的命令（M6-3，m6-plan v2.4 第 8 条）：
#   1. 空跑输出里每一条 CHANGE:、QUERY: 都在 commands.txt 的清单里；
#   2. 禁止项：pfctl、networksetup、route、ifconfig、launchctl、kill、pkill、killall、open、dscacheutil -flushcache、scutil 的写操作、tee；
#   3. 顺序：每一次写入锁文件都在某一轮死人开关启动之后、取消之前，并且这一轮取消之前删掉了它；
#      测试域名的预演在真实域名之前；真实域名的写入在“输入 yes”的确认之后；确认之前判断了基线出口；
#      空跑不知道出口：请用户打开已登录 Claude 的每一组提示（WAIT: Claude Code…/桌面版/浏览器）之前，都要有直连的单独确认
#      （CONFIRM-LOGIN:，整个运行一次）和这个场景的出口确认（CONFIRM-EXIT:，每组提示用掉一次）；
#   4. 源码检查（run.sh、restore.sh、lib.sh 去掉注释后）：不出现上面的禁止命令；不调用 common.sh 里会改系统或发信号的函数；
#      sudo 只用于 “sudo -u 你自己的账号” 编译；rm 都带 ${VAR:?} 保护。
# 用法：scripts/m6/resolver/check-commands.sh <run.sh 空跑输出> <restore.sh 空跑输出>
#      （M6R_CC_SRC_DIR 只给自测的反例用：换一个目录做源码检查）
set -uo pipefail
cd "$(dirname "$0")/../../.." || exit 1
[ $# -ge 2 ] || { echo "用法：$0 <run 空跑输出> <restore 空跑输出>" >&2; exit 2; }
fails=0
bad() { echo "✗ $*"; fails=$((fails + 1)); }
LIST=scripts/m6/resolver/commands.txt
SRC="${M6R_CC_SRC_DIR:-scripts/m6/resolver}"
TMPD=$(mktemp -d "${TMPDIR:-/tmp}/m6rcc.XXXXXX")
trap 'rm -rf "${TMPD:?}"' EXIT
sed -n 's/^CHANGE //p' "$LIST" >"$TMPD/change.pat"
sed -n 's/^QUERY //p' "$LIST" >"$TMPD/query.pat"
BAN='(^|[ /])(pfctl|networksetup|route|ifconfig|launchctl|kill|pkill|killall|open|tee)( |$)|dscacheutil -flushcache|scutil( .*)? (set|remove|add|d\.)'

for f in "$1" "$2"; do
  sed -n 's/^CHANGE: //p' "$f" >"$TMPD/c"
  sed -n 's/^QUERY: //p' "$f" >"$TMPD/q"
  while IFS= read -r c; do bad "不在清单里的改动：$c"; done < <(grep -vE -f "$TMPD/change.pat" "$TMPD/c")
  while IFS= read -r c; do bad "不在清单里的查询：$c"; done < <(grep -vE -f "$TMPD/query.pat" "$TMPD/q")
  while IFS= read -r c; do bad "禁止的命令：$c"; done < <(cat "$TMPD/c" "$TMPD/q" | grep -E -- "$BAN")
done

# ---- 顺序（run.sh 的空跑输出）----
awk '
  /^NOTE: 基线出口/ { based = 1; next }
  /^CONFIRM: / { confirmed = 1; if (!based) print "确认之前没有判断基线出口"; next }
  /^CONFIRM-LOGIN: / { login = 1; if (!confirmed) print "单独确认应在 yes 之后"; next }
  /^CONFIRM-EXIT: / { exitok = 1; if (!confirmed) print "出口确认应在 yes 之后"; next }
  /^WAIT: (Claude Code：|Claude 桌面版：|浏览器)/ {
    if (!inblock) {
      if (!login) print "没有直连时的单独确认（real-ip-ok）就请用户打开已登录的 Claude：" $0
      if (!exitok) print "这个场景没有出口确认（exit-trusted）就请用户打开已登录的 Claude：" $0
      inblock = 1; blocks++
    }
    manual = 1; next
  }
  { if (inblock) { inblock = 0; exitok = 0 } }
  /^CHANGE: 死人开关 / { if (round) { print "上一轮死人开关还没取消就开了新一轮"; } match($0, /第 [0-9]+ 轮/); round = substr($0, RSTART, RLENGTH); next }
  /^CHANGE: 取消死人开关/ {
    for (p in open_) { print "这一轮取消死人开关之前没有删掉：" p }
    delete open_; round = ""; next
  }
  /^CHANGE: install -m 644 .* \/etc\/resolver\// {
    p = $NF
    if (!round) print "写入锁文件时没有死人开关：" p
    if (p ~ /lock-test\.claude-abroad\.invalid$/) { if (confirmed) print "测试域名的预演应在确认锁真实域名之前" ; tested = 1 }
    else { if (!confirmed) print "没有“输入 yes”的确认就锁了真实域名：" p; if (!tested) print "没有先做测试域名的预演：" p; real = 1 }
    open_[p] = 1; wrote = 1; next
  }
  /^CHANGE: rm -f \/etc\/resolver\// { delete open_[$NF]; next }
  END {
    if (!wrote) print "空跑里没有写入锁文件的步骤"
    if (!tested) print "空跑里没有测试域名的预演"
    if (!real) print "空跑里没有锁真实域名的步骤"
    if (!manual) print "空跑里没有已登录程序的观察步骤"
    if (round) print "最后一轮的死人开关没有取消"
    for (p in open_) print "写了没删：" p
  }' "$1" >"$TMPD/order"
while IFS= read -r l; do bad "顺序：$l"; done <"$TMPD/order"
for form in default loopback; do
  grep -q "^STATE: lock\..*content\.${form}" "$1" || bad "空跑里没有 ${form} 写法的一轮"
done
grep -q '^CHANGE: rm -f /etc/resolver/<记录里的域名>' "$2" || bad "restore.sh 的空跑里没有删锁文件的步骤"

# ---- 源码检查 ----
for s in run.sh restore.sh lib.sh; do
  [ -f "$SRC/$s" ] || { bad "源码检查：缺少 $SRC/$s"; continue; }
  # 去掉整行注释和行尾注释（“ #” 之后），再去掉单引号里的打印文本里的 CHANGE/QUERY 说明行。
  sed -e 's/^[[:space:]]*#.*$//' -e 's/[[:space:]]#[[:space:]].*$//' "$SRC/$s" >"$TMPD/src"
  grep -nE '(^|[^A-Za-z0-9_./-])(pfctl|networksetup|route|ifconfig|launchctl|kill|pkill|killall)([^A-Za-z0-9_-]|$)' "$TMPD/src" |
    while IFS= read -r l; do echo "✗ 源码检查 $s：不允许的命令：$l"; done >"$TMPD/s1"
  grep -nE '(^|[;&|(][[:space:]]*|then[[:space:]]+|do[[:space:]]+)open[[:space:]]|dscacheutil -flushcache|scutil[^|]*[[:space:]](set|remove|add)[[:space:]]|(^|[^A-Za-z0-9_-])tee([^A-Za-z0-9_-]|$)' "$TMPD/src" |
    while IFS= read -r l; do echo "✗ 源码检查 $s：不允许的命令：$l"; done >>"$TMPD/s1"
  grep -nE 'ab_(kill_pidfile|deadman_start|deadman_stop|user_create|user_delete|free_id|default_iface|default_gw|services|wrap|state_init)([^A-Za-z0-9_]|$)' "$TMPD/src" |
    while IFS= read -r l; do echo "✗ 源码检查 $s：调用了会改系统或发信号的公共函数：$l"; done >>"$TMPD/s1"
  grep -nE '(^|[;&|(]|then|do|else)[[:space:]]*sudo([[:space:]]|$)' "$TMPD/src" | grep -vE '\$\(sudo -u "\$\{SUDO_USER\}" |^[0-9]+:[[:space:]]*sudo -u "\$\{SUDO_USER\}" ' |
    while IFS= read -r l; do echo "✗ 源码检查 $s：sudo 只能用来以你自己的账号编译：$l"; done >>"$TMPD/s1"
  grep -nE '(^|[^A-Za-z0-9_-])rm[[:space:]]' "$TMPD/src" | grep -v 'printf' | grep -v ':?}' |
    while IFS= read -r l; do echo "✗ 源码检查 $s：rm 要带 \${VAR:?} 保护：$l"; done >>"$TMPD/s1"
  grep -nE '(^|[^A-Za-z0-9_-])rmdir[[:space:]]' "$TMPD/src" | grep -v 'printf' | grep -v ':?}' |
    while IFS= read -r l; do echo "✗ 源码检查 $s：rmdir 要带 \${VAR:?} 保护：$l"; done >>"$TMPD/s1"
  if [ -s "$TMPD/s1" ]; then cat "$TMPD/s1"; fails=$((fails + $(wc -l <"$TMPD/s1"))); fi
done

[ $fails = 0 ] && echo "✓ 命令核对通过" || echo "命令核对：${fails} 处不符"
exit $((fails > 0))
