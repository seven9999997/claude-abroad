#!/usr/bin/env bash
# M3-8 的自测（不用 root、不改网络，状态目录指到临时目录；m3-plan v2.10 验收 c–g，v2.11 验收 l、m）：
#   a. 死人开关在状态目录原本不存在时也能启动、核对存活、到点执行（假的）restore；
#   b. m3_kill_pidfile 按 PID 文件结束进程；命令行不匹配的 PID 不动；
#   c. DRY_RUN=1 跑完 run.sh、restore.sh 退出码 0，前后用户环境不变；
#   d. 命令核对通过；反例各一条都被拦下；
#   f. --check-only 的拦截（旧状态文件、系统代理、没确认 M2 已通过）；
#   g/m. 判定函数喂样本：通过和不通过各有用例；
#   trap、护栏：同 M2。
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
[ "$(id -u)" != 0 ] || { echo "不要以 root 运行" >&2; exit 1; }
TMP=$(mktemp -d "${TMPDIR:-/tmp}/m3st.XXXXXX")
trap 'rm -rf "${TMP}" "${TMP}"/st-snap' EXIT
fails=0
ok() { echo "✓ $*"; }
bad() { echo "✗ $*"; fails=$((fails + 1)); }
expect() { # expect <期望的返回码> <说明> <命令…>
  local want=$1 what=$2 out rc; shift 2
  out=$("$@"); rc=$?
  [ "$rc" = "$want" ] && ok "${what}（${out}）" || bad "${what}：返回 ${rc}，期望 ${want}（${out}）"
}

# ---- a. 死人开关 ----
ST="${TMP}/st"
FAKE="${TMP}/fake-restore.sh"
printf '#!/bin/bash\necho "ran SUDO_USER=${SUDO_USER:-} TEST=${M3_TEST_STATE:-}" > "%s/marker"\n' "${TMP}" >"${FAKE}"
(
  export M3_TEST_STATE="${ST}" SUDO_USER=selftest-user DRY_RUN=0
  . scripts/m3/lib.sh
  m3_state_init
  m3_deadman_start 2 "${FAKE}"
  pid=$(m3_state_get deadman_pid)
  kill -0 "$pid" 2>/dev/null && echo "alive $pid"
) >"${TMP}/a.out" 2>&1
grep -q '^alive ' "${TMP}/a.out" && ok "a. 状态目录原本不存在时死人开关也能启动，PID 文件在、进程活着" || bad "a. 死人开关没起来：$(cat "${TMP}/a.out")"
sleep 3
if grep -q "ran SUDO_USER=selftest-user TEST=${ST}" "${TMP}/marker" 2>/dev/null; then ok "a. 到点执行了假的 restore，SUDO_USER 和自测目录显式传进去了"; else bad "a. 到点没有执行假的 restore：$(cat "${TMP}/marker" 2>/dev/null)"; fi

# ---- b. 按 PID 文件结束进程 ----
(
  export M3_TEST_STATE="${ST}" DRY_RUN=0
  . scripts/m3/lib.sh
  sleep 31 & p1=$!
  echo "$p1" >"${ST}/t1.pid"
  m3_kill_pidfile "${ST}/t1.pid" "sleep 31" 2>/dev/null
  sleep 0.5; kill -0 "$p1" 2>/dev/null && echo "still-alive" || echo "killed"
  sleep 32 & p2=$!
  echo "$p2" >"${ST}/t2.pid"
  m3_kill_pidfile "${ST}/t2.pid" "/usr/sbin/tcpdump -i " 2>/dev/null
  kill -0 "$p2" 2>/dev/null && echo "untouched" || echo "wrongly-killed"
  kill "$p2" 2>/dev/null
) >"${TMP}/b.out" 2>&1
grep -q '^killed' "${TMP}/b.out" && ok "b. 按 PID 文件结束了命令行匹配的进程" || bad "b. 没结束掉：$(cat "${TMP}/b.out")"
grep -q '^untouched' "${TMP}/b.out" && ok "b. 命令行不匹配的 PID 没有动" || bad "b. 动了不该动的进程"

# ---- c、d. 空跑：退出码、环境不变、命令核对 ----
env_snap() {
  echo "routes: $(netstat -rn -f inet 2>/dev/null | awk '{print $1, $4}' | sort | shasum -a 256 | cut -c1-16)"
  echo "proxy: $(scutil --proxy 2>/dev/null | shasum -a 256 | cut -c1-16)"
  echo "dns: $(scutil --dns 2>/dev/null | grep -E 'nameserver|search' | sort -u | shasum -a 256 | cut -c1-16)"
  echo "user: $(dscl . -read /Users/_abroadm3 >/dev/null 2>&1 && echo 有 || echo 无)"
  for p in /var/run/claude-abroad-m3 "/Library/Application Support/claude-abroad-m3" "/Library/Application Support/claude-abroad-m3-snap" \
    /Library/LaunchDaemons/com.claude-abroad.m3.abroadd.plist; do
    echo "exists ${p}: $([ -e "$p" ] && echo 有 || echo 无)"
  done
}
E0=$(env_snap)
DRY_RUN=1 scripts/m3/run.sh >"${TMP}/run.out" 2>"${TMP}/run.err"; rr=$?
DRY_RUN=1 scripts/m3/restore.sh >"${TMP}/restore.out" 2>"${TMP}/restore.err"; rs=$?
E1=$(env_snap)
[ $rr = 0 ] && [ $rs = 0 ] && ok "c. DRY_RUN=1 跑完 run.sh、restore.sh，退出码都是 0" || bad "c. 空跑退出码：run ${rr}、restore ${rs}：$(tail -3 "${TMP}/run.err")"
[ "$E0" = "$E1" ] && ok "c. 空跑前后用户环境一致（路由、代理、DNS、测试用户、状态和安装目录、LaunchDaemon）" || bad "c. 空跑前后环境不同：$(diff <(echo "$E0") <(echo "$E1"))"
if scripts/m3/check-commands.sh "${TMP}/run.out" "${TMP}/restore.out" >"${TMP}/cc.out" 2>&1; then ok "d. 命令核对通过（$(grep -c '^CHANGE' "${TMP}/run.out") + $(grep -c '^CHANGE' "${TMP}/restore.out") 条）"; else bad "d. 命令核对没通过：$(cat "${TMP}/cc.out")"; fi
# 反例：在真实的空跑输出里各插一条违规，核对必须失败并指出原因。
neg() { # neg <说明> <插入的行> <期望的报错片段> [restore]
  local f="${TMP}/neg.out" g="${TMP}/neg-r.out"
  cp "${TMP}/run.out" "$f"; cp "${TMP}/restore.out" "$g"
  if [ "${4:-}" = restore ]; then printf '%s\n' "$2" >>"$g"; else printf '%s\n' "$2" >>"$f"; fi
  if out=$(scripts/m3/check-commands.sh "$f" "$g" 2>&1); then bad "d. 反例没拦下：$1"
  elif printf '%s' "$out" | grep -qF -- "$3"; then ok "d. 反例被拦下：$1"
  else bad "d. 反例拦下了但原因不对：$1（$(printf '%s' "$out" | grep '✗' | head -2)）"; fi
}
neg "tcpdump 写文件" "CHANGE: /usr/sbin/tcpdump -i en0 -n -l -w /tmp/x.pcap port 53" "tcpdump 只能只读抓包"
neg "tcpdump 执行命令" "CHANGE: /usr/sbin/tcpdump -i en0 -n -l -z /bin/sh port 53" "tcpdump 只能只读抓包"
neg "以 root 启动 Chrome" "CHANGE: open -na Google Chrome --args --user-data-dir=/var/run/claude-abroad-m3/chrome --no-first-run --no-default-browser-check https://claude.ai/" "Chrome 必须以用户身份"
neg "pfctl -d" "CHANGE: /sbin/pfctl -d" "禁止的命令"
neg "按名字结束进程" "CHANGE: pkill -x tcpdump" "禁止的命令"
neg "改 DNS" "CHANGE: networksetup -setdnsservers Wi-Fi 1.1.1.1" "禁止的命令"
neg "清单外的命令" "CHANGE: rm -rf /var/run/claude-abroad-m2" "不在清单里"
# 顺序反例：死人开关之前多了一条改动。
cp "${TMP}/run.out" "${TMP}/neg2.out"
perl -0pi -e 's/(CHANGE: 死人开关)/CHANGE: mkdir -p \/var\/run\/claude-abroad-m3\/up\n$1/' "${TMP}/neg2.out"
# 先把输出存下来再找（pipefail 下，核对脚本的非零退出码会让整条管道判假）。
out=$(scripts/m3/check-commands.sh "${TMP}/neg2.out" "${TMP}/restore.out" 2>&1)
if printf '%s' "$out" | grep -q '死人开关之前有建状态目录以外的改动'; then ok "d. 反例被拦下：死人开关之前有别的改动"; else bad "d. 反例没拦下：死人开关之前有别的改动"; fi
# 顺序反例：restore 里先删用户再清 pf。
awk '/CHANGE: \/sbin\/pfctl -a .* -F all/{held=$0; next} {print} /CHANGE: dscl . -delete \/Groups/{print held}' "${TMP}/restore.out" >"${TMP}/neg3.out"
out=$(scripts/m3/check-commands.sh "${TMP}/run.out" "${TMP}/neg3.out" 2>&1)
if printf '%s' "$out" | grep -q 'restore.sh 的顺序不对'; then ok "d. 反例被拦下：restore 先删用户再清 pf"; else bad "d. 反例没拦下：restore 的顺序"; fi

# ---- f. --check-only 的拦截 ----
rm -rf "${ST}"; mkdir -p "${ST}/abroadd"; echo '{"version":1,"state":"ENABLED"}' >"${ST}/abroadd/state.json"
out=$(M3_TEST_STATE="${ST}" scripts/m3/run.sh --check-only 2>&1); rc=$?
[ $rc -ne 0 ] && printf '%s' "$out" | grep -q '上一次运行留下的 abroadd 状态文件还在' && ok "f. 旧的 abroadd 状态文件还在时拦下" || bad "f. 没拦下旧状态文件：$out"
rm -rf "${ST}"
out=$(M3_TEST_STATE="${ST}" M3_TEST_PROXY_FILE=internal/preflight/testdata/proxy-http.constructed.txt scripts/m3/run.sh --check-only 2>&1)
printf '%s' "$out" | grep -q '系统代理开着' && ok "f. 系统代理开着的样本被拦下" || bad "f. 系统代理没拦下"
out=$(M3_TEST_STATE="${ST}" M3_TEST_PROXY_FILE=internal/preflight/testdata/proxy-off.txt scripts/m3/run.sh --check-only 2>&1)
printf '%s' "$out" | grep -q '系统代理开着' && bad "f. 系统代理关着的样本被误拦" || ok "f. 系统代理关着的样本不拦"
printf '%s' "$out" | grep -q '没有确认 M2 的真机脚本已经通过' && ok "f. 没设 M3_M2_PASSED=1 时拦下" || bad "f. 没拦下 M2 未确认"
out=$(M3_TEST_STATE="${ST}" M3_M2_PASSED=1 M3_TEST_PROXY_FILE=internal/preflight/testdata/proxy-off.txt scripts/m3/run.sh --check-only 2>&1)
printf '%s' "$out" | grep -q '没有确认 M2' && bad "f. 设了 M3_M2_PASSED=1 仍报未确认" || ok "f. 设了 M3_M2_PASSED=1 后不再报这一条"

# ---- g、m. 判定函数 ----
. scripts/m3/judge.sh
LA="${TMP}/a.jsonl"; LB="${TMP}/b.jsonl"
printf '%s\n' '{"cmd":"connect","atyp":3,"host":"clients2.google.com","port":443,"result":"closed"}' \
  '{"cmd":"connect","atyp":3,"host":"m3-1a2b3c4d-a.m3.test","port":80,"result":"whoami"}' >"$LA"
printf '%s\n' '{"cmd":"connect","atyp":3,"host":"m3-1a2b3c4d-b.m3.test","port":80,"result":"whoami"}' >"$LB"
expect 0 "g. 标记只在这个出口的日志里：通过" judge_marker "$LA" "$LB" m3-1a2b3c4d-a
expect 1 "g. 标记在另一个出口的日志里：不通过" judge_marker "$LB" "$LA" m3-1a2b3c4d-a
expect 1 "g. 两个日志里都没有标记：不通过" judge_marker "$LA" "$LB" m3-ffffffff-a
expect 1 "g. 标记的前缀相同但不是本次的：不通过" judge_marker "$LA" "$LB" m3-1a2b3c4e-a
CAP="${TMP}/cap.txt"
printf '%s\n' 'tcpdump: verbose output suppressed' 'listening on en0, link-type EN10MB' >"$CAP"
[ "$(count_packets "$CAP")" = 0 ] && ok "g. 抓包只有表头：0 个包" || bad "g. 抓包表头被算成包"
printf '%s\n' '17:00:00.123456 IP 192.0.2.5.50000 > 160.79.104.10.443: tcp 0' >>"$CAP"
PK=$(count_packets "$CAP")
[ "$PK" = 1 ] && ok "g. 抓到 1 个发往 160.79.104.0/21 的包：计 1" || bad "g. 包数不对：$PK"
expect 1 "g. 抓到 1 个发往 Anthropic 段的包：S4 判不通过" judge_pinned_case 3 5 "$PK" 0
expect 0 "g. 四条都成立：S4 判通过" judge_pinned_case 3 5 0 0
# M4-8：S4 的“主代理一侧有新连接”取 status.opens.main.tcp（主代理出口自己的计数，分流出口的连接不算）。
ST_A='{"opens":{"main":{"tcp":4,"udp":0},"wg-a":{"tcp":9}},"stack":{"tcp_proxied":13}}'
ST_B='{"opens":{"main":{"tcp":4,"udp":0},"wg-a":{"tcp":20}},"stack":{"tcp_proxied":24}}'
ST_C='{"opens":{"main":{"tcp":7,"udp":0},"wg-a":{"tcp":20}},"stack":{"tcp_proxied":27}}'
expect 1 "m. 只有分流出口 wg-a 有新连接：S4 判不通过（主代理出口没有新连接）" judge_pinned_case 3 "$(delta "$(jget "$ST_A" opens.main.tcp)" "$(jget "$ST_B" opens.main.tcp)")" 0 0
expect 0 "m. 主代理出口有新连接：S4 判通过" judge_pinned_case 3 "$(delta "$(jget "$ST_B" opens.main.tcp)" "$(jget "$ST_C" opens.main.tcp)")" 0 0
expect 1 "g. 置顶计数没增长：S4 判不通过" judge_pinned_case 0 5 0 0
expect 1 "g. 主代理没有新连接：S4 判不通过" judge_pinned_case 3 0 0 0
DNSF="${TMP}/dns.txt"
printf '%s\n' 'listening on en0' \
  '17:00:01.000001 IP 192.0.2.5.5353 > 192.0.2.1.53: 1234+ A? www.apple.com. (31)' \
  '17:00:01.000002 IP 192.0.2.5.5353 > 192.0.2.1.53: 1235+ A? notclaude.ai. (30)' \
  '17:00:01.000003 IP 192.0.2.1.53 > 192.0.2.5.5353: 1234 1/0/0 A 192.0.2.80 (47)' >"$DNSF"
[ "$(count_dns_queries "$DNSF")" = 0 ] && ok "g. 没有对 claude/anthropic 的查询（notclaude.ai 不算）：0" || bad "g. DNS 查询误计：$(count_dns_queries "$DNSF")"
printf '%s\n' '17:00:02.000001 IP 192.0.2.5.5353 > 192.0.2.1.53: 77+ AAAA? API.Anthropic.com. (35)' \
  '17:00:02.000002 IP 192.0.2.5.5353 > 192.0.2.1.53: 78+ A? claude.ai. (27)' >>"$DNSF"
DQ=$(count_dns_queries "$DNSF")
[ "$DQ" = 2 ] && ok "g. 对 claude.ai、api.anthropic.com（大小写不敏感）的查询：2" || bad "g. DNS 查询数不对：$DQ"
expect 1 "g. 有相关 DNS 查询：S4 判不通过" judge_pinned_case 3 5 0 "$DQ"
expect 0 "m. 裸 IP 的 443：直连没增长、port443_timeout 增长：通过" judge_rawip443 0 1
expect 1 "m. 裸 IP 的 443：直连增长了：不通过" judge_rawip443 1 1
expect 1 "m. 裸 IP 的 443：port443_timeout 没增长：不通过" judge_rawip443 0 0
expect 1 "m. S3 那种情况（直连没增长）：不通过" judge_direct_domain 0
expect 0 "m. S3：直连增长：通过" judge_direct_domain 1
expect 0 "g. 停用后：普通域名直连、claude.ai 不通、没包：通过" judge_offline 1 0 0
expect 1 "g. 停用后：claude.ai 经直连发出：不通过" judge_offline 1 1 0
expect 1 "g. 停用后：抓到发往 Anthropic 段的包：不通过" judge_offline 1 0 1
expect 1 "g. 停用后：普通域名没通：不通过" judge_offline 0 0 0
expect 0 "g. 批量 300 次全部成功：通过" judge_batch '{"count":300,"ok":300,"dial_fail":0,"handshake_fail":0}'
expect 2 "g. 拨号都成功但握手全部失败：跳过（目标不可达）" judge_batch '{"count":300,"ok":0,"dial_fail":0,"handshake_fail":300}'
expect 1 "g. 有 1 次握手失败（在途 fd 被冲掉）：不通过" judge_batch '{"count":300,"ok":299,"dial_fail":0,"handshake_fail":1}'
expect 1 "g. 有拨号失败：不通过" judge_batch '{"count":300,"ok":298,"dial_fail":2,"handshake_fail":0}'
expect 1 "g. 输出不是 JSON：不通过" judge_batch 'abroad-dev: 连不上直连拨号模块'
expect 0 "g. 自愈：stale_config 和直连都增长：通过" judge_selfheal 1 1
expect 1 "g. 自愈：直连没增长：不通过" judge_selfheal 1 0
[ "$(jget '{"stack":{"reasons":{"pinned":7}}}' stack.reasons.pinned)" = 7 ] && ok "g. jget 取嵌套的数" || bad "g. jget 取不到嵌套的数"
[ "$(jget '{}' stack.direct_ok)" = 0 ] && [ "$(delta '' 5)" = 5 ] && ok "g. 取不到时按 0" || bad "g. 取不到时没按 0"

# ---- trap：中途退出时自动还原 ----
FAKE2="${TMP}/fake-restore2.sh"
printf '#!/bin/bash\necho ran >> "%s/marker2"\n' "${TMP}" >"${FAKE2}"
run_case() {
  rm -f "${TMP}/marker2"
  ( export M3_TEST_STATE="${ST}" DRY_RUN=0; . scripts/m3/lib.sh; m3_arm_restore_trap "${FAKE2}"; M3_RESTORED=$1; exit "$2" ) >/dev/null 2>&1
  [ -f "${TMP}/marker2" ] && echo ran || echo not-ran
}
[ "$(run_case 0 1)" = ran ] && ok "trap. 中途非零退出时执行了还原" || bad "trap. 非零退出时没还原"
[ "$(run_case 0 0)" = not-ran ] && ok "trap. 正常退出时不执行" || bad "trap. 正常退出时也执行了"
[ "$(run_case 1 1)" = not-ran ] && ok "trap. 已经还原过（M3_RESTORED=1）时不再执行" || bad "trap. 重复执行了还原"

# ---- 护栏 ----
out=$(M3_TEST_STATE="${ST}" scripts/m3/restore.sh 2>&1); rc=$?
[ $rc -ne 0 ] && printf '%s' "$out" | grep -q '只能配 DRY_RUN=1' && ok "护栏：自测目录不配空跑时 restore.sh 拒绝执行" || bad "护栏没起作用：$out"
out=$(M3_TEST_STATE="${ST}" scripts/m3/run.sh 2>&1); rc=$?
[ $rc -ne 0 ] && printf '%s' "$out" | grep -q '只能配 DRY_RUN=1' && ok "护栏：自测目录不配空跑时 run.sh 拒绝执行" || bad "护栏没起作用（run.sh）：$out"

echo
# m4-plan v2.5 建议改 3：abroadd 的 LaunchDaemon 模板带 --dev-skip-region（这份脚本不改时区和语言）；启用之前先应用主代理。
if grep -q '<string>--dev-skip-region</string>' scripts/m3/launchd.plist.tmpl; then ok "plist 模板带 --dev-skip-region"; else bad "plist 模板没带 --dev-skip-region"; fi
if awk '/api POST \/v1\/main-proxy\/apply/{a=NR} /api POST \/v1\/enable/{e=NR} END{exit !(a && e && a<e)}' scripts/m3/run.sh; then ok "启用之前先应用主代理"; else bad "启用之前没有应用主代理"; fi
# ---- M6-1：二进制认识要传的开发参数（正式构建在改动之前被拦下的反例）----
devout=$(scripts/lib/devflags-selftest.sh m3 --dev-skip-claude-kill --dev-skip-region 2>&1)
while IFS= read -r l; do case "$l" in ✓*) ok "M6-1 ${l#✓ }" ;; ✗*) bad "M6-1 ${l#✗ }" ;; esac; done <<<"$devout"

[ $fails = 0 ] && echo "✓ 自测全部通过" || echo "✗ 自测有 $fails 处没过"
exit $((fails > 0))
