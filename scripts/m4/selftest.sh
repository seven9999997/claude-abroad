#!/usr/bin/env bash
# M4-9 的自测（不用 root、不改网络和地区，状态目录指到临时目录；m4-plan v2.13 验收 a–h，v2.14 验收 m–o）：
#   a. 死人开关在状态目录原本不存在时也能启动、核对存活、到点执行（假的）restore；
#   b. m4_kill_pidfile 按 PID 文件结束进程；命令行不匹配的 PID 不动（假 Claude 进程按临时目录路径核对）；
#   c. DRY_RUN=1 跑完 run.sh、restore.sh 退出码 0，前后用户环境不变（含时区、自动时区、语言、地区格式的读回值）；
#   d. 命令核对通过；反例各一条都被拦下；
#   e. 地区快照与还原（假的读写命令）：原来没有语言键、自动时区原来开着、还原后读回不一致、快照缺失；
#   f. 默认、M4_CHANGE_LANGUAGE=1、M4_REAL_KILL=1、M4_TZ 四份空跑，差异与预期一致；
#   g/m. 判定函数喂样本：每步通过、不通过、跳过；三种泄漏的检出与恢复；T4 断开后的四种情况；真实 IP 命中的反例；
#   h. --check-only 的拦截；
#   n. 只读识别子命令认假 Claude 进程的路径；
#   trap、护栏：同 M3。
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
[ "$(id -u)" != 0 ] || { echo "不要以 root 运行" >&2; exit 1; }
TMP=$(mktemp -d "${TMPDIR:-/tmp}/m4st.XXXXXX")
trap 'rm -rf "${TMP}" "${TMP}"/st-snap' EXIT
fails=0
ok() { echo "✓ $*"; }
bad() { echo "✗ $*"; fails=$((fails + 1)); }
expect() { # expect <期望的返回码> <说明> <命令…>
  local want=$1 what=$2 out rc; shift 2
  out=$("$@"); rc=$?
  [ "$rc" = "$want" ] && ok "${what}（${out}）" || bad "${what}：返回 ${rc}，期望 ${want}（${out}）"
}
ST="${TMP}/st"

# ---- a. 死人开关 ----
FAKE="${TMP}/fake-restore.sh"
printf '#!/bin/bash\necho "ran SUDO_USER=${SUDO_USER:-} TEST=${M4_TEST_STATE:-}" > "%s/marker"\n' "${TMP}" >"${FAKE}"
(
  export M4_TEST_STATE="${ST}" SUDO_USER=selftest-user DRY_RUN=0
  . scripts/m4/lib.sh
  m4_state_init
  m4_deadman_start 2 "${FAKE}"
  pid=$(m4_state_get deadman_pid)
  kill -0 "$pid" 2>/dev/null && echo "alive $pid"
) >"${TMP}/a.out" 2>&1
grep -q '^alive ' "${TMP}/a.out" && ok "a. 状态目录原本不存在时死人开关也能启动" || bad "a. 死人开关没起来：$(cat "${TMP}/a.out")"
sleep 3
grep -q "ran SUDO_USER=selftest-user TEST=${ST}" "${TMP}/marker" 2>/dev/null && ok "a. 到点执行了假的 restore，SUDO_USER 和自测目录显式传进去了" || bad "a. 到点没有执行假的 restore"

# ---- b. 按 PID 文件结束进程（含假 Claude 进程：/bin/sleep 的副本，按临时目录路径核对）----
(
  export M4_TEST_STATE="${ST}" DRY_RUN=0
  . scripts/m4/lib.sh
  mkdir -p "${M4_FAKE_DIR}" && cp /bin/sleep "${M4_FAKE_BIN}"
  "${M4_FAKE_BIN}" 900 & p1=$!
  echo "$p1" >"${ST}/fake.pid"
  m4_kill_pidfile "${ST}/fake.pid" "${M4_FAKE_BIN} 900" 2>/dev/null
  sleep 0.5; kill -0 "$p1" 2>/dev/null && echo "still-alive" || echo "killed"
  sleep 33 & p2=$!
  echo "$p2" >"${ST}/fake.pid"
  m4_kill_pidfile "${ST}/fake.pid" "${M4_FAKE_BIN} 900" 2>/dev/null
  kill -0 "$p2" 2>/dev/null && echo "untouched" || echo "wrongly-killed"
  kill "$p2" 2>/dev/null
) >"${TMP}/b.out" 2>&1
grep -q '^killed' "${TMP}/b.out" && ok "b. 按 PID 文件结束了假 Claude 进程（命令行里有临时目录的路径）" || bad "b. 没结束掉：$(cat "${TMP}/b.out")"
grep -q '^untouched' "${TMP}/b.out" && ok "b. 命令行里没有临时目录路径的 PID 没有动" || bad "b. 动了不该动的进程"
rm -rf "${ST}"

# ---- n. 只读识别子命令认假 Claude 进程的路径 ----
go build -o "${TMP}/abroad-dev" ./cmd/abroad-dev 2>"${TMP}/build.err" || bad "n. 编译 abroad-dev 失败：$(cat "${TMP}/build.err")"
. scripts/m4/judge.sh
if [ -x "${TMP}/abroad-dev" ]; then
  v=$("${TMP}/abroad-dev" claude-facts --path /var/run/claude-abroad-m4/m4fake/.local/share/claude/versions/0.0.0-m4fake/claude)
  expect 0 "n. 识别子命令把假 Claude 进程的路径认作 Claude Code（依据安装位置）" judge_fake_claude "$v"
  v=$("${TMP}/abroad-dev" claude-facts --path /tmp/claude)
  expect 1 "n. 只是叫 claude 的文件不算" judge_fake_claude "$v"
fi

# ---- c、d、f. 空跑：退出码、环境不变、命令核对、四份差异 ----
env_snap() {
  echo "routes: $(netstat -rn -f inet 2>/dev/null | awk '{print $1, $4}' | sort | shasum -a 256 | cut -c1-16)"
  echo "proxy: $(scutil --proxy 2>/dev/null | shasum -a 256 | cut -c1-16)"
  echo "dns: $(scutil --dns 2>/dev/null | grep -E 'nameserver|search' | sort -u | shasum -a 256 | cut -c1-16)"
  echo "tz: $(readlink /etc/localtime 2>/dev/null)"
  echo "auto: $(defaults read /Library/Preferences/com.apple.timezone.auto Active 2>&1 | head -1)"
  echo "langs: $(defaults read -g AppleLanguages 2>&1 | tr -d ' \n')"
  echo "locale: $(defaults read -g AppleLocale 2>&1)"
  echo "user: $(dscl . -read /Users/_abroadm4 >/dev/null 2>&1 && echo 有 || echo 无)"
  for p in /var/run/claude-abroad-m4 "/Library/Application Support/claude-abroad-m4" "/Library/Application Support/claude-abroad-m4-snap" \
    /Library/LaunchDaemons/com.claude-abroad.m4.abroadd.plist; do
    echo "exists ${p}: $([ -e "$p" ] && echo 有 || echo 无)"
  done
}
echo x >"${TMP}/c.conf"
dry() { # dry <名字> [环境变量…]：一份 run.sh 和 restore.sh 的空跑
  env DRY_RUN=1 M4_WG_CONF="${TMP}/c.conf" M4_OVPN_CONF="${TMP}/c.conf" M4_SUB="${TMP}/c.conf" "${@:2}" scripts/m4/run.sh >"${TMP}/run-$1.out" 2>"${TMP}/run-$1.err"
  echo $?
}
E0=$(env_snap)
rr=$(dry default)
DRY_RUN=1 scripts/m4/restore.sh >"${TMP}/restore.out" 2>"${TMP}/restore.err"; rs=$?
E1=$(env_snap)
[ "$rr" = 0 ] && [ $rs = 0 ] && ok "c. DRY_RUN=1 跑完 run.sh、restore.sh，退出码都是 0" || bad "c. 空跑退出码：run ${rr}、restore ${rs}：$(tail -3 "${TMP}/run-default.err")"
[ "$E0" = "$E1" ] && ok "c. 空跑前后用户环境一致（路由、代理、DNS、时区、自动时区、语言、地区格式、测试用户、目录）" || bad "c. 空跑前后环境不同：$(diff <(echo "$E0") <(echo "$E1"))"
if scripts/m4/check-commands.sh "${TMP}/run-default.out" "${TMP}/restore.out" >"${TMP}/cc.out" 2>&1; then ok "d. 命令核对通过（$(grep -c '^CHANGE' "${TMP}/run-default.out") + $(grep -c '^CHANGE' "${TMP}/restore.out") 条）"; else bad "d. 命令核对没通过：$(cat "${TMP}/cc.out")"; fi
neg() { # neg <说明> <插入的行> <期望的报错片段> [restore]
  local f="${TMP}/neg.out" g="${TMP}/neg-r.out" out
  cp "${TMP}/run-default.out" "$f"; cp "${TMP}/restore.out" "$g"
  if [ "${4:-}" = restore ]; then printf '%s\n' "$2" >>"$g"; else printf '%s\n' "$2" >>"$f"; fi
  if out=$(scripts/m4/check-commands.sh "$f" "$g" 2>&1); then bad "d. 反例没拦下：$1"
  elif printf '%s' "$out" | grep -qF -- "$3"; then ok "d. 反例被拦下：$1"
  else bad "d. 反例拦下了但原因不对：$1（$(printf '%s' "$out" | grep '✗' | head -2)）"; fi
}
neg "tcpdump 写文件" "CHANGE: /usr/sbin/tcpdump -i en0 -n -l -q -w /tmp/x.pcap port 53" "tcpdump 只能只读抓包"
neg "pfctl -d" "CHANGE: /sbin/pfctl -d" "禁止的命令"
neg "按名字结束进程" "CHANGE: pkill -x claude" "禁止的命令"
neg "改 DNS" "CHANGE: networksetup -setdnsservers Wi-Fi 1.1.1.1" "禁止的命令"
neg "清单外的命令" "CHANGE: rm -rf /var/run/claude-abroad-m3" "不在清单里"
neg "脚本自己改语言" "CHANGE: /bin/launchctl asuser 501 /usr/bin/sudo -u u /usr/bin/defaults write -g AppleLanguages -array en-US" "脚本不能自己改语言"
neg "向别的进程发信号" "CHANGE: kill -TERM 4242" "只能向本脚本记下的 PID 文件发信号"
neg "启动真实的 claude" "USER: env HTTPS_PROXY=http://127.0.0.1:9 claude -p ping" "启动了真实的 claude"
neg "启动别处的 claude" "CHANGE: sudo -u u env HTTPS_PROXY=http://127.0.0.1:9 /Users/u/.local/bin/claude 900" "启动了真实的 claude"
# 成对：打开系统代理之后删掉关闭那一行。
grep -v -E '^CHANGE: networksetup -setsecurewebproxystate Wi-Fi off$|^CHANGE: networksetup -setsecurewebproxystate [^<].* off$' "${TMP}/run-default.out" >"${TMP}/neg-p.out"
out=$(scripts/m4/check-commands.sh "${TMP}/neg-p.out" "${TMP}/restore.out" 2>&1)
printf '%s' "$out" | grep -q '打开了系统代理却没有成对的关闭' && ok "d. 反例被拦下：打开系统代理却没有关闭" || bad "d. 反例没拦下：系统代理不成对"
grep -v -E '^CHANGE: networksetup -setv6off [^<]' "${TMP}/run-default.out" >"${TMP}/neg-6.out"
out=$(scripts/m4/check-commands.sh "${TMP}/neg-6.out" "${TMP}/restore.out" 2>&1)
printf '%s' "$out" | grep -q '打开了 IPv6 却没有成对的关闭' && ok "d. 反例被拦下：打开 IPv6 却没有关闭" || bad "d. 反例没拦下：IPv6 不成对"
grep -v -E 'setsecurewebproxystate .* off$' "${TMP}/restore.out" >"${TMP}/neg-r2.out"
out=$(scripts/m4/check-commands.sh "${TMP}/run-default.out" "${TMP}/neg-r2.out" 2>&1)
printf '%s' "$out" | grep -q 'restore 里没有关闭系统代理的兜底' && ok "d. 反例被拦下：restore 没有系统代理的兜底" || bad "d. 反例没拦下：restore 缺兜底"
cp "${TMP}/run-default.out" "${TMP}/neg2.out"
perl -0pi -e 's/(CHANGE: 死人开关)/CHANGE: mkdir -p \/var\/run\/claude-abroad-m4\/vm\n$1/' "${TMP}/neg2.out"
out=$(scripts/m4/check-commands.sh "${TMP}/neg2.out" "${TMP}/restore.out" 2>&1)
printf '%s' "$out" | grep -q '死人开关之前有建状态目录以外的改动' && ok "d. 反例被拦下：死人开关之前有别的改动" || bad "d. 反例没拦下：死人开关之前有别的改动"
awk '/CHANGE: \/usr\/sbin\/systemsetup -settimezone/{held=$0; next} {print} /CHANGE: \/sbin\/pfctl -a .* -F all/{print held}' "${TMP}/restore.out" >"${TMP}/neg3.out"
out=$(scripts/m4/check-commands.sh "${TMP}/run-default.out" "${TMP}/neg3.out" 2>&1)
printf '%s' "$out" | grep -q 'restore.sh 的顺序不对' && ok "d. 反例被拦下：restore 先清 pf 再还原地区" || bad "d. 反例没拦下：restore 的顺序"
# f. 开关：改语言、真的结束 Claude、显式时区。
[ "$(dry lang M4_CHANGE_LANGUAGE=1 M4_LANG=en-US)" = 0 ] && scripts/m4/check-commands.sh "${TMP}/run-lang.out" "${TMP}/restore.out" lang >"${TMP}/cc-lang.out" 2>&1 &&
  ok "f. M4_CHANGE_LANGUAGE=1 的空跑：命令核对通过，目标里有语言" || bad "f. 改语言的空跑：$(grep '✗' "${TMP}/cc-lang.out")"
[ "$(dry kill M4_REAL_KILL=1)" = 0 ] && scripts/m4/check-commands.sh "${TMP}/run-kill.out" "${TMP}/restore.out" kill >"${TMP}/cc-kill.out" 2>&1 &&
  ok "f. M4_REAL_KILL=1 的空跑：命令核对通过，abroadd 不带跳过结束 Claude 的参数" || bad "f. 结束 Claude 的空跑：$(grep '✗' "${TMP}/cc-kill.out")"
[ "$(dry tz M4_TZ=America/New_York)" = 0 ] && scripts/m4/check-commands.sh "${TMP}/run-tz.out" "${TMP}/restore.out" tz >"${TMP}/cc-tz.out" 2>&1 &&
  ok "f. M4_TZ 的空跑：用了覆盖值" || bad "f. 显式时区的空跑：$(grep '✗' "${TMP}/cc-tz.out")"
strip() { grep -E '^(CHANGE|STEP|ABROADD|API|USER|QUERY):' "$1" | sed -E 's/[0-9]{8}-[0-9]{6}/<时间>/g; s/abroadd\.bak-[0-9-]+/abroadd.bak-<时间>/'; }
d1=$(diff <(strip "${TMP}/run-default.out") <(strip "${TMP}/run-lang.out") | grep -E '^[<>]' | sed 's/^[<>] //' | sort -u)
[ "$(printf '%s\n' "$d1" | grep -c .)" = 2 ] && printf '%s' "$d1" | grep -q '语言和地区格式保持现在的' && printf '%s' "$d1" | grep -q '、语言 en-US' &&
  ok "f. 默认与改语言两份空跑只差“锁定手动目标”那一行" || bad "f. 改语言的差异不对：$(printf '%s' "$d1" | tr '\n' '|')"
d2=$(diff <(strip "${TMP}/run-default.out") <(strip "${TMP}/run-kill.out") | grep -E '^[<>]' | sed 's/^[<>] //' | sort -u)
[ "$(printf '%s\n' "$d2" | grep -c .)" = 2 ] && printf '%s' "$d2" | grep -q '^ABROADD: .*--dev-skip-claude-kill$' &&
  ok "f. 默认与 M4_REAL_KILL=1 两份空跑只差 abroadd 的跳过参数" || bad "f. 结束 Claude 的差异不对：$(printf '%s' "$d2" | tr '\n' '|')"
for f in default lang kill tz; do
  grep -qE '^(CHANGE|USER): .*(^|[ /])claude( |$)' "${TMP}/run-$f.out" && ! grep -E '^(CHANGE|USER): ' "${TMP}/run-$f.out" | grep -E '(^|[ /])claude( |$)' | grep -vqE 'm4fake|claude-(abroad|facts)' &&
    true
done
grep -E '^(CHANGE|USER): ' "${TMP}/run-default.out" | grep -E '(^|[ /])claude( |$)' | grep -vE 'claude-(abroad|facts)' | grep -vq 'm4fake' &&
  bad "n. 默认的空跑里有启动真实 claude 的命令" || ok "n. 默认的空跑里没有任何启动真实 claude 的命令（只有 m4fake 下的副本）"

# ---- e. 地区快照与还原（假的 systemsetup、defaults、launchctl，写进临时目录，不碰本机）----
FK="${TMP}/fakebin"; DB="${TMP}/regiondb"
mkdir -p "$FK" "$DB"
cat >"$FK/systemsetup" <<EOF
#!/bin/bash
case "\$1" in
  -gettimezone) echo "Time Zone: \$(cat "$DB/tz")" ;;
  -settimezone) echo "\$2" >"$DB/tz" ;;
esac
EOF
cat >"$FK/defaults" <<EOF
#!/bin/bash
# 域：-g 是用户的，.GlobalPreferences 是系统的，timezone.auto 是自动时区。FAKE_STUCK 里的键写不进去（模拟读回不一致）。
op=\$1; dom=\$2; key=\$3; shift 3
case "\$dom" in -g) f="$DB/user_\$key" ;; */.GlobalPreferences) f="$DB/sys_\$key" ;; *timezone.auto) f="$DB/auto_\$key" ;; *) exit 2 ;; esac
case "\$op" in
  read) [ -f "\$f" ] || { echo "does not exist" >&2; exit 1; }; cat "\$f" ;;
  delete) [ -f "\$f" ] || exit 1; rm -f "\$f" ;;
  write)
    [ "\${FAKE_STUCK:-}" = "\$key" ] && exit 0
    case "\$1" in
      -bool) [ "\$2" = true ] && echo 1 >"\$f" || echo 0 >"\$f" ;;
      -string) echo "\$2" >"\$f" ;;
      -array) shift; { echo "("; for x in "\$@"; do echo "    \"\$x\","; done; echo ")"; } | sed '\$!N;s/,\n)/\n)/;P;D' >"\$f" ;;
    esac ;;
esac
EOF
cat >"$FK/launchctl" <<'EOF'
#!/bin/bash
# launchctl asuser <UID> /usr/bin/sudo -u <用户> <defaults> …：去掉前 5 个参数执行后面的命令。
[ "$1" = asuser ] || exit 2
shift 5
exec "$@"
EOF
chmod +x "$FK"/*
seed() { # seed <tz> <auto|none> <用户语言|none> <地区格式|none> <系统语言|none>：设“启用前”的值
  rm -f "$DB"/*
  echo "$1" >"$DB/tz"
  [ "$2" = none ] || echo "$2" >"$DB/auto_Active"
  [ "$3" = none ] || "$FK/defaults" write -g AppleLanguages -array $(echo "$3" | tr ',' ' ')
  [ "$4" = none ] || echo "$4" >"$DB/user_AppleLocale"
  [ "$5" = none ] || "$FK/defaults" write /Library/Preferences/.GlobalPreferences AppleLanguages -array $(echo "$5" | tr ',' ' ')
}
abroadd_like() { # 模拟 abroadd 启用后改了地区
  echo America/Los_Angeles >"$DB/tz"; echo 0 >"$DB/auto_Active"
  "$FK/defaults" write -g AppleLanguages -array en-US zh-Hans-CN
  echo en_US >"$DB/user_AppleLocale"
  "$FK/defaults" write /Library/Preferences/.GlobalPreferences AppleLanguages -array en-US
}
rcase() { # rcase <说明> <期望返回码> <期望输出片段或空> <snapshot 前的 seed 参数…> ——拍快照、模拟改动、还原
  local what=$1 want=$2 frag=$3 out rc; shift 3
  seed "$@"
  out=$(
    export M4_TEST_STATE="${ST}" DRY_RUN=0 M4_CMD_SYSTEMSETUP="$FK/systemsetup" M4_CMD_DEFAULTS="$FK/defaults" M4_CMD_LAUNCHCTL="$FK/launchctl"
    . scripts/m4/lib.sh; . scripts/m4/region.sh
    mkdir -p "${ST}/snap"
    rg_snapshot "${ST}/snap/region" u 501 || echo "snapshot-failed"
    cp "${ST}/snap/region" "${TMP}/region.snap"
    abroadd_like
    [ "${NO_SNAP:-0}" = 1 ] && rm -f "${ST}/snap/region"
    r=$(rg_restore "${ST}/snap/region" 2>/dev/null); echo "rc=$? ${r}"
  )
  rc=$(printf '%s' "$out" | sed -n 's/.*rc=\([0-9]*\).*/\1/p' | tail -1)
  if [ "$rc" = "$want" ] && { [ -z "$frag" ] || printf '%s' "$out" | grep -q -- "$frag"; }; then ok "e. ${what}（返回 ${rc}）"; else bad "e. ${what}：返回 ${rc:-?}，期望 ${want}（$(printf '%s' "$out" | tr '\n' ' ')）"; fi
}
rcase "原来没有用户的语言键：还原时删掉 abroadd 写的键，读回一致" 0 "" Asia/Shanghai 0 none zh_CN zh-Hans-CN
[ ! -f "$DB/user_AppleLanguages" ] && ok "e. 用户的语言键已删除（回到“原来没有”）" || bad "e. 用户的语言键没删"
rcase "自动时区原来开着：还原成开，读回一致" 0 "" Asia/Shanghai 1 zh-Hans-CN,en-US zh_CN none
[ "$(cat "$DB/auto_Active")" = 1 ] && [ ! -f "$DB/sys_AppleLanguages" ] && [ "$(cat "$DB/tz")" = Asia/Shanghai ] && ok "e. 自动时区回到开、时区回到原值、系统语言键回到没有" || bad "e. 还原后的值不对"
grep -q '^user_langs=zh-Hans-CN,en-US$' "${TMP}/region.snap" && ok "e. 快照把数组记成逗号分隔（zh-Hans-CN,en-US）" || bad "e. 快照的数组格式不对：$(cat "${TMP}/region.snap")"
FAKE_STUCK=AppleLocale rcase "还原后读回不一致（地区格式写不进去）：返回 1 并列出 user_locale" 1 "user_locale" Asia/Shanghai 0 zh-Hans-CN,en-US zh_CN zh-Hans-CN
NO_SNAP=1 rcase "快照文件缺失：返回 3，什么都不改" 3 "" Asia/Shanghai 0 zh-Hans-CN zh_CN none
[ "$(cat "$DB/tz")" = America/Los_Angeles ] && ok "e. 快照缺失时没有改动（时区还是 abroadd 写的值）" || bad "e. 快照缺失时却改了"
out=$(
  export M4_TEST_STATE="${ST}" DRY_RUN=0 M4_CMD_SYSTEMSETUP="${TMP}/nonexistent" M4_CMD_DEFAULTS="$FK/defaults" M4_CMD_LAUNCHCTL="$FK/launchctl"
  . scripts/m4/lib.sh; . scripts/m4/region.sh; rg_snapshot "${ST}/snap/region" u 501 2>/dev/null; echo "rc=$?"
)
printf '%s' "$out" | grep -q 'rc=1' && ok "e. 时区读不出来时快照无效（run.sh 据此不继续）" || bad "e. 读不出时区却拍了快照：$out"
rm -rf "${ST}"

# ---- h. --check-only 的拦截 ----
mkdir -p "${ST}/abroadd"; echo '{"version":1,"state":"ENABLED"}' >"${ST}/abroadd/state.json"
out=$(M4_TEST_STATE="${ST}" scripts/m4/run.sh --check-only 2>&1); rc=$?
[ $rc -ne 0 ] && printf '%s' "$out" | grep -q '上一次运行留下的 abroadd 状态文件还在' && ok "h. 旧的 abroadd 状态文件还在时拦下" || bad "h. 没拦下旧状态文件"
rm -rf "${ST}"
out=$(M4_TEST_STATE="${ST}" M4_TEST_PROXY_FILE=internal/preflight/testdata/proxy-http.constructed.txt scripts/m4/run.sh --check-only 2>&1)
printf '%s' "$out" | grep -q '系统代理开着' && ok "h. 系统代理开着的样本被拦下" || bad "h. 系统代理没拦下"
out=$(M4_TEST_STATE="${ST}" M4_TEST_PROXY_FILE=internal/preflight/testdata/proxy-off.txt scripts/m4/run.sh --check-only 2>&1)
printf '%s' "$out" | grep -q '没有确认 M3 的真机脚本已经通过' && ok "h. 没设 M4_M3_PASSED=1 时拦下" || bad "h. 没拦下 M3 未确认"
printf '%s' "$out" | grep -q '取“应用主代理”时按出口算出的时区' && ok "h. 前提检查显示目标时区的来源（默认按出口）" || bad "h. 前提检查没显示目标时区"
out=$(M4_TEST_STATE="${ST}" M4_M2_PASSED=1 M4_M3_PASSED=1 M4_WG_CONF=/nonexistent M4_TEST_PROXY_FILE=internal/preflight/testdata/proxy-off.txt scripts/m4/run.sh --check-only 2>&1)
printf '%s' "$out" | grep -q 'M4_WG_CONF 指向的文件不存在' && ok "h. 分流出口的配置文件不存在时拦下" || bad "h. 没拦下不存在的配置"
printf '%s' "$out" | grep -q '没有确认 M3' && bad "h. 设了 M4_M3_PASSED=1 仍报未确认" || ok "h. 设了 M4_M3_PASSED=1 后不再报这一条"

# ---- g、m. 判定函数 ----
SN="${TMP}/snap.region"; NOW="${TMP}/now.region"
printf 'tz=Asia/Shanghai\nauto=1\nuser=u\nuid=501\nuser_langs=zh-Hans-CN,en-US\nuser_locale=zh_CN\nsys_langs=none\n' >"$SN"
printf 'tz=America/Los_Angeles\nauto=0\nuser=u\nuid=501\nuser_langs=zh-Hans-CN,en-US\nuser_locale=zh_CN\nsys_langs=none\n' >"$NOW"
expect 0 "g. T1：时区换了、自动时区关、语言和地区格式没变：通过" judge_region "$SN" "$NOW" America/Los_Angeles 0 zh-Hans-CN zh_CN
expect 1 "g. T1：时区不是目标：不通过" judge_region "$SN" "$NOW" Asia/Tokyo 0 zh-Hans-CN zh_CN
printf 'tz=America/Los_Angeles\nauto=0\nuser=u\nuid=501\nuser_langs=zh-Hans-CN,en-US\nuser_locale=zh-Hans_CN\nsys_langs=none\n' >"$NOW"
expect 1 "g. T1：没开改语言的开关，地区格式却变了：不通过" judge_region "$SN" "$NOW" America/Los_Angeles 0 zh-Hans-CN zh_CN
printf 'tz=America/Los_Angeles\nauto=0\nuser=u\nuid=501\nuser_langs=en-US,zh-Hans-CN\nuser_locale=en_US\nsys_langs=en-US\n' >"$NOW"
expect 0 "g. T1：打开改语言的开关，首选语言和地区格式是目标：通过" judge_region "$SN" "$NOW" America/Los_Angeles 1 en-US en_US
items() { # items <硬性项结果…>：生成 17 项的报告，参数形如 03=fail
  /usr/bin/perl -MJSON::PP -e 'my %o = map { split /=/ } @ARGV; my @it; for my $i (1..17) { my $id = sprintf "%02d", $i; push @it, {id=>$id, level=>($i==13||$i==14||$i>=15?"warn":"hard"), result=>($o{$id}||"pass")} } print JSON::PP->new->canonical->encode({items=>\@it})' "$@"
}
expect 0 "g. T2：17 项、硬性项都通过：通过" judge_envcheck "$(items)"
expect 0 "g. T2：有硬性项无法判断（环境原因）：通过并列出" judge_envcheck "$(items 10=unknown)"
expect 1 "g. T2：硬性项 03 不通过：不通过" judge_envcheck "$(items 03=fail)"
expect 1 "g. T2：结果不足 17 项：不通过" judge_envcheck '{"items":[{"id":"01","level":"hard","result":"pass"}]}'
for lk in "系统代理:11" "代理环境变量:12" "IPv6:06"; do
  n=${lk%%:*}; id=${lk##*:}
  expect 0 "m. ${n}：检出（第 ${id} 项不通过、闸门关）、撤销后通过、闸门开：通过" judge_leak "$n" "$id" "$(items "${id}=fail")" false "$(items)" true
  expect 1 "m. ${n}：没检出（第 ${id} 项仍通过）：不通过" judge_leak "$n" "$id" "$(items)" true "$(items)" true
  expect 1 "m. ${n}：撤销后闸门没重新打开：不通过" judge_leak "$n" "$id" "$(items "${id}=fail")" false "$(items)" false
done
expect 0 "g. T4：出口 IP 与本机、主代理都不同：通过" judge_exit_route ne ne ne
expect 0 "g. T4：与主代理相同（同一家同一个出口）：如实记录、通过" judge_exit_route eq ne ne
expect 1 "g. T4：出口 IP 等于本机真实 IP：不通过" judge_exit_route ne eq ne
expect 1 "g. T4：请求失败、没测到：不通过" judge_exit_route none none ne
expect 0 "m. T4 断开后：回退主代理（出口 IP 等于主代理的）、没包：通过" judge_exit_down 1 eq ne ne 0
expect 0 "m. T4 断开后：请求失败、没包：通过" judge_exit_down 0 none none none 0
expect 1 "m. T4 断开后：出口 IP 等于本机真实 IP：不通过" judge_exit_down 1 ne eq ne 0
expect 1 "m. T4 断开后：物理网卡上抓到发往目标的包：不通过" judge_exit_down 0 none none none 3
expect 1 "m. T4 断开后：仍是原分流出口的 IP（配置没生效）：不通过" judge_exit_down 1 ne ne eq 0
expect 0 "g. T4b：有应答、没有发往 53 的包：通过" judge_udp_domain 1 0
expect 1 "g. T4b：抓到发往 53 的包：不通过" judge_udp_domain 1 2
expect 1 "g. T4b：没应答：不通过" judge_udp_domain 0 0
expect 0 "g. T6：真实 IP 出现 0 次：通过" judge_realip 1 0
expect 1 "m. T6：真实 IP 出现在状态目录或日志里（命中的反例）：不通过" judge_realip 1 2
expect 2 "g. T6：没测到真实 IP：跳过" judge_realip 0 0
# T6 的搜索本身：在一个假的状态目录里埋一次地址，按 run.sh 的写法能搜到（secret 子目录除外）。
mkdir -p "${TMP}/ipst/secret" "${TMP}/ipst/abroadd"
echo "203.0.113.77" >"${TMP}/ipst/secret/realip"
echo '{"note":"ok"}' >"${TMP}/ipst/abroadd/state.json"
n0=$(grep -rF --exclude-dir=secret -e 203.0.113.77 "${TMP}/ipst" 2>/dev/null | grep -c . || true)
echo 'exit 203.0.113.77 seen' >>"${TMP}/ipst/abroadd/log.txt"
n1=$(grep -rF --exclude-dir=secret -e 203.0.113.77 "${TMP}/ipst" 2>/dev/null | grep -c . || true)
[ "$n0" = 0 ] && [ "$n1" = 1 ] && ok "m. T6 的搜索：secret 子目录不算，埋进日志的一次能搜到" || bad "m. T6 的搜索不对：${n0}、${n1}"
PS="${TMP}/proxy.snap"
printf 'service=Wi-Fi\nEnabled=No\nServer=\nPort=0\nAuthenticated Proxy Enabled=0\n' >"$PS"
expect 0 "a. 撤销系统代理：原来为空、读回也清空了：通过" judge_proxy_restored "$PS" "$(printf 'Enabled: No\nServer: \nPort: 0\nAuthenticated Proxy Enabled: 0\n')"
expect 1 "a. 撤销系统代理：只关了开关、本机地址还留着：不通过" judge_proxy_restored "$PS" "$(printf 'Enabled: No\nServer: 127.0.0.1\nPort: 9\n')"
expect 1 "a. 撤销系统代理：开关没关：不通过" judge_proxy_restored "$PS" "$(printf 'Enabled: Yes\nServer: \nPort: 0\n')"
printf 'service=Wi-Fi\nEnabled=No\nServer=proxy.lan\nPort=3128\n' >"$PS"
expect 0 "a. 撤销系统代理：原来有服务器设置、写回去了：通过" judge_proxy_restored "$PS" "$(printf 'Enabled: No\nServer: proxy.lan\nPort: 3128\n')"
expect 0 "g. T8：Team ID 相同：通过" judge_team Q6L2SF6YDW Q6L2SF6YDW
expect 1 "g. T8：Team ID 不同：不通过" judge_team Q6L2SF6YDW ABCDE12345
expect 2 "g. T8：没装桌面版：跳过" judge_team Q6L2SF6YDW ""
expect 0 "g. T9：有确认码、预览里路径已换成 ~：通过" judge_preview '{"confirm":"0123456789abcdef0123456789abcdef","preview":{"processes":[{"pid":7,"cmd":"~/.local/bin/claude"}]}}'
expect 1 "g. T9：预览里有主目录的绝对路径：不通过" judge_preview '{"confirm":"0123456789abcdef0123456789abcdef","preview":{"processes":[{"cmd":"/Users/u/.local/bin/claude"}]}}'
expect 1 "g. T9：没有确认码：不通过" judge_preview '{"preview":{}}'
[ "$(jget '{"region":{"timezone":"America/Chicago"}}' region.timezone)" = America/Chicago ] && ok "o. 从应用主代理的结果里取出口时区" || bad "o. 取不到出口时区"
[ "$(jget '{"gate_open":false}' gate_open)" = false ] && [ "$(env_item "$(items 06=fail)" 06)" = fail ] && ok "g. 取闸门状态和某一项的结果" || bad "g. jget/env_item 不对"

# ---- trap、护栏 ----
FAKE2="${TMP}/fake-restore2.sh"
printf '#!/bin/bash\necho ran >> "%s/marker2"\n' "${TMP}" >"${FAKE2}"
run_case() {
  rm -f "${TMP}/marker2"
  ( export M4_TEST_STATE="${ST}" DRY_RUN=0; . scripts/m4/lib.sh; m4_arm_restore_trap "${FAKE2}"; M4_RESTORED=$1; exit "$2" ) >/dev/null 2>&1
  [ -f "${TMP}/marker2" ] && echo ran || echo not-ran
}
[ "$(run_case 0 1)" = ran ] && ok "trap. 中途非零退出时执行了还原" || bad "trap. 非零退出时没还原"
[ "$(run_case 0 0)" = not-ran ] && ok "trap. 正常退出时不执行" || bad "trap. 正常退出时也执行了"
[ "$(run_case 1 1)" = not-ran ] && ok "trap. 已经还原过时不再执行" || bad "trap. 重复执行了还原"
out=$(M4_TEST_STATE="${ST}" scripts/m4/restore.sh 2>&1); rc=$?
[ $rc -ne 0 ] && printf '%s' "$out" | grep -q '只能配 DRY_RUN=1' && ok "护栏：自测目录不配空跑时 restore.sh 拒绝执行" || bad "护栏没起作用：$out"
out=$(M4_TEST_STATE="${ST}" scripts/m4/run.sh 2>&1); rc=$?
[ $rc -ne 0 ] && printf '%s' "$out" | grep -q '只能配 DRY_RUN=1' && ok "护栏：自测目录不配空跑时 run.sh 拒绝执行" || bad "护栏没起作用（run.sh）"
grep -q -- '<string>--dev-skip-region</string>' scripts/m4/launchd.plist.tmpl && bad "plist 模板不能带 --dev-skip-region" || ok "plist 模板不带 --dev-skip-region（地区同步真正接上）"
awk '/api POST \/v1\/main-proxy\/apply/{a=NR} /confirm_call POST \/v1\/enable/{e=NR} END{exit !(a && e && a<e)}' scripts/m4/run.sh && ok "启用之前先应用主代理（取出口时区）" || bad "启用之前没有应用主代理"

echo
# ---- M6-1：二进制认识要传的开发参数（正式构建在改动之前被拦下的反例）----
devout=$(scripts/lib/devflags-selftest.sh m4 --dev-skip-claude-kill --dev-skip-region 2>&1)
while IFS= read -r l; do case "$l" in ✓*) ok "M6-1 ${l#✓ }" ;; ✗*) bad "M6-1 ${l#✗ }" ;; esac; done <<<"$devout"

[ $fails = 0 ] && echo "✓ 自测全部通过" || echo "✗ 自测有 $fails 处没过"
exit $((fails > 0))
