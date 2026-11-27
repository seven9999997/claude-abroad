#!/usr/bin/env bash
# 开发安装、卸载脚本的自测（M6-8b）：不用 root，不碰真实的系统路径，不执行任何系统命令（dscl、launchctl、pfctl……只记日志）。
# 每个场景用 M6I_TEST_DIR 指到一个新的临时目录：所有目标路径加上这个前缀，文件真的写在里面，便于核对。
#   a. 安装：程序、镜像、清单、应用、命令行链接、两个 plist（开发参数、完整性参数、guard 和 abroadd 的状态目录一致）都在；
#      系统命令只有建用户、注册服务这些，没有 pfctl、networksetup、scutil；
#   b. 卸载（正常途径）：装上的全部撤掉，和安装前的快照一致，快照目录删掉；走了 abroad uninstall，没有走兜底；再卸载一次什么都不做；
#   c. 卸载（正常途径失败）：走兜底（pf 锚点、DNS 键）；解析锁和托管设置只删内容和记录一致的，别人的文件留着；
#      被改过的锁文件留下，对照不一致时退出码非零、快照目录保留；
#   d. 原来就有的应用和命令行链接：安装不覆盖，卸载不删；
#   e. 安装中途失败：自动卸载，回到安装前；
#   f. 前提不满足时不改任何东西：已经装过、上一次没走完、用户已存在、走查脚本的状态还在、缺构建产物；
#   g. 模式护栏：测试变量离开自测目录被拒绝；自测目录必须是绝对路径、自己拥有的；自测和空跑不能同时用；
#   h. 空跑：退出码 0，不写任何文件；命令核对通过；反例（多出的命令、安装里碰网络、顺序错、源码里的禁止命令）都被拦下；
# 原地升级（v0.71，docs/app-install-plan.md 2.6）：
#   i. 升级成功（已回国、已停用）：程序、镜像、清单、版本号换成新的，安装目录有脚本；state/、日志目录逐字节不变；
#      只停、启 abroadd；不碰 pf、网络、DNS、专用用户；
#   j. plist 变了才换（guard 的换了时重新注册一次）；
#   k. 已启用、启用中、回国中、借用中、忙、卸载中、已卸载、状态文件坏了、不响应：退出码 10，什么都不改；
#   l. 不降级（11）；一样时什么都不做（0）；  m. 状态格式不一样（13），老安装读 state.json 的 version；
#   n. 版本不明：非交互要 --accept-unknown-version（否则 14）；
#   o. 每一步注入失败、重启后应答不对：退回旧版本（20），和升级前逐字节一样；
#   p. 退回也失败（21，prev 和标记留着）；再运行 --upgrade 接着退回；标记在时卸载照常；
#   q. 中断后退回；  r. 升级后卸载和安装前一致；应用不在安装记录里时不参与卸载对照；
#   s. payload.sh 的布局、--from 安装和升级、摘要对不上拒绝、--check-only --upgrade 只读；升级的空跑和命令核对及反例；
#   u. B0：应用经系统密码框执行的命令（Swift golden）用 /bin/sh 真跑：命令行不对时 31、输入文件被改过时 32；
#   t. 应用的交接：老安装记录里有 app 时，升级后属主改成执行 sudo 的用户、记录里删掉 app；再卸载时对照一致、应用留着。
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "${ROOT}" || exit 1
[ "$(id -u)" != 0 ] || { echo "不要以 root 运行" >&2; exit 1; }
TMP=$(mktemp -d "${TMPDIR:-/tmp}/m6ist.XXXXXX")
trap 'rm -rf "${TMP:?}"' EXIT
fails=0
ok() { echo "✓ $*"; }
bad() { echo "✗ $*"; fails=$((fails + 1)); }
INSTALL=scripts/m6/install/install.sh
UNINSTALL=scripts/m6/install/uninstall.sh
I="/Library/Application Support/claude-abroad"
SNAP="/Library/Application Support/claude-abroad-install-snap"

# 假的构建产物和应用包
SRC="${TMP}/src"
mkdir -p "${SRC}/bin" "${SRC}/image" "${TMP}/App.app/Contents/MacOS"
for f in abroad abroad-rescue abroadd abroad-guard abroad-vm; do printf '#!/bin/sh\n# %s\n' "$f" >"${SRC}/bin/$f"; chmod +x "${SRC}/bin/$f"; done
printf '# links /System/Library/Frameworks/Virtualization.framework\n' >>"${SRC}/bin/abroad-vm" # 假的“链接了虚拟化框架”
for f in Image initramfs.gz modloop rootfs.img; do printf 'img %s\n' "$f" >"${SRC}/image/$f"; done
echo '{"version":1,"files":{}}' >"${SRC}/integrity.json"
echo '{"commit":"1111111aaaaa","build_time":"20261101T000000Z","dirty":false,"state_format":1}' >"${SRC}/build.json"
echo 'app' >"${TMP}/App.app/Contents/MacOS/ClaudeAbroad"
echo '<plist/>' >"${TMP}/App.app/Contents/Info.plist"
export M6I_TEST_SRC="${SRC}" M6I_TEST_APP="${TMP}/App.app"
# 不带测试变量运行（g 的真实模式、h 的空跑）
plain() { env -u M6I_TEST_SRC -u M6I_TEST_APP "$@"; }

# 每个场景一个新目录，先建好系统本来就有的目录（/Applications、/Library/LaunchDaemons……）。
mkcase() {
  local d; d=$(mktemp -d "${TMP}/case.XXXXXX")
  mkdir -p "$d/Applications" "$d/Library/LaunchDaemons" "$d/Library/Application Support" "$d/var/run" "$d/var/log" "$d/usr/local/bin"
  echo "$d"
}
inst() { M6I_TEST_DIR="$1" M6I_YES=1 bash "${INSTALL}" >"$1/install.out" 2>"$1/install.err"; }
uninst() { M6I_TEST_DIR="$1" bash "${UNINSTALL}" >"$1/uninstall.out" 2>"$1/uninstall.err"; }
tree() { (cd "$1" && find . -path ./sys.log -prune -o -path './*.out' -prune -o -path './*.err' -prune -o -path './*.tree' -prune -o -path ./err -prune -o -print | LC_ALL=C sort); }

# ---- a. 安装 ----
A=$(mkcase)
mkdir -p "$A/etc/resolver" && echo 'nameserver 192.0.2.53' >"$A/etc/resolver/corp.example"
tree "$A" >"$A/before.tree"
if inst "$A"; then ok "a. 安装退出码 0"; else bad "a. 安装失败：$(cat "$A/install.err")"; fi
for f in bin/abroad bin/abroad-rescue bin/abroadd bin/abroad-guard bin/abroad-vm image/Image image/initramfs.gz image/modloop image/rootfs.img integrity.json; do
  cmp -s "${SRC}/$f" "$A$I/$f" || bad "a. 没装上 $f"
done
[ -f "$A/Applications/ClaudeAbroad.app/Contents/Info.plist" ] && ok "a. 应用装到了 /Applications" || bad "a. 应用没装上"
[ "$(readlink "$A/usr/local/bin/abroad")" = "$I/bin/abroad" ] && ok "a. 命令行链接指向安装位置（另存的应急命令行）" || bad "a. 命令行链接不对"
D="$A/Library/LaunchDaemons/com.claude-abroad.dev.abroadd.plist"
G="$A/Library/LaunchDaemons/com.claude-abroad.dev.guard.plist"
for want in -- --launch-control --dev-fake-claude-dir "$I/fake-claude" --dev-resolver-test-domain --vm-exec "$I/bin/abroad-vm" "--vm-user" "--integrity-manifest" "$I/integrity.json" \
  "--state-dir" "$I/state/abroadd" "--socket-dir" "/var/run/claude-abroad" "<key>KeepAlive</key><true/>"; do
  grep -qF -- "$want" "$D" || bad "a. abroadd 的 plist 里没有 $want"
done
for want in "$I/bin/abroad-guard" "$I/state/abroadd" "--run-dir" "/var/run/claude-abroad" "--integrity-base" "<key>LaunchOnlyOnce</key><true/>"; do
  grep -qF -- "$want" "$G" || bad "a. guard 的 plist 里没有 $want"
done
plutil -lint "$D" >/dev/null 2>&1 && plutil -lint "$G" >/dev/null 2>&1 && ok "a. 两个 plist 格式正确" || bad "a. plist 格式不对"
grep -q '^SYS: dscl . -create /Users/_abroad$' "$A/sys.log" && grep -q '^SYS: launchctl bootstrap system .*guard.plist$' "$A/sys.log" &&
  grep -q '^SYS: launchctl bootstrap system .*abroadd.plist$' "$A/sys.log" && ok "a. 建用户、注册两个服务" || bad "a. 系统命令不全：$(cat "$A/sys.log")"
grep -qE '^SYS: (pfctl|networksetup|scutil)' "$A/sys.log" && bad "a. 安装碰了网络" || ok "a. 安装没有碰 pf、网络服务、DNS"
[ -e "$A$SNAP/installing" ] && bad "a. 安装走完了 installing 标记还在" || ok "a. 安装走完后去掉 installing 标记"
[ -f "$A$SNAP/files.before" ] && ok "a. 有安装前的快照" || bad "a. 没有安装前的快照"

# ---- b. 卸载（正常途径），再卸载一次 ----
if uninst "$A"; then ok "b. 卸载退出码 0"; else bad "b. 卸载失败：$(cat "$A/uninstall.err")"; fi
tree "$A" >"$A/after.tree"
diff -q "$A/before.tree" "$A/after.tree" >/dev/null && ok "b. 卸载后和安装前的文件完全一致（快照目录也删了）" || bad "b. 卸载后有差异：$(diff "$A/before.tree" "$A/after.tree" | head -5)"
grep -q 'abroad uninstall$' "$A/sys.log" && ok "b. 先走了 abroad uninstall" || bad "b. 没有走 abroad uninstall"
grep -qE '^SYS: (pfctl|scutil|networksetup)' "$A/sys.log" && bad "b. 正常途径走通时不该走兜底" || ok "b. 正常途径走通时没有走兜底"
grep -q '^SYS: launchctl bootout system/com.claude-abroad.dev.abroadd$' "$A/sys.log" && grep -q '^SYS: dscl . -delete /Users/_abroad$' "$A/sys.log" &&
  ok "b. 停了后台服务、删了用户" || bad "b. 没有停服务或删用户"
uninst "$A" && grep -q '没有安装记录' "$A/uninstall.err" && ok "b. 再卸载一次：没有记录，什么都不做" || bad "b. 重复卸载不对"

# ---- c. 正常途径失败：兜底；只删自己的 ----
C=$(mkcase)
mkdir -p "$C/etc/resolver" && echo 'nameserver 192.0.2.53' >"$C/etc/resolver/corp.example"
tree "$C" >"$C/before.tree"
inst "$C" || bad "c. 安装失败"
LOCK='nameserver 198.18.0.2
timeout 1
'
printf '%s' "$LOCK" >"$C/etc/resolver/lock-test.claude-abroad.invalid"
/usr/bin/python3 -I -c 'import json,sys; json.dump({"version":1,"form":"default","domains":["lock-test.claude-abroad.invalid","../x"],"content":sys.argv[2]}, open(sys.argv[1],"w"))' \
  "$C$I/state/abroadd/resolver-lock.json" "$LOCK"
M6I_TEST_NORMAL_FAILS=1 uninst "$C"; rc=$?
[ $rc = 0 ] && ok "c. 兜底卸载退出码 0" || bad "c. 兜底卸载退出码 $rc：$(cat "$C/uninstall.err")"
grep -q '^SYS: pfctl -a com.apple/250.abroad -F all$' "$C/sys.log" && grep -q '^SYS: scutil remove State:/Network/Service/claude-abroad/DNS$' "$C/sys.log" &&
  ok "c. 兜底撤了自己的 pf 锚点和 DNS 键" || bad "c. 兜底没撤 pf 或 DNS：$(grep -E 'pfctl|scutil' "$C/sys.log")"
[ -e "$C/etc/resolver/lock-test.claude-abroad.invalid" ] && bad "c. 自己写的锁文件没删" || ok "c. 删了自己写的锁文件"
[ "$(cat "$C/etc/resolver/corp.example")" = "nameserver 192.0.2.53" ] && ok "c. 别人的文件 corp.example 没被动" || bad "c. 别人的文件被动了"
tree "$C" >"$C/after.tree"
diff -q "$C/before.tree" "$C/after.tree" >/dev/null && ok "c. 兜底后和安装前一致" || bad "c. 兜底后有差异：$(diff "$C/before.tree" "$C/after.tree" | head -5)"
# 被改过的锁文件：不删，对照不一致，快照保留
C2=$(mkcase)
inst "$C2" || bad "c2. 安装失败"
mkdir -p "$C2/etc/resolver"
echo 'nameserver 9.9.9.9' >"$C2/etc/resolver/lock-test.claude-abroad.invalid"
/usr/bin/python3 -I -c 'import json,sys; json.dump({"version":1,"form":"default","domains":["lock-test.claude-abroad.invalid"],"content":sys.argv[2],"created_dir":True}, open(sys.argv[1],"w"))' \
  "$C2$I/state/abroadd/resolver-lock.json" "$LOCK"
M6I_TEST_NORMAL_FAILS=1 uninst "$C2"; rc=$?
[ $rc != 0 ] && ok "c2. 对照不一致时退出码非零" || bad "c2. 对照不一致时退出码应非零"
[ "$(cat "$C2/etc/resolver/lock-test.claude-abroad.invalid" 2>/dev/null)" = "nameserver 9.9.9.9" ] && ok "c2. 被改过的锁文件没删" || bad "c2. 被改过的锁文件被删了"
[ -d "$C2$SNAP" ] && grep -q '和安装前的快照不一致' "$C2/uninstall.err" && ok "c2. 快照目录保留，报告不一致" || bad "c2. 没有保留快照或没报告"

# ---- d. 原来就有的应用和命令行链接 ----
Dd=$(mkcase)
mkdir -p "$Dd/Applications/ClaudeAbroad.app" "$Dd/usr/local/bin"
echo mine >"$Dd/Applications/ClaudeAbroad.app/mine"
ln -s /opt/elsewhere/abroad "$Dd/usr/local/bin/abroad"
tree "$Dd" >"$Dd/before.tree"
inst "$Dd" || bad "d. 安装失败"
[ "$(cat "$Dd/Applications/ClaudeAbroad.app/mine")" = mine ] && [ "$(readlink "$Dd/usr/local/bin/abroad")" = /opt/elsewhere/abroad ] &&
  ok "d. 安装没覆盖原来的应用和链接" || bad "d. 原来的应用或链接被覆盖"
uninst "$Dd" || bad "d. 卸载失败：$(cat "$Dd/uninstall.err")"
tree "$Dd" >"$Dd/after.tree"
diff -q "$Dd/before.tree" "$Dd/after.tree" >/dev/null && [ -L "$Dd/usr/local/bin/abroad" ] && ok "d. 卸载没删原来的应用和链接" || bad "d. 原来的应用或链接被删了"

# ---- e. 安装中途失败：自动卸载 ----
E=$(mkcase)
rmdir "$E/Library/LaunchDaemons" && echo x >"$E/Library/LaunchDaemons" # 放 plist 的位置被普通文件占着
tree "$E" >"$E/before.tree"
inst "$E"; rc=$?
[ $rc != 0 ] && ok "e. 中途失败时退出码非零" || bad "e. 中途失败时退出码应非零"
grep -q '自动执行卸载' "$E/install.err" && ok "e. 自动执行了卸载" || bad "e. 没有自动卸载"
tree "$E" >"$E/after.tree"
diff -q "$E/before.tree" "$E/after.tree" >/dev/null && ok "e. 回到了安装前" || bad "e. 有残留：$(diff "$E/before.tree" "$E/after.tree" | head -5)"
grep -q 'abroad uninstall$' "$E/sys.log" && bad "e. 安装中途失败时不该走 abroad uninstall" || ok "e. 安装中途失败时跳过 abroad uninstall"

# ---- f. 前提不满足时不改任何东西 ----
pre() { # pre <名字> <准备命令> <期望的说明>
  local F; F=$(mkcase)
  (cd "$F" && eval "$2")
  tree "$F" >"$F/before.tree"
  inst "$F"; local rc=$?
  tree "$F" >"$F/after.tree"
  if [ $rc = 3 ] && grep -q -- "$3" "$F/install.err" && diff -q "$F/before.tree" "$F/after.tree" >/dev/null && [ ! -s "$F/sys.log" ]; then
    ok "f. $1：拒绝，什么都没改"
  else
    bad "f. $1：rc=$rc $(head -3 "$F/install.err")"
  fi
}
pre "已经装过" "mkdir -p './Library/Application Support/claude-abroad'" "已经装过"
pre "上一次没走完" "mkdir -p './Library/Application Support/claude-abroad-install-snap'" "没有走完"
pre "走查脚本的状态还在" "mkdir -p ./var/run/claude-abroad-m5" "m5 走查脚本"
pre "本地接口目录已存在" "mkdir -p ./var/run/claude-abroad" "/var/run/claude-abroad 已经存在"
F=$(mkcase); M6I_TEST_USER_EXISTS=1 M6I_TEST_DIR="$F" M6I_YES=1 bash "${INSTALL}" >/dev/null 2>"$F/err"; rc=$?
[ $rc = 3 ] && grep -q '_abroad 已经存在' "$F/err" && [ ! -e "$F$I" ] && [ ! -e "$F$SNAP" ] && ok "f. 用户已存在：拒绝" || bad "f. 用户已存在：rc=$rc"
F=$(mkcase); M6I_TEST_SRC="${TMP}/nosuch" M6I_TEST_DIR="$F" bash "${INSTALL}" >/dev/null 2>"$F/err"; rc=$?
[ $rc = 3 ] && grep -q 'integrity-build.sh --dev' "$F/err" && ok "f. 缺构建产物：拒绝并提示怎么构建" || bad "f. 缺构建产物：rc=$rc"

# ---- g. 模式护栏 ----
plain M6I_TEST_SRC=/x bash "${INSTALL}" --check-only >/dev/null 2>"${TMP}/g1"; rc=$?
[ $rc != 0 ] && grep -q '只能配 M6I_TEST_DIR' "${TMP}/g1" && ok "g. 测试变量离开自测目录被拒绝" || bad "g. 测试变量离开自测目录没被拒绝"
M6I_TEST_DIR=relative bash "${INSTALL}" >/dev/null 2>&1 && bad "g. 相对路径的自测目录没被拒绝" || ok "g. 相对路径的自测目录被拒绝"
M6I_TEST_DIR=/ bash "${UNINSTALL}" >/dev/null 2>&1 && bad "g. 自测目录 / 没被拒绝" || ok "g. 自测目录 / 被拒绝"
F=$(mkcase); DRY_RUN=1 M6I_TEST_DIR="$F" bash "${INSTALL}" >/dev/null 2>&1 && bad "g. 自测和空跑同时用没被拒绝" || ok "g. 自测和空跑不能同时用"
plain bash "${INSTALL}" >/dev/null 2>"${TMP}/g5" && bad "g. 不用 sudo 的真实安装没被拒绝" || { grep -q 'sudo' "${TMP}/g5" && ok "g. 不用 sudo 的真实安装被拒绝" || bad "g. 拒绝的说明不对"; }

# ---- h. 空跑和命令核对 ----
# 这台 Mac 真装着开发版时（architect 2026-11-27）：“运行前这些路径不在”跳过并说明；“空跑没写系统路径”改成比较空跑前后这些
# 路径的状态（存在与否、大小、修改时间、顶层目录的列表），一样就算没写。没装过的机器上行为和以前完全一样。
# M6I_SELFTEST_INSTALLED_PATH 只给自测的自测用：换成一个不存在的路径，验证“没装过”的分支照常执行。
SYSP=("$I" "$SNAP" /var/run/claude-abroad /Library/LaunchDaemons/com.claude-abroad.dev.abroadd.plist)
INSTALLED=0
{ [ -e "${M6I_SELFTEST_INSTALLED_PATH:-$I}" ] || { [ -z "${M6I_SELFTEST_INSTALLED_PATH:-}" ] && [ -e "$SNAP" ]; }; } && INSTALLED=1
skips=0
skip() { echo "- 跳过：$*"; skips=$((skips + 1)); }
syssnap() { local p; for p in "${SYSP[@]}"; do
  if [ -e "$p" ]; then printf '%s\t%s\n' "$p" "$(stat -f '%z %m %p' "$p" 2>/dev/null)"; [ -d "$p" ] && ls -la "$p" 2>/dev/null | tail -n +2; else printf '%s\t无\n' "$p"; fi
done; }
if [ "${INSTALLED}" = 1 ]; then
  skip "h. 运行前 ${I}、${SNAP} 不存在：这台 Mac 装着开发版，这一项要在没装过的机器上验证"
else
  for p in "$I" "$SNAP"; do [ -e "$p" ] && bad "h. 运行前 $p 就存在（不该在开发机上出现）"; done
fi
syssnap >"${TMP}/sys.before"
plain DRY_RUN=1 bash "${INSTALL}" >"${TMP}/i.dry" 2>/dev/null && ok "h. 安装空跑退出码 0" || bad "h. 安装空跑退出码非零"
plain DRY_RUN=1 bash "${UNINSTALL}" >"${TMP}/u.dry" 2>/dev/null && ok "h. 卸载空跑退出码 0" || bad "h. 卸载空跑退出码非零"
syssnap >"${TMP}/sys.after"
if [ "${INSTALLED}" = 1 ]; then
  cmp -s "${TMP}/sys.before" "${TMP}/sys.after" && ok "h. 空跑前后系统路径的状态没有变化（这台 Mac 装着开发版，按前后对照判断）" || bad "h. 空跑改动了系统路径：$(diff "${TMP}/sys.before" "${TMP}/sys.after" | head -5)"
else
  w=0
  for p in "${SYSP[@]}"; do [ -e "$p" ] && { bad "h. 空跑写了 $p"; w=1; }; done
  [ $w = 0 ] && ok "h. 空跑没有写任何系统路径"
fi
scripts/m6/install/check-commands.sh "${TMP}/i.dry" "${TMP}/u.dry" >"${TMP}/cc" && ok "h. 命令核对通过" || bad "h. 命令核对不通过：$(cat "${TMP}/cc")"
neg() { # neg <名字> <install 输出> <uninstall 输出> [源码目录]
  if M6I_CC_SRC_DIR="${4:-}" scripts/m6/install/check-commands.sh "$2" "$3" >/dev/null 2>&1; then bad "h. 反例没被拦下：$1"; else ok "h. 反例被拦下：$1"; fi
}
{ cat "${TMP}/i.dry"; echo 'CHANGE: rm -rf /Users'; } >"${TMP}/n1"; neg "清单外的命令" "${TMP}/n1" "${TMP}/u.dry"
{ cat "${TMP}/i.dry"; echo 'CHANGE: pfctl -a com.apple/250.abroad -F all'; } >"${TMP}/n2"; neg "安装里碰 pf" "${TMP}/n2" "${TMP}/u.dry"
grep -v 'abroad uninstall$' "${TMP}/u.dry" >"${TMP}/n3"; neg "卸载没先走 abroad uninstall" "${TMP}/i.dry" "${TMP}/n3"
awk '/launchctl bootstrap .*guard/ { g = $0; next } { print } /launchctl bootstrap .*abroadd/ { print g }' "${TMP}/i.dry" >"${TMP}/n4"; neg "abroadd 先于 guard 注册" "${TMP}/n4" "${TMP}/u.dry"
{ cat "${TMP}/u.dry"; echo 'CHANGE: pfctl -d'; } >"${TMP}/n5"; neg "pfctl -d" "${TMP}/i.dry" "${TMP}/n5"
mkdir -p "${TMP}/badsrc" && cp scripts/m6/install/*.sh "${TMP}/badsrc/" && echo 'route -n delete default' >>"${TMP}/badsrc/uninstall.sh"
neg "源码里有 route" "${TMP}/i.dry" "${TMP}/u.dry" "${TMP}/badsrc"
cp scripts/m6/install/*.sh "${TMP}/badsrc/" && echo 'rm -rf "$X"' >>"${TMP}/badsrc/lib.sh"
neg "源码里 rm -rf 没有保护" "${TMP}/i.dry" "${TMP}/u.dry" "${TMP}/badsrc"

# M6-11b：开发安装的回国拦截只用测试段——脚本、命令清单、空跑输出里都不出现真实的 Anthropic 地址段；安装后核对测试段
if cat scripts/m6/install/install.sh scripts/m6/install/uninstall.sh scripts/m6/install/lib.sh scripts/m6/install/check-commands.sh scripts/m6/install/README.md scripts/m6/install/commands.txt "${TMP}/i.dry" "${TMP}/u.dry" | grep -qE '160\.79\.104|2607:6bc0'; then
  bad "M6-11b. 开发安装里出现了真实的 Anthropic 地址段"
else
  ok "M6-11b. 开发安装里没有真实的 Anthropic 地址段"
fi
grep -qF 'QUERY: curl --unix-socket /var/run/claude-abroad/api.sock http://abroadd/v1/status（return_block 应为 on、active、test，nets=192.0.2.0/24,2001:db8:abd::/48）' "${TMP}/i.dry" &&
  grep -qF 'QUERY: pfctl -a com.apple/250.abroad -s rules' "${TMP}/i.dry" && ok "M6-11b. 安装后只读核对回国拦截是测试段" || bad "M6-11b. 安装后没有核对回国拦截"
grep -qF "pf.enabled" scripts/m6/install/lib.sh && ok "M6-11b. 快照里有 pf 的启用状态" || bad "M6-11b. 快照里没有 pf 的启用状态"
printf 'user\tno\npf.enabled\tDisabled\n' >"${TMP}/sa"; printf 'user\tno\npf.enabled.unknown\tStatus: ???\n' >"${TMP}/sb"; printf 'user\tno\npf.enabled\tEnabled\n' >"${TMP}/sc"
env -u M6I_TEST_SRC -u M6I_TEST_APP -u M6I_TEST_DIR bash -c ". scripts/m6/install/lib.sh; m6i_sys_cmp '${TMP}/sa' '${TMP}/sb'" >/dev/null 2>&1 && ok "M6-11b. pf 是否启用无法判断时不算不一致" || bad "M6-11b. pf 是否启用无法判断时被当成不一致"
env -u M6I_TEST_SRC -u M6I_TEST_APP -u M6I_TEST_DIR bash -c ". scripts/m6/install/lib.sh; m6i_sys_cmp '${TMP}/sa' '${TMP}/sc'" >/dev/null 2>&1 && bad "M6-11b. pf 启用状态变了没被发现" || ok "M6-11b. pf 启用状态变了会被发现"

# ---- 2026-11-27 真机：卸载对照接受“别人持有的 pf 引用”（architect）----
REFS_PFD="${TMP}/refs.pfd"; printf 'No ALTQ support in kernel\nTOKENS:\nPID      Process Name                 TOKEN                    TIMESTAMP\n34183    pfd                          7864932399086914672      0 days 00:02:27\n' >"${REFS_PFD}"
REFS_OURS="${TMP}/refs.ours"; printf 'TOKENS:\nPID      Process Name                 TOKEN                    TIMESTAMP\n34183    pfd                          1      0 days 00:02:27\n501      pfctl                        2      0 days 01:00:00\n' >"${REFS_OURS}"
REFS_BAD="${TMP}/refs.bad"; printf 'pfctl: something went wrong\n' >"${REFS_BAD}"
printf 'user\tno\npf.anchor\t0\npf.enabled\tDisabled\n' >"${TMP}/sys.off"
printf 'user\tno\npf.anchor\t0\npf.enabled\tEnabled\n' >"${TMP}/sys.on"
cmpcase() { # cmpcase <说明> <期望 pass|fail> <安装前> <卸载后> <References 文件> [锚点规则数]
  local what=$1 want=$2 out rc
  out=$( export M6I_TEST_DIR="${TMP}" M6I_TEST_PF_REFS="$5" M6I_TEST_PF_ANCHOR="${6:-0}"; . scripts/m6/install/lib.sh; m6i_pf_info "${TMP}/pfinfo"; m6i_sys_cmp "$3" "$4" "${TMP}/pfinfo" 2>"${TMP}/cmp.err"; )
  rc=$?
  if { [ "$want" = pass ] && [ -z "$out" ]; } || { [ "$want" = fail ] && [ -n "$out" ]; }; then ok "syscmp. ${what}"; else bad "syscmp. ${what}：输出 ${out}"; fi
}
cmpcase "别人（pfd）持有引用、我们的锚点空：通过" pass "${TMP}/sys.off" "${TMP}/sys.on" "${REFS_PFD}"
( export M6I_TEST_DIR="${TMP}" M6I_TEST_PF_REFS="${REFS_PFD}"; . scripts/m6/install/lib.sh; m6i_pf_info "${TMP}/pfinfo"; m6i_sys_cmp "${TMP}/sys.off" "${TMP}/sys.on" "${TMP}/pfinfo" ) 2>&1 >/dev/null | grep -q '由别的程序持有引用（进程名：pfd）' && ok "syscmp. 通过时记一条说明，写出持有者 pfd" || bad "syscmp. 通过时没有说明"
cmpcase "有 pfctl 名下的 token：失败" fail "${TMP}/sys.off" "${TMP}/sys.on" "${REFS_OURS}"
cmpcase "锚点里还有规则：失败" fail "${TMP}/sys.off" "${TMP}/sys.on" "${REFS_PFD}" 2
cmpcase "References 读不出：严格对照、失败" fail "${TMP}/sys.off" "${TMP}/sys.on" "${REFS_BAD}"
cmpcase "安装前开着、卸载后关了：失败" fail "${TMP}/sys.on" "${TMP}/sys.off" "${REFS_PFD}"
cmpcase "安装前后一样：通过" pass "${TMP}/sys.off" "${TMP}/sys.off" "${REFS_PFD}"
( export M6I_TEST_DIR="${TMP}" M6I_TEST_PF_REFS="${REFS_PFD}"; . scripts/m6/install/lib.sh; m6i_pf_info "${TMP}/pfinfo"; m6i_sys_cmp "${TMP}/sys.off" "${TMP}/sys.off" "${TMP}/pfinfo" ) 2>&1 >/dev/null | grep -q '持有引用' && bad "syscmp. 前后一样时不该有说明" || ok "syscmp. 前后一样时没有说明"
# 真实场景：卸载做完、只是 pf 对照没过（有 pfctl 的 token）→ 快照留着、安装目录已删；之后 pf 只被 pfd 持有，再卸载：只重做对照、删快照、退出码 0
S=$(mkcase)
M6I_TEST_SYS_SNAP="${TMP}/sys.off" inst "$S" || bad "only-snap. 安装失败：$(cat "$S/install.err")"
if M6I_TEST_SYS_SNAP="${TMP}/sys.on" M6I_TEST_PF_REFS="${REFS_OURS}" uninst "$S"; then bad "only-snap. 第一次卸载应因 pf 对照不一致而失败"; else ok "only-snap. 第一次卸载：有我们名下的 token 时对照不过、退出码非零"; fi
[ -d "$S$SNAP" ] && [ ! -e "$S$I" ] && ok "only-snap. 快照留着、安装目录已删（复现真机状态）" || bad "only-snap. 没有复现出“只剩快照”"
cp "$S/sys.log" "$S/sys.log.1" 2>/dev/null || : >"$S/sys.log.1"
if M6I_TEST_SYS_SNAP="${TMP}/sys.on" M6I_TEST_PF_REFS="${REFS_PFD}" uninst "$S"; then ok "only-snap. 再卸载：退出码 0"; else bad "only-snap. 再卸载失败：$(cat "$S/uninstall.err")"; fi
[ ! -e "$S$SNAP" ] && ok "only-snap. 快照被删" || bad "only-snap. 快照没删"
grep -q '只剩安装前的快照' "$S/uninstall.err" && grep -q '进程名：pfd' "$S/uninstall.err" && ok "only-snap. 只重做对照，并说明 pf 由 pfd 持有" || bad "only-snap. 输出不对：$(cat "$S/uninstall.err")"
cmp -s "$S/sys.log" "$S/sys.log.1" && ok "only-snap. 再卸载没有执行任何系统命令" || bad "only-snap. 再卸载执行了系统命令：$(diff "$S/sys.log.1" "$S/sys.log")"

# ================= 原地升级（v0.71，docs/app-install-plan.md 2.6 的 i–s）=================
# 每个场景：先用旧 payload 真装一份，在 state/ 里放假的主代理、规则、开关、记录，再用新 payload 真升级（系统命令只记 SYS:，
# 状态接口是假的 JSON 文件：报告的版本按安装目录里的 build.json 改写）。
# mkpayload <目录> <提交号> <构建时间> [状态格式]：假的 payload（程序内容带版本，便于核对换没换）。
mkpayload() {
  local d=$1 f
  mkdir -p "$d/bin" "$d/image"
  for f in abroad abroad-rescue abroadd abroad-guard abroad-vm; do printf '#!/bin/sh\n# %s %s\n' "$f" "$2" >"$d/bin/$f"; chmod +x "$d/bin/$f"; done
  printf '# links /System/Library/Frameworks/Virtualization.framework\n' >>"$d/bin/abroad-vm"
  for f in Image initramfs.gz modloop rootfs.img; do printf 'img %s %s\n' "$f" "$2" >"$d/image/$f"; done
  printf '{"version":1,"files":{},"v":"%s"}\n' "$2" >"$d/integrity.json"
  printf '{"commit":"%s","build_time":"%s","dirty":false,"state_format":%s}\n' "$2" "$3" "${4:-1}" >"$d/build.json"
}
P1="${TMP}/p1"; mkpayload "$P1" 1111111aaaaa 20261101T000000Z
P2="${TMP}/p2"; mkpayload "$P2" 2222222bbbbb 20261127T080000Z
P0="${TMP}/p0"; mkpayload "$P0" 0000000ccccc 20261001T000000Z
PF="${TMP}/pf"; mkpayload "$PF" 3333333ddddd 20261128T000000Z 2
# 假的状态接口应答
mkstatus() { # mkstatus <文件> <状态> [额外的 JSON 片段，逗号开头]
  printf '{"state":"%s","prepare":{"done":true},"integrity":{"checked":true,"ok":true},"return_block":{"on":true,"active":true,"test":true},"uninstall":{"running":false,"done":false}%s}\n' "$2" "${3:-}" >"$1"
}
PL="${TMP}/payload" # payload.sh 组装的（s 里核对它的布局）
scripts/dev/payload.sh --src "$P2" --out "$PL" >"${TMP}/pl.out" 2>&1
ST_R="${TMP}/st.restored"; mkstatus "$ST_R" RESTORED
ST_D="${TMP}/st.disabled"; mkstatus "$ST_D" DISABLED
# 装好旧版并放进假的状态（主代理来源和选择、规则、开关、首次准备标记、解析锁和托管设置的记录、分流出口、地区）
setup_old() { # setup_old <目录> [payload]
  M6I_TEST_DIR="$1" M6I_TEST_SRC="${2:-$P1}" M6I_YES=1 bash "${INSTALL}" >"$1/install.out" 2>"$1/install.err" || { bad "升级的准备：安装失败：$(cat "$1/install.err")"; return 1; }
  local s="$1$I/state"
  mkdir -p "$s/store/proxy" "$s/abroadd/secrets"
  echo '{"version":1,"state":"RESTORED","step":0}' >"$s/abroadd/state.json"
  echo '{"version":1,"sources":[{"id":"s1"}],"selected":"n1"}' >"$s/store/proxy/sources.json"
  echo '{"digest":"d1"}' >"$s/store/proxy/applied.json"
  echo 'version: 1' >"$s/abroadd/rules.yaml"
  echo '{"on":false}' >"$s/abroadd/allow-lan.json"
  echo '2026-11-27T00:00:00Z' >"$s/abroadd/prepared"
  echo '{"version":1,"domains":["lock-test.claude-abroad.invalid"]}' >"$s/abroadd/resolver-lock.json"
  echo '{"tag":"jp"}' >"$s/abroadd/secrets/exit-jp.json"
  echo '{"timezone":"Asia/Tokyo"}' >"$s/abroadd/region.json"
  mkdir -p "$1/var/log/claude-abroad" && echo 'old log' >"$1/var/log/claude-abroad/abroadd.log"
}
# hashes <目录> <子树>：子树下每个文件一行（相对路径、摘要）
hashes() { (cd "$1$2" 2>/dev/null && find . -type f -print0 | LC_ALL=C sort -z | xargs -0 shasum -a 256) ; }
# 安装目录里除 state/ 以外的东西 + 两份 plist + 应用（升级会碰的全部）
upgset() { { (cd "$1$I" && find . -path ./state -prune -o -print | LC_ALL=C sort; find . -path ./state -prune -o -type f -print0 | LC_ALL=C sort -z | xargs -0 shasum -a 256)
  shasum -a 256 "$1/Library/LaunchDaemons/"*.plist | sed "s|$1||"; hashes "$1" /Applications; } 2>/dev/null; }
upg() { # upg <目录> <payload> <状态文件> [参数…]
  local d=$1 src=$2 st=$3; shift 3
  M6I_TEST_DIR="$d" M6I_TEST_SRC="$src" M6I_TEST_STATUS="$st" bash "${INSTALL}" --upgrade --yes "$@" >"$d/up.out" 2>"$d/up.err"
}

# ---- i. 升级成功（已回国、已停用）----
for which in RESTORED DISABLED; do
  U=$(mkcase); setup_old "$U" || continue
  st=$ST_R; [ $which = DISABLED ] && st=$ST_D
  hashes "$U" "$I/state" >"$U/state.before"; hashes "$U" /var/log/claude-abroad >"$U/log.before"
  cp "$U/sys.log" "$U/sys.log.0"
  if upg "$U" "$P2" "$st"; then ok "i. ${which}：升级退出码 0"; else bad "i. ${which}：升级失败 rc=$?：$(cat "$U/up.err")"; fi
  okfiles=1
  for f in bin/abroad bin/abroad-rescue bin/abroadd bin/abroad-guard bin/abroad-vm image/Image image/initramfs.gz image/modloop image/rootfs.img integrity.json build.json; do
    cmp -s "$P2/$f" "$U$I/$f" || { okfiles=0; bad "i. ${which}：$f 没换成新的"; }
  done
  [ $okfiles = 1 ] && ok "i. ${which}：程序、镜像、清单、版本号都换成了新的"
  [ -f "$U$I/scripts/m6/install/uninstall.sh" ] && [ -f "$U$I/scripts/m6/install/lib.sh" ] && [ -f "$U$I/scripts/lib/common.sh" ] && ok "i. ${which}：安装目录里有卸载要用的脚本" || bad "i. ${which}：安装目录里没有脚本"
  hashes "$U" "$I/state" >"$U/state.after"; hashes "$U" /var/log/claude-abroad >"$U/log.after"
  cmp -s "$U/state.before" "$U/state.after" && [ -s "$U/state.before" ] && ok "i. ${which}：state/ 逐字节没变（主代理、规则、开关、记录都在）" || bad "i. ${which}：state/ 变了：$(diff "$U/state.before" "$U/state.after" | head -3)"
  cmp -s "$U/log.before" "$U/log.after" && ok "i. ${which}：日志目录没变" || bad "i. ${which}：日志目录变了"
  [ ! -e "$U$I/prev" ] && [ ! -e "$U$SNAP/upgrading" ] && ok "i. ${which}：prev 和升级标记都不在" || bad "i. ${which}：prev 或标记还在"
  newsys=$(diff "$U/sys.log.0" "$U/sys.log" | sed -n 's/^> //p')
  [ "$(grep launchctl <<<"$newsys")" = "SYS: launchctl bootout system/com.claude-abroad.dev.abroadd
SYS: launchctl bootstrap system $U/Library/LaunchDaemons/com.claude-abroad.dev.abroadd.plist" ] &&
    ok "i. ${which}：plist 没变时只停、启 abroadd，不碰 guard" || bad "i. ${which}：launchctl 不对：$(grep launchctl <<<"$newsys")"
  grep -qE 'SYS: (pfctl|networksetup|scutil|dscl|dscacheutil)' <<<"$newsys" && bad "i. ${which}：升级碰了网络或专用用户" || ok "i. ${which}：升级没有碰 pf、网络、DNS、专用用户"
  grep -q '升级完成：1111111aaaaa（构建于 20261101T000000Z） → 2222222bbbbb' "$U/up.err" && ok "i. ${which}：输出写明从哪个版本升到哪个" || bad "i. ${which}：输出不对：$(tail -2 "$U/up.err")"
done
I_DONE=$U # 已升级到 P2 的一份，后面 l、r 用

# ---- j. plist 变了才换 ----
J=$(mkcase); setup_old "$J"
cp "$J/sys.log" "$J/sys.log.0"
M6I_EXIT_HOST=exit-check.example M6I_TEST_DIR="$J" M6I_TEST_SRC="$P2" M6I_TEST_STATUS="$ST_R" bash "${INSTALL}" --upgrade --yes >"$J/up.out" 2>"$J/up.err"; rc=$?
newsys=$(diff "$J/sys.log.0" "$J/sys.log" | sed -n 's/^> //p')
[ $rc = 0 ] && grep -q 'exit-check.example' "$J/Library/LaunchDaemons/com.claude-abroad.dev.abroadd.plist" && ! grep -q 'launchctl bootout system/com.claude-abroad.dev.guard' <<<"$newsys" &&
  ok "j. abroadd 的参数变了：只换 abroadd 的 plist，guard 不动" || bad "j. abroadd 的 plist：rc=$rc $(grep launchctl <<<"$newsys")"
J2=$(mkcase); setup_old "$J2"
echo '<!-- 老版本 -->' >>"$J2/Library/LaunchDaemons/com.claude-abroad.dev.guard.plist"
cp "$J2/sys.log" "$J2/sys.log.0"
upg "$J2" "$P2" "$ST_R"; rc=$?
newsys=$(diff "$J2/sys.log.0" "$J2/sys.log" | sed -n 's/^> //p')
[ $rc = 0 ] && ! grep -q '老版本' "$J2/Library/LaunchDaemons/com.claude-abroad.dev.guard.plist" &&
  [ "$(grep launchctl <<<"$newsys" | sed "s|$J2||")" = "SYS: launchctl bootout system/com.claude-abroad.dev.abroadd
SYS: launchctl bootout system/com.claude-abroad.dev.guard
SYS: launchctl bootstrap system /Library/LaunchDaemons/com.claude-abroad.dev.guard.plist
SYS: launchctl bootstrap system /Library/LaunchDaemons/com.claude-abroad.dev.abroadd.plist" ] &&
  ok "j. guard 的 plist 变了：换掉并像开机一样重新注册一次（在 abroadd 之前）" || bad "j. guard 的 plist：rc=$rc $(grep launchctl <<<"$newsys")"

# ---- k. 状态不允许：拒绝、什么都不改 ----
refuse() { # refuse <说明> <状态文件或空> <期望退出码> <期望的说明> [payload] [额外参数…]
  local what=$1 st=$2 want=$3 msg=$4 src=${5:-$P2} K
  shift 5 2>/dev/null || shift $#
  K=$(mkcase); setup_old "$K" || return
  upgset "$K" >"$K/set.before"; hashes "$K" "$I/state" >"$K/state.before"; cp "$K/sys.log" "$K/sys.log.0"
  upg "$K" "$src" "$st" "$@"; local rc=$?
  upgset "$K" >"$K/set.after"; hashes "$K" "$I/state" >"$K/state.after"
  if [ $rc = "$want" ] && grep -q -- "$msg" "$K/up.err" && cmp -s "$K/set.before" "$K/set.after" && cmp -s "$K/state.before" "$K/state.after" &&
    cmp -s "$K/sys.log" "$K/sys.log.0" && [ ! -e "$K$SNAP/upgrading" ]; then
    ok "${what}：退出码 ${want}，什么都没改"
  else
    bad "${what}：rc=$rc $(grep -v '^$' "$K/up.err" | tail -2)"
  fi
}
for s in ENABLED ENABLING RESTORING; do mkstatus "${TMP}/st.$s" "$s"; done
mkstatus "${TMP}/st.borrow" RESTORED ',"borrow":{"phase":"starting"}'
mkstatus "${TMP}/st.busy" RESTORED ',"busy":true'
printf '{"state":"RESTORED","uninstall":{"running":true,"done":false}}\n' >"${TMP}/st.uning"
printf '{"state":"RESTORED","uninstall":{"running":false,"done":true}}\n' >"${TMP}/st.uned"
printf '{"state":"UNKNOWN","corrupt":true}\n' >"${TMP}/st.corrupt"
refuse "k. 已启用" "${TMP}/st.ENABLED" 10 "已启用时不能升级：请先停用"
refuse "k. 启用中" "${TMP}/st.ENABLING" 10 "正在启用"
refuse "k. 回国中" "${TMP}/st.RESTORING" 10 "正在回国"
refuse "k. 借用虚拟机中" "${TMP}/st.borrow" 10 "正在借用虚拟机"
refuse "k. 一直忙" "${TMP}/st.busy" 10 "正在进行别的操作"
refuse "k. 卸载中" "${TMP}/st.uning" 10 "正在一键卸载"
refuse "k. 已卸载" "${TMP}/st.uned" 10 "已经一键卸载过"
refuse "k. 状态文件坏了" "${TMP}/st.corrupt" 10 "读不出来"
refuse "k. 后台服务不响应" "${TMP}/nosuch" 10 "后台服务没有响应"
# ---- l. 不降级；一样时什么都不做 ----
refuse "l. 要装的比后台旧（不降级）" "$ST_R" 11 "不降级" "$P0"
refuse "l. 已经是这个版本" "$ST_R" 0 "已经是这个版本，什么都不做" "$P1"
# ---- m. 状态格式不一样 ----
refuse "m. 状态格式变了" "$ST_R" 13 "状态格式变了（旧 1、新 2）" "$PF"
# ---- n. 版本不明（老安装：没有 build.json 和脚本）----
old_install() { rm -f "$1$I/build.json"; rm -rf "$1$I/scripts"; }
N=$(mkcase); setup_old "$N"; old_install "$N"
upgset "$N" >"$N/set.before"; cp "$N/sys.log" "$N/sys.log.0"
upg "$N" "$P2" "$ST_R"; rc=$?
upgset "$N" >"$N/set.after"
[ $rc = 14 ] && grep -q '版本不明' "$N/up.err" && grep -q -- '--accept-unknown-version' "$N/up.err" && cmp -s "$N/set.before" "$N/set.after" && cmp -s "$N/sys.log" "$N/sys.log.0" &&
  ok "n. 版本不明、非交互、没加 --accept-unknown-version：退出码 14，什么都没改" || bad "n. 版本不明：rc=$rc $(tail -2 "$N/up.err")"
upg "$N" "$P2" "$ST_R" --accept-unknown-version; rc=$?
[ $rc = 0 ] && cmp -s "$P2/build.json" "$N$I/build.json" && [ -f "$N$I/scripts/m6/install/uninstall.sh" ] && grep -q '版本不明 → 2222222bbbbb' "$N/up.err" &&
  ok "n. 加上 --accept-unknown-version：升级成功，补上了版本号和脚本" || bad "n. 加上 --accept-unknown-version：rc=$rc $(tail -2 "$N/up.err")"
# 状态格式缺失时读 state.json 的 version：1 继续、2 拒绝
N2=$(mkcase); setup_old "$N2"; old_install "$N2"
echo '{"version":2,"state":"RESTORED","step":0}' >"$N2$I/state/abroadd/state.json"
upg "$N2" "$P2" "$ST_R" --accept-unknown-version; rc=$?
[ $rc = 13 ] && grep -q '状态文件的版本也读不出或不是 1' "$N2/up.err" && [ ! -e "$N2$I/build.json" ] && ok "m. 老安装、状态文件版本是 2：退出码 13，什么都没改" || bad "m. 老安装的状态格式：rc=$rc $(tail -2 "$N2/up.err")"
# 2026-11-27 真机：老安装装好后从没启用过，没有 state.json → 按格式 1，可以升级；state.json 损坏照旧拒绝
N3=$(mkcase); setup_old "$N3"; old_install "$N3"; rm -f "$N3$I/state/abroadd/state.json"
upg "$N3" "$P2" "$ST_R" --accept-unknown-version; rc=$?
[ $rc = 0 ] && cmp -s "$P2/build.json" "$N3$I/build.json" && [ ! -e "$N3$I/state/abroadd/state.json" ] &&
  ok "m. 老安装、没有 state.json（从没启用过）：升级成功，不会凭空写出状态文件" || bad "m. 老安装没有 state.json：rc=$rc $(tail -2 "$N3/up.err")"
N4=$(mkcase); setup_old "$N4"; old_install "$N4"; echo 'not json' >"$N4$I/state/abroadd/state.json"
upg "$N4" "$P2" "$ST_R" --accept-unknown-version; rc=$?
[ $rc = 13 ] && [ ! -e "$N4$I/build.json" ] && ok "m. 老安装、state.json 损坏：退出码 13，什么都没改" || bad "m. 老安装 state.json 损坏：rc=$rc $(tail -2 "$N4/up.err")"

# ---- o. 注入失败后退回旧版本 ----
rollback_case() { # rollback_case <说明> <老安装 0|1> <环境变量赋值…>
  local what=$1 old=$2 R; shift 2
  R=$(mkcase); setup_old "$R" || return
  [ "$old" = 1 ] && old_install "$R"
  upgset "$R" >"$R/set.before"; hashes "$R" "$I/state" >"$R/state.before"
  local extra=(); [ "$old" = 1 ] && extra=(--accept-unknown-version)
  env "$@" M6I_TEST_DIR="$R" M6I_TEST_SRC="$P2" M6I_TEST_STATUS="$ST_R" bash "${INSTALL}" --upgrade --yes ${extra[@]+"${extra[@]}"} >"$R/up.out" 2>"$R/up.err"; local rc=$?
  upgset "$R" >"$R/set.after"; hashes "$R" "$I/state" >"$R/state.after"
  if [ $rc = 20 ] && grep -q '升级失败，已退回旧版本' "$R/up.err" && cmp -s "$R/set.before" "$R/set.after" && cmp -s "$R/state.before" "$R/state.after" &&
    [ ! -e "$R$I/prev" ] && [ ! -e "$R$SNAP/upgrading" ] && [ "$(grep launchctl "$R/sys.log" | tail -1 | sed "s|$R||")" = "SYS: launchctl bootstrap system /Library/LaunchDaemons/com.claude-abroad.dev.abroadd.plist" ]; then
    ok "o. ${what}：退出码 20，程序、镜像、清单、plist 和升级前逐字节一样，state/ 没变，最后重启了 abroadd"
  else
    bad "o. ${what}：rc=$rc $(grep -v '^$' "$R/up.err" | tail -2) $(diff "$R/set.before" "$R/set.after" | head -3)"
  fi
}
for step in stop move place plist start verify; do rollback_case "在“${step}”注入失败" 0 M6I_TEST_FAIL_AT=$step; done
rollback_case "老安装（没有版本号和脚本）在“place”注入失败：新放的版本号和脚本被删掉" 1 M6I_TEST_FAIL_AT=place
rollback_case "老安装在“verify”注入失败" 1 M6I_TEST_FAIL_AT=verify
for b in version integrity state; do rollback_case "重启后的应答不对（${b}）" 0 M6I_TEST_STATUS_BAD=$b; done
# guard 的 plist 换过之后失败：放回旧的、重新注册 guard
R=$(mkcase); setup_old "$R"; echo '<!-- 老版本 -->' >>"$R/Library/LaunchDaemons/com.claude-abroad.dev.guard.plist"
upgset "$R" >"$R/set.before"
M6I_TEST_FAIL_AT=start M6I_TEST_DIR="$R" M6I_TEST_SRC="$P2" M6I_TEST_STATUS="$ST_R" bash "${INSTALL}" --upgrade --yes >/dev/null 2>"$R/up.err"; rc=$?
upgset "$R" >"$R/set.after"
[ $rc = 20 ] && cmp -s "$R/set.before" "$R/set.after" && [ "$(grep -c 'launchctl bootstrap system .*guard.plist' "$R/sys.log")" = 3 ] &&
  ok "o. guard 的 plist 换过之后失败：放回旧的 plist，并重新注册了 guard" || bad "o. guard 的 plist 退回：rc=$rc $(grep -c 'bootstrap .*guard' "$R/sys.log")"

# ---- p. 退回也失败；再运行一次 --upgrade 接着退回 ----
R=$(mkcase); setup_old "$R"; upgset "$R" >"$R/set.before"; hashes "$R" "$I/state" >"$R/state.before"
M6I_TEST_FAIL_AT=start,rollback M6I_TEST_DIR="$R" M6I_TEST_SRC="$P2" M6I_TEST_STATUS="$ST_R" bash "${INSTALL}" --upgrade --yes >/dev/null 2>"$R/up.err"; rc=$?
[ $rc = 21 ] && [ -d "$R$I/prev" ] && [ -f "$R$SNAP/upgrading" ] && grep -q 'docs/emergency.md' "$R/up.err" && grep -q '退回旧版本也没有完成' "$R/up.err" &&
  ok "p. 退回也失败：退出码 21，prev 和升级标记留着，指向 docs/emergency.md" || bad "p. 退回也失败：rc=$rc $(tail -2 "$R/up.err")"
upg "$R" "$P2" "$ST_R"; rc=$?
upgset "$R" >"$R/set.after"; hashes "$R" "$I/state" >"$R/state.after"
[ $rc = 20 ] && grep -q '上一次升级没有走完' "$R/up.err" && cmp -s "$R/set.before" "$R/set.after" && cmp -s "$R/state.before" "$R/state.after" && [ ! -e "$R$I/prev" ] && [ ! -e "$R$SNAP/upgrading" ] &&
  ok "p. 再运行一次 --upgrade：先接着退回，回到升级前，退出码 20" || bad "p. 接着退回：rc=$rc $(tail -2 "$R/up.err")"
# 升级标记还在时，全新安装拒绝（安装目录已经存在），卸载照常删掉整个安装目录（含 prev）
R2=$(mkcase); setup_old "$R2"
M6I_TEST_FAIL_AT=start,rollback M6I_TEST_DIR="$R2" M6I_TEST_SRC="$P2" M6I_TEST_STATUS="$ST_R" bash "${INSTALL}" --upgrade --yes >/dev/null 2>&1
uninst "$R2"; rc=$?
[ $rc = 0 ] && [ ! -e "$R2$I" ] && [ ! -e "$R2$SNAP" ] && ok "p. 升级标记在时卸载照常走完（prev 随安装目录一起删）" || bad "p. 升级标记在时卸载：rc=$rc $(tail -2 "$R2/uninstall.err")"

# ---- q. 中断 ----
for step in move place; do
  R=$(mkcase); setup_old "$R"; upgset "$R" >"$R/set.before"
  M6I_TEST_KILL_AT=$step M6I_TEST_DIR="$R" M6I_TEST_SRC="$P2" M6I_TEST_STATUS="$ST_R" bash "${INSTALL}" --upgrade --yes >/dev/null 2>"$R/up.err"; rc=$?
  upgset "$R" >"$R/set.after"
  [ $rc = 20 ] && grep -q '状态 130' "$R/up.err" && cmp -s "$R/set.before" "$R/set.after" && ok "q. 在“${step}”被中断：退回旧版本" || bad "q. 在“${step}”被中断：rc=$rc $(tail -2 "$R/up.err")"
done

# ---- r. 升级后再卸载：和安装前的快照一致 ----
R=$(mkcase); tree "$R" >"$R/before.tree"
setup_old "$R"; rm -rf "$R$I/state/store/proxy" "$R$I/state/abroadd/secrets"
upg "$R" "$P2" "$ST_R" || bad "r. 升级失败"
uninst "$R"; rc=$?
tree "$R" >"$R/after.tree"
[ $rc = 0 ] && diff -q "$R/before.tree" "$R/after.tree" >/dev/null && ok "r. 升级后卸载：和安装前一致" || bad "r. 升级后卸载：rc=$rc $(diff "$R/before.tree" "$R/after.tree" | head -3) $(tail -2 "$R/uninstall.err")"
# 卸载对照里应用那一行只在安装记录里有它时参与：应用是安装之后才放进去的（记录里没有）→ 不算不一致
R=$(mkcase); M6I_TEST_DIR="$R" M6I_TEST_SRC="$P1" M6I_YES=1 bash "${INSTALL}" --from "$PL" >/dev/null 2>&1 || true
[ -d "$R$SNAP" ] && mkdir -p "$R/Applications/ClaudeAbroad.app" && echo later >"$R/Applications/ClaudeAbroad.app/x"
uninst "$R"; rc=$?
[ $rc = 0 ] && [ -f "$R/Applications/ClaudeAbroad.app/x" ] && ok "r. 应用不在安装记录里：卸载不删它，也不因它不一致" || bad "r. 应用不在记录里：rc=$rc $(tail -3 "$R/uninstall.err")"

# ---- s. --from 和 payload ----
[ -f "$PL/payload.sha256" ] && [ -f "$PL/scripts/m6/install/install.sh" ] && [ -f "$PL/scripts/lib/common.sh" ] &&
  [ "$(wc -l <"$PL/payload.sha256" | tr -d ' ')" = 15 ] && ok "s. payload.sh：布局对，15 个文件都有摘要" || bad "s. payload.sh：$(cat "${TMP}/pl.out")"
scripts/dev/payload.sh --src "${TMP}/nosuch" --out "${TMP}/pl2" >/dev/null 2>&1 && bad "s. payload.sh：缺文件时没有失败" || ok "s. payload.sh：缺文件时失败"
S=$(mkcase); M6I_TEST_DIR="$S" M6I_YES=1 bash "$PL/scripts/m6/install/install.sh" --from "$PL" >"$S/i.out" 2>"$S/i.err"; rc=$?
[ $rc = 0 ] && [ ! -e "$S/Applications/ClaudeAbroad.app" ] && cmp -s "$PL/bin/abroadd" "$S$I/bin/abroadd" && cmp -s "$PL/scripts/m6/install/uninstall.sh" "$S$I/scripts/m6/install/uninstall.sh" &&
  ok "s. 从 payload 里的脚本、--from 安装：装上了，没复制应用，安装目录的脚本来自 payload" || bad "s. --from 安装：rc=$rc $(tail -2 "$S/i.err")"
cp -R "$PL" "${TMP}/plbad" && echo tampered >>"${TMP}/plbad/bin/abroad-vm"
S=$(mkcase); tree "$S" >"$S/before.tree"
M6I_TEST_DIR="$S" M6I_YES=1 bash "${INSTALL}" --from "${TMP}/plbad" >/dev/null 2>"$S/i.err"; rc=$?
tree "$S" >"$S/after.tree"
[ $rc = 3 ] && grep -q 'payload.sha256 和里面的文件对不上' "$S/i.err" && diff -q "$S/before.tree" "$S/after.tree" >/dev/null && [ ! -s "$S/sys.log" ] &&
  ok "s. payload 被改过（摘要对不上）：拒绝，什么都没改" || bad "s. payload 被改过：rc=$rc"
cp -R "$PL" "${TMP}/plnosum" && rm "${TMP}/plnosum/payload.sha256"
S=$(mkcase); M6I_TEST_DIR="$S" M6I_YES=1 bash "${INSTALL}" --from "${TMP}/plnosum" >/dev/null 2>"$S/i.err"; rc=$?
[ $rc = 3 ] && grep -q '没有 payload.sha256' "$S/i.err" && ok "s. --from 的目录里没有 payload.sha256：拒绝" || bad "s. 没有 payload.sha256：rc=$rc"
M6I_TEST_DIR="$S" bash "${INSTALL}" --from relative >/dev/null 2>&1; [ $? = 2 ] && ok "s. --from 要绝对路径" || bad "s. --from 相对路径没被拒绝"
grep -q '\[ "${YES}" = 1 \] && M6I_FROM=1' scripts/m6/install/uninstall.sh && ok "s. 卸载带 --yes（应用经管理员授权运行）时不要求 SUDO_USER（仍要求 root；源码核对）" || bad "s. 卸载带 --yes 时仍要求 SUDO_USER"
grep -q '"${M6I_FROM:-0}" = 1 ] ||' scripts/m6/install/lib.sh && ok "s. --from 时真实模式不要求 SUDO_USER（仍要求 root；源码核对）" || bad "s. --from 时仍要求 SUDO_USER"
# 升级用 payload（含 payload.sha256）
S=$(mkcase); setup_old "$S"
M6I_TEST_DIR="$S" M6I_TEST_STATUS="$ST_R" bash "$PL/scripts/m6/install/install.sh" --upgrade --from "$PL" --yes >/dev/null 2>"$S/up.err"; rc=$?
[ $rc = 0 ] && cmp -s "$PL/build.json" "$S$I/build.json" && ok "s. --upgrade --from payload：升级成功" || bad "s. --upgrade --from：rc=$rc $(tail -2 "$S/up.err")"
# --check-only --upgrade：只读
S=$(mkcase); setup_old "$S"; upgset "$S" >"$S/set.before"; cp "$S/sys.log" "$S/sys.log.0"
M6I_TEST_DIR="$S" M6I_TEST_SRC="$P2" M6I_TEST_STATUS="$ST_R" bash "${INSTALL}" --check-only --upgrade >/dev/null 2>"$S/c.err"; rc=$?
upgset "$S" >"$S/set.after"
[ $rc = 0 ] && grep -q '可以升级' "$S/c.err" && cmp -s "$S/set.before" "$S/set.after" && cmp -s "$S/sys.log" "$S/sys.log.0" && ok "s. --check-only --upgrade：报告可以升级，什么都没改" || bad "s. --check-only --upgrade：rc=$rc"

# ---- t. /Applications 应用的交接（顾问 2026-11-27）：老安装记录里有 app → 升级后属主是执行 sudo 的用户、记录里没有 app；
#      再卸载时快照对照一致、应用留着。拿不到用户时不替换、只提示。失败时旧应用放回、记录还在。----
T=$(mkcase); tree "$T" >"$T/before.tree"; setup_old "$T"
[ -n "$(cat "$T$SNAP/app" 2>/dev/null)" ] || bad "t. 准备：安装记录里应有 app"
echo old >"$T/Applications/ClaudeAbroad.app/old-marker"
M6I_TEST_SUDO_USER=alice M6I_TEST_DIR="$T" M6I_TEST_SRC="$P2" M6I_TEST_STATUS="$ST_R" bash "${INSTALL}" --upgrade --yes >/dev/null 2>"$T/up.err"; rc=$?
[ $rc = 0 ] && grep -qx "SYS: chown -R alice $T/Applications/ClaudeAbroad.app" "$T/sys.log" && [ ! -e "$T$SNAP/app" ] &&
  [ ! -e "$T/Applications/ClaudeAbroad.app/old-marker" ] && [ -f "$T/Applications/ClaudeAbroad.app/Contents/Info.plist" ] && grep -q '交给 alice' "$T/up.err" &&
  ok "t. 升级：应用换成新的，属主改成执行 sudo 的用户（alice），安装记录里没有 app 了" || bad "t. 应用交接：rc=$rc $(grep -c chown "$T/sys.log") $(tail -2 "$T/up.err")"
uninst "$T"; rc=$?
tree "$T" >"$T/after.tree"
[ $rc = 0 ] && [ -f "$T/Applications/ClaudeAbroad.app/Contents/Info.plist" ] && grep -q '应用 /Applications/ClaudeAbroad.app 留着，不需要的话可以自己移到废纸篓。' "$T/uninstall.err" &&
  [ "$(diff "$T/before.tree" "$T/after.tree" | grep '^[<>]' | grep -v 'Applications/ClaudeAbroad.app')" = "" ] &&
  ok "t. 交接后卸载：快照对照一致，应用留着，并提示可以自己移到废纸篓" || bad "t. 交接后卸载：rc=$rc $(cat "$T/uninstall.err") $(diff "$T/before.tree" "$T/after.tree" | head -5)"
T=$(mkcase); setup_old "$T"; echo old >"$T/Applications/ClaudeAbroad.app/old-marker"
upg "$T" "$P2" "$ST_R"; rc=$?
[ $rc = 0 ] && [ -f "$T/Applications/ClaudeAbroad.app/old-marker" ] && [ -n "$(cat "$T$SNAP/app" 2>/dev/null)" ] && ! grep -q 'chown -R .*Applications' "$T/sys.log" && grep -q '拿不到执行 sudo 的用户' "$T/up.err" &&
  ok "t. 拿不到执行 sudo 的用户：应用不替换、记录不变，只提示" || bad "t. 拿不到用户：rc=$rc"
T=$(mkcase); setup_old "$T"; echo old >"$T/Applications/ClaudeAbroad.app/old-marker"; upgset "$T" >"$T/set.before"
M6I_TEST_SUDO_USER=alice M6I_TEST_FAIL_AT=verify M6I_TEST_DIR="$T" M6I_TEST_SRC="$P2" M6I_TEST_STATUS="$ST_R" bash "${INSTALL}" --upgrade --yes >/dev/null 2>&1; rc=$?
upgset "$T" >"$T/set.after"
[ $rc = 20 ] && cmp -s "$T/set.before" "$T/set.after" && [ -n "$(cat "$T$SNAP/app" 2>/dev/null)" ] && ok "t. 交接之后升级失败：旧应用放回，安装记录里的 app 还在" || bad "t. 交接后失败：rc=$rc"

# ---- u. B0（v0.71b）：应用经系统密码框执行的命令（Swift 生成的 golden）真跑一遍，不提权：路径换成自测目录、属主条件换成
#      当前用户、去掉 chown。核对：命令行不对时停在 31、输入文件在确认后被改过时停在 32，都不调用命令行、临时目录不留。----
GOLD=app/Tests/AbroadKitTests/testdata/privileged-commands.txt
UD=$(mktemp -d "${TMP}/priv.XXXXXX")
cmdof() { awk -v n="### $1" '$0 == n { f = 1; next } /^### / { f = 0 } f' "${GOLD}"; }
mkfake() { # 假的已安装命令行：记下参数、拷下 --url-file 指的文件内容
  rm -rf "${UD}/inst" "${UD}/args" "${UD}/got"; mkdir -p "${UD}/inst/bin"
  cat >"${UD}/inst/bin/abroad" <<EOS
#!/bin/sh
printf '%s\n' "\$@" >"${UD}/args"
prev=""; for a in "\$@"; do [ "\$prev" = --url-file ] && cat "\$a" >"${UD}/got"; prev=\$a; done
echo '{"ok":true}'
EOS
  chmod 755 "${UD}/inst/bin/abroad" "${UD}/inst/bin" "${UD}/inst"
}
runpriv() { # runpriv <摘要>：按 golden 的 addsource 拼好并用 /bin/sh 执行，输出最后一行
  cmdof addsource | sed -e "s|@INST@|${UD}/inst|g" -e "s|@STG@|${UD}/stg|g" -e "s|@IN@|${UD}/input.txt|g" \
    -e "s|$(printf 'a%.0s' $(seq 1 64))|$1|g" -e "s|\")\" = 0 \]|\")\" = $(id -u) ]|" -e 's|/usr/sbin/chown root:wheel "\$S" \&\& ||' >"${UD}/cmd.sh"
  /bin/sh "${UD}/cmd.sh" | tail -1
}
if [ -s "${GOLD}" ]; then
  printf 'https://sub.example/x\n' >"${UD}/input.txt"
  SUM=$(shasum -a 256 "${UD}/input.txt" | cut -c1-64)
  mkfake; out=$(runpriv "${SUM}")
  [ "${out}" = RESULT=0 ] && [ "$(cat "${UD}/got" 2>/dev/null)" = "https://sub.example/x" ] && grep -qx -- "--url-file" "${UD}/args" && grep -qx "${UD}/stg/in1" "${UD}/args" &&
    [ "$(head -2 "${UD}/args" | tr '\n' ' ')" = "--yes --json " ] && [ ! -e "${UD}/stg" ] &&
    ok "u. 命令行对、摘要对：以 --yes --json 运行，交给命令行的是临时目录里的副本，用完删掉" || bad "u. 正常执行：${out} $(cat "${UD}/args" 2>/dev/null | tr '\n' ' ')"
  mkfake; printf 'https://evil.example/\n' >"${UD}/input.txt"; out=$(runpriv "${SUM}")
  [ "${out}" = RESULT=32 ] && [ ! -e "${UD}/args" ] && [ ! -e "${UD}/stg" ] && ok "u. 确认后输入文件被改过：退出码 32，没有调用命令行，临时目录不留" || bad "u. 输入文件被改：${out}"
  printf 'https://sub.example/x\n' >"${UD}/input.txt"
  mkfake; chmod 775 "${UD}/inst/bin"; out=$(runpriv "${SUM}")
  [ "${out}" = RESULT=31 ] && [ ! -e "${UD}/args" ] && ok "u. bin 目录组可写：退出码 31，没有执行" || bad "u. bin 组可写：${out}"
  mkfake; chmod 777 "${UD}/inst"; out=$(runpriv "${SUM}")
  [ "${out}" = RESULT=31 ] && [ ! -e "${UD}/args" ] && ok "u. 安装目录别人可写：退出码 31" || bad "u. 安装目录可写：${out}"
  mkfake; chmod 775 "${UD}/inst/bin/abroad"; out=$(runpriv "${SUM}")
  [ "${out}" = RESULT=31 ] && [ ! -e "${UD}/args" ] && ok "u. 命令行组可写：退出码 31" || bad "u. 命令行组可写：${out}"
  mkfake; mv "${UD}/inst/bin/abroad" "${UD}/real-abroad"; ln -s "${UD}/real-abroad" "${UD}/inst/bin/abroad"; out=$(runpriv "${SUM}")
  [ "${out}" = RESULT=31 ] && [ ! -e "${UD}/args" ] && ok "u. 命令行是符号链接：退出码 31" || bad "u. 符号链接：${out}"
  mkfake; rm "${UD}/inst/bin/abroad"; out=$(runpriv "${SUM}")
  [ "${out}" = RESULT=31 ] && ok "u. 没有已安装的命令行：退出码 31" || bad "u. 没有命令行：${out}"
  grep -q '= 0 \]' <<<"$(cmdof addsource)" && ok "u. golden 里属主条件是 root（uid 0）" || bad "u. golden 里没有属主是 root 的条件"
else
  bad "u. 没有 ${GOLD}（由 swift 测试 privilegedCommandGolden 生成）"
fi

# ---- u（阶段 B，v0.71c）：安装、升级、卸载后台服务的三条命令（Swift golden，app/Tests/AbroadKitTests/testdata/service-commands.txt）真跑：
#      payload 复制到临时目录、核对 payload.sha256（对不上 33）；从副本执行 install.sh --from --yes；卸载执行已安装的 uninstall.sh
#      （没有 scripts 时 30，属主或权限不对 31），带 --yes / --kill-browsers；临时目录都不留。----
SGOLD=app/Tests/AbroadKitTests/testdata/service-commands.txt
scmd() { awk -v n="### $1" '$0 == n { f = 1; next } /^### / { f = 0 } f' "${SGOLD}" | sed -e "s|@INST@|${UD}/sinst|g" -e "s|@STG@|${UD}/sstg|g" \
  -e "s|/@PAYLOAD@|${UD}/spl|g" -e "s|\")\" = 0 \]|\")\" = $(id -u) ]|" -e 's|/usr/sbin/chown root:wheel "\$S" \&\& ||' >"${UD}/scmd.sh"; /bin/sh "${UD}/scmd.sh" | tail -1; }
mkspl() { # 假 payload：install.sh 记下参数和它所在的目录
  rm -rf "${UD}/spl" "${UD}/sargs"; mkdir -p "${UD}/spl/scripts/m6/install" "${UD}/spl/scripts/lib" "${UD}/spl/bin"
  printf '#!/bin/bash\necho "$0 $*" >"%s/sargs"\n' "${UD}" >"${UD}/spl/scripts/m6/install/install.sh"
  echo x >"${UD}/spl/bin/abroadd"
  (cd "${UD}/spl" && shasum -a 256 scripts/m6/install/install.sh bin/abroadd >payload.sha256)
}
mksinst() { # 假的已安装目录：scripts 里的 uninstall.sh 记下参数
  rm -rf "${UD}/sinst" "${UD}/sargs"; mkdir -p "${UD}/sinst/scripts/m6/install" "${UD}/sinst/scripts/lib"
  printf '#!/bin/bash\necho "$0 $*" >"%s/sargs"\n' "${UD}" >"${UD}/sinst/scripts/m6/install/uninstall.sh"
  echo '# lib' >"${UD}/sinst/scripts/m6/install/lib.sh"; echo '# common' >"${UD}/sinst/scripts/lib/common.sh"
  chmod -R go-w "${UD}/sinst"
}
if [ -s "${SGOLD}" ]; then
  mkspl; out=$(scmd install)
  [ "${out}" = RESULT=0 ] && [ "$(cat "${UD}/sargs" 2>/dev/null)" = "${UD}/sstg/p/scripts/m6/install/install.sh --from ${UD}/sstg/p --yes" ] && [ ! -e "${UD}/sstg" ] &&
    ok "u. 安装：payload 复制到临时目录、核对通过，从副本执行 install.sh --from <副本> --yes，临时目录不留" || bad "u. 安装：${out} $(cat "${UD}/sargs" 2>/dev/null)"
  mkspl; out=$(scmd upgrade-unknown)
  grep -q -- "--from ${UD}/sstg/p --yes --upgrade --accept-unknown-version$" "${UD}/sargs" 2>/dev/null && [ "${out}" = RESULT=0 ] && ok "u. 升级（版本不明）：带 --upgrade --accept-unknown-version" || bad "u. 升级：${out}"
  mkspl; echo tampered >>"${UD}/spl/bin/abroadd"; out=$(scmd install)
  [ "${out}" = RESULT=33 ] && [ ! -e "${UD}/sargs" ] && [ ! -e "${UD}/sstg" ] && ok "u. 应用包里的安装文件被改过（摘要对不上）：退出码 33，没有执行 install.sh" || bad "u. payload 被改：${out}"
  mkspl; rm "${UD}/spl/bin/abroadd"; out=$(scmd upgrade)
  [ "${out}" = RESULT=33 ] && [ ! -e "${UD}/sargs" ] && ok "u. 应用包里的安装文件缺了：退出码 33" || bad "u. payload 缺文件：${out}"
  mksinst; out=$(scmd uninstall-kill)
  [ "${out}" = RESULT=0 ] && [ "$(cat "${UD}/sargs" 2>/dev/null)" = "${UD}/sstg/p/scripts/m6/install/uninstall.sh --yes --kill-browsers" ] && [ ! -e "${UD}/sstg" ] && [ -f "${UD}/sinst/scripts/m6/install/uninstall.sh" ] &&
    ok "u. 卸载：已安装的 uninstall.sh 复制到临时目录后执行，带 --yes --kill-browsers，临时目录不留" || bad "u. 卸载：${out} $(cat "${UD}/sargs" 2>/dev/null)"
  mksinst; rm -rf "${UD}/sinst/scripts"; out=$(scmd uninstall)
  [ "${out}" = RESULT=30 ] && [ ! -e "${UD}/sargs" ] && ok "u. 安装目录里没有卸载脚本（老安装）：退出码 30" || bad "u. 没有 scripts：${out}"
  mksinst; chmod g+w "${UD}/sinst/scripts/m6/install/uninstall.sh"; out=$(scmd uninstall)
  [ "${out}" = RESULT=31 ] && [ ! -e "${UD}/sargs" ] && ok "u. 已安装的卸载脚本组可写：退出码 31，没有执行" || bad "u. 卸载脚本可写：${out}"
  mksinst; mv "${UD}/sinst/scripts/lib/common.sh" "${UD}/c.sh"; ln -s "${UD}/c.sh" "${UD}/sinst/scripts/lib/common.sh"; out=$(scmd uninstall)
  [ "${out}" = RESULT=31 ] && [ ! -e "${UD}/sargs" ] && ok "u. 已安装的脚本是符号链接：退出码 31" || bad "u. 符号链接：${out}"
else
  bad "u. 没有 ${SGOLD}（由 swift 测试 serviceCommandGolden 生成）"
fi

# ---- 升级的空跑和命令核对 ----
plain DRY_RUN=1 bash "${INSTALL}" --upgrade >"${TMP}/up.dry" 2>/dev/null && ok "升级空跑退出码 0" || bad "升级空跑退出码非零"
scripts/m6/install/check-commands.sh "${TMP}/i.dry" "${TMP}/u.dry" "${TMP}/up.dry" >"${TMP}/cc3" && ok "升级的命令核对通过" || bad "升级的命令核对不通过：$(cat "${TMP}/cc3")"
negu() { if scripts/m6/install/check-commands.sh "${TMP}/i.dry" "${TMP}/u.dry" "$2" >/dev/null 2>&1; then bad "升级的反例没被拦下：$1"; else ok "升级的反例被拦下：$1"; fi; }
for c in 'pfctl -a com.apple/250.abroad -F all' 'networksetup -setv6automatic Wi-Fi' 'scutil remove State:/Network/Service/claude-abroad/DNS' 'dscl . -delete /Users/_abroad'; do
  { cat "${TMP}/up.dry"; echo "CHANGE: $c"; } >"${TMP}/nu"; negu "升级里出现 ${c%% *}" "${TMP}/nu"
done
{ cat "${TMP}/up.dry"; echo 'CHANGE: rm -rf /Library/Application Support/claude-abroad/state'; } >"${TMP}/nu"; negu "升级里删状态目录" "${TMP}/nu"
grep -v 'launchctl bootstrap system .*abroadd.plist' "${TMP}/up.dry" | awk '{ print } /^CHANGE: launchctl bootout system\/com.claude-abroad.dev.abroadd$/ { print "CHANGE: launchctl bootstrap system /Library/LaunchDaemons/com.claude-abroad.dev.abroadd.plist" }' >"${TMP}/nu"
negu "放新文件之前就启动 abroadd" "${TMP}/nu"
grep -v 'launchctl bootout system/com.claude-abroad.dev.abroadd' "${TMP}/up.dry" >"${TMP}/nu"; negu "没停 abroadd 就搬旧版" "${TMP}/nu"

# ---- 2026-11-27 真机：abroad-vm 必须链接虚拟化框架（构建时没开 cgo 的产物，安装和升级都拒绝、什么都不改）----
NV=$(mkcase); cp -R "${SRC}" "${TMP}/src-nocgo"; printf '#!/bin/sh\n# abroad-vm built without cgo\n' >"${TMP}/src-nocgo/bin/abroad-vm"
tree "$NV" >"$NV/before.tree"
if M6I_TEST_DIR="$NV" M6I_TEST_SRC="${TMP}/src-nocgo" M6I_YES=1 bash "${INSTALL}" >/dev/null 2>"$NV/err"; then bad "vz. 没链接虚拟化框架的 abroad-vm 也装上了"; else
  tree "$NV" >"$NV/after.tree"
  grep -q '没有链接虚拟化框架' "$NV/err" && cmp -s "$NV/before.tree" "$NV/after.tree" && ok "vz. 全新安装：拒绝没链接虚拟化框架的 abroad-vm，什么都没改" || bad "vz. 全新安装的拒绝不对：$(tail -2 "$NV/err")"
fi
PV="${TMP}/p-nocgo"; cp -R "$P2" "$PV"; printf '#!/bin/sh\n# abroad-vm built without cgo\n' >"$PV/bin/abroad-vm"
UV=$(mkcase); setup_old "$UV" && {
  upgset "$UV" >"$UV/set.before"; hashes "$UV" "$I/state" >"$UV/state.before"
  if upg "$UV" "$PV" "$ST_R"; then bad "vz. 升级用了没链接虚拟化框架的 abroad-vm"; else
    upgset "$UV" >"$UV/set.after"; hashes "$UV" "$I/state" >"$UV/state.after"
    grep -q '没有链接虚拟化框架' "$UV/up.err" && cmp -s "$UV/set.before" "$UV/set.after" && cmp -s "$UV/state.before" "$UV/state.after" &&
      ok "vz. 原地升级：拒绝没链接虚拟化框架的 abroad-vm，什么都没改" || bad "vz. 升级的拒绝不对：$(tail -2 "$UV/up.err")"
  fi
}
if scripts/dev/payload.sh --src "$PV" --out "${TMP}/pl-nocgo" >/dev/null 2>"${TMP}/pl-nocgo.err"; then bad "vz. payload.sh 组装了没链接虚拟化框架的 abroad-vm"; else
  grep -q '没有链接虚拟化框架' "${TMP}/pl-nocgo.err" && ok "vz. payload.sh 遇到没链接虚拟化框架的 abroad-vm 就失败" || bad "vz. payload.sh 的提示不对：$(cat "${TMP}/pl-nocgo.err")"
fi

# ---- 2026-11-27 真机：镜像是 0600 时，装上和升级后虚拟机（_abroad）也要读得到 ----
readable() { local f m; for f in "$@"; do m=$(stat -f '%Lp' "$f"); [ $(( 0$m & 04 )) != 0 ] || { echo "$f $m"; return 1; }; done; }
PM="${TMP}/src-0600"; cp -R "${SRC}" "$PM"; chmod 600 "$PM"/image/*
IM=$(mkcase)
if M6I_TEST_DIR="$IM" M6I_TEST_SRC="$PM" M6I_YES=1 bash "${INSTALL}" >/dev/null 2>"$IM/err"; then
  if why=$(readable "$IM$I"/image/* "$IM$I"/bin/*); then ok "perm. 全新安装：源头镜像是 0600，装上后所有人可读（_abroad 读得到）"; else bad "perm. 全新安装后读不到：$why"; fi
else bad "perm. 全新安装失败：$(tail -2 "$IM/err")"; fi
P2M="${TMP}/p2-0600"; cp -R "$P2" "$P2M"; chmod 600 "$P2M"/image/*
UM=$(mkcase); setup_old "$UM" && {
  if upg "$UM" "$P2M" "$ST_R"; then
    if why=$(readable "$UM$I"/image/* "$UM$I"/bin/*); then ok "perm. 原地升级：新镜像是 0600，升级后所有人可读"; else bad "perm. 升级后读不到：$why"; fi
  else bad "perm. 升级失败：$(tail -2 "$UM/up.err")"; fi
}

# 2026-11-27：真机路径上“是不是开发构建”的判断（测试模式跳过它），由公共函数的自测覆盖
scripts/lib/buildcheck-selftest.sh >"${TMP}/bc" 2>&1 && ok "“是不是开发构建”的判断（$(grep -c '^✓' "${TMP}/bc") 项，含退出码 2 的假程序）" || bad "“是不是开发构建”的判断：$(grep '^✗' "${TMP}/bc" | head -3)"

echo
[ "${skips:-0}" -gt 0 ] && echo "跳过 ${skips} 项（原因见上面“- 跳过”各行；不算失败）"
[ $fails = 0 ] && echo "自测全部通过" || echo "自测：${fails} 处不通过"
exit $((fails > 0))
