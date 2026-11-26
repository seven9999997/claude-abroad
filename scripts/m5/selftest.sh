#!/usr/bin/env bash
# M5-11 的自测（不用 root、不改网络和地区，状态目录指到临时目录）：
#   a. 死人开关在状态目录原本不存在时也能启动、核对存活、到点执行（假的）restore；
#   b. m5_kill_pidfile 按 PID 文件结束进程；命令行不匹配的 PID 不动；
#   c. DRY_RUN=1 跑完 run.sh、restore.sh 退出码 0，前后用户环境不变（路由、代理、DNS、时区、自动时区、语言）；
#   d. 命令核对通过；反例（禁止的命令、清单外的命令、打开应用、自己改语言）各一条都被拦下；
#   e. --check-only 在没有确认 M2–M4 通过时拒绝继续；
#   f. 源码检查：第一级启动脚本不含 sudo、pfctl、networksetup、ifconfig、route；打包脚本不安装、不注册、不启动；
#   g. 第一级启动脚本的空跑（--check）：起假后台 → 只停后台 → 同一个 socket 路径再起来 → 读到状态；跑完没有残留进程和临时目录；
#   h. 走查清单里引用的自动测试名真实存在（scripts/m5/check-criteria.sh）。
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
[ "$(id -u)" != 0 ] || { echo "不要以 root 运行" >&2; exit 1; }
TMP=$(mktemp -d "${TMPDIR:-/tmp}/m5st.XXXXXX")
trap 'rm -rf "${TMP}"' EXIT
fails=0
ok() { echo "✓ $*"; }
bad() { echo "✗ $*"; fails=$((fails + 1)); }
ST="${TMP}/st"

# ---- a. 死人开关 ----
FAKE="${TMP}/fake-restore.sh"
printf '#!/bin/bash\necho "ran SUDO_USER=${SUDO_USER:-} TEST=${M5_TEST_STATE:-}" > "%s/marker"\n' "${TMP}" >"${FAKE}"
(
  export M5_TEST_STATE="${ST}" SUDO_USER=selftest-user DRY_RUN=0
  . scripts/m5/lib.sh
  m5_state_init
  m5_deadman_start 2 "${FAKE}"
  pid=$(m5_state_get deadman_pid)
  kill -0 "$pid" 2>/dev/null && echo "alive $pid"
) >"${TMP}/a.out" 2>&1
grep -q '^alive ' "${TMP}/a.out" && ok "a. 状态目录原本不存在时死人开关也能启动" || bad "a. 死人开关没起来：$(cat "${TMP}/a.out")"
sleep 3
grep -q "ran SUDO_USER=selftest-user TEST=${ST}" "${TMP}/marker" 2>/dev/null && ok "a. 到点执行了假的 restore" || bad "a. 到点没有执行假的 restore"

# ---- b. 按 PID 文件结束进程 ----
(
  export M5_TEST_STATE="${ST}" DRY_RUN=0
  . scripts/m5/lib.sh
  mkdir -p "${ST}/vm"
  cp /bin/sleep "${TMP}/abroad-vm"
  "${TMP}/abroad-vm" 900 & p1=$!
  echo "$p1" >"${ST}/vm/abroad-vm.pid"
  m5_kill_pidfile "${ST}/vm/abroad-vm.pid" "${TMP}/abroad-vm 900" 2>/dev/null
  sleep 0.5; kill -0 "$p1" 2>/dev/null && echo "still-alive" || echo "killed"
  sleep 33 & p2=$!
  echo "$p2" >"${ST}/vm/abroad-vm.pid"
  m5_kill_pidfile "${ST}/vm/abroad-vm.pid" "abroad-vm run" 2>/dev/null
  kill -0 "$p2" 2>/dev/null && echo "untouched" || echo "wrongly-killed"
  kill "$p2" 2>/dev/null
) >"${TMP}/b.out" 2>&1
grep -q '^killed' "${TMP}/b.out" && ok "b. 按 PID 文件结束了进程（命令行核对过）" || bad "b. 没结束掉：$(cat "${TMP}/b.out")"
grep -q '^untouched' "${TMP}/b.out" && ok "b. 命令行不匹配的 PID 没有动" || bad "b. 动了不该动的进程"
rm -rf "${ST}"

# ---- c、d. 空跑：退出码、环境不变、命令核对 ----
env_snap() {
  echo "routes: $(netstat -rn -f inet 2>/dev/null | awk '{print $1, $4}' | sort | shasum -a 256 | cut -c1-16)"
  echo "proxy: $(scutil --proxy 2>/dev/null | shasum -a 256 | cut -c1-16)"
  echo "dns: $(scutil --dns 2>/dev/null | grep -E 'nameserver|search' | sort -u | shasum -a 256 | cut -c1-16)"
  echo "tz: $(readlink /etc/localtime 2>/dev/null)"
  echo "auto: $(defaults read /Library/Preferences/com.apple.timezone.auto Active 2>&1 | head -1)"
  echo "langs: $(defaults read -g AppleLanguages 2>&1 | tr -d ' \n')"
  echo "sockdir: $([ -e /var/run/claude-abroad ] && echo exists || echo none)"
  echo "cli: $([ -L /usr/local/bin/abroad ] && readlink /usr/local/bin/abroad || echo none)"
}
before=$(env_snap)
DRY_RUN=1 bash scripts/m5/run.sh >"${TMP}/run.out" 2>&1; rc1=$?
DRY_RUN=1 bash scripts/m5/restore.sh >"${TMP}/restore.out" 2>&1; rc2=$?
after=$(env_snap)
[ $rc1 = 0 ] && [ $rc2 = 0 ] && ok "c. 空跑 run.sh、restore.sh 退出码 0" || bad "c. 空跑退出码 run=${rc1} restore=${rc2}：$(tail -3 "${TMP}/run.out")"
[ "$before" = "$after" ] && ok "c. 空跑前后用户环境不变（路由、代理、DNS、时区、自动时区、语言、socket 目录、命令行链接）" || bad "c. 空跑前后环境变了：$(diff <(echo "$before") <(echo "$after"))"
grep -q '^WAIT: ' "${TMP}/run.out" && ok "c. 空跑里能看到 7 个检查点要用户做的事" || bad "c. 空跑里没有检查点"
scripts/m5/check-commands.sh "${TMP}/run.out" "${TMP}/restore.out" >"${TMP}/cc.out" 2>&1 && ok "d. 命令核对通过" || bad "d. 命令核对：$(cat "${TMP}/cc.out")"
for evil in "pfctl -d" "networksetup -setdnsservers Wi-Fi 8.8.8.8" "open /tmp/x.app" "curl http://example.com" "/usr/bin/defaults write -g AppleLanguages -array en-US" "kill 123"; do
  cp "${TMP}/run.out" "${TMP}/evil.out"
  echo "CHANGE: ${evil}" >>"${TMP}/evil.out"
  if scripts/m5/check-commands.sh "${TMP}/evil.out" "${TMP}/restore.out" >/dev/null 2>&1; then bad "d. 反例没拦下：${evil}"; else ok "d. 反例被拦下：${evil}"; fi
done

# ---- e. --check-only 的拦截 ----
out=$(env -u M5_M2_PASSED -u M5_M3_PASSED -u M5_M4_PASSED M5_TEST_STATE="${TMP}/chk" bash scripts/m5/run.sh --check-only 2>&1); rc=$?
[ $rc != 0 ] && printf '%s' "$out" | grep -q 'M2 的真机脚本' && printf '%s' "$out" | grep -q 'M4 的真机脚本' && ok "e. 没有确认 M2–M4 通过时拒绝继续" || bad "e. --check-only 没有拦下：rc=$rc $out"

# ---- f. 源码检查 ----
code() { grep -v '^[[:space:]]*#' "$1"; }
if code scripts/m5/walk-fake.sh | grep -nE '(^|[^a-z_-])(sudo|pfctl|networksetup|ifconfig|route|launchctl)([^a-z_-]|$)' >"${TMP}/f1"; then bad "f. 第一级启动脚本里有不该有的命令：$(cat "${TMP}/f1")"; else ok "f. 第一级启动脚本不含 sudo、pfctl、networksetup、ifconfig、route、launchctl"; fi
if code scripts/dev/app-bundle.sh | grep -nE '(^|[^a-z_-])(open|launchctl|sudo)([^a-z_-]|$)|/Applications|LaunchDaemons|LaunchAgents|SMAppService' >"${TMP}/f2"; then bad "f. 打包脚本里有安装、注册或启动：$(cat "${TMP}/f2")"; else ok "f. 打包脚本不安装、不注册、不启动"; fi
code scripts/m5/walk-fake.sh | grep -q -- '--fake-system --fake-main-proxy' && ok "f. 第一级只启动假系统模式的 abroadd" || bad "f. 第一级没有用假系统模式"

# ---- g. 第一级启动脚本的空跑 ----
before_p=$(pgrep -f 'abroad-walk\.' | sort | tr '\n' ' ')
scripts/m5/walk-fake.sh --check >"${TMP}/walk.out" 2>&1; rc=$?
after_p=$(pgrep -f 'abroad-walk\.' | sort | tr '\n' ' ')
[ $rc = 0 ] && grep -q '空跑：不打开应用' "${TMP}/walk.out" && ok "g. 第一级空跑：启动假系统 abroadd、读到状态、不打开应用" || bad "g. 第一级空跑失败：$(cat "${TMP}/walk.out")"
socks=$(sed -n 's/.*socket \(\/tmp\/abroad-walk\.[^，]*\)，状态 \([A-Z]*\)）.*/\1 \2/p' "${TMP}/walk.out" | sort -u)
[ "$(printf '%s\n' "$socks" | grep -c .)" = 1 ] && [ "$(grep -c '^假后台已启动' "${TMP}/walk.out")" = 2 ] && grep -q '假后台已停' "${TMP}/walk.out" && grep -q '两次的 socket 路径相同' "${TMP}/walk.out" &&
  ok "g. 只停后台再用同一路径起来：两次 socket 路径相同、都读到状态（$(printf '%s' "$socks" | awk '{print $2}')）" || bad "g. 停后台再起来不对：$(cat "${TMP}/walk.out")"
grep -q '重启后假主代理的出口基准还在' "${TMP}/walk.out" && ok "g. 重启后假主代理的出口基准还在（同一个状态目录）" || bad "g. 重启后出口基准不在：$(grep 出口基准 "${TMP}/walk.out")"
grep -q -- '--app-first' scripts/m5/walk-fake.sh && grep -qE 'd\) stop_daemon' scripts/m5/walk-fake.sh && grep -qE 'u\) start_daemon' scripts/m5/walk-fake.sh && ok "g. 有先开应用的方式和只停、重起后台的按键" || bad "g. 缺 --app-first 或 d、u 按键"
[ "$before_p" = "$after_p" ] && [ -z "$(ls -d /tmp/abroad-walk.* 2>/dev/null)" ] && ok "g. 跑完没有残留进程和临时目录" || bad "g. 有残留：进程 ${after_p}；目录 $(ls -d /tmp/abroad-walk.* 2>/dev/null)"

# ---- h. 完成标准对照表里的测试名 ----
scripts/m5/check-criteria.sh >"${TMP}/crit.out" 2>&1 && ok "h. $(tail -1 "${TMP}/crit.out")" || bad "h. $(cat "${TMP}/crit.out")"
sed 's/swift:pageLoadStates/swift:noSuchTestXYZ/; s/^| D6 |.*/| D6 | 诊断包 | — | — | — | — |/' docs/m5-walkthrough.md >"${TMP}/crit-evil.md"
out=$(M5_CRITERIA_DOC="${TMP}/crit-evil.md" scripts/m5/check-criteria.sh 2>&1); rc=$?
[ $rc != 0 ] && printf '%s' "$out" | grep -q 'noSuchTestXYZ' && printf '%s' "$out" | grep -q 'D6 没有着落' && ok "h. 反例：不存在的测试名、没有着落的一行都被拦下" || bad "h. 反例没拦下：$out"

# ---- M6-1：二进制认识要传的开发参数（正式构建在改动之前被拦下的反例）----
devout=$(scripts/lib/devflags-selftest.sh m5 --dev-skip-claude-kill 2>&1)
while IFS= read -r l; do case "$l" in ✓*) ok "M6-1 ${l#✓ }" ;; ✗*) bad "M6-1 ${l#✗ }" ;; esac; done <<<"$devout"

# M6-4b-1 返工：这份脚本启动的 abroadd 不打开启动管控（不带 --launch-control）：不做首次准备、不写解析锁和托管设置、不接第三层。
if grep -q -- '--launch-control' scripts/m5/launchd.plist.tmpl scripts/m5/run.sh; then
  bad "启动 abroadd 的参数里不能有 --launch-control（会写入解析锁和托管设置，旧脚本不会删）"
else
  ok "启动 abroadd 的参数里没有 --launch-control（不打开启动管控）"
fi
[ $fails = 0 ] && echo "✓ 自测全部通过" || { echo "✗ 自测有 ${fails} 处不通过"; exit 1; }
