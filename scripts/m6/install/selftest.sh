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
#   h. 空跑：退出码 0，不写任何文件；命令核对通过；反例（多出的命令、安装里碰网络、顺序错、源码里的禁止命令）都被拦下。
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
for f in Image initramfs.gz modloop rootfs.img; do printf 'img %s\n' "$f" >"${SRC}/image/$f"; done
echo '{"version":1,"files":{}}' >"${SRC}/integrity.json"
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

# 2026-11-27：真机路径上“是不是开发构建”的判断（测试模式跳过它），由公共函数的自测覆盖
scripts/lib/buildcheck-selftest.sh >"${TMP}/bc" 2>&1 && ok "“是不是开发构建”的判断（$(grep -c '^✓' "${TMP}/bc") 项，含退出码 2 的假程序）" || bad "“是不是开发构建”的判断：$(grep '^✗' "${TMP}/bc" | head -3)"

echo
[ "${skips:-0}" -gt 0 ] && echo "跳过 ${skips} 项（原因见上面“- 跳过”各行；不算失败）"
[ $fails = 0 ] && echo "自测全部通过" || echo "自测：${fails} 处不通过"
exit $((fails > 0))
