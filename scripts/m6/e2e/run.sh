#!/usr/bin/env bash
# M6-9 开发安装的端到端（真机，m6-plan M6-9）：开发安装 → 在应用里配好主代理 → 启用 → 回国 → 再启用 → 重启电脑看开机时的
# guard → 回国 → 已回国时再重启看 guard 加载回国拦截（M6-11b）→ 卸载 → 对照安装前的快照和地区快照。分三段，中间由你重启电脑。
# 默认开发构建（abroadd 带 --dev-skip-claude-kill、--dev-resolver-test-domain）：不结束任何 Claude 进程，解析锁只锁测试域名，
# 不写 Claude Code 的托管设置。对真实域名和真实程序的验证（R9、R18）用 scripts/m6/resolver，不在这里。
# 会改动本机网络和时区、需要 sudo，启用期间和重启后的一段时间会断网（Claude Code 会话会断开）：由你在不需要会话的时候执行。
# 会话里只做空跑、--check-only 和自测。用法见 README.md。
#
#   sudo scripts/m6/e2e/run.sh                  # 第一段（装到启用，最后请你重启）
#   sudo scripts/m6/e2e/run.sh --after-reboot   # 第二段（重启后；最后请你再重启一次）
#   sudo scripts/m6/e2e/run.sh --after-reboot-2 # 第三段（已回国状态下重启后；卸载、收尾）
#   scripts/m6/e2e/run.sh --check-only          # 前提检查（只读）
#   DRY_RUN=1 scripts/m6/e2e/run.sh [--after-reboot]   # 空跑
#
# 环境变量：M6E_NODES（主代理节点的 Clash YAML，必填）、M6E_M5_PASSED=1（M5 第二级走查已通过，必填）、
#   M6E_DEADMAN_SECS（死人开关，默认 3600 秒）、M6E_YES=1（跳过开始前的按键确认）。
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "${ROOT}" || exit 1
. scripts/m6/e2e/lib.sh
. scripts/m4/region.sh

exec 3>&1
PHASE=1
CHECK_ONLY=0
case "${1:-}" in
  --check-only) CHECK_ONLY=1 ;;
  --after-reboot) PHASE=2 ;;
  --after-reboot-2) PHASE=3 ;;
  "") ;;
  *) echo "用法：sudo $0 [--check-only|--after-reboot|--after-reboot-2]" >&2; exit 2 ;;
esac

OUT="${ROOT}/scripts/m6/e2e/out"
TS="$(date +%Y%m%d-%H%M%S)"
mkdir -p "${OUT}"
M6E_LOG="${OUT}/m6e-${PHASE}-${TS}.log"
RESULTS="${OUT}/m6e-results.txt" # 两段共用一份结果
: >"${M6E_LOG}"
[ "${PHASE}" = 1 ] && [ "${CHECK_ONLY}" = 0 ] && [ "$DRY_RUN" != 1 ] && : >"${RESULTS}"
DEADMAN_SECS="${M6E_DEADMAN_SECS:-3600}"
API="${M6E_SOCK}/api.sock"
RESTORE="${ROOT}/scripts/m6/e2e/restore.sh"

res() { printf '%-4s %-6s %s\n' "$1" "$2" "$3" | tee -a "${RESULTS}" >&2; }
step() { printf 'STEP: %s\n' "$*" >&3; m6e_log "$*"; }
jget() { printf '%s' "$1" | /usr/bin/python3 -I -c 'import json,sys
try: d=json.load(sys.stdin)
except Exception: sys.exit(0)
for k in sys.argv[1].split("."):
    if isinstance(d, list):
        try: d=d[int(k)]
        except Exception: d=None
    elif isinstance(d, dict): d=d.get(k)
    else: d=None
print("" if d is None else (json.dumps(d) if isinstance(d,(dict,list)) else str(d).lower() if isinstance(d,bool) else d))' "$2"; }
get() {
  if [ "$DRY_RUN" = 1 ]; then printf 'API: GET %s\n' "$1" >&3; echo '{}'; return; fi
  curl -sS -m 10 --unix-socket "${API}" "http://abroadd$1" 2>/dev/null || echo '{}'
}
checkpoint() { # checkpoint <编号> <要你在应用里做的事> <核对函数> <说明>
  local id=$1 todo=$2 fn=$3 what=$4 ans why
  step "检查点 ${id}：${todo}"
  if [ "$DRY_RUN" = 1 ]; then printf 'WAIT: %s\n' "${todo}" >&3; res "${id}" 跳过 "空跑"; return; fi
  while true; do
    m6e_log "在应用里：${todo}。做完按回车核对（输入 s 跳过）。"
    read -r ans </dev/tty || ans=s
    [ "${ans}" = s ] && { res "${id}" 跳过 "你跳过了：${what}"; return; }
    if why=$("${fn}"); then res "${id}" 通过 "${what}：${why}"; return; fi
    m6e_log "还不满足：${why}。按回车重新核对，输入 s 跳过。"
  done
}
# check <编号> <核对函数> <说明> [等待秒数]：不用你操作的核对（等到满足或超时）。
check() {
  local id=$1 fn=$2 what=$3 secs=${4:-1} why i
  step "核对 ${id}：${what}"
  if [ "$DRY_RUN" = 1 ]; then res "${id}" 跳过 "空跑"; return; fi
  for i in $(seq 1 "${secs}"); do
    if why=$("${fn}"); then res "${id}" 通过 "${what}：${why}"; return; fi
    [ "$i" -lt "${secs}" ] && sleep 1
  done
  res "${id}" 不通过 "${what}：${why}"
}

c_mainproxy() {
  local s m; s=$(get /v1/setup); m=$(get /v1/main-proxy)
  [ -n "$(jget "$m" selected.id)" ] || { echo "还没有选择节点"; return 1; }
  [ "$(jget "$s" completed)" = true ] || { echo "安装引导没完成（要应用主代理并点完成）"; return 1; }
  echo "选了节点，引导已完成"
}
c_installed() {
  local st; st=$(get /v1/status)
  [ "$(jget "$st" prepare.done)" = true ] || { echo "首次准备没完成：$(jget "$st" prepare)"; return 1; }
  [ "$(jget "$st" integrity.checked)" = true ] && [ "$(jget "$st" integrity.ok)" = true ] || { echo "完整性校验：$(jget "$st" integrity)"; return 1; }
  [ "$(jget "$st" resolver_lock.effective)" = true ] || { echo "解析锁（测试域名）没生效：$(jget "$st" resolver_lock)"; return 1; }
  echo "首次准备完成、完整性通过、解析锁（测试域名）生效"
}
c_enabled() {
  local st; st=$(get /v1/status)
  [ "$(jget "$st" state)" = ENABLED ] && [ "$(jget "$st" gate_open)" = true ] || { echo "状态 $(jget "$st" state)，闸门 $(jget "$st" gate_open)，原因 $(jget "$st" gate.reasons)"; return 1; }
  echo "已启用、闸门开"
}
c_restored() {
  local st code; st=$(get /v1/status)
  [ "$(jget "$st" state)" = RESTORED ] || { echo "状态 $(jget "$st" state)"; return 1; }
  [ "$(jget "$st" resolver_lock.effective)" = true ] || { echo "回国后解析锁应仍在：$(jget "$st" resolver_lock)"; return 1; }
  code=$(curl -sS -m 8 -o /dev/null -w '%{http_code}' https://www.gstatic.com/generate_204 2>/dev/null)
  [ "${code}" = 204 ] || { echo "本机网络还没恢复（直连探测 ${code:-无应答}）"; return 1; }
  echo "已回国、解析锁仍在、本机网络恢复"
}
# M6-11b：回国拦截（开发安装只管测试目标：测试段）。状态里生效且是测试段、锚点里恰好是那两条（只读，经 pfcheck.sh）、
# 不带凭据地连测试段的一个地址应立即失败（被拒；没有拦截时这个地址不可达，会一直等到超时）。
RB_RULES="block return out quick inet from any to 192.0.2.0/24
block return out quick inet6 from any to 2001:db8:abd::/48"
c_rb() {
  local st r t0 t1 rc
  st=$(get /v1/status)
  [ "$(jget "$st" return_block.active)" = true ] && [ "$(jget "$st" return_block.test)" = true ] || { echo "回国拦截应在生效且是测试段：$(jget "$st" return_block)"; return 1; }
  r=$(bash scripts/m6/e2e/pfcheck.sh rules 2>/dev/null)
  [ "$r" = "${RB_RULES}" ] || { echo "锚点里应恰好是测试段的两条：${r}"; return 1; }
  t0=$(date +%s); curl -sS -m 8 --connect-timeout 8 -o /dev/null https://192.0.2.1/ 2>/dev/null; rc=$?; t1=$(date +%s)
  [ "$rc" != 0 ] && [ $((t1 - t0)) -le 3 ] || { echo "连 192.0.2.1:443 应立即失败（退出码 ${rc}，用了 $((t1 - t0)) 秒）"; return 1; }
  echo "回国拦截生效（测试段），锚点恰好两条，连测试段 $((t1 - t0)) 秒内被拒（curl 退出码 ${rc}）"
}
c_guard_rb() {
  [ -f "${M6E_GUARD_LOG}" ] || { echo "没有 guard 的日志"; return 1; }
  tail -20 "${M6E_GUARD_LOG}" | grep -q '已加载回国拦截' || { echo "guard 这次开机没有加载回国拦截：$(tail -3 "${M6E_GUARD_LOG}" | tr '\n' ' ')"; return 1; }
  echo "开机时 guard 加载了回国拦截"
}
c_guard_loaded() {
  [ -f "${M6E_GUARD_LOG}" ] || { echo "没有 guard 的日志"; return 1; }
  tail -20 "${M6E_GUARD_LOG}" | grep -q '已加载断网规则' || { echo "guard 这次开机没有加载断网规则：$(tail -3 "${M6E_GUARD_LOG}" | tr '\n' ' ')"; return 1; }
  tail -20 "${M6E_GUARD_LOG}" | grep -q '完整性校验通过' || { echo "guard 的完整性校验没通过：$(tail -5 "${M6E_GUARD_LOG}" | tr '\n' ' ')"; return 1; }
  echo "开机时 guard 加载了断网规则、完整性校验通过"
}

precheck() {
  local reasons=() conf
  conf=$(netstat -rn -f inet 2>/dev/null | awk '$4 ~ /^(utun|ipsec|ppp|tun|tap|wg)[0-9]+$/ && $1 == "default" { print $4 }' | sort -u | tr '\n' ' ')
  [ -n "${conf}" ] && reasons+=("别的代理软件的 TUN 或 VPN 开着（${conf% }）：请先关掉")
  m6e_system_proxy_on && reasons+=("系统代理开着：请先全部关掉")
  [ "${M6E_M5_PASSED:-0}" = 1 ] || reasons+=("没有确认 M5 的第二级走查已经通过：通过后设 M6E_M5_PASSED=1")
  [ -n "${M6E_NODES:-}" ] && [ -f "${M6E_NODES:-/nonexistent}" ] || reasons+=("没有主代理：请设置 M6E_NODES（Clash YAML，在应用里用“本地配置文件”添加）")
  [ -e "${M6E_STATE}/phase" ] && reasons+=("上一次运行还没结束（${M6E_STATE}）：重启后的第二段用 --after-reboot；放弃的话执行 sudo scripts/m6/e2e/restore.sh")
  scripts/m6/install/install.sh --check-only >/dev/null 2>"${OUT}/m6e-install-check.txt" || reasons+=("开发安装的前提不满足：$(tr '\n' ' ' <"${OUT}/m6e-install-check.txt")")
  if [ ${#reasons[@]} -gt 0 ]; then
    for r in "${reasons[@]}"; do m6e_log "前提不满足：${r}"; done
    return 1
  fi
  m6e_log "前提检查通过"
}

if [ "${CHECK_ONLY}" = 1 ]; then m6e_test_guard check; precheck; exit $?; fi
m6e_test_guard
m6e_require_root
[ "$DRY_RUN" = 1 ] && SUDO_USER=${SUDO_USER:-$(id -un)}
SUID=$(id -u "${SUDO_USER}" 2>/dev/null || echo 501)

if [ "${PHASE}" = 1 ]; then
  if ! precheck; then
    if [ "$DRY_RUN" = 1 ]; then m6e_log "（空跑：前提不满足，继续打印后面的命令）"; else exit 3; fi
  fi
  m6e_log "提醒：启用期间和第二段重启后的一段时间本机会断网，时区和语言会改成出口所在地（结束时还原）。"
  if [ "$DRY_RUN" != 1 ] && [ "${M6E_YES:-0}" != 1 ]; then m6e_log "按回车开始（Ctrl+C 取消）。"; read -r _ </dev/tty; fi
  m6e_state_init
  m6e_deadman_start "${DEADMAN_SECS}" "${RESTORE}"
  m6e_arm_restore_trap "${RESTORE}"
  rg_snapshot "${M6E_STATE}/region" "${SUDO_USER}" "${SUID}" || m6e_die "地区有读不出来的项，没有原值就不能保证还原，不继续"
  m6e_state_set phase 1

  step "开发安装"
  if [ "$DRY_RUN" = 1 ]; then printf 'CHANGE: scripts/m6/install/install.sh\n'; else
    M6I_YES=1 bash scripts/m6/install/install.sh || m6e_die "开发安装失败（它已经自动卸载），见 scripts/m6/install/out/"
  fi
  check I1 c_installed "首次准备、完整性、解析锁" 15
  m6e_log "现在以你自己的身份（不要 sudo）在另一个终端执行：open /Applications/ClaudeAbroad.app"
  checkpoint P1 "在安装引导里添加来源（本地配置文件选 M6E_NODES 那份）→ 选节点 → 应用主代理（按提示在终端执行 sudo abroad …）→ 完成" c_mainproxy "从引导里配好主代理"
  checkpoint P2 "点“启用”（地区会变时确认框等 3 秒再确认）。这一步会接管本机网络" c_enabled "启用"
  checkpoint P3 "点“一键回国”并确认（开发构建只预览、不结束 Claude）" c_restored "回国：网络恢复、解析锁仍在"
  check RB1 c_rb "回国后：回国拦截（测试段）" 15
  # 定向清理连接状态的写法（M6-11b，abroadd 的 --return-block-kill-states 默认关，等这一步的结论）：只对测试段做。
  step "定向清理：只清目的地在测试段里的连接状态（测试段没有真实连接）"
  if [ "$DRY_RUN" = 1 ]; then DRY_RUN=1 bash scripts/m6/e2e/pfcheck.sh kill; res K1 跳过 "空跑"; else
    if kout=$(bash scripts/m6/e2e/pfcheck.sh kill 2>&1); then res K1 通过 "pfctl -k 的写法被接受：$(printf '%s' "$kout" | tr '\n' ' ')"; else res K1 不通过 "pfctl -k 被拒：$(printf '%s' "$kout" | tr '\n' ' ')"; fi
  fi
  # 第三层演练（M6-4b-1 返工）：开发安装带 --dev-fake-claude-dir，第三层只针对那个目录下的进程。已回国时在里面启动一个
  # 测试进程（/bin/sleep 的副本，以你自己的身份运行），应在几秒内被结束；本机真实的 Claude 不受影响。
  FAKE_DIR="/Library/Application Support/claude-abroad/fake-claude"
  step "第三层演练：在测试目录里启动一个测试进程，看它被结束"
  m6e_do cp /bin/sleep "${FAKE_DIR}/claude-fake"
  if [ "$DRY_RUN" = 1 ]; then printf 'CHANGE: sudo -u <你> %s/claude-fake 120（后台）\n' "${FAKE_DIR}"; res L1 跳过 "空跑"; else
    sudo -u "${SUDO_USER}" "${FAKE_DIR}/claude-fake" 120 &
    fpid=$!
    gone=0
    for _ in $(seq 1 30); do kill -0 "${fpid}" 2>/dev/null || { gone=1; break; }; sleep 0.2; done
    if [ "${gone}" = 1 ]; then res L1 通过 "回国后测试进程在 6 秒内被第三层结束"; else res L1 不通过 "测试进程 6 秒内没被结束（第三层没生效？）"; wait "${fpid}" 2>/dev/null; fi
  fi
  m6e_do rm -f "${FAKE_DIR}/claude-fake"
  checkpoint P4 "再点“启用”" c_enabled "再启用（带着启用状态重启，看开机时的 guard）"
  m6e_state_set phase reboot
  m6e_deadman_stop # 重启会结束死人开关；第二段开头重新启动
  M6E_RESTORED=1
  step "第一段结束"
  printf 'WAIT: 重启电脑；开机后（先不要打开应用）执行 sudo scripts/m6/e2e/run.sh --after-reboot\n' >&3
  m6e_log "现在重启电脑。开机后会断网，直到 abroadd 接管（一般一两分钟）；如果一直没网，在终端执行 sudo scripts/m6/e2e/restore.sh（不需要网络）。"
  m6e_log "开机后（先不要打开应用）执行：sudo scripts/m6/e2e/run.sh --after-reboot"
  exit 0
fi

# 卸载、核对地区、收尾（第三段最后）。
finish() {
  step "卸载（提示时输入 uninstall）"
  if [ "$DRY_RUN" = 1 ]; then printf 'CHANGE: scripts/m6/install/uninstall.sh\n'; else
    if bash scripts/m6/install/uninstall.sh; then res U1 通过 "卸载后和安装前的快照逐项一致（含 pf 锚点和 pf 是否启用）"; else res U1 不通过 "卸载后和安装前的快照不一致（见上）"; fi
  fi
  step "核对地区"
  if [ "$DRY_RUN" = 1 ]; then res R1 跳过 "空跑"; else
    cur="${M6E_STATE}/region.now"
    rg_current "${SUDO_USER}" "${SUID}" >"${cur}"
    d=$(rg_diff "${M6E_STATE}/region" "${cur}" | tr '\n' ' ')
    if [ -z "$d" ]; then res R1 通过 "回国后地区和开始前一致"; else res R1 不通过 "回国后地区和开始前不一致：${d}（下面按快照还原）"; fi
  fi
  M6E_RESTORED=1
  trap - EXIT
  if [ "$DRY_RUN" = 1 ]; then DRY_RUN=1 bash "${RESTORE}"; else bash "${RESTORE}"; fi
  m6e_log "结果汇总（${RESULTS}）："
  [ "$DRY_RUN" = 1 ] || cat "${RESULTS}" >&2
  m6e_log "请贴回：${RESULTS}，以及 scripts/m6/e2e/out/ 和 scripts/m6/install/out/ 下的日志"
}

# ---- 第三段（已回国状态下再重启一次之后，M6-11b）----
if [ "${PHASE}" = 3 ]; then
  if [ "$DRY_RUN" != 1 ] && [ "$(m6e_state_get phase)" != reboot2 ]; then
    m6e_die "没有第二段的记录（${M6E_STATE}/phase 不是 reboot2）：先执行第二段"
  fi
  m6e_deadman_start "${DEADMAN_SECS}" "${RESTORE}"
  m6e_arm_restore_trap "${RESTORE}"
  m6e_state_set phase 3
  check B3 c_guard_rb "已回国时重启：开机时的 guard 加载回国拦截"
  check RB3 c_rb "重启后：回国拦截（测试段）" 180
  finish
  exit 0
fi

# ---- 第二段（重启后）----
if [ "$DRY_RUN" != 1 ] && [ "$(m6e_state_get phase)" != reboot ]; then
  m6e_die "没有第一段的记录（${M6E_STATE}/phase 不是 reboot）：先执行第一段"
fi
m6e_deadman_start "${DEADMAN_SECS}" "${RESTORE}"
m6e_arm_restore_trap "${RESTORE}"
m6e_state_set phase 2
check B1 c_guard_loaded "开机时的 guard"
check B2 c_enabled "abroadd 接管后恢复启用" 180
# 应急演练（M6-5b）：启用状态下停掉后台服务，执行应急程序，核对网络恢复、状态是已回国；再把后台服务注册回来。
RESCUE="/Library/Application Support/claude-abroad/bin/abroad-rescue"
D_PLIST=/Library/LaunchDaemons/com.claude-abroad.dev.abroadd.plist
step "应急演练：停掉后台服务（这时还是断网的），执行应急程序（看过清单后输入 rescue）"
m6e_do launchctl bootout system/com.claude-abroad.dev.abroadd
if [ "$DRY_RUN" = 1 ]; then printf 'CHANGE: %s\n' "${RESCUE}"; else
  if "${RESCUE}" </dev/tty; then res E1 通过 "应急程序走完"; else res E1 不通过 "应急程序退出码非零（输出见上）"; fi
fi
c_rescued() {
  local st code
  st=$(/usr/bin/python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1])).get("state",""))' "/Library/Application Support/claude-abroad/state/abroadd/state.json" 2>/dev/null)
  [ "${st}" = RESTORED ] || { echo "状态文件是 ${st:-读不出}"; return 1; }
  code=$(curl -sS -m 8 -o /dev/null -w '%{http_code}' https://www.gstatic.com/generate_204 2>/dev/null)
  [ "${code}" = 204 ] || { echo "本机网络还没恢复（直连探测 ${code:-无应答}）"; return 1; }
  echo "状态文件是已回国、本机网络恢复"
}
check E2 c_rescued "应急程序之后" 15
m6e_do launchctl bootstrap system "${D_PLIST}"
check E3 c_restored "后台服务回来后仍是已回国（不重新接管）、解析锁仍在" 60
check RB2 c_rb "应急程序之后：回国拦截（测试段）仍在" 30
checkpoint P5 "打开应用，确认显示“已回国”" c_restored "应用里是已回国"
m6e_state_set phase reboot2
m6e_deadman_stop # 重启会结束死人开关；第三段开头重新启动
M6E_RESTORED=1
step "第二段结束"
printf 'WAIT: 再重启一次电脑（这次是已回国状态，看开机时的 guard 加载回国拦截）；开机后执行 sudo scripts/m6/e2e/run.sh --after-reboot-2\n' >&3
m6e_log "现在再重启一次电脑（已回国状态，开机后不会断网）。开机后执行：sudo scripts/m6/e2e/run.sh --after-reboot-2"
exit 0
