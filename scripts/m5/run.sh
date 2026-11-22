#!/usr/bin/env bash
# M5 第二级走查（真机，M5-11，m5-plan v2.11 决定 1）：在这台 Mac 上以真实模式跑 abroadd（地区同步真正接上），
# 本地接口放在共享目录 /var/run/claude-abroad，用户以自己的身份打开打包好的应用，从安装引导配好主代理到启用、停用、
# 一键回国，所有操作都在应用里完成（设计第 14 节 M5 的完成标准）；脚本在每个检查点经接口核对结果。
# 会改动本机网络和时区、需要 sudo，会话会断开：由用户在不需要 Claude Code 会话的时候执行；主会话只做语法检查、
# DRY_RUN=1 空跑、--check-only 和自测。用法见 scripts/m5/README.md。
#
#   sudo scripts/m5/run.sh                # 正式运行
#   scripts/m5/run.sh --check-only        # 只做前提检查（只读，不需要 root）
#   DRY_RUN=1 scripts/m5/run.sh           # 空跑：只打印会执行的改动命令（CHANGE:）、查询（QUERY:）、接口调用（API:）
#
# 环境变量：
#   M5_NODES                 主代理节点的 Clash YAML（在应用里用“本地配置文件”添加它；必填）
#   M5_M2_PASSED=1、M5_M3_PASSED=1、M5_M4_PASSED=1   确认 M2、M3、M4 的真机脚本已经通过（必填）
#   M5_REAL_KILL=1           回国时真的结束 Claude 进程（默认只预览，不发信号）
#   M5_EXIT_HOST             出口检查主机（默认 www.gstatic.com）
#   M5_DEADMAN_SECS          死人开关（默认 2700 秒）
#   M5_YES=1                 跳过开始前的按键确认（不建议）
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
. scripts/m5/lib.sh
. scripts/m4/region.sh

exec 3>&1
CHECK_ONLY=0
case "${1:-}" in
  --check-only) CHECK_ONLY=1 ;;
  "") ;;
  *) echo "用法：sudo $0 [--check-only]" >&2; exit 2 ;;
esac

OUT="${ROOT}/scripts/m5/out"
TS="$(date +%Y%m%d-%H%M%S)"
mkdir -p "${OUT}"
M5_LOG="${OUT}/m5-${TS}.log"
RESULTS="${OUT}/m5-results-${TS}.txt"
: >"${M5_LOG}"
: >"${RESULTS}"
BIN="${ROOT}/scripts/dev/out/bin"
APP="${ROOT}/app/.build/bundle/ClaudeAbroad.app"
EXIT_HOST="${M5_EXIT_HOST:-www.gstatic.com}"
DEADMAN_SECS="${M5_DEADMAN_SECS:-2700}"
REAL_KILL="${M5_REAL_KILL:-0}"
API="${M5_SOCKDIR}/api.sock"
INSTBIN="${M5_INSTALL}/bin"
RESTORE="${ROOT}/scripts/m5/restore.sh"

res() { printf '%-4s %-6s %s\n' "$1" "$2" "$3" | tee -a "${RESULTS}" >&2; }
step() { printf 'STEP: %s\n' "$*" >&3; m5_log "$*"; }
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
get() { # get <路径>：以 root 读本地接口（完全信任的调用方，只读）
  if [ "$DRY_RUN" = 1 ]; then printf 'API: GET %s\n' "$1" >&3; echo '{}'; return; fi
  curl -sS -m 10 --unix-socket "${API}" "http://abroadd$1" 2>/dev/null || echo '{}'
}

# checkpoint <编号> <要用户在应用里做的事> <核对函数> <说明>：等用户在应用里做完按回车，再经接口核对；不通过时可以重试或跳过。
checkpoint() {
  local id=$1 todo=$2 fn=$3 what=$4 ans why
  step "检查点 ${id}：${todo}"
  if [ "$DRY_RUN" = 1 ]; then printf 'WAIT: %s\n' "${todo}" >&3; res "${id}" 跳过 "空跑"; return; fi
  while true; do
    m5_log "在应用里：${todo}。做完按回车核对（输入 s 跳过）。"
    read -r ans </dev/tty || ans=s
    [ "${ans}" = s ] && { res "${id}" 跳过 "用户跳过：${what}"; return; }
    if why=$("${fn}"); then res "${id}" 通过 "${what}：${why}"; return; fi
    m5_log "还不满足：${why}。可以在应用里再看看，按回车重新核对，输入 s 跳过。"
  done
}

precheck() {
  local reasons=() conf
  conf=$(netstat -rn -f inet 2>/dev/null | awk '
    $4 ~ /^(utun|ipsec|ppp|tun|tap|wg)[0-9]+$/ {
      d=$1
      if (d=="default") { print $4; next }
      if (d ~ /^(10|127)(\.|\/|$)/ || d ~ /^169\.254/ || d ~ /^192\.168/ || d ~ /^172\.(1[6-9]|2[0-9]|3[01])(\.|\/|$)/ || d ~ /^(22[4-9]|23[0-9]|24[0-9]|25[0-5])(\.|\/|$)/) next
      print $4
    }' | sort -u | tr '\n' ' ')
  [ -n "${conf}" ] && reasons+=("别的代理软件的 TUN 或 VPN 开着（接口：${conf% }）：请先关掉 Clash 等软件的 TUN 模式和 VPN")
  m5_system_proxy_on && reasons+=("系统代理开着：请在“系统设置 → 网络 → 详细信息 → 代理”里全部关掉")
  [ -e "${M5_STATE}/abroadd/state.json" ] && reasons+=("上一次运行留下的 abroadd 状态文件还在：请先执行 sudo scripts/m5/restore.sh")
  for t in m2 m3 m4; do
    [ -e "/var/run/claude-abroad-${t}/abroadd/state.json" ] && reasons+=("${t} 脚本留下的 abroadd 状态文件还在：请先执行 sudo scripts/${t}/restore.sh")
  done
  [ -e "${M5_SOCKDIR}" ] && reasons+=("${M5_SOCKDIR} 已经存在（别的 abroadd 或上一次运行）：请先执行 sudo scripts/m5/restore.sh，或确认它不是别的程序的")
  [ "${M5_M2_PASSED:-0}" = 1 ] || reasons+=("没有确认 M2 的真机脚本已经通过：通过后设 M5_M2_PASSED=1")
  [ "${M5_M3_PASSED:-0}" = 1 ] || reasons+=("没有确认 M3 的真机脚本已经通过：通过后设 M5_M3_PASSED=1")
  [ "${M5_M4_PASSED:-0}" = 1 ] || reasons+=("没有确认 M4 的真机脚本已经通过：通过后设 M5_M4_PASSED=1")
  if [ -z "${M5_NODES:-}" ] || [ ! -f "${M5_NODES:-/nonexistent}" ]; then
    reasons+=("没有主代理：请设置 M5_NODES（远端或局域网节点的 Clash YAML，在应用里用“本地配置文件”添加它）")
  fi
  for f in abroad-vm abroadd abroad; do
    [ -x "${BIN}/${f}" ] || reasons+=("缺少 ${BIN}/${f}：见 README 的准备步骤")
  done
  [ -x "${APP}/Contents/MacOS/ClaudeAbroad" ] || reasons+=("缺少应用包 ${APP}：先执行 scripts/dev/app-bundle.sh")
  [ -f guest/out/Image ] || reasons+=("缺少虚拟机镜像 guest/out：先跑一次 scripts/dev/vm-up.sh --no-panic")
  m5_log "回国时 Claude 进程：$([ "${REAL_KILL}" = 1 ] && echo "M5_REAL_KILL=1：会被强制结束，未保存的内容会丢" || echo "默认只预览，不结束")"
  m5_log "提醒：运行期间这台 Mac 会断网（启用时）、时区会被改（结束时还原）。必须在不需要 Claude Code 会话的时候执行。"
  if [ ${#reasons[@]} -gt 0 ]; then
    for r in "${reasons[@]}"; do m5_log "前提不满足：${r}"; done
    return 1
  fi
  m5_log "前提检查通过"
  return 0
}

if [ "${CHECK_ONLY}" = 1 ]; then m5_test_guard check; else m5_test_guard; fi
if [ "${CHECK_ONLY}" = 1 ]; then
  precheck
  exit $?
fi
if ! precheck; then
  if [ "$DRY_RUN" = 1 ]; then m5_log "（空跑：前提不满足，继续打印后面的命令）"; else exit 3; fi
fi
m5_require_root
[ "$DRY_RUN" = 1 ] && SUDO_USER=${SUDO_USER:-$(id -un)}
if [ "$DRY_RUN" != 1 ] && [ "${M5_YES:-0}" != 1 ]; then
  m5_log "按回车开始（Ctrl+C 取消）。"
  read -r _ </dev/tty
fi
SUID=$(id -u "${SUDO_USER}" 2>/dev/null || echo 501)

# ---- 先建状态目录，紧接着启动死人开关；其余改动都在它之后 ----
m5_state_init
m5_deadman_start "${DEADMAN_SECS}" "${RESTORE}"
m5_arm_restore_trap "${RESTORE}"

# ---- 快照：网络和地区，各有持久副本；restore.sh 只靠它们还原，不依赖 abroadd ----
m5_do mkdir -p "${M5_STATE}/snap"
if [ "$DRY_RUN" = 1 ]; then printf 'STATE: snap/ipv6、routes、dns、pf\n'; else
  : >"${M5_STATE}/snap/ipv6"
  while IFS= read -r s; do
    [ -n "$s" ] || continue
    printf '%s\t%s\n' "$s" "$(networksetup -getinfo "$s" 2>/dev/null | awk -F': ' '/^IPv6: /{print $2}')" >>"${M5_STATE}/snap/ipv6"
  done < <(m5_services)
  netstat -rn -f inet >"${M5_STATE}/snap/routes" 2>&1
  printf 'show %s\nquit\n' "${M5_DNS_KEY}" | scutil >"${M5_STATE}/snap/dns" 2>&1
  pfctl -s info 2>/dev/null | head -1 >"${M5_STATE}/snap/pf"
fi
rg_snapshot "${M5_STATE}/snap/region" "${SUDO_USER}" "${SUID}" || m5_die "地区有读不出来的项，没有原值就不能保证还原，不继续"
m5_do mkdir -p "${M5_SNAP}"
m5_do chmod 700 "${M5_SNAP}"
m5_do cp -R "${M5_STATE}/snap/." "${M5_SNAP}/"
m5_log "已拍快照：网络（IPv6、路由表、DNS 键、pf）和地区（时区、自动时区、语言、地区格式），另存一份到 ${M5_SNAP}，重启不丢"

# ---- 安装程序、建测试用户、启动 abroad-vm（主代理由 abroadd 在应用里配好后下发，这里不预先推节点）----
m5_do mkdir -p "${INSTBIN}" "${M5_INSTALL}/image"
for f in abroad-vm abroadd abroad; do m5_do cp "${BIN}/${f}" "${INSTBIN}/${f}"; done
m5_do cp -R guest/out/. "${M5_INSTALL}/image/"
m5_do chown -R root:wheel "${M5_INSTALL}"
m5_do chmod -R go-w "${M5_INSTALL}"
m5_user_create
m5_do mkdir -p "${M5_VMDIR}"
m5_do chown "${M5_USER}" "${M5_VMDIR}"
m5_do chmod 700 "${M5_VMDIR}"
m5_do mkdir -p "${M5_STORE}"
m5_do chmod 700 "${M5_STORE}"
VMARGS=(--image-dir "${M5_INSTALL}/image" --state-dir "${M5_VMDIR}" --direct-sock "${M5_VMDIR}/direct.sock" --direct-peer-uid 0)
if [ "$DRY_RUN" = 1 ]; then
  printf 'CHANGE: sudo -u %s %s/abroad-vm run %s\n' "${M5_USER}" "${INSTBIN}" "${VMARGS[*]}"
else
  sudo -u "${M5_USER}" "${INSTBIN}/abroad-vm" run "${VMARGS[@]}" >>"${OUT}/m5-abroad-vm-${TS}.log" 2>&1 &
  ok=0
  for _ in $(seq 1 90); do
    curl -fsS -m 3 --unix-socket "${M5_VMDIR}/ctl.sock" http://abroad-agent/v1/health >/dev/null 2>&1 && { ok=1; break; }
    sleep 1
  done
  [ $ok = 1 ] || m5_die "abroad-vm 以测试用户 ${M5_USER} 起不来，见 m5-abroad-vm-${TS}.log"
fi

# ---- 共享 socket 目录和命令行（终端退路给的命令是 sudo abroad …）----
m5_do mkdir -p "${M5_SOCKDIR}"
m5_do chown root:wheel "${M5_SOCKDIR}"
m5_do chmod 755 "${M5_SOCKDIR}"
m5_state_set sockdir "${M5_SOCKDIR}"
if [ "$DRY_RUN" = 1 ] || [ ! -e "${M5_CLI_LINK}" ]; then
  m5_do mkdir -p /usr/local/bin
  m5_do ln -s "${INSTBIN}/abroad" "${M5_CLI_LINK}"
  m5_state_set cli_link "${M5_CLI_LINK}"
else
  m5_log "${M5_CLI_LINK} 已经存在，不覆盖：终端退路的命令请把 abroad 换成 ${INSTBIN}/abroad"
fi

# ---- 安装开发用 LaunchDaemon 启动 abroadd（真实模式；不带 --dev-skip-region；默认带 --dev-skip-claude-kill）----
PL="${M5_STATE}/${M5_LABEL}.plist"
SKIPKILL='    <string>--dev-skip-claude-kill</string>'
[ "${REAL_KILL}" = 1 ] && SKIPKILL=''
if [ "$DRY_RUN" = 1 ]; then
  printf 'STATE: %s（由 launchd.plist.tmpl 生成）\n' "${PL}"
  printf 'ABROADD: --state-dir %s/abroadd --vm-dir %s --main-proxy-store %s --exit-check-host %s --pf-user %s --socket-dir %s%s\n' \
    "${M5_STATE}" "${M5_VMDIR}" "${M5_STORE}" "${EXIT_HOST}" "${M5_USER}" "${M5_SOCKDIR}" "$([ "${REAL_KILL}" = 1 ] || echo ' --dev-skip-claude-kill')"
else
  sed -e "s|@ABROADD@|${INSTBIN}/abroadd|" -e "s|@STATE@|${M5_STATE}/abroadd|" -e "s|@VMDIR@|${M5_VMDIR}|" \
    -e "s|@STORE@|${M5_STORE}|" -e "s|@EXIT@|${EXIT_HOST}|" -e "s|@USER@|${M5_USER}|" -e "s|@SOCKDIR@|${M5_SOCKDIR}|" \
    -e "s|@LOG@|${OUT}/m5-abroadd-${TS}.log|" -e "s|@LABEL@|${M5_LABEL}|" -e "s|^@SKIPKILL@$|${SKIPKILL}|" scripts/m5/launchd.plist.tmpl >"${PL}"
fi
m5_do cp "${PL}" "${M5_PLIST}"
m5_do chown root:wheel "${M5_PLIST}"
m5_do chmod 644 "${M5_PLIST}"
m5_do launchctl bootstrap system "${M5_PLIST}"
if [ "$DRY_RUN" != 1 ]; then
  for _ in $(seq 1 30); do [ -S "${API}" ] && break; sleep 1; done
  [ -S "${API}" ] || m5_die "30 秒内 abroadd 没起来，见 m5-abroadd-${TS}.log"
fi

# ---- 检查点：用户在应用里操作，脚本经接口核对（设计第 14 节 M5 的完成标准）----
m5_log "现在以你自己的身份（不要 sudo）在另一个终端执行：open \"${APP}\"；然后按下面的检查点在应用里操作。"
m5_log "需要本人授权的操作，应用会给出 sudo abroad … 的命令：复制到终端执行（会问密码）。主代理节点文件：${M5_NODES:-<M5_NODES>}"

c_service() { local s; s=$(get /v1/setup); [ "$(jget "$s" steps.0.done)" = true ] && echo "后台服务一步已完成" || { echo "引导的后台服务一步没完成：$(jget "$s" steps)"; return 1; }; }
c_mainproxy() {
  local s m; s=$(get /v1/setup); m=$(get /v1/main-proxy)
  [ "$(jget "$m" sources)" != "" ] && [ "$(jget "$m" sources)" != 0 ] || { echo "还没有来源"; return 1; }
  [ -n "$(jget "$m" selected.id)" ] || { echo "还没有选择节点"; return 1; }
  [ "$(jget "$s" steps.1.done)" = true ] || { echo "还没有应用主代理（出口基准没建立）"; return 1; }
  echo "有 $(jget "$m" sources) 个来源，选了节点，出口基准已建立"
}
c_setupdone() { local s; s=$(get /v1/setup); [ "$(jget "$s" completed)" = true ] && echo "安装引导已完成" || { echo "引导没完成"; return 1; }; }
c_enabled() {
  local st e; st=$(get /v1/status); e=$(get /v1/main-proxy/exit)
  [ "$(jget "$st" state)" = ENABLED ] && [ "$(jget "$st" gate_open)" = true ] || { echo "状态 $(jget "$st" state)，闸门 $(jget "$st" gate_open)"; return 1; }
  [ "$(jget "$e" available)" = true ] || { echo "出口信息还没有"; return 1; }
  echo "已启用、闸门开、出口在 $(jget "$e" country)"
}
c_disabled() { local st; st=$(get /v1/status); [ "$(jget "$st" state)" = DISABLED ] && echo "已停用" || { echo "状态 $(jget "$st" state)"; return 1; }; }
c_restored() { local st; st=$(get /v1/status); [ "$(jget "$st" state)" = RESTORED ] && echo "已回国" || { echo "状态 $(jget "$st" state)"; return 1; }; }

checkpoint P1 "打开应用，进入安装引导；在“后台服务”一步看到已连上，点“下一步”" c_service "引导的后台服务一步"
checkpoint P2 "在“配置主代理”一步：添加来源（本地配置文件选 M5_NODES 那份）→ 选出口节点 → 点“应用主代理”（按提示在终端执行 sudo abroad … 的命令）" c_mainproxy "从引导里配好主代理"
checkpoint P3 "点“完成”，进入首页" c_setupdone "完成安装引导"
checkpoint P4 "在菜单栏图标或首页点“启用”（地区会变时确认框等 3 秒再确认；要本人授权时按提示在终端执行）。注意：这一步会接管本机网络" c_enabled "在应用里启用"
checkpoint P5 "点“停用”" c_disabled "在应用里停用"
checkpoint P6 "再点“启用”，然后点“一键回国”，在确认框里确认（要本人授权时按提示在终端执行）" c_restored "在应用里一键回国"
step "检查点 P7：通知和菜单栏图标（人工看，脚本不核对）：启用后图标变成飞机，停用后是 Wi-Fi 划线，回国后是房子；回国完成时弹出“已回国”的通知（应用包第一次发通知时系统会询问权限）"
res P7 人工 "图标和通知按 docs/m5-walkthrough.md 第二级 P7 回填"

# ---- 收尾：restore.sh（按脚本自己的快照再兜底一遍）----
m5_log "收尾：执行 restore.sh"
M5_RESTORED=1
if [ "$DRY_RUN" = 1 ]; then DRY_RUN=1 bash "${RESTORE}"; else bash "${RESTORE}"; fi
m5_log "结果汇总（${RESULTS}）："
cat "${RESULTS}" >&2
m5_log "请贴回：${M5_LOG}、${RESULTS}，以及 scripts/m5/out/m5-abroadd-${TS}.log、m5-abroad-vm-${TS}.log"
