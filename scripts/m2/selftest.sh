#!/usr/bin/env bash
# M2-9c 复审的自测（不用 root、不改网络，状态目录指到临时目录）：
#   a. 死人开关在状态目录原本不存在时也能启动、核对存活、到点执行（假的）restore；
#   b. 长连接测试程序自己写 PID，m2_kill_pidfile 能按它结束；命令行不匹配的 PID 不动；
#   d. 上一次的 abroadd 状态文件还在时，--check-only 拦下；
#   e. 系统代理开着的样本，--check-only 拦下；关着的样本不拦；
#   trap：中途非零退出时执行（假的）restore，正常退出或已还原时不执行。
#   perm：镜像源头是 0600 时，按 run.sh 复制后测试用户也读得到（2026-11-27 真机）。
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
[ "$(id -u)" != 0 ] || { echo "不要以 root 运行" >&2; exit 1; }
TMP=$(mktemp -d "${TMPDIR:-/tmp}/m2st.XXXXXX")
trap 'rm -rf "${TMP}" "${TMP}"/st-snap' EXIT
fails=0
ok() { echo "✓ $*"; }
bad() { echo "✗ $*"; fails=$((fails + 1)); }

# ---- a. 死人开关 ----
ST="${TMP}/st" # 原本不存在
FAKE="${TMP}/fake-restore.sh"
printf '#!/bin/bash\necho "ran SUDO_USER=${SUDO_USER:-}" > "%s/marker"\n' "${TMP}" >"${FAKE}"
(
  export M2_TEST_STATE="${ST}" SUDO_USER=selftest-user DRY_RUN=0
  . scripts/m2/lib.sh
  m2_state_init
  m2_deadman_start 2 "${FAKE}"
  pid=$(m2_state_get deadman_pid)
  kill -0 "$pid" 2>/dev/null && echo "alive $pid"
) >"${TMP}/a.out" 2>&1
grep -q '^alive ' "${TMP}/a.out" && ok "a. 状态目录原本不存在时死人开关也能启动，PID 文件在、进程活着" || bad "a. 死人开关没起来：$(cat "${TMP}/a.out")"
sleep 3
if grep -q 'ran SUDO_USER=selftest-user' "${TMP}/marker" 2>/dev/null; then ok "a. 到点执行了假的 restore，SUDO_USER 显式传进去了"; else bad "a. 到点没有执行假的 restore"; fi
grep -q 'ab_die "死人开关没有起来' scripts/lib/common.sh && ok "a. 有“启动后核对 PID 存活，否则退出”的代码路径" || bad "a. 缺少存活核对"

# ---- b. 长连接测试程序的 PID ----
(
  export M2_TEST_STATE="${ST}" DRY_RUN=0
  . scripts/m2/lib.sh
  LC="${ST}/longconn.py"
  cp scripts/m2/longconn.py "${LC}"
  python3 "${LC}" lo 47997 "${ST}/lc-lo.st" "${ST}/lc-lo.pid" >/dev/null 2>&1 &
  for _ in $(seq 1 25); do [ -s "${ST}/lc-lo.pid" ] && break; sleep 0.2; done
  pid=$(cat "${ST}/lc-lo.pid")
  cmd=$(ps -p "$pid" -o command= 2>/dev/null)
  case "$cmd" in *"${LC}"*) echo "pidok $pid" ;; *) echo "pidbad $pid $cmd" ;; esac
  sleep 1; tail -1 "${ST}/lc-lo.st" | sed 's/^/first /'
  m2_kill_pidfile "${ST}/lc-lo.pid" "${LC}" 2>/dev/null
  sleep 0.5; kill -0 "$pid" 2>/dev/null && echo "still-alive" || echo "killed"
  # 命令行不匹配：一个 sleep 进程的 PID 写进 PID 文件，不应被结束。
  sleep 30 & sp=$!
  echo "$sp" >"${ST}/other.pid"
  m2_kill_pidfile "${ST}/other.pid" "${LC}" 2>/dev/null
  kill -0 "$sp" 2>/dev/null && echo "untouched" || echo "wrongly-killed"
  kill "$sp" 2>/dev/null
) >"${TMP}/b.out" 2>&1
grep -q '^pidok ' "${TMP}/b.out" && ok "b. PID 文件里是长连接测试程序自己的 PID（命令行含 longconn.py）" || bad "b. PID 不对：$(cat "${TMP}/b.out")"
grep -q '^first ok' "${TMP}/b.out" && ok "b. 回环长连接首次检查 ok" || bad "b. 回环长连接首次检查没过：$(cat "${TMP}/b.out")"
grep -q '^killed' "${TMP}/b.out" && ok "b. m2_kill_pidfile 按 PID 文件结束了它" || bad "b. 没结束掉"
grep -q '^untouched' "${TMP}/b.out" && ok "b. 命令行不匹配的 PID 没有动" || bad "b. 动了不该动的进程"

# ---- d. 旧的 abroadd 状态文件 ----
rm -rf "${ST}"; mkdir -p "${ST}/abroadd"; echo '{"version":1,"state":"ENABLED","step":8}' >"${ST}/abroadd/state.json"
out=$(M2_TEST_STATE="${ST}" scripts/m2/run.sh --check-only 2>&1); rc=$?
if [ $rc -ne 0 ] && printf '%s' "$out" | grep -q '上一次运行留下的 abroadd 状态文件还在'; then ok "d. 状态文件还在时 --check-only 拦下（退出码 ${rc}）"; else bad "d. 没拦下：$out"; fi
grep -q 'abroadd/state.json" \] && m2_die' scripts/m2/run.sh && ok "d. run.sh 在安装 LaunchDaemon 之前还有一道检查" || bad "d. 缺少安装前的检查"

# ---- e. 系统代理 ----
rm -rf "${ST}"
out=$(M2_TEST_STATE="${ST}" M2_TEST_PROXY_FILE=internal/preflight/testdata/proxy-http.constructed.txt scripts/m2/run.sh --check-only 2>&1)
printf '%s' "$out" | grep -q '系统代理开着' && ok "e. 系统代理开着的样本（HTTP/HTTPS）被拦下" || bad "e. 没拦下：$out"
out=$(M2_TEST_STATE="${ST}" M2_TEST_PROXY_FILE=internal/preflight/testdata/proxy-pac.constructed.txt scripts/m2/run.sh --check-only 2>&1)
printf '%s' "$out" | grep -q '系统代理开着' && ok "e. PAC 开着的样本被拦下" || bad "e. PAC 没拦下"
out=$(M2_TEST_STATE="${ST}" M2_TEST_PROXY_FILE=internal/preflight/testdata/proxy-off.txt scripts/m2/run.sh --check-only 2>&1)
printf '%s' "$out" | grep -q '系统代理开着' && bad "e. 关着的样本被误拦" || ok "e. 系统代理关着的样本不拦"
( . scripts/m2/lib.sh; m2_system_proxy_on ) && echo "  本机现状：系统代理开着" || echo "  本机现状：系统代理没开（HTTP/HTTPS/SOCKS/PAC/自动发现都是 0）"

# ---- trap：中途退出时自动还原 ----
FAKE2="${TMP}/fake-restore2.sh"
printf '#!/bin/bash\necho ran >> "%s/marker2"\n' "${TMP}" >"${FAKE2}"
run_case() { # run_case <M2_RESTORED> <退出码>
  rm -f "${TMP}/marker2"
  ( export M2_TEST_STATE="${ST}" DRY_RUN=0; . scripts/m2/lib.sh; m2_arm_restore_trap "${FAKE2}"; M2_RESTORED=$1; exit "$2" ) >/dev/null 2>&1
  [ -f "${TMP}/marker2" ] && echo ran || echo not-ran
}
[ "$(run_case 0 1)" = ran ] && ok "trap. 中途非零退出时执行了还原" || bad "trap. 非零退出时没还原"
[ "$(run_case 0 0)" = not-ran ] && ok "trap. 正常退出时不执行" || bad "trap. 正常退出时也执行了"
[ "$(run_case 1 1)" = not-ran ] && ok "trap. 已经还原过（M2_RESTORED=1）时不再执行" || bad "trap. 重复执行了还原"
rm -f "${TMP}/marker2"
( export M2_TEST_STATE="${ST}" DRY_RUN=0; . scripts/m2/lib.sh; m2_arm_restore_trap "${FAKE2}"; kill -INT $BASHPID; sleep 2 ) >/dev/null 2>&1
[ -f "${TMP}/marker2" ] && ok "trap. 收到 INT 时执行了还原" || bad "trap. INT 时没还原"

# ---- 自测目录只能配空跑或 --check-only ----
out=$(M2_TEST_STATE="${ST}" scripts/m2/restore.sh 2>&1); rc=$?
[ $rc -ne 0 ] && printf '%s' "$out" | grep -q '只能配 DRY_RUN=1' && ok "护栏：自测目录不配空跑时 restore.sh 拒绝执行" || bad "护栏没起作用：$out"

echo
# m4-plan v2.5 建议改 3：abroadd 的 LaunchDaemon 模板带 --dev-skip-region（这份脚本不改时区和语言）；启用之前先应用主代理。
if grep -q '<string>--dev-skip-region</string>' scripts/m2/launchd.plist.tmpl; then ok "plist 模板带 --dev-skip-region"; else bad "plist 模板没带 --dev-skip-region"; fi
if awk '/api POST \/v1\/main-proxy\/apply/{a=NR} /api POST \/v1\/enable/{e=NR} END{exit !(a && e && a<e)}' scripts/m2/run.sh; then ok "启用之前先应用主代理"; else bad "启用之前没有应用主代理"; fi
# ---- M6-1：二进制认识要传的开发参数（正式构建在改动之前被拦下的反例）----
devout=$(scripts/lib/devflags-selftest.sh m2 --dev-skip-claude-kill --dev-skip-region 2>&1)
while IFS= read -r l; do case "$l" in ✓*) ok "M6-1 ${l#✓ }" ;; ✗*) bad "M6-1 ${l#✗ }" ;; esac; done <<<"$devout"

# M6-4b-1 返工：这份脚本启动的 abroadd 不打开启动管控（不带 --launch-control）：不做首次准备、不写解析锁和托管设置、不接第三层。
if grep -q -- '--launch-control' scripts/m2/launchd.plist.tmpl scripts/m2/run.sh; then
  bad "启动 abroadd 的参数里不能有 --launch-control（会写入解析锁和托管设置，旧脚本不会删）"
else
  ok "启动 abroadd 的参数里没有 --launch-control（不打开启动管控）"
fi
# ---- 2026-11-27 真机：镜像源头是 0600，复制后虚拟机（以测试用户运行）要读得到 ----
# 按 run.sh 里的原样两行（复制镜像、改镜像权限）在临时目录里真跑一遍（m2_do 换成直接执行），核对所有人可读、组和其他人不可写。
cpl=$(grep -m1 -F 'm2_do cp -R guest/out/. "${M2_INSTALL}/image/"' scripts/m2/run.sh)
chl=$(grep -m1 -F 'm2_do chmod -R go-w,a+rX "${M2_INSTALL}/image"' scripts/m2/run.sh)
if [ -z "$cpl" ] || [ -z "$chl" ]; then
  bad "perm. run.sh 里找不到复制镜像或改镜像权限的那一行"
elif ! awk -v a="$cpl" -v b="$chl" '$0 == a { x = NR } $0 == b { y = NR } END { exit !(x && y && x < y) }' scripts/m2/run.sh; then
  bad "perm. 改镜像权限应在复制镜像之后"
else
  PW="${TMP}/perm"; mkdir -p "${PW}/repo/guest/out" "${PW}/inst/image"
  for f in Image initramfs.gz modloop rootfs.img; do echo "img $f" >"${PW}/repo/guest/out/$f"; chmod 600 "${PW}/repo/guest/out/$f"; done
  (cd "${PW}/repo" && M2_INSTALL="${PW}/inst" && m2_do() { "$@"; } && eval "$cpl" && eval "$chl")
  pbad=""
  for f in Image initramfs.gz modloop rootfs.img; do
    m=$(stat -f '%Lp' "${PW}/inst/image/$f"); [ $((0$m & 044)) = 36 ] && [ $((0$m & 022)) = 0 ] || pbad="${pbad} $f=$m"
  done
  m=$(stat -f '%Lp' "${PW}/inst/image"); [ $((0$m & 055)) = 45 ] || pbad="${pbad} image/=$m"
  [ -z "$pbad" ] && ok "perm. 源头镜像 0600，按 run.sh 复制后所有人可读（组和其他人不可写）" || bad "perm. 复制后读不到：${pbad}"
fi

[ $fails = 0 ] && echo "✓ 自测全部通过" || echo "✗ 自测有 $fails 处没过"
exit $((fails > 0))
