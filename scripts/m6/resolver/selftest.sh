#!/usr/bin/env bash
# 解析锁真机脚本的自测（M6-3）：不用 root，不碰真实的 /etc/resolver，不做真实的 DNS 查询和 curl。
# M6R_TEST_DIR 指到临时目录：锁文件目录换成 <临时目录>/resolver，状态目录换成 <临时目录>/state，要用户输入的地方从标准输入读。
# 每个场景的临时目录里都预先放一个别人的文件（corp.example），最后核对它没被删、没被改。
#   a. 正常走完（预演 + yes + 两种写法）：自己写的锁文件都删了，状态目录清掉，证据里有手动观察的记录；
#   b. 没输入 yes：只做预演，不碰真实域名；
#   c. 出错（自测注入错误，预演那一轮和真实域名那一轮各一次）：自己写的都删了，退出码非零；
#   d. 中断：Ctrl+C 对应的 TERM、关掉终端对应的 HUP：自己写的都删了；
#   e. 死人开关到点：脚本还在等输入时锁文件被自动删掉，脚本随后发现并停下；取消了的死人开关到点不动任何文件；
#   f. 反例：同名文件已经存在（别人的、或内容和我们的一样但没有记录）时一开始就停下，不覆盖、不写任何文件；
#      写入后被别的程序改过的文件，restore.sh 不删、报告出来；
#   g. 不用 sudo、不配自测目录的真实模式被拒绝；自测目录不能在 /etc 下；
#   h. 空跑：退出码 0，不写任何文件（/etc/resolver、状态目录不变）；命令核对通过；反例都被拦下；
#   i. 证据和日志里没有用户名、主目录路径；脱敏函数的样例；
#   k. 基线出口三种结果（M6R_TEST_BASELINE 模拟）；直连可达时没有单独确认就不出现请用户打开 Claude 的提示；
#      经代理或 TUN 时每个场景没有 exit-trusted 就不出现请用户打开 Claude 的提示（记“跳过：出口未确认”）；
#      不带凭据的几项显示锁无效（M6R_TEST_LOCK_LEAK=1）时后面的手动项默认跳过，经代理时先确认出口才允许 open-anyway；
#      原始输出存档只取字段、不留任何地址；测试变量离开自测目录被拒绝；
#   j. 跑完没有残留的死人开关进程。
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "${ROOT}" || exit 1
[ "$(id -u)" != 0 ] || { echo "不要以 root 运行" >&2; exit 1; }
TMP=$(mktemp -d "${TMPDIR:-/tmp}/m6rst.XXXXXX")
trap 'rm -rf "${TMP:?}"' EXIT
fails=0
ok() { echo "✓ $*"; }
bad() { echo "✗ $*"; fails=$((fails + 1)); }
RUN=scripts/m6/resolver/run.sh
RESTORE=scripts/m6/resolver/restore.sh

go build -o "${TMP}/abroad-dev" ./cmd/abroad-dev || { echo "编不出 abroad-dev" >&2; exit 1; }
export M6R_ABROAD_DEV="${TMP}/abroad-dev"
"${M6R_ABROAD_DEV}" resolver-lock content --form default >"${TMP}/content.default"
FOREIGN='nameserver 192.0.2.53
'

# 新建一个场景目录：resolver/ 里预先放别人的文件。
mkcase() {
  local d="${TMP}/$1"
  mkdir -p "$d/resolver"
  printf '%s' "${FOREIGN}" >"$d/resolver/corp.example"
  echo "$d"
}
# 场景结束后核对：自己写的都没了，别人的文件原样还在。
check_clean() { # $1 场景目录 $2 场景名
  local d=$1 left
  left=$(cd "$d/resolver" 2>/dev/null && ls -A | grep -vx corp.example | tr '\n' ' ')
  [ -z "${left}" ] && ok "$2：自己写的锁文件都删了" || bad "$2：还有没删的：${left}"
  [ "$(cat "$d/resolver/corp.example" 2>/dev/null)" = "nameserver 192.0.2.53" ] && ok "$2：别人的文件 corp.example 没被删、没被改" || bad "$2：别人的文件被动过"
}
wait_for() { # $1 文件 $2 秒：等它出现
  local i=0
  while [ ! -e "$1" ] && [ "$i" -lt $(($2 * 10)) ]; do sleep 0.1; i=$((i + 1)); done
  [ -e "$1" ]
}
ALL_ANS='yes
s
s
s
s
y 第一种
n
n
s

n
n
n
s

'

# ---- a. 正常走完 ----
D=$(mkcase a)
printf '%s' "${ALL_ANS}" | M6R_TEST_DIR="$D" bash "${RUN}" >"${TMP}/a.out" 2>&1; rc=$?
[ $rc = 0 ] && ok "a. 正常走完，退出码 0" || bad "a. 退出码 ${rc}：$(tail -5 "${TMP}/a.out")"
check_clean "$D" "a"
[ ! -e "$D/state" ] && ok "a. 状态目录清掉了" || bad "a. 状态目录还在：$(ls -A "$D/state")"
NDOM=$("${M6R_ABROAD_DEV}" resolver-lock domains | grep -c .)
NWANT=$((1 + 2 * NDOM))
[ "$(grep -c '已写入锁文件' "${TMP}/a.out")" = "${NWANT}" ] && [ "$(grep -c '已删除锁文件' "${TMP}/a.out")" = "${NWANT}" ] && ok "a. 预演 1 个 + 两种写法各 ${NDOM} 个（内置名单），写入、删除各 ${NWANT} 次" || bad "a. 写入、删除次数不对：$(grep -cE '已写入锁文件' "${TMP}/a.out")/$(grep -c '已删除锁文件' "${TMP}/a.out")"
[ ! -e "$D/resolver" ] || [ -d "$D/resolver" ] && ok "a. 原来就有的锁文件目录没被删" || bad "a. 锁文件目录被动了"
EV=$(ls "$D"/out/evidence-*.txt 2>/dev/null | head -1)
if [ -n "${EV}" ] && grep -q '手动 写法=default 场景=当前 claude_code=y 备注：第一种' "${EV}" && grep -q '手动 写法=loopback 场景=当前 claude_code=n' "${EV}" &&
  grep -q '锁文件内容（loopback）：nameserver 127.0.0.1;port [0-9]*;timeout 1;' "${EV}" && grep -q '用户确认锁真实域名：yes' "${EV}"; then
  ok "a. 证据里有两种写法的内容、手动观察的记录和 yes 的确认"
else
  bad "a. 证据不全：$(cat "${EV:-/dev/null}")"
fi

# 没有 /etc/resolver 目录时：脚本建、还原时空了就删。
D=$(mkcase a2); rm -rf "${D:?}/resolver"
printf '%s' "${ALL_ANS}" | M6R_TEST_DIR="$D" bash "${RUN}" >"${TMP}/a2.out" 2>&1; rc=$?
[ $rc = 0 ] && [ ! -e "$D/resolver" ] && grep -q '已建目录' "${TMP}/a2.out" && ok "a. 原来没有锁文件目录：脚本建了，结束时空了就删掉" || bad "a. 建、删目录不对：rc=$rc $(ls -A "$D")"

# ---- b. 没输入 yes ----
D=$(mkcase b)
printf 'no\n' | M6R_TEST_DIR="$D" bash "${RUN}" >"${TMP}/b.out" 2>&1; rc=$?
[ $rc = 0 ] && [ "$(grep -c '已写入锁文件' "${TMP}/b.out")" = 1 ] && grep -q '没有输入 yes，跳过真实域名' "${TMP}/b.out" && ok "b. 没输入 yes：只做了测试域名的预演" || bad "b. 不对：rc=$rc $(tail -3 "${TMP}/b.out")"
grep -q '已写入锁文件 .*/claude\.ai' "${TMP}/b.out" && bad "b. 没确认却写了真实域名" || ok "b. 没确认时没有写真实域名"
check_clean "$D" "b"
D=$(mkcase b2)
M6R_TEST_DIR="$D" bash "${RUN}" --test-domain-only </dev/null >"${TMP}/b2.out" 2>&1; rc=$?
[ $rc = 0 ] && [ "$(grep -c '已写入锁文件' "${TMP}/b2.out")" = 1 ] && ! grep -q 'yes' "${TMP}/b2.out" && ok "b. --test-domain-only 只做预演、不问 yes" || bad "b. --test-domain-only 不对：$(tail -3 "${TMP}/b2.out")"
check_clean "$D" "b2"

# ---- c. 出错 ----
for at in after_write:1 after_write:2 after_write:3 after_deadman:2; do
  D=$(mkcase "c-${at%%:*}-${at##*:}")
  printf '%s' "${ALL_ANS}" | M6R_TEST_DIR="$D" M6R_TEST_FAIL_AT="${at}" bash "${RUN}" >"${TMP}/c.out" 2>&1; rc=$?
  [ $rc != 0 ] && grep -q "自测注入的错误：${at}" "${TMP}/c.out" && grep -q '中途退出' "${TMP}/c.out" && ok "c. 出错（${at}）：退出码 ${rc}，自动执行了还原" || bad "c. 出错（${at}）不对：rc=$rc $(tail -5 "${TMP}/c.out")"
  check_clean "$D" "c（${at}）"
  [ ! -e "$D/state" ] && ok "c（${at}）：状态目录清掉了" || bad "c（${at}）：状态目录还在：$(ls -A "$D/state")"
done

# ---- d. 中断（TERM = Ctrl+C 的同类，HUP = 关掉终端）----
for sig in TERM HUP INT; do
  D=$(mkcase "d-${sig}")
  { printf 'yes\ns\ns\ns\ns\n'; sleep 8; } | M6R_TEST_DIR="$D" bash "${RUN}" >"${TMP}/d.out" 2>&1 &
  pid=$!
  if wait_for "$D/resolver/anthropic.com" 10; then
    sleep 0.3
    kill -s "${sig}" "${pid}" 2>/dev/null
    wait "${pid}"; rc=$?
    if [ "${sig}" = INT ]; then
      # 非交互 shell 的后台任务忽略 SIGINT（bash 的规定），这里只核对它没有破坏任何东西；真终端里的 Ctrl+C 走 trap 的 INT。
      if [ $rc = 0 ]; then ok "d. INT（后台任务忽略，脚本照常走完）"; else ok "d. INT：退出码 ${rc}"; fi
    else
      [ $rc != 0 ] && grep -q '中途退出' "${TMP}/d.out" && ok "d. ${sig}：退出码 ${rc}，自动执行了还原" || bad "d. ${sig} 不对：rc=$rc $(tail -5 "${TMP}/d.out")"
    fi
  else
    bad "d. ${sig}：10 秒内没等到锁文件写入：$(tail -5 "${TMP}/d.out")"
    kill "${pid}" 2>/dev/null; wait "${pid}" 2>/dev/null
  fi
  check_clean "$D" "d（${sig}）"
done
grep -q "trap 'exit 129' HUP" "${RUN}" && grep -q "trap 'exit 130' INT TERM" scripts/lib/common.sh && ok "d. 源码：INT、TERM、HUP 都接到了还原" || bad "d. 源码里没有接 INT、TERM、HUP"

# ---- e. 死人开关 ----
D=$(mkcase e)
{ printf 'yes\ns\ns\ns\ns\n'; sleep 7; } | M6R_TEST_DIR="$D" M6R_DEADMAN_SECS=2 bash "${RUN}" >"${TMP}/e.out" 2>&1 &
pid=$!
if wait_for "$D/resolver/claude.ai" 10; then
  i=0; while [ -e "$D/resolver/claude.ai" ] && [ $i -lt 60 ]; do sleep 0.1; i=$((i + 1)); done
  if [ ! -e "$D/resolver/claude.ai" ] && ps -p "${pid}" >/dev/null 2>&1; then
    ok "e. 死人开关到点：脚本还在等输入时锁文件已被删掉"
  else
    bad "e. 死人开关没有按时删掉锁文件"
  fi
else
  bad "e. 没等到锁文件写入：$(tail -5 "${TMP}/e.out")"
fi
wait "${pid}"; rc=$?
[ $rc = 4 ] && grep -q '死人开关到点，或被别的程序删了' "${TMP}/e.out" && ok "e. 脚本随后发现锁已不在，不再继续后面的轮次（退出码 4）" || bad "e. 脚本没有发现：rc=$rc $(tail -5 "${TMP}/e.out")"
check_clean "$D" "e"
grep -q '死人开关第 2 轮到点' "$D"/out/run-*.log 2>/dev/null && ok "e. 日志里记下了死人开关到点" || bad "e. 日志里没有死人开关到点：$(cat "$D"/out/run-*.log 2>/dev/null | tail -5)"
# 取消了的死人开关到点不动任何文件（这一轮的锁文件还在）。
D=$(mkcase e2)
(
  export M6R_TEST_DIR="$D" DRY_RUN=0
  . scripts/m6/resolver/lib.sh
  M6R_LOG="${TMP}/e2.log"
  m6r_state_init
  m6r_deadman_start 2 "${ROOT}/${RESTORE}" 7
  m6r_deadman_cancel 7
  m6r_deadman_start 30 "${ROOT}/${RESTORE}" 8
  m6r_write_lock claude.ai "${TMP}/content.default"
) >"${TMP}/e2.out" 2>&1
sleep 3.5
[ -f "$D/resolver/claude.ai" ] && ok "e. 取消了的死人开关（第 7 轮）到点不动第 8 轮的锁文件" || bad "e. 取消了的死人开关删了文件：$(cat "${TMP}/e2.out")"
M6R_TEST_DIR="$D" bash "${RESTORE}" >"${TMP}/e2r.out" 2>&1 && [ ! -e "$D/resolver/claude.ai" ] && ok "e. 之后手动执行 restore.sh 删掉了" || bad "e. restore.sh 没删掉：$(cat "${TMP}/e2r.out")"
check_clean "$D" "e2"

# ---- f. 反例：同名文件、被改过的文件 ----
D=$(mkcase f1)
printf 'nameserver 10.0.0.1\n' >"$D/resolver/claude.ai"
printf '%s' "${ALL_ANS}" | M6R_TEST_DIR="$D" bash "${RUN}" >"${TMP}/f1.out" 2>&1; rc=$?
[ $rc = 3 ] && grep -q 'claude.ai 已经存在' "${TMP}/f1.out" && ok "f. 别人的同名文件：一开始就停下（退出码 3）" || bad "f. 同名文件没拦下：rc=$rc $(tail -3 "${TMP}/f1.out")"
[ "$(cat "$D/resolver/claude.ai")" = "nameserver 10.0.0.1" ] && ok "f. 别人的同名文件没被覆盖、没被删" || bad "f. 别人的同名文件被动了"
[ "$(ls -A "$D/resolver" | sort | tr '\n' ' ')" = "claude.ai corp.example " ] && [ ! -e "$D/state" ] && ok "f. 停下时没写任何文件（包括测试域名）" || bad "f. 停下前写了文件：$(ls -A "$D/resolver") $(ls -A "$D/state" 2>/dev/null)"
D=$(mkcase f2)
cp "${TMP}/content.default" "$D/resolver/lock-test.claude-abroad.invalid"
printf '%s' "${ALL_ANS}" | M6R_TEST_DIR="$D" bash "${RUN}" >"${TMP}/f2.out" 2>&1; rc=$?
[ $rc = 3 ] && cmp -s "${TMP}/content.default" "$D/resolver/lock-test.claude-abroad.invalid" && ok "f. 内容和我们一样、但没有记录的同名文件：也停下，不覆盖、不删" || bad "f. 没有记录的同名文件没拦下：rc=$rc"
D=$(mkcase f3)
mkdir -p "$D/state"
cp "${TMP}/content.default" "$D/state/lock.claude.com"
printf 'nameserver 10.9.9.9\n' >"$D/resolver/claude.com"
cp "${TMP}/content.default" "$D/state/lock.anthropic.com"
cp "${TMP}/content.default" "$D/resolver/anthropic.com"
M6R_TEST_DIR="$D" bash "${RESTORE}" >"${TMP}/f3.out" 2>&1; rc=$?
[ $rc = 1 ] && [ "$(cat "$D/resolver/claude.com")" = "nameserver 10.9.9.9" ] && grep -q '不一致' "${TMP}/f3.out" && ok "f. 写入后被改过的文件：restore.sh 不删、报告出来（退出码 1）" || bad "f. 被改过的文件处理不对：rc=$rc $(cat "${TMP}/f3.out")"
[ ! -e "$D/resolver/anthropic.com" ] && [ -f "$D/state/lock.claude.com" ] && ok "f. 同一次还原里内容一致的照常删掉，不一致的记录留着" || bad "f. 一致的没删或记录丢了"
printf '%s' "${ALL_ANS}" | M6R_TEST_DIR="$D" bash "${RUN}" >"${TMP}/f4.out" 2>&1; rc=$?
[ $rc = 3 ] && grep -q '上一次没清理的记录' "${TMP}/f4.out" && ok "f. 有上一次没清理的记录时 run.sh 不开始" || bad "f. 有残留记录时没拦下：rc=$rc"

# ---- g. 模式的护栏 ----
out=$(env -u M6R_TEST_DIR -u M6R_ABROAD_DEV bash "${RUN}" </dev/null 2>&1); rc=$?
[ $rc != 0 ] && printf '%s' "$out" | grep -q '需要用 sudo 运行' && ok "g. 不用 sudo、不配自测目录的真实模式被拒绝" || bad "g. 真实模式没有要求 sudo：rc=$rc $out"
out=$(env -u M6R_TEST_DIR -u M6R_ABROAD_DEV bash "${RESTORE}" </dev/null 2>&1); rc=$?
[ $rc != 0 ] && printf '%s' "$out" | grep -q '需要用 sudo 运行' && ok "g. restore.sh 的真实模式也要求 sudo" || bad "g. restore.sh 没有要求 sudo：rc=$rc $out"
out=$(M6R_TEST_DIR=/etc/resolver bash "${RUN}" </dev/null 2>&1); rc=$?
[ $rc != 0 ] && printf '%s' "$out" | grep -q '不能在 /etc 下' && ok "g. 自测目录不能在 /etc 下" || bad "g. /etc 下的自测目录没拦下：$out"
out=$(M6R_TEST_DIR=relative/dir bash "${RUN}" </dev/null 2>&1); rc=$?
[ $rc != 0 ] && ok "g. 自测目录必须是绝对路径" || bad "g. 相对路径没拦下"
grep -qF 'for v in ${M6R_TEST_VARS} M6R_TEST_DIR M6R_ABROAD_DEV' scripts/m6/resolver/lib.sh && grep -q '以 root 运行时不接受' scripts/m6/resolver/lib.sh &&
  ok "g. 源码：以 root 运行时拒绝 M6R_TEST_DIR、M6R_ABROAD_DEV 等变量（会话里不能用 root 实测，只做源码检查）" || bad "g. 源码里没有 root 下的拒绝"
grep -qF 'M6R_REAL_RESOLVER_DIR=/etc/resolver' scripts/m6/resolver/lib.sh && [ "$(grep -c '/etc/resolver' scripts/m6/resolver/lib.sh)" -ge 1 ] && ok "g. 源码：真实模式的锁文件目录写死为 /etc/resolver" || bad "g. 源码里真实目录不是写死的"

# ---- h. 空跑 ----
etc_before=$(ls -la /etc/resolver 2>&1; ls -la "/Library/Application Support/claude-abroad-m6r" 2>&1)
env -u M6R_ABROAD_DEV bash "${RUN}" --dry-run >"${TMP}/run.out" 2>&1; rc1=$?
DRY_RUN=1 bash "${RESTORE}" >"${TMP}/restore.out" 2>&1; rc2=$?
etc_after=$(ls -la /etc/resolver 2>&1; ls -la "/Library/Application Support/claude-abroad-m6r" 2>&1)
[ $rc1 = 0 ] && [ $rc2 = 0 ] && ok "h. 空跑 run.sh（自己编 abroad-dev）、restore.sh 退出码 0" || bad "h. 空跑退出码 run=${rc1} restore=${rc2}：$(tail -3 "${TMP}/run.out")"
[ "${etc_before}" = "${etc_after}" ] && ok "h. 空跑前后 /etc/resolver 和状态目录不变" || bad "h. 空跑前后变了"
grep -q '^CHANGE: install -m 644 .*/etc/resolver/claude.ai$' "${TMP}/run.out" && grep -q '^CONFIRM: ' "${TMP}/run.out" && ok "h. 空跑打印了写入、确认、删除的每一步" || bad "h. 空跑输出不全"
scripts/m6/resolver/check-commands.sh "${TMP}/run.out" "${TMP}/restore.out" >"${TMP}/cc.out" 2>&1 && ok "h. 命令核对通过" || bad "h. 命令核对：$(cat "${TMP}/cc.out")"
evil_case() { # $1 说明 $2 追加到 run 空跑输出的行
  cp "${TMP}/run.out" "${TMP}/evil.out"
  printf '%s\n' "$2" >>"${TMP}/evil.out"
  if scripts/m6/resolver/check-commands.sh "${TMP}/evil.out" "${TMP}/restore.out" >/dev/null 2>&1; then bad "h. 反例没拦下：$1"; else ok "h. 反例被拦下：$1"; fi
}
evil_case "pfctl -d" "CHANGE: pfctl -d"
evil_case "networksetup 改 DNS" "CHANGE: networksetup -setdnsservers Wi-Fi 8.8.8.8"
evil_case "结束 Claude" "CHANGE: pkill -x Claude"
evil_case "kill" "CHANGE: kill 123"
evil_case "launchctl" "CHANGE: launchctl kickstart -k system/com.apple.mDNSResponder"
evil_case "刷新 DNS 缓存" "CHANGE: dscacheutil -flushcache"
evil_case "锁别的域名" "CHANGE: install -m 644 /Library/Application Support/claude-abroad-m6r/lock.claude.ai /etc/resolver/example.com"
evil_case "删别的文件" "CHANGE: rm -f /etc/resolver/example.com"
evil_case "清单外的查询" "QUERY: curl -s http://example.com/"
evil_case "没有死人开关就写入" "CHANGE: install -m 644 /Library/Application Support/claude-abroad-m6r/lock.claude.ai /etc/resolver/claude.ai"
sed '/^CONFIRM: /d' "${TMP}/run.out" >"${TMP}/evil.out"
if scripts/m6/resolver/check-commands.sh "${TMP}/evil.out" "${TMP}/restore.out" >/dev/null 2>&1; then bad "h. 反例没拦下：没有 yes 确认就锁真实域名"; else ok "h. 反例被拦下：没有 yes 确认就锁真实域名"; fi
awk '/^CHANGE: rm -f \/etc\/resolver\/claude.com$/ { next } { print }' "${TMP}/run.out" >"${TMP}/evil.out"
if scripts/m6/resolver/check-commands.sh "${TMP}/evil.out" "${TMP}/restore.out" >/dev/null 2>&1; then bad "h. 反例没拦下：写了没删"; else ok "h. 反例被拦下：写了不删"; fi
mkdir -p "${TMP}/src"
cp scripts/m6/resolver/run.sh scripts/m6/resolver/restore.sh scripts/m6/resolver/lib.sh "${TMP}/src/"
for evil in 'pkill -x Claude' 'killall Claude' 'networksetup -setdnsservers Wi-Fi Empty' 'sudo rm -f /etc/hosts' 'rm -f "$X"' 'open -a Claude' 'echo x | tee /etc/resolver/claude.ai'; do
  cp scripts/m6/resolver/run.sh "${TMP}/src/run.sh"
  printf '%s\n' "${evil}" >>"${TMP}/src/run.sh"
  if M6R_CC_SRC_DIR="${TMP}/src" scripts/m6/resolver/check-commands.sh "${TMP}/run.out" "${TMP}/restore.out" >/dev/null 2>&1; then bad "h. 源码反例没拦下：${evil}"; else ok "h. 源码反例被拦下：${evil}"; fi
done

# ---- i. 证据和日志里没有用户名、主目录路径 ----
me=$(id -un)
leak=$(grep -rlE "${HOME}|/Users/${me}" "${TMP}"/*/out 2>/dev/null; grep -rlwF "${me}" "${TMP}"/*/out 2>/dev/null)
[ -z "${leak}" ] && ok "i. 证据和日志里没有用户名、主目录路径" || bad "i. 有泄露：${leak}"
grep -qE "${HOME}|/Users/${me}" "${TMP}/run.out" && bad "i. 空跑输出里有主目录路径" || ok "i. 空跑输出里没有主目录路径"
got=$(
  export M6R_TEST_DIR="${TMP}" DRY_RUN=0 SUDO_USER=alice
  . scripts/m6/resolver/lib.sh
  printf '/Users/alice/x 192.168.1.5 10.1.2.3 198.18.0.2 127.0.0.1 alice alice.example fe80::1\n' | m6r_scrub
)
[ "${got}" = '~/x <IPv4> <IPv4> 198.18.0.2 127.0.0.1 <user> <user>.example <IPv6>' ] && ok "i. 脱敏：主目录→~、局域网地址→<IPv4>、用户名（含出现在主机名里的）→<user>，锁地址和回环保留" || bad "i. 脱敏不对：${got}"

# ---- k. 基线出口和已登录程序的观察 ----
PROMPT='Claude Code：在你平常用的终端'
kcase() { # $1 名字 $2 回答 其余：环境变量；输出到 ${TMP}/k-<名字>.out，证据路径放进 KEV
  local name=$1 ans=$2 d; shift 2
  d=$(mkcase "k-${name}")
  printf '%s' "${ans}" | env M6R_TEST_DIR="$d" "$@" bash "${RUN}" >"${TMP}/k-${name}.out" 2>&1; KRC=$?
  KEV=$(ls "$d"/out/evidence-*.txt 2>/dev/null | head -1)
  KD=$d
}
kcase unreach "${ALL_ANS}" M6R_TEST_BASELINE=unreachable
[ $KRC = 0 ] && grep -q '^基线出口：到不了' "${KEV}" && [ "$(grep -c "${PROMPT}" "${TMP}/k-unreach.out")" = 3 ] && ! grep -q 'real-ip-ok' "${TMP}/k-unreach.out" &&
  ok "k. 基线“到不了”：记进结果文件，已登录程序的观察照常（基线 + 两种写法各一次），不要单独确认" || bad "k. 基线到不了不对：rc=$KRC $(grep 基线出口 "${KEV}")"
check_clean "$KD" "k（到不了）"
# 经代理或 TUN：每个场景要单独输入 exit-trusted（脚本分不出隧道的出口在哪里）。
kcase proxy "yes
no
no

no

" M6R_TEST_BASELINE=proxy
[ $KRC = 0 ] && grep -q '^基线出口：能到，经本机的代理或 TUN' "${KEV}" && ok "k. 基线“经代理或 TUN”：记进结果文件" || bad "k. 基线经代理不对：rc=$KRC $(grep 基线出口 "${KEV}")"
grep -q '此刻它会从这里出去：能到，经本机的代理或 TUN' "${TMP}/k-proxy.out" && grep -q '锁无效时：你按提示打开的 Claude' "${TMP}/k-proxy.out" && ok "k. 确认提示里写了锁无效时会真的连上去、此刻的出口" || bad "k. 确认提示里没有反方向的风险"
grep -q '脚本分不出这条隧道的出口在哪里；如果它的出口仍在国内（例如公司的 VPN），就等同直连' "${TMP}/k-proxy.out" && grep -q '还要单独输入 exit-trusted' "${TMP}/k-proxy.out" &&
  ok "k. 经代理时提示里写明“分不出隧道的出口在哪里，出口仍在国内就等同直连”，并说明要输入 exit-trusted" || bad "k. 经代理时没有写明出口的不确定"
grep -q "${PROMPT}" "${TMP}/k-proxy.out" && bad "k. 经代理、没有确认出口，却出现了请用户打开 Claude 的提示" || ok "k. 经代理、没有确认出口：不出现请用户打开已登录 Claude 的提示"
[ "$(grep -c '=跳过：出口未确认$' "${KEV}")" = 12 ] && ok "k. 跳过的 12 项（基线 + 两种写法，各 4 项）记“跳过：出口未确认”" || bad "k. 出口未确认的跳过记录不对：$(grep -c '=跳过：出口未确认' "${KEV}")"
check_clean "$KD" "k（经代理未确认）"
kcase proxyok "yes
exit-trusted
s
s
s
s
exit-trusted
n
n
n
s
tun
no

exit-trusted
n
n
n
s

" M6R_TEST_BASELINE=proxy
[ $KRC = 0 ] && [ "$(grep -c "${PROMPT}" "${TMP}/k-proxyok.out")" = 3 ] && [ "$(grep -c '：exit-trusted$' "${KEV}")" = 3 ] &&
  ok "k. 经代理：每个场景输入 exit-trusted 之后才出现已登录程序的观察" || bad "k. 确认出口后不对：rc=$KRC $(grep -c "${PROMPT}" "${TMP}/k-proxyok.out") $(grep -c '：exit-trusted$' "${KEV}")"
grep -q '^用户没有确认出口（写法=default 场景=tun）' "${KEV}" && [ "$(grep -c '^手动 写法=default 场景=tun .*=跳过：出口未确认$' "${KEV}")" = 4 ] &&
  ok "k. 切换场景后重新判断是经代理的，同样要再确认一次（没确认的场景记“跳过：出口未确认”）" || bad "k. 切换场景后没有重新要求确认：$(grep '场景=tun' "${KEV}" | head -3)"
check_clean "$KD" "k（经代理并确认）"
kcase proxyleak "yes
exit-trusted
s
s
s
s
no

exit-trusted
open-anyway
n
n
n
s

" M6R_TEST_BASELINE=proxy M6R_TEST_LOCK_LEAK=1
[ $KRC = 0 ] && [ "$(grep -c '=跳过：锁对系统解析无效，且出口未确认$' "${KEV}")" = 4 ] && [ "$(grep -c "${PROMPT}" "${TMP}/k-proxyleak.out")" = 2 ] &&
  grep -q '用户在锁无效时仍要做已登录程序的观察（出口：proxy）' "${KEV}" && [ "$(grep -c '仍要做就输入 open-anyway' "${TMP}/k-proxyleak.out")" = 1 ] &&
  ok "k. 锁无效、经代理：没确认出口时连 open-anyway 都不问、直接跳过；确认出口后才允许 open-anyway 继续" || bad "k. 锁无效经代理时不对：rc=$KRC $(grep -c '且出口未确认' "${KEV}") $(grep -c "${PROMPT}" "${TMP}/k-proxyleak.out")"
grep -q '^原始 netstat -rn：0/1→utun9(UGSc) 128.0/1→utun9(UGSc) 198.18/15内→utun9(UCS) default→en0(UGScg)' "${KEV}" &&
  grep -q '^原始 scutil --proxy：HTTPEnable=1 HTTPSEnable=1 SOCKSEnable=0' "${KEV}" &&
  grep -q '^原始 scutil --dns：resolver#1 search=有 ns=其他 reach=Reachable,Directly Reachable Address;resolver#2 domain=claude.ai ns=198.18/15 reach=Reachable,Transient Connection;' "${KEV}" &&
  grep -q '^原始 dscacheutil claude.ai：地址 1 个（198.18/15）' "${KEV}" &&
  grep -q '^原始 dig @锁地址（default 写法） claude.ai：有应答 1 条（其他），状态 NOERROR' "${KEV}" &&
  grep -q '^原始 curl https://claude.ai/：状态码 403，退出码 0，对端地址类别 198.18/15' "${KEV}" &&
  ok "k. 原始输出存档：scutil --proxy、netstat -rn、scutil --dns、dscacheutil、dig、curl 只取接口名、代理项、状态码、有无应答、地址类别" || bad "k. 原始输出存档不对：$(grep '^原始' "${KEV}" | head -8)"
check_clean "$KD" "k（锁无效经代理）"
kcase direct "yes
no



" M6R_TEST_BASELINE=direct
[ $KRC = 0 ] && grep -q '^基线出口：能到，而且是直连' "${KEV}" && ok "k. 基线“直连”：记进结果文件" || bad "k. 基线直连不对：rc=$KRC $(tail -3 "${TMP}/k-direct.out")"
grep -q '【已登录的账号会从本机 IP 访问 Anthropic】' "${TMP}/k-direct.out" && grep -q '【直连】：锁无效时，已登录的账号会从本机 IP 访问' "${TMP}/k-direct.out" && ok "k. 直连时确认提示和单独确认里醒目写出“已登录的账号会从本机 IP 访问”" || bad "k. 直连时没有醒目提示"
grep -q "${PROMPT}" "${TMP}/k-direct.out" && bad "k. 直连可达、没有单独确认，却出现了请用户打开 Claude 的提示" || ok "k. 直连可达、没有单独确认：不出现请用户打开已登录 Claude 的提示"
[ "$(grep -c '=跳过：直连可达' "${KEV}")" = 12 ] && ok "k. 跳过的 12 项（基线 + 两种写法，各 4 项）记“跳过：直连可达”" || bad "k. 跳过记录不对：$(grep -c '=跳过：直连可达' "${KEV}")"
grep -q '^判断 写法=default 场景=当前：锁对系统解析有效.*此刻出口：能到，而且是直连' "${KEV}" && ok "k. 每种写法记下“锁是否有效”和此刻的出口" || bad "k. 判断行不对：$(grep '^判断' "${KEV}")"
check_clean "$KD" "k（直连）"
kcase directok "yes
real-ip-ok
s
s
s
s
n
n
n
s

n
n
n
s

" M6R_TEST_BASELINE=direct
[ $KRC = 0 ] && [ "$(grep -c "${PROMPT}" "${TMP}/k-directok.out")" = 3 ] && grep -q '用户单独确认直连时也做已登录程序的观察：real-ip-ok' "${KEV}" &&
  ok "k. 直连可达、单独输入 real-ip-ok 后才出现已登录程序的观察" || bad "k. 单独确认后不对：rc=$KRC $(grep -c "${PROMPT}" "${TMP}/k-directok.out")"
check_clean "$KD" "k（直连并确认）"
kcase leak "yes
s
s
s
s




" M6R_TEST_BASELINE=unreachable M6R_TEST_LOCK_LEAK=1
[ $KRC = 0 ] && grep -q '锁对系统解析无效.*不建议再打开已登录的程序' "${TMP}/k-leak.out" && [ "$(grep -c "${PROMPT}" "${TMP}/k-leak.out")" = 1 ] &&
  [ "$(grep -c '=跳过：锁对系统解析无效$' "${KEV}")" = 8 ] && grep -q '^判断 写法=loopback 场景=当前：锁对系统解析无效' "${KEV}" &&
  ok "k. 不带凭据的几项显示锁无效：提示不建议再打开，后面的手动项默认跳过（只剩无锁基线那一次）" || bad "k. 锁无效时没跳过：rc=$KRC $(grep -c "${PROMPT}" "${TMP}/k-leak.out") $(grep -c '跳过：锁对' "${KEV}")"
check_clean "$KD" "k（锁无效）"
kcase leakdirect "yes
real-ip-ok
s
s
s
s


" M6R_TEST_BASELINE=direct M6R_TEST_LOCK_LEAK=1
[ $KRC = 0 ] && [ "$(grep -c "${PROMPT}" "${TMP}/k-leakdirect.out")" = 1 ] && [ "$(grep -c '=跳过：锁对系统解析无效，且直连可达' "${KEV}")" = 8 ] &&
  ! grep -q '仍要做就输入 open-anyway' "${TMP}/k-leakdirect.out" && ok "k. 锁无效且直连可达：即使确认过 real-ip-ok，也不请用户打开已登录的程序，不给 open-anyway 的机会" || bad "k. 锁无效且直连时没拦住：rc=$KRC $(grep -c "${PROMPT}" "${TMP}/k-leakdirect.out")"
check_clean "$KD" "k（锁无效且直连）"
out=$(M6R_TEST_BASELINE=direct bash "${RUN}" --dry-run </dev/null 2>&1); rc=$?
[ $rc != 0 ] && printf '%s' "$out" | grep -q 'M6R_TEST_BASELINE 只能配 M6R_TEST_DIR 用' && ok "k. 没有自测目录时 M6R_TEST_BASELINE 被拒绝" || bad "k. 测试变量离开自测目录没被拒绝：$out"
out=$(M6R_TEST_LOCK_LEAK=1 bash "${RESTORE}" 2>&1); rc=$?
[ $rc != 0 ] && printf '%s' "$out" | grep -q 'M6R_TEST_LOCK_LEAK 只能配' && ok "k. restore.sh 同样拒绝离开自测目录的测试变量" || bad "k. restore.sh 没拒绝：$out"
D=$(mkcase k-bad)
out=$(M6R_TEST_DIR="$D" M6R_TEST_BASELINE=maybe bash "${RUN}" </dev/null 2>&1); rc=$?
[ $rc != 0 ] && printf '%s' "$out" | grep -q '只能是 unreachable、proxy、direct' && ok "k. M6R_TEST_BASELINE 的值不认识时拒绝" || bad "k. 不认识的值没拒绝：$out"
grep -qF "sed -n 's/^\(M6R_TEST_[A-Za-z0-9_]*\)=.*/\1/p'" scripts/m6/resolver/lib.sh && grep -qF 'for v in ${M6R_TEST_VARS} M6R_TEST_DIR M6R_ABROAD_DEV' scripts/m6/resolver/lib.sh &&
  ok "k. 源码：以 root 运行时按前缀拒绝所有 M6R_TEST_* 变量（含 M6R_TEST_BASELINE、M6R_TEST_LOCK_LEAK；会话里不能用 root 实测）" || bad "k. 源码里 root 下没有按前缀拒绝"
grep -q '^CONFIRM-LOGIN: ' "${TMP}/run.out" && ok "k. 空跑按直连处理，打印了单独确认（命令核对要求已登录程序的提示都在它之后）" || bad "k. 空跑里没有单独确认"
sed '/^CONFIRM-LOGIN: /d' "${TMP}/run.out" >"${TMP}/evil.out"
if scripts/m6/resolver/check-commands.sh "${TMP}/evil.out" "${TMP}/restore.out" >/dev/null 2>&1; then bad "k. 反例没拦下：没有单独确认就请用户打开已登录的 Claude"; else ok "k. 反例被拦下：没有单独确认就请用户打开已登录的 Claude"; fi
grep -q '^CONFIRM-EXIT: ' "${TMP}/run.out" && ok "k. 空跑打印了出口确认（命令核对要求每组已登录程序的提示都在这个场景的出口确认之后）" || bad "k. 空跑里没有出口确认"
sed '/^CONFIRM-EXIT: /d' "${TMP}/run.out" >"${TMP}/evil.out"
if scripts/m6/resolver/check-commands.sh "${TMP}/evil.out" "${TMP}/restore.out" >/dev/null 2>&1; then bad "k. 反例没拦下：没有出口确认就请用户打开已登录的 Claude"; else ok "k. 反例被拦下：没有出口确认（exit-trusted）就请用户打开已登录的 Claude"; fi
awk '/^CONFIRM-EXIT: / && ++n == 2 { next } { print }' "${TMP}/run.out" >"${TMP}/evil.out"
if scripts/m6/resolver/check-commands.sh "${TMP}/evil.out" "${TMP}/restore.out" >/dev/null 2>&1; then bad "k. 反例没拦下：换了场景沿用上一个场景的出口确认"; else ok "k. 反例被拦下：换了场景沿用上一个场景的出口确认"; fi
# 原始输出存档里不能有地址（样例里故意带了局域网地址、网关、代理地址、对端地址）。
raws=$(cat "${TMP}"/k-*/out/evidence-*.txt 2>/dev/null | grep '^原始 ')
nraw=$(printf '%s\n' "${raws}" | grep -c '^原始 ')
if [ "${nraw}" -gt 50 ] && ! printf '%s\n' "${raws}" | grep -qE '[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+|<IPv4>|<IPv6>|[0-9a-fA-F]{1,4}::|::[0-9a-fA-F]{1,4}|corp\.example'; then
  ok "k. 原始输出存档（${nraw} 行）里没有任何 IP 地址、脱敏占位或别人的域名"
else
  bad "k. 原始输出存档里有地址：$(printf '%s\n' "${raws}" | grep -E '[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+|<IPv4>|<IPv6>|::|corp\.example' | head -3)"
fi
sed '/^NOTE: 基线出口/d' "${TMP}/run.out" >"${TMP}/evil.out"
if scripts/m6/resolver/check-commands.sh "${TMP}/evil.out" "${TMP}/restore.out" >/dev/null 2>&1; then bad "k. 反例没拦下：确认之前没判断基线出口"; else ok "k. 反例被拦下：确认之前没判断基线出口"; fi

# ---- j. 没有残留的死人开关进程 ----
i=0; while pgrep -f "m6r-deadman .*${TMP}" >/dev/null 2>&1 && [ $i -lt 60 ]; do sleep 0.5; i=$((i + 1)); done
pgrep -f "m6r-deadman .*${TMP}" >/dev/null 2>&1 && bad "j. 还有残留的死人开关进程" || ok "j. 没有残留的死人开关进程（取消后一秒内自己退出）"

[ $fails = 0 ] && echo "✓ 自测全部通过" || { echo "✗ 自测有 ${fails} 处不通过"; exit 1; }
