#!/usr/bin/env bash
# scripts/dev/build-app.sh 的自测（v0.71c）：目标换成临时目录、用现成的假应用包跳过构建；不碰真实的 /Applications。
#   a. 没有同名的：复制进去；
#   b. 有我们自己的（包标识 com.claude-abroad.app）：替换成新的，不留临时文件；
#   c. 别人的同名应用（包标识不同、或是符号链接）：不碰，退出码 3；
#   d. 我们的但改不了（只读）：不碰，提示先运行 reinstall.sh，退出码 4；
#   e. 护栏：测试变量要一起给、目标不能是 /Applications；以 root 运行被拒；空跑只打印、不写；
#   f. app-bundle.sh --payload 的参数检查：不是 payload 目录、摘要对不上时失败（不编译）；
#   g. --place-only（reinstall.sh 升级后台后调用）：不构建，只放；同名的属主是别人、是符号链接时不碰并提示；
#      应用还开着时照样替换，提示退出并重新打开；参数不对被拒。
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
[ "$(id -u)" != 0 ] || { echo "不要以 root 运行" >&2; exit 1; }
TMP=$(mktemp -d "${TMPDIR:-/tmp}/buildapp.XXXXXX")
trap 'chmod -R u+w "${TMP}" 2>/dev/null; rm -rf "${TMP:?}"' EXIT
fails=0
ok() { echo "✓ $*"; }
bad() { echo "✗ $*"; fails=$((fails + 1)); }
S=scripts/dev/build-app.sh

mkapp() { # mkapp <目录> <包标识> <标记>
  mkdir -p "$1/Contents/MacOS" "$1/Contents/Resources/payload"
  /usr/bin/plutil -create xml1 "$1/Contents/Info.plist"
  /usr/bin/plutil -insert CFBundleIdentifier -string "$2" "$1/Contents/Info.plist"
  echo "$3" >"$1/Contents/MacOS/ClaudeAbroad"
}
NEWB="${TMP}/new/ClaudeAbroad.app"; mkapp "${NEWB}" com.claude-abroad.app new
run() { BUILD_APP_TEST_DEST="$1" BUILD_APP_TEST_BUNDLE="${NEWB}" bash "$S" >"$1.out" 2>"$1.err"; }

# a
A="${TMP}/a"; mkdir -p "$A"
run "$A"; rc=$?
[ $rc = 0 ] && [ "$(cat "$A/ClaudeAbroad.app/Contents/MacOS/ClaudeAbroad")" = new ] && ok "a. 没有同名的：复制进去" || bad "a. rc=$rc $(cat "$A.err")"
# b
B="${TMP}/b"; mkdir -p "$B"; mkapp "$B/ClaudeAbroad.app" com.claude-abroad.app old; echo stale >"$B/ClaudeAbroad.app/stale"
run "$B"; rc=$?
[ $rc = 0 ] && [ "$(cat "$B/ClaudeAbroad.app/Contents/MacOS/ClaudeAbroad")" = new ] && [ ! -e "$B/ClaudeAbroad.app/stale" ] && [ -z "$(ls -A "$B" | grep -v '^ClaudeAbroad.app$')" ] &&
  ok "b. 我们自己的应用：整个换成新的，不留临时文件" || bad "b. rc=$rc $(ls -A "$B") $(cat "$B.err")"
# c
C="${TMP}/c"; mkdir -p "$C"; mkapp "$C/ClaudeAbroad.app" com.example.other theirs
run "$C"; rc=$?
[ $rc = 3 ] && [ "$(cat "$C/ClaudeAbroad.app/Contents/MacOS/ClaudeAbroad")" = theirs ] && grep -q '不是 Claude 出国的应用' "$C.err" && ok "c. 别人的同名应用：不碰，退出码 3" || bad "c. rc=$rc"
C2="${TMP}/c2"; mkdir -p "$C2" "${TMP}/elsewhere"; mkapp "${TMP}/elsewhere/X.app" com.claude-abroad.app linked; ln -s "${TMP}/elsewhere/X.app" "$C2/ClaudeAbroad.app"
run "$C2"; rc=$?
[ $rc = 3 ] && [ -L "$C2/ClaudeAbroad.app" ] && [ "$(cat "${TMP}/elsewhere/X.app/Contents/MacOS/ClaudeAbroad")" = linked ] && ok "c. 同名的是符号链接：不碰，退出码 3" || bad "c2. rc=$rc"
# d
D="${TMP}/d"; mkdir -p "$D"; mkapp "$D/ClaudeAbroad.app" com.claude-abroad.app old; chmod -R a-w "$D/ClaudeAbroad.app"
run "$D"; rc=$?
[ $rc = 4 ] && [ "$(cat "$D/ClaudeAbroad.app/Contents/MacOS/ClaudeAbroad")" = old ] && grep -q 'scripts/dev/reinstall.sh' "$D.err" && ok "d. 应用不可写：不碰，提示先运行 reinstall.sh，退出码 4" || bad "d. rc=$rc $(cat "$D.err")"
chmod -R u+w "$D/ClaudeAbroad.app"
# e
BUILD_APP_TEST_DEST="${TMP}/x" bash "$S" >/dev/null 2>&1; [ $? = 2 ] && ok "e. 测试变量只给一个：拒绝" || bad "e. 测试变量只给一个没被拒绝"
BUILD_APP_TEST_DEST=/Applications BUILD_APP_TEST_BUNDLE="${NEWB}" bash "$S" >/dev/null 2>&1; [ $? = 2 ] && ok "e. 自测的目标是 /Applications：拒绝" || bad "e. /Applications 没被拒绝"
E="${TMP}/e"; mkdir -p "$E"
DRY_RUN=1 BUILD_APP_TEST_DEST="$E" BUILD_APP_TEST_BUNDLE="${NEWB}" bash "$S" >"$E.out" 2>/dev/null; rc=$?
[ $rc = 0 ] && grep -q '^RUN: cp -R' "$E.out" && [ -z "$(ls -A "$E")" ] && ok "e. 空跑：只打印，不写" || bad "e. 空跑：rc=$rc"
DRY_RUN=1 bash "$S" >"${TMP}/plan.out" 2>/dev/null
[ "$(grep -c '^RUN:' "${TMP}/plan.out")" = 4 ] && grep -q '^RUN: scripts/dev/app-bundle.sh --payload scripts/dev/out/payload$' "${TMP}/plan.out" &&
  ok "e. 空跑的步骤：构建、payload、打包（带 payload）、放进应用程序" || bad "e. 空跑的步骤：$(cat "${TMP}/plan.out")"
grep -nE '(^|[^a-z-])(sudo|pfctl|networksetup|launchctl|dscl)([^a-z]|$)' <(sed -e 's/^[[:space:]]*#.*$//' "$S") | grep -vE 'say |echo ' | grep -q . && bad "e. 源码里有 sudo、pfctl、networksetup、launchctl、dscl" || ok "e. 源码里没有 sudo、pfctl、networksetup、launchctl、dscl"
# f
scripts/dev/app-bundle.sh --payload "${TMP}/nosuch" >/dev/null 2>"${TMP}/f1"; [ $? != 0 ] && grep -q '不是 payload 目录' "${TMP}/f1" && ok "f. app-bundle.sh --payload 不是 payload 目录：失败" || bad "f. 不是 payload 目录没失败"
P="${TMP}/pl"; mkdir -p "$P"; echo '{"commit":"abc1234","build_time":"20261127T000000Z","dirty":false,"state_format":1}' >"$P/build.json"
(cd "$P" && shasum -a 256 build.json >payload.sha256); echo changed >>"$P/build.json"
scripts/dev/app-bundle.sh --payload "$P" >/dev/null 2>"${TMP}/f2"; [ $? != 0 ] && grep -q '对不上' "${TMP}/f2" && ok "f. app-bundle.sh --payload 摘要对不上：失败" || bad "f. 摘要对不上没失败"
# g
runp() { local d=$1; shift; env BUILD_APP_TEST_DEST="$d" BUILD_APP_TEST_BUNDLE="${NEWB}" "$@" bash "$S" --place-only >"$d.out" 2>"$d.err"; }
G="${TMP}/g"; mkdir -p "$G"; mkapp "$G/ClaudeAbroad.app" com.claude-abroad.app old
runp "$G"; rc=$?
[ $rc = 0 ] && [ "$(cat "$G/ClaudeAbroad.app/Contents/MacOS/ClaudeAbroad")" = new ] && grep -q '应用已放进' "$G.err" && ! grep -q '第 1 步' "$G.err" &&
  ok "g. --place-only：不构建，把我们自己的应用换成新的" || bad "g. --place-only：rc=$rc $(cat "$G.err")"
G2="${TMP}/g2"; mkdir -p "$G2"; mkapp "$G2/ClaudeAbroad.app" com.claude-abroad.app old
runp "$G2" BUILD_APP_TEST_FOREIGN_OWNER=1; rc=$?
[ $rc = 4 ] && [ "$(cat "$G2/ClaudeAbroad.app/Contents/MacOS/ClaudeAbroad")" = old ] && grep -q '属主是别的账户' "$G2.err" &&
  ok "g. --place-only 属主是别人：不碰，提示，退出码 4" || bad "g. 属主是别人：rc=$rc $(cat "$G2.err")"
G3="${TMP}/g3"; mkdir -p "$G3"; ln -s "${TMP}/elsewhere/X.app" "$G3/ClaudeAbroad.app"
runp "$G3"; rc=$?
[ $rc = 3 ] && [ -L "$G3/ClaudeAbroad.app" ] && [ "$(cat "${TMP}/elsewhere/X.app/Contents/MacOS/ClaudeAbroad")" = linked ] && grep -q '不碰它' "$G3.err" &&
  ok "g. --place-only 同名的是符号链接：不碰，提示，退出码 3" || bad "g. 符号链接：rc=$rc"
G4="${TMP}/g4"; mkdir -p "$G4"; mkapp "$G4/ClaudeAbroad.app" com.claude-abroad.app old
(exec -a "$G4/ClaudeAbroad.app/Contents/MacOS/ClaudeAbroad" sleep 60) & spid=$! # 进程的命令行和应用一样（假装应用开着）
runp "$G4"; rc=$?
kill "$spid" 2>/dev/null; wait "$spid" 2>/dev/null
[ $rc = 0 ] && [ "$(cat "$G4/ClaudeAbroad.app/Contents/MacOS/ClaudeAbroad")" = new ] && grep -q '请退出并重新打开应用' "$G4.err" &&
  ok "g. --place-only 应用还开着：照样替换，提示退出并重新打开" || bad "g. 应用开着：rc=$rc $(cat "$G4.err")"
grep -q '请退出并重新打开应用' "$G.err" && bad "g. 应用没开也提示重新打开" || ok "g. 应用没开时不提示重新打开"
BUILD_APP_TEST_FOREIGN_OWNER=1 bash "$S" --place-only >/dev/null 2>&1; [ $? = 2 ] && ok "g. BUILD_APP_TEST_FOREIGN_OWNER 单独给：拒绝" || bad "g. BUILD_APP_TEST_FOREIGN_OWNER 单独给没被拒绝"
bash "$S" --bogus >/dev/null 2>&1; [ $? = 2 ] && ok "g. 不认识的参数：拒绝" || bad "g. 不认识的参数没被拒绝"
DRY_RUN=1 bash "$S" --place-only >"${TMP}/plan2.out" 2>/dev/null
[ "$(grep -c '^RUN:' "${TMP}/plan2.out")" = 1 ] && grep -q '^RUN: cp -R' "${TMP}/plan2.out" && ok "g. --place-only 空跑：只有放置一步" || bad "g. --place-only 空跑：$(cat "${TMP}/plan2.out")"

echo
[ $fails = 0 ] && echo "自测全部通过" || echo "自测：${fails} 处不通过"
exit $((fails > 0))
