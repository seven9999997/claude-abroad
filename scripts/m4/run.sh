#!/usr/bin/env bash
# M4 真机验证（M4-9，m4-plan v2.13、v2.14）：在这台 Mac 上以真实模式跑 abroadd，地区同步真正接上（不带
# --dev-skip-region），验证 R4（时区、自动时区，可选语言）、环境检测 17 项和三种人为泄漏、真实服务商的分流出口（R5）、
# 订阅出口的 UDP 域名目标、按出口检查与回退、第 10 项与本机真实 IP 不落盘、R10、R17、结束 Claude 的预览、切换地区、R7。
# 会改动本机网络和时区、需要 sudo，由用户在不需要 Claude Code 会话的时候执行；主会话只做语法检查、DRY_RUN=1 空跑、
# --check-only 和自测。用法见 scripts/m4/README.md。
#
#   sudo scripts/m4/run.sh                # 正式运行
#   scripts/m4/run.sh --check-only        # 只做前提检查（只读，不需要 root）
#   DRY_RUN=1 scripts/m4/run.sh           # 空跑：只打印会执行的改动命令（CHANGE:）、查询（QUERY:）、接口调用（API:）
#
# 环境变量：
#   M4_NODES、M4_NODE        主代理节点的 Clash YAML 和节点名（必填，同 M2、M3）
#   M4_M2_PASSED=1、M4_M3_PASSED=1   确认 M2、M3 的真机脚本已经通过（必填）
#   M4_TZ                    目标时区的显式覆盖（默认取“应用主代理”时按出口算出的时区）
#   M4_CHANGE_LANGUAGE=1     也改语言（默认不改：语言和地区格式保持你现在的）；M4_LANG 目标语言，默认 en-US
#   M4_REAL_KILL=1           恢复时真的结束 Claude 进程（默认只预览，不发信号）
#   M4_WG_CONF、M4_OVPN_CONF 真实服务商的 WireGuard .conf、OpenVPN .ovpn（没给的步骤记“跳过”）
#   M4_SUB                   订阅的 Clash YAML（作为订阅出口；没给记“跳过”）
#   M4_EXIT_HOST             出口检查主机（默认 www.gstatic.com）
#   M4_DEADMAN_SECS          死人开关（默认 2700 秒）
#   M4_R7_SECS               停用后观察推送和对时的秒数（默认 120）
#   M4_CAPTIVE=1             额外做 R16（需要认证的 Wi-Fi，人工操作）
#   M4_YES=1                 跳过开始前的按键确认（不建议）
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
. scripts/m4/lib.sh
. scripts/m4/region.sh
. scripts/m4/judge.sh

exec 3>&1
CHECK_ONLY=0
case "${1:-}" in
  --check-only) CHECK_ONLY=1 ;;
  "") ;;
  *) echo "用法：sudo $0 [--check-only]" >&2; exit 2 ;;
esac

OUT="${ROOT}/scripts/m4/out"
TS="$(date +%Y%m%d-%H%M%S)"
mkdir -p "${OUT}"
M4_LOG="${OUT}/m4-${TS}.log"
SAMPLES="${OUT}/m4-samples-${TS}.txt"
RESULTS="${OUT}/m4-results-${TS}.txt"
: >"${M4_LOG}"
: >"${SAMPLES}"
: >"${RESULTS}"
BIN="${ROOT}/scripts/dev/out/bin"
EXIT_HOST="${M4_EXIT_HOST:-www.gstatic.com}"
DEADMAN_SECS="${M4_DEADMAN_SECS:-2700}"
R7_SECS="${M4_R7_SECS:-120}"
CHANGE_LANG="${M4_CHANGE_LANGUAGE:-0}"
REAL_KILL="${M4_REAL_KILL:-0}"
API="${M4_STATE}/abroadd/api.sock"
INSTBIN="${M4_INSTALL}/bin"
RESTORE="${ROOT}/scripts/m4/restore.sh"
IPECHO="https://api.ipify.org"
IPECHO_HOST="api.ipify.org"

res() { printf '%-4s %-6s %s\n' "$1" "$2" "$3" | tee -a "${RESULTS}" >&2; }
res_judge() { case "$2" in 0) res "$1" 通过 "$3" ;; 2) res "$1" 跳过 "$3" ;; *) res "$1" 不通过 "$3" ;; esac; }
step() { printf 'STEP: %s\n' "$*" >&3; m4_log "$*"; } # 空跑里看得到的步骤说明
sample() { # sample <标题> <文本>：脱敏后写进采样文件
  [ "$DRY_RUN" = 1 ] && return 0
  printf '\n==== %s ====\n%s\n' "$1" "$2" | m4_redact >>"${SAMPLES}"
}
as_user() { if [ "$DRY_RUN" = 1 ]; then printf 'USER: %s\n' "$*" >&3; return 0; fi; sudo -u "${SUDO_USER}" "$@"; }
api() { # api <方法> <路径> [请求体文件]：输出 HTTP 状态码，应答体在 ${M4_STATE}/api.out
  if [ "$DRY_RUN" = 1 ]; then printf 'API: %s %s\n' "$1" "$2" >&3; echo 200; return 0; fi
  local data=()
  [ -n "${3:-}" ] && data=(--data-binary "@$3")
  curl -sS -m 300 --unix-socket "${API}" -X "$1" ${data[@]+"${data[@]}"} -o "${M4_STATE}/api.out" -w '%{http_code}' "http://abroadd$2" 2>/dev/null
}
api_body() { [ "$DRY_RUN" = 1 ] && { echo '{}'; return; }; cat "${M4_STATE}/api.out" 2>/dev/null || echo '{}'; }
# json_with_confirm <JSON> <确认码>：给请求体加上 confirm 字段。
json_with_confirm() {
  printf '%s' "$1" | /usr/bin/perl -MJSON::PP -e 'my $d = JSON::PP->new->decode(do { local $/; <STDIN> } || "{}"); $d->{confirm} = $ARGV[0]; print JSON::PP->new->canonical->encode($d)' "$2"
}
# confirm_call <方法> <路径> <JSON 请求体>：两步确认（m4-plan v2.3 决定 5、v2.4）。第一次回 409 时取确认码再调一次；
# 确认码只写进 0600 的临时文件当请求体，用完就删。最终状态码放在 RCODE，第一次的 409 应答放在 PREVIEW。
confirm_call() {
  local f="${M4_STATE}/req.json" c cc
  PREVIEW=""
  if [ "$DRY_RUN" = 1 ]; then
    printf 'API: %s %s（第一步：拿预览和确认码）\n' "$1" "$2" >&3
    printf 'API: %s %s（第二步：带确认码）\n' "$1" "$2" >&3
    RCODE=200; return 0
  fi
  (umask 077 && printf '%s' "$3" >"$f")
  c=$(api "$1" "$2" "$f")
  if [ "$c" = 409 ]; then
    PREVIEW=$(api_body)
    cc=$(jget "${PREVIEW}" confirm)
    if [ ${#cc} = 32 ]; then
      (umask 077 && json_with_confirm "$3" "$cc" >"$f")
      c=$(api "$1" "$2" "$f")
    fi
  fi
  rm -f "$f"
  RCODE=$c
}
status_json() { [ "$DRY_RUN" = 1 ] && { echo '{}'; return; }; curl -sS -m 10 --unix-socket "${API}" http://abroadd/v1/status 2>/dev/null || echo '{}'; }
gate_open() { jget "$(status_json)" gate_open; }
envrun() { # 跑一轮环境检测，输出报告 JSON
  local c; c=$(api POST /v1/envcheck/run)
  [ "$DRY_RUN" = 1 ] && { echo '{}'; return; }
  [ "$c" = 200 ] && api_body || echo '{}'
}
secret_put() { [ "$DRY_RUN" = 1 ] && return 0; (umask 077 && printf '%s\n' "$2" >"${M4_SECRET}/$1"); }
secret_get() { [ "$DRY_RUN" = 1 ] && return 0; cat "${M4_SECRET}/$1" 2>/dev/null; }
eqv() { [ -z "$1" ] || [ -z "$2" ] && { echo none; return; }; [ "$1" = "$2" ] && echo eq || echo ne; } # IP 只比相等
my_ip() { as_user curl -sS -m 15 "${IPECHO}" 2>/dev/null | tr -d '[:space:]' | grep -E '^[0-9a-fA-F:.]+$' || true; }
primary_service() { # 默认路由的网卡对应的网络服务名
  local dev; dev=$(m4_default_iface)
  networksetup -listnetworkserviceorder 2>/dev/null | awk -v d="$dev" '/^\([0-9]+\) /{sub(/^\([0-9]+\) /,""); s=$0} /Device: /{if (index($0, "Device: " d ")")) {print s; exit}}'
}

# ---- 前提检查（只读，不需要 root）----
precheck() {
  local reasons=() line conf n
  conf=$(netstat -rn -f inet 2>/dev/null | awk '
    $4 ~ /^(utun|ipsec|ppp|tun|tap|wg)[0-9]+$/ {
      d=$1
      if (d=="default") { print $4; next }
      if (d ~ /^(10|127)(\.|\/|$)/ || d ~ /^169\.254/ || d ~ /^192\.168/ || d ~ /^172\.(1[6-9]|2[0-9]|3[01])(\.|\/|$)/ || d ~ /^(22[4-9]|23[0-9]|24[0-9]|25[0-5])(\.|\/|$)/) next
      print $4
    }' | sort -u | tr '\n' ' ')
  [ -n "${conf}" ] && reasons+=("别的代理软件的 TUN 或 VPN 开着（接口：${conf% }）：请先关掉 Clash 等软件的 TUN 模式和 VPN")
  m4_system_proxy_on && reasons+=("系统代理开着：请在“系统设置 → 网络 → 详细信息 → 代理”里全部关掉")
  [ -e "${M4_STATE}/abroadd/state.json" ] && reasons+=("上一次运行留下的 abroadd 状态文件还在：请先执行 sudo scripts/m4/restore.sh")
  for t in m2 m3; do
    [ -e "/var/run/claude-abroad-${t}/abroadd/state.json" ] && reasons+=("${t} 脚本留下的 abroadd 状态文件还在：请先执行 sudo scripts/${t}/restore.sh")
  done
  [ "${M4_M2_PASSED:-0}" = 1 ] || reasons+=("没有确认 M2 的真机脚本已经通过：通过后设 M4_M2_PASSED=1")
  [ "${M4_M3_PASSED:-0}" = 1 ] || reasons+=("没有确认 M3 的真机脚本已经通过：通过后设 M4_M3_PASSED=1")
  if [ -z "${M4_NODES:-}" ] || [ ! -f "${M4_NODES:-/nonexistent}" ] || [ -z "${M4_NODE:-}" ]; then
    reasons+=("没有主代理：请设置 M4_NODES（远端或局域网节点的 Clash YAML）和 M4_NODE（节点名）")
  elif [ -x "${BIN}/abroad-dev" ]; then
    local allow
    if ! allow=$("${BIN}/abroad-dev" apply-main-proxy --nodes "${M4_NODES}" --selected "${M4_NODE}" --print-allow 2>&1); then
      reasons+=("主代理节点不可用：$(printf '%s' "${allow}" | head -1 | m4_redact)")
    elif printf '%s\n' "${allow}" | grep -q '^127\.'; then
      reasons+=("主代理的第一跳是本机回环：请换成远端或局域网节点")
    fi
  fi
  for v in M4_WG_CONF M4_OVPN_CONF M4_SUB; do
    if [ -n "${!v:-}" ] && [ ! -f "${!v}" ]; then reasons+=("${v} 指向的文件不存在"); fi
  done
  while IFS= read -r line; do
    [ -n "${line}" ] || continue
    if networksetup -getinfo "${line}" 2>/dev/null | grep -q '^IPv6: Manual'; then
      reasons+=("网络服务的 IPv6 是手动配置：请先改成自动")
      break
    fi
  done < <(m4_services)
  for f in abroad-vm abroadd abroad-dev; do
    [ -x "${BIN}/${f}" ] || reasons+=("缺少 ${BIN}/${f}：见 README 的准备步骤")
  done
  [ -f guest/out/Image ] || reasons+=("缺少虚拟机镜像 guest/out：先跑一次 scripts/dev/vm-up.sh --no-panic")
  # 将要改成的时区、会被预览或结束的进程数（只读估算）。
  m4_log "目标时区：${M4_TZ:-（取“应用主代理”时按出口算出的时区）}；语言：$([ "${CHANGE_LANG}" = 1 ] && echo "改成 ${M4_LANG:-en-US}" || echo 不改（保持你现在的语言和地区格式）)"
  if [ -x "${BIN}/abroad-dev" ]; then
    n=$("${BIN}/abroad-dev" claude-facts --count 2>/dev/null || echo '{}')
    m4_log "现在能看到的 Claude 进程：$(c=$(jget "$n" targets); echo "${c:-未知（abroad-dev 不是最新的，请按准备步骤重新编出）}") 个$([ "${REAL_KILL}" = 1 ] && echo "（M4_REAL_KILL=1：恢复时会被强制结束，未保存的内容会丢）" || echo "（默认只预览，不结束）")"
  fi
  m4_log "提醒：运行期间这台 Mac 会断网多次、时区会被改（结束时还原）。必须在不需要 Claude Code 会话的时候执行。"
  if [ ${#reasons[@]} -gt 0 ]; then
    for r in "${reasons[@]}"; do m4_log "前提不满足：${r}"; done
    return 1
  fi
  m4_log "前提检查通过"
  return 0
}

if [ "${CHECK_ONLY}" = 1 ]; then m4_test_guard check; else m4_test_guard; fi
if [ "${CHECK_ONLY}" = 1 ]; then
  precheck
  exit $?
fi
if ! precheck; then
  if [ "$DRY_RUN" = 1 ]; then m4_log "（空跑：前提不满足，继续打印后面的命令）"; else exit 3; fi
fi
m4_require_root
[ "$DRY_RUN" = 1 ] && SUDO_USER=${SUDO_USER:-$(id -un)}
if [ "$DRY_RUN" != 1 ] && [ "${M4_YES:-0}" != 1 ]; then
  m4_log "确认上面的时区、语言和进程数，按回车开始（Ctrl+C 取消）。"
  read -r _ </dev/tty
fi
SUID=$(id -u "${SUDO_USER}" 2>/dev/null || echo 501)

# ---- 先建状态目录，紧接着启动死人开关；其余改动都在它之后 ----
m4_state_init
m4_deadman_start "${DEADMAN_SECS}" "${RESTORE}"
m4_arm_restore_trap "${RESTORE}"
GW_BEFORE="$(m4_default_gw)"
IF_BEFORE="$(m4_default_iface)"
[ "$DRY_RUN" = 1 ] && IF_BEFORE=${IF_BEFORE:-en0}
SVC="$(primary_service)"
[ "$DRY_RUN" = 1 ] && SVC=${SVC:-Wi-Fi}

# ---- 快照：网络（同 M3）和地区（region.sh），各有持久副本；restore.sh 只靠它们还原，不依赖 abroadd ----
snapshot() {
  m4_do mkdir -p "${M4_STATE}/snap"
  if [ "$DRY_RUN" = 1 ]; then printf 'STATE: snap/ipv6、routes、dns、pf、proxy\n'; else
    local s
    : >"${M4_STATE}/snap/ipv6"
    while IFS= read -r s; do
      [ -n "$s" ] || continue
      printf '%s\t%s\n' "$s" "$(networksetup -getinfo "$s" 2>/dev/null | awk -F': ' '/^IPv6: /{print $2}')" >>"${M4_STATE}/snap/ipv6"
    done < <(m4_services)
    netstat -rn -f inet >"${M4_STATE}/snap/routes" 2>&1
    printf 'show %s\nquit\n' "${M4_DNS_KEY}" | scutil >"${M4_STATE}/snap/dns" 2>&1
    pfctl -s info 2>/dev/null | head -1 >"${M4_STATE}/snap/pf"
    { printf 'service=%s\n' "${SVC}"; networksetup -getsecurewebproxy "${SVC}" 2>/dev/null | sed -e 's/: /=/'; } >"${M4_STATE}/snap/proxy"
  fi
  rg_snapshot "${M4_STATE}/snap/region" "${SUDO_USER}" "${SUID}" || m4_die "地区有读不出来的项，没有原值就不能保证还原，不继续"
}
snapshot
m4_do mkdir -p "${M4_SNAP}"
m4_do chmod 700 "${M4_SNAP}"
m4_do cp -R "${M4_STATE}/snap/." "${M4_SNAP}/"
m4_log "已拍快照：网络（IPv6、路由表、DNS 键、pf、代理设置）和地区（时区、自动时区、语言、地区格式），另存一份到 ${M4_SNAP}，重启不丢"
sample "启用前的地区（R4 的原值）" "$(cat "${M4_STATE}/snap/region" 2>/dev/null | grep -v '^uid=' | sed 's/^user=.*/user=<登录用户>/')"
sample "系统版本" "$(sw_vers 2>/dev/null)"

# ---- 本机网络还正常时：本机真实公网 IP、回显服务的地址（只在 0700 的子目录里，用完即删；v2.14 建议改 4）----
m4_do mkdir -p "${M4_SECRET}"
m4_do chmod 700 "${M4_SECRET}"
REAL_IP=$(my_ip)
secret_put realip "${REAL_IP}"
ECHO_IPS=$(as_user /usr/bin/dig +short "${IPECHO_HOST}" A 2>/dev/null | grep -E '^[0-9.]+$' | tr '\n' ' ')
secret_put echoips "${ECHO_IPS}"
[ "$DRY_RUN" = 1 ] || m4_log "本机真实公网 IP：$([ -n "${REAL_IP}" ] && echo 已测得 || echo 没测到)"

# ---- 安装程序（正式构建的 abroad-vm，不带开发标记；v2.14 建议改 7）、建测试用户、准备主代理 ----
m4_do mkdir -p "${INSTBIN}" "${M4_INSTALL}/image"
for f in abroad-vm abroadd abroad-dev; do m4_do cp "${BIN}/${f}" "${INSTBIN}/${f}"; done
m4_do cp -R guest/out/. "${M4_INSTALL}/image/"
m4_do chown -R root:wheel "${M4_INSTALL}"
m4_do chmod -R go-w "${M4_INSTALL}"
m4_user_create
m4_do mkdir -p "${M4_VMDIR}"
m4_do chown "${M4_USER}" "${M4_VMDIR}"
m4_do chmod 700 "${M4_VMDIR}"
m4_do "${INSTBIN}/abroad-dev" apply-main-proxy --nodes "${M4_NODES:-<节点文件>}" --selected "${M4_NODE:-<节点名>}" --save-store "${M4_STATE}/store"
m4_do chmod -R go-rwx "${M4_STATE}/store"
ALLOW=""; HOPS=""
if [ "$DRY_RUN" != 1 ]; then
  ALLOW=$("${INSTBIN}/abroad-dev" apply-main-proxy --nodes "${M4_NODES}" --selected "${M4_NODE}" --print-allow)
  HOPS=$("${INSTBIN}/abroad-dev" apply-main-proxy --nodes "${M4_NODES}" --selected "${M4_NODE}" --print-first-hop)
fi
VMARGS=(--image-dir "${M4_INSTALL}/image" --state-dir "${M4_VMDIR}" --direct-sock "${M4_VMDIR}/direct.sock" --direct-peer-uid 0)
for a in ${ALLOW}; do VMARGS+=(--allow-outbound "$a"); done
for h in ${HOPS}; do VMARGS+=(--first-hop "$h"); done
if [ "$DRY_RUN" = 1 ]; then
  printf 'CHANGE: sudo -u %s %s/abroad-vm run %s\n' "${M4_USER}" "${INSTBIN}" "${VMARGS[*]}"
else
  sudo -u "${M4_USER}" "${INSTBIN}/abroad-vm" run "${VMARGS[@]}" >>"${OUT}/m4-abroad-vm-${TS}.log" 2>&1 &
  ok=0
  for _ in $(seq 1 90); do
    curl -fsS -m 3 --unix-socket "${M4_VMDIR}/ctl.sock" http://abroad-agent/v1/health >/dev/null 2>&1 && { ok=1; break; }
    sleep 1
  done
  [ $ok = 1 ] || m4_die "abroad-vm 以测试用户 ${M4_USER} 起不来，见 m4-abroad-vm-${TS}.log"
fi
m4_do "${INSTBIN}/abroad-dev" apply-main-proxy --nodes "${M4_NODES:-<节点文件>}" --selected "${M4_NODE:-<节点名>}" --ctl-sock "${M4_VMDIR}/ctl.sock"

# ---- 安装开发用 LaunchDaemon 启动 abroadd（不带 --dev-skip-region；默认带 --dev-skip-claude-kill）----
[ -e "${M4_STATE}/abroadd/state.json" ] && m4_die "上一次的 abroadd 状态文件还在：先执行 sudo scripts/m4/restore.sh"
PL="${M4_STATE}/${M4_LABEL}.plist"
SKIPKILL='    <string>--dev-skip-claude-kill</string>'
[ "${REAL_KILL}" = 1 ] && SKIPKILL=''
if [ "$DRY_RUN" = 1 ]; then
  printf 'STATE: %s（由 launchd.plist.tmpl 生成）\n' "${PL}"
  printf 'ABROADD: --state-dir %s/abroadd --vm-dir %s --main-proxy-store %s/store --exit-check-host %s --pf-user %s%s\n' \
    "${M4_STATE}" "${M4_VMDIR}" "${M4_STATE}" "${EXIT_HOST}" "${M4_USER}" "$([ "${REAL_KILL}" = 1 ] || echo ' --dev-skip-claude-kill')"
else
  sed -e "s|@ABROADD@|${INSTBIN}/abroadd|" -e "s|@STATE@|${M4_STATE}/abroadd|" -e "s|@VMDIR@|${M4_VMDIR}|" \
    -e "s|@STORE@|${M4_STATE}/store|" -e "s|@EXIT@|${EXIT_HOST}|" -e "s|@USER@|${M4_USER}|" -e "s|@LOG@|${OUT}/m4-abroadd-${TS}.log|" \
    -e "s|@LABEL@|${M4_LABEL}|" -e "s|^@SKIPKILL@$|${SKIPKILL}|" scripts/m4/launchd.plist.tmpl >"${PL}"
fi
m4_do cp "${PL}" "${M4_PLIST}"
m4_do chown root:wheel "${M4_PLIST}"
m4_do chmod 644 "${M4_PLIST}"
m4_do launchctl bootstrap system "${M4_PLIST}"
if [ "$DRY_RUN" != 1 ]; then
  for _ in $(seq 1 30); do [ -S "${API}" ] && break; sleep 1; done
  [ -S "${API}" ] || m4_die "30 秒内 abroadd 没起来，见 m4-abroadd-${TS}.log"
fi

RULES_FILE="${M4_STATE}/rules.yaml"
put_rules() { # put_rules <curl 的动作> [dig 的动作]：其余应用走默认（主代理）
  if [ "$DRY_RUN" = 1 ]; then printf 'STATE: rules.yaml（curl → %s，dig → %s）\n' "$1" "${2:-默认}"; api PUT "/v1/rules?drop_affected=1" >/dev/null; return; fi
  {
    echo "rules:"
    [ "$1" != default ] && echo "  - {app: \"path:/usr/bin/curl\", action: \"$1\"}"
    [ -n "${2:-}" ] && echo "  - {app: \"path:/usr/bin/dig\", action: \"$2\"}"
    true
  } >"${RULES_FILE}"
  local c; c=$(api PUT "/v1/rules?drop_affected=1" "${RULES_FILE}")
  [ "$c" = 200 ] || { m4_redact <"${M4_STATE}/api.out" >>"${M4_LOG}"; m4_die "写规则返回 HTTP ${c}"; }
}

# ---- T1（R4）：应用主代理 → 取按出口算出的时区 → 锁定手动目标（时区 + 当前语言和地区格式）→ 两步启用 → 读回 ----
m4_log "应用主代理（经主代理跑一轮出口检查，建立出口基准；未启用时只锁定目标，不改 Mac）。"
code=$(api POST /v1/main-proxy/apply)
[ "$code" = 200 ] || { [ "$DRY_RUN" != 1 ] && m4_redact <"${M4_STATE}/api.out" >>"${M4_LOG}"; m4_die "应用主代理返回 HTTP ${code}"; }
step "取“应用主代理”预览里的出口时区（region.timezone）作为默认目标时区（v2.14 必须改 3）"
TZ_EXIT=$(jget "$(api_body)" region.timezone); COUNTRY=$(jget "$(api_body)" region.country)
[ "$DRY_RUN" = 1 ] && { TZ_EXIT="<出口时区>"; COUNTRY="<出口国家>"; }
TZ_T="${M4_TZ:-${TZ_EXIT}}"
[ -n "${TZ_T}" ] || m4_die "没能按出口确定时区（应用主代理的结果里没有 region），请用 M4_TZ 显式指定"
[ -n "${M4_TZ:-}" ] && step "目标时区用显式覆盖 M4_TZ=${M4_TZ}"
if [ "${CHANGE_LANG}" = 1 ]; then
  LANG_T="${M4_LANG:-en-US}"; LOC_T=""
else
  LANG_T=$(rg_get "${M4_STATE}/snap/region" user_langs 2>/dev/null | cut -d, -f1)
  LOC_T=$(rg_get "${M4_STATE}/snap/region" user_locale 2>/dev/null)
  [ "$DRY_RUN" = 1 ] && { LANG_T="<当前首选语言>"; LOC_T="<当前地区格式>"; }
  { [ -z "${LANG_T}" ] || [ "${LANG_T}" = none ] || [ -z "${LOC_T}" ] || [ "${LOC_T}" = none ]; } &&
    m4_die "读不到你现在的首选语言或地区格式，没法“保持不变”：设 M4_CHANGE_LANGUAGE=1 指定目标语言再运行"
fi
REGION_BODY=$(/usr/bin/perl -MJSON::PP -e 'my %h=(mode=>"manual", timezone=>$ARGV[0], lang=>$ARGV[1]); $h{locale}=$ARGV[2] if $ARGV[2] ne ""; print JSON::PP->new->canonical->encode(\%h)' "${TZ_T}" "${LANG_T}" "${LOC_T}")
step "锁定手动目标：$([ "${CHANGE_LANG}" = 1 ] && echo "时区 ${TZ_T}、语言 ${LANG_T}" || echo "时区 ${TZ_T}，语言和地区格式保持现在的")"
confirm_call PUT /v1/region "${REGION_BODY}"
[ "$RCODE" = 200 ] || { [ "$DRY_RUN" != 1 ] && m4_redact <"${M4_STATE}/api.out" >>"${M4_LOG}"; m4_die "锁定目标地区返回 HTTP ${RCODE}"; }
m4_log "启用（两步：地区会变，先看预览再确认）。从这里开始，除了经主代理和分流出口，本机上网会断开。"
confirm_call POST /v1/enable '{}'
[ "$RCODE" = 200 ] || { [ "$DRY_RUN" != 1 ] && m4_redact <"${M4_STATE}/api.out" >>"${M4_LOG}"; m4_die "启用返回 HTTP ${RCODE}"; }
[ -n "${PREVIEW}" ] && sample "启用前的确认预览（地区变化、要结束的 Claude 进程）" "${PREVIEW}"
put_rules default
if [ "$DRY_RUN" = 1 ]; then res T1 跳过 "空跑"; else
  sleep 2
  rg_current "${SUDO_USER}" "${SUID}" >"${M4_STATE}/region.now"
  why=$(judge_region "${M4_STATE}/snap/region" "${M4_STATE}/region.now" "${TZ_T}" "${CHANGE_LANG}" "${LANG_T}" "${LOC_T:-}")
  res_judge T1 $? "R4 时区与语言：${why}"
  sample "启用后读回的地区（R4：系统实际写下的语言和地区格式写法）" "$(grep -v '^uid=' "${M4_STATE}/region.now" | sed 's/^user=.*/user=<登录用户>/')"
fi

# ---- T2：环境检测，正常环境下 17 项 ----
REP=$(envrun)
sample "环境检测（正常环境）" "${REP}"
if [ "$DRY_RUN" = 1 ]; then res T2 跳过 "空跑"; else why=$(judge_envcheck "${REP}"); res_judge T2 $? "环境检测：${why}"; fi

# ---- T3：三种人为泄漏（制造 → 检出、闸门关 → 撤销 → 通过、闸门开）；制造和撤销都由脚本完成，restore 兜底 ----
leak_round() { # leak_round <结果编号> <名字> <项目编号> <制造函数> <撤销函数>
  local r1 g1 r2 g2
  "$4"
  [ "$DRY_RUN" != 1 ] && sleep 2
  r1=$(envrun); g1=$(gate_open)
  "$5"
  [ "$DRY_RUN" != 1 ] && sleep 2
  r2=$(envrun); g2=$(gate_open)
  if [ "$DRY_RUN" = 1 ]; then res "$1" 跳过 "空跑"; return; fi
  why=$(judge_leak "$2" "$3" "$r1" "$g1" "$r2" "$g2"); res_judge "$1" $? "$why"
}
proxy_on() {
  m4_state_add leaks proxy
  m4_do networksetup -setsecurewebproxy "${SVC}" 127.0.0.1 9
  m4_do networksetup -setsecurewebproxystate "${SVC}" on
}
proxy_off() { # 按快照还原这个服务的 HTTPS 代理（v2.14 验收后建议改 1）：原来有服务器就写回去，原来为空就清空，最后关掉；读回核对
  local srv port
  srv=$(sed -n 's/^Server=//p' "${M4_STATE}/snap/proxy" 2>/dev/null); port=$(sed -n 's/^Port=//p' "${M4_STATE}/snap/proxy" 2>/dev/null)
  if [ -n "${srv}" ] && [ "${port:-0}" != 0 ]; then m4_do networksetup -setsecurewebproxy "${SVC}" "${srv}" "${port}"
  else m4_do networksetup -setsecurewebproxy "${SVC}" "" 0; fi # 清掉制造泄漏时写进去的 127.0.0.1:9
  m4_do networksetup -setsecurewebproxystate "${SVC}" off
  if [ "$DRY_RUN" != 1 ]; then
    why=$(judge_proxy_restored "${M4_STATE}/snap/proxy" "$(networksetup -getsecurewebproxy "${SVC}" 2>/dev/null)")
    res_judge T3r $? "撤销系统代理后读回：${why}"
  fi
}
fake_on() { # 假的 Claude 进程：系统自带的 sleep 复制到带 Claude Code 安装位置特征的路径，以用户身份带代理变量启动
  m4_state_add leaks fake
  m4_do mkdir -p "${M4_FAKE_DIR}"
  m4_do chmod -R 755 "${M4_FAKE_ROOT}"
  m4_do cp /bin/sleep "${M4_FAKE_BIN}"
  m4_do chmod 755 "${M4_FAKE_BIN}"
  local v
  v=$("${INSTBIN}/abroad-dev" claude-facts --path "${M4_FAKE_BIN}" 2>/dev/null)
  [ "$DRY_RUN" = 1 ] && printf 'QUERY: %s/abroad-dev claude-facts --path %s\n' "${INSTBIN}" "${M4_FAKE_BIN}"
  if [ "$DRY_RUN" = 1 ]; then res T3f 跳过 "空跑"; else why=$(judge_fake_claude "${v}"); res_judge T3f $? "$why"; fi
  if [ "$DRY_RUN" = 1 ]; then
    printf 'CHANGE: sudo -u %s env HTTPS_PROXY=http://127.0.0.1:9 %s 900\n' "${SUDO_USER}" "${M4_FAKE_BIN}"
  else
    sudo -u "${SUDO_USER}" env HTTPS_PROXY=http://127.0.0.1:9 "${M4_FAKE_BIN}" 900 >/dev/null 2>&1 &
    local p=""
    for _ in $(seq 1 20); do
      p=$(ps -axo pid=,command= | awk -v b="${M4_FAKE_BIN} 900" 'index($0, b) {print $1; exit}')
      [ -n "$p" ] && break; sleep 0.2
    done
    [ -n "$p" ] && m4_state_set fake.pid "$p"
  fi
}
fake_off() { m4_kill_pidfile "${M4_STATE}/fake.pid" "${M4_FAKE_BIN} 900"; }
v6_on() { m4_state_add leaks ipv6; m4_do networksetup -setv6automatic "${SVC}"; }
v6_off() { m4_do networksetup -setv6off "${SVC}"; }
leak_round T3a "系统代理" 11 proxy_on proxy_off
leak_round T3b "代理环境变量（假 Claude 进程）" 12 fake_on fake_off
leak_round T3c "重新打开 IPv6" 06 v6_on v6_off

# ---- T4（R5）、T4b、T5：真实服务商的分流出口 ----
MAIN_IP=$(my_ip) # 规则里 curl 走默认（主代理）
EXIT_TAGS=""
exit_put() { # exit_put <标签> <种类> <配置文件>：PUT /v1/exits/<标签>
  local f="${M4_SECRET}/exit-$1.json"
  if [ "$DRY_RUN" = 1 ]; then api PUT "/v1/exits/$1" >/dev/null; return 0; fi
  /usr/bin/perl -MJSON::PP -e 'local $/; open my $h, "<", $ARGV[1] or die; my $c = <$h>; print JSON::PP->new->encode({kind => $ARGV[0], conf => $c})' "$2" "$3" >"$f"
  local c; c=$(api PUT "/v1/exits/$1" "$f"); rm -f "$f"
  [ "$c" = 200 ]
}
broken_conf() { # broken_conf <种类> <原配置> <输出>：同一份配置，端点端口换成不通的 9（模拟隧道起不来；v2.14 必须改 1）
  case "$1" in
    wireguard) sed -E 's/^([[:space:]]*Endpoint[[:space:]]*=[[:space:]]*.+):[0-9]+[[:space:]]*$/\1:9/' "$2" >"$3" ;;
    openvpn) sed -E 's/^([[:space:]]*remote[[:space:]]+[^[:space:]]+)[[:space:]]+[0-9]+/\1 9/' "$2" >"$3" ;;
    clash) sed -E 's/(^|[{,[:space:]])port:[[:space:]]*[0-9]+/\1port: 9/' "$2" >"$3" ;;
  esac
}
cap_start() { # cap_start <名字> <过滤式…>：物理网卡上只读抓包，只统计行数（文本只给 root 读，统计完就删）
  local n=$1; shift
  if [ "$DRY_RUN" = 1 ]; then printf 'CHANGE: /usr/sbin/tcpdump -i %s -n -l -q %s\n' "${IF_BEFORE}" "$*"; return; fi
  (umask 077; : >"${M4_STATE}/cap-$n.txt")
  /usr/sbin/tcpdump -i "${IF_BEFORE}" -n -l -q "$@" >>"${M4_STATE}/cap-$n.txt" 2>/dev/null &
  m4_state_set "tcpdump-$n.pid" "$!"
}
cap_stop() { m4_kill_pidfile "${M4_STATE}/tcpdump-$1.pid" "/usr/sbin/tcpdump -i ${IF_BEFORE}"; }
cap_count() { local c; c=$(count_packets "${M4_STATE}/cap-$1.txt"); m4_do rm -f "${M4_STATE}/cap-$1.txt"; echo "${c:-0}"; }
echo_filter() { local f="" a; for a in $(secret_get echoips); do f="${f:+${f} or }dst host ${a}"; done; echo "${f:-dst host 192.0.2.1}"; }
exit_round() { # exit_round <编号> <标签> <种类> <配置文件或空>
  local id=$1 tag=$2 kind=$3 conf=$4 eip ip2 ok pk st0 st1
  [ -n "$conf" ] || { res "$id" 跳过 "没给 ${kind} 的配置（环境变量）"; return; }
  exit_put "$tag" "$kind" "$conf" || { res "$id" 不通过 "保存出口 ${tag} 失败（见日志）"; m4_redact <"${M4_STATE}/api.out" >>"${M4_LOG}" 2>/dev/null; return; }
  EXIT_TAGS="${EXIT_TAGS} ${tag}"
  put_rules "proxy:${tag}"
  [ "$DRY_RUN" != 1 ] && for _ in $(seq 1 30); do eip=$(my_ip); [ -n "$eip" ] && break; sleep 2; done
  sample "出口 ${tag} 的状态" "$(status_json | /usr/bin/perl -MJSON::PP -e 'my $d=eval{JSON::PP->new->decode(do{local $/;<STDIN>})}||{}; print JSON::PP->new->canonical->encode([grep {$_->{tag} eq $ARGV[0]} @{$d->{exits}||[]}])' "$tag" 2>/dev/null)"
  if [ "$DRY_RUN" = 1 ]; then res "$id" 跳过 "空跑"; else
    why=$(judge_exit_route "$(eqv "$eip" "${MAIN_IP}")" "$(eqv "$eip" "${REAL_IP}")" "$(eqv "${MAIN_IP}" "${REAL_IP}")"); res_judge "$id" $? "出口 ${tag}（${kind}）：${why}"
  fi
  # 断开后（端点端口不通）：请求失败或回退主代理；绝不是本机或原出口；物理网卡上没有发往回显服务的包。
  [ "$DRY_RUN" != 1 ] && broken_conf "$kind" "$conf" "${M4_SECRET}/broken-${tag}"
  exit_put "$tag" "$kind" "${M4_SECRET}/broken-${tag}" || true
  m4_do rm -f "${M4_SECRET}/broken-${tag}"
  [ "$DRY_RUN" != 1 ] && sleep 15
  st0=$(status_json)
  cap_start "down-${tag}" "$(echo_filter)"
  [ "$DRY_RUN" != 1 ] && sleep 1
  ip2=$(my_ip); ok=0; [ -n "$ip2" ] && ok=1
  st1=$(status_json)
  cap_stop "down-${tag}"
  pk=$(cap_count "down-${tag}")
  if [ "$DRY_RUN" = 1 ]; then res "${id}d" 跳过 "空跑"; else
    why=$(judge_exit_down "$ok" "$(eqv "$ip2" "${MAIN_IP}")" "$(eqv "$ip2" "${REAL_IP}")" "$(eqv "$ip2" "$eip")" "$pk")
    res_judge "${id}d" $? "出口 ${tag} 断开后：${why}；旁证：主代理开流 +$(delta "$(jget "$st0" opens.main.tcp)" "$(jget "$st1" opens.main.tcp)")"
  fi
  sample "T5 出口 ${tag} 端点不通时的检查结果和可用状态" "$(status_json | /usr/bin/perl -MJSON::PP -e 'my $d=eval{JSON::PP->new->decode(do{local $/;<STDIN>})}||{}; print JSON::PP->new->canonical->encode([grep {$_->{tag} eq $ARGV[0]} @{$d->{exits}||[]}])' "$tag" 2>/dev/null)"
  exit_put "$tag" "$kind" "$conf" || true # 换回原配置（T4b 要用订阅出口）
  put_rules default
}
exit_round T4w wg wireguard "${M4_WG_CONF:-}"
exit_round T4o ovpn openvpn "${M4_OVPN_CONF:-}"
if [ -n "${M4_OVPN_CONF:-}" ] && [ "$DRY_RUN" != 1 ]; then
  sample "OpenVPN 推送项（abroad-vm 日志里与推送有关的行）" "$(grep -E '推送|push' "${OUT}/m4-abroad-vm-${TS}.log" 2>/dev/null | tail -40)"
fi
exit_round T4s sub clash "${M4_SUB:-}"
if [ -n "${M4_SUB:-}" ]; then
  put_rules default "proxy:sub"
  cap_start dns53 "port 53"
  [ "$DRY_RUN" != 1 ] && sleep 1
  ans=$(as_user /usr/bin/dig +time=5 +tries=1 @dns.google example.com A 2>/dev/null)
  cap_stop dns53
  pk=$(cap_count dns53)
  if [ "$DRY_RUN" = 1 ]; then res T4b 跳过 "空跑"; else
    got=0; printf '%s\n' "$ans" | grep -q 'status: NOERROR' && printf '%s\n' "$ans" | grep -q 'ANSWER SECTION' && got=1
    why=$(judge_udp_domain "$got" "$pk"); res_judge T4b $? "订阅出口的 UDP 域名目标：${why}"
  fi
  put_rules default
else
  res T4b 跳过 "没给订阅（M4_SUB）"
fi
res T5 记录 "按出口检查的结果和端点不通时的可用状态见采样文件（T5 两段）；回退行为见 T4*d"

# ---- T6：第 10 项与本机真实 IP 不落盘 ----
REP=$(envrun)
sample "环境检测（分流出口之后）" "${REP}"
if [ "$DRY_RUN" = 1 ]; then
  printf 'QUERY: grep -rcF <本机真实 IP> %s %s/m4-abroadd-%s.log %s/m4-abroad-vm-%s.log <status>\n' "${M4_STATE}" "${OUT}" "${TS}" "${OUT}" "${TS}"
  res T6 跳过 "空跑"
else
  res T6a 记录 "第 10 项（Claude 可达性）：$(env_item "${REP}" 10)"
  n=0
  if [ -n "${REAL_IP}" ]; then
    n=$(grep -rF --exclude-dir=secret -e "${REAL_IP}" "${M4_STATE}" "${OUT}/m4-abroadd-${TS}.log" "${OUT}/m4-abroad-vm-${TS}.log" 2>/dev/null | grep -c . || true)
    printf '%s' "$(status_json)" | grep -qF -- "${REAL_IP}" && n=$((n + 1))
  fi
  why=$(judge_realip "$([ -n "${REAL_IP}" ] && echo 1 || echo 0)" "$n"); res_judge T6 $? "本机真实 IP：${why}"
fi

# ---- T7（R10，只读）：桌面版保存界面语言的位置 ----
UHOME=$(dscl . -read "/Users/${SUDO_USER}" NFSHomeDirectory 2>/dev/null | awk '{print $2}')
[ "$DRY_RUN" = 1 ] && UHOME="~"
CFG="${UHOME}/Library/Application Support/Claude/config.json"
if [ "$DRY_RUN" = 1 ]; then printf 'USER: /usr/bin/plutil -extract locale raw -o - %s\n' "${CFG}" >&3; res T7 跳过 "空跑"
elif [ -f "${CFG}" ]; then
  loc=$(as_user /usr/bin/plutil -extract locale raw -o - "${CFG}" 2>/dev/null || true)
  fmt=没有; [ -n "$loc" ] && { printf '%s' "$loc" | grep -qE '^[a-z]{2,3}(-[A-Z]{2})?$' && fmt="形如 xx-XX" || fmt="其他写法"; }
  res T7 记录 "R10：config.json 的顶层 locale：${fmt}；环境检测第 15 项：$(env_item "${REP}" 15)"
else
  res T7 跳过 "没装 Claude 桌面版（没有 config.json）"
fi

# ---- T8（R17，只读）：桌面版签名的 Team ID、几种安装位置是否命中特征 ----
FACTS=$("${INSTBIN}/abroad-dev" claude-facts --facts 2>/dev/null || echo '{}')
[ "$DRY_RUN" = 1 ] && printf 'QUERY: %s/abroad-dev claude-facts --facts / --team-of /Applications/Claude.app / --path …\n' "${INSTBIN}"
if [ "$DRY_RUN" = 1 ]; then res T8 跳过 "空跑"; else
  team=""; [ -d /Applications/Claude.app ] && team=$(jget "$(M4_USER_HOME="${UHOME}" "${INSTBIN}/abroad-dev" claude-facts --team-of /Applications/Claude.app 2>/dev/null)" team_id)
  why=$(judge_team "$(jget "${FACTS}" team_id)" "${team}"); res_judge T8 $? "R17：${why}"
  paths=()
  for p in "${UHOME}/.local/bin/claude" /opt/homebrew/bin/claude /usr/local/bin/claude "${UHOME}"/.vscode/extensions/anthropic.claude-code-*/resources/native-binary/claude \
    "${UHOME}"/.cursor/extensions/anthropic.claude-code-*/resources/native-binary/claude "${UHOME}"/Library/Application\ Support/JetBrains/*/plugins/claude-code-jetbrains-plugin; do
    [ -e "$p" ] || continue
    paths+=(--path "$(/usr/bin/perl -MCwd -e 'print Cwd::abs_path($ARGV[0])' "$p")")
  done
  if [ ${#paths[@]} -gt 0 ]; then
    sample "R17 安装位置是否命中特征（路径已换成 ~）" "$(M4_USER_HOME="${UHOME}" "${INSTBIN}/abroad-dev" claude-facts "${paths[@]}" 2>&1)"
    res T8p 记录 "R17：检查了 $((${#paths[@]} / 2)) 个安装位置，结果见采样文件"
  else
    res T8p 跳过 "没找到任何 Claude Code 的安装位置"
  fi
fi

# ---- T9：结束 Claude 的预览（不确认；真的结束只在最后恢复时、且 M4_REAL_KILL=1）----
code=$(api POST /v1/restore)
if [ "$DRY_RUN" = 1 ]; then res T9 跳过 "空跑"
elif [ "$code" = 409 ]; then
  PV=$(api_body); sample "T9 恢复的确认预览（$([ "${REAL_KILL}" = 1 ] && echo 会真的结束 || echo 只预览不结束)）" "${PV}"
  why=$(judge_preview "${PV}"); res_judge T9 $? "${why}"
else
  res T9 不通过 "恢复的第一步应回 409（带预览），实际 ${code}"
fi

# ---- T10：切换地区（同一个国家的另一个时区）再切回 ----
alt_tz() { # alt_tz <国家> <当前时区>：同一国家的另一个时区（内置表），找不到输出空串
  local l
  case "$1" in
    US) l="America/New_York America/Chicago America/Los_Angeles" ;;
    CA) l="America/Toronto America/Vancouver" ;;
    AU) l="Australia/Sydney Australia/Perth" ;;
    BR) l="America/Sao_Paulo America/Manaus" ;;
    RU) l="Europe/Moscow Asia/Vladivostok" ;;
    MX) l="America/Mexico_City America/Tijuana" ;;
    *) l="" ;;
  esac
  for z in $l; do [ "$z" != "$2" ] && { echo "$z"; return; }; done
}
ALT=$(alt_tz "${COUNTRY}" "${TZ_T}")
[ "$DRY_RUN" = 1 ] && ALT="<同一国家的另一个时区>"
if [ -z "${ALT}" ]; then
  res T10 跳过 "出口国家（${COUNTRY:-未知}）在内置表里没有第二个时区"
else
  step "切换地区到同一国家的另一个时区，再切回"
  B2=$(/usr/bin/perl -MJSON::PP -e 'my %h=(mode=>"manual", timezone=>$ARGV[0], lang=>$ARGV[1]); $h{locale}=$ARGV[2] if $ARGV[2] ne ""; print JSON::PP->new->canonical->encode(\%h)' "${ALT}" "${LANG_T}" "${LOC_T}")
  confirm_call PUT /v1/region "${B2}"; c1=$RCODE
  t1=$(rg_read_tz 2>/dev/null)
  confirm_call PUT /v1/region "${REGION_BODY}"; c2=$RCODE
  t2=$(rg_read_tz 2>/dev/null)
  if [ "$DRY_RUN" = 1 ]; then res T10 跳过 "空跑"
  elif [ "$c1" = 200 ] && [ "$t1" = "${ALT}" ] && [ "$c2" = 200 ] && [ "$t2" = "${TZ_T}" ]; then res T10 通过 "两步确认后时区切到同一国家的另一个时区，再切回"
  else res T10 不通过 "切换 HTTP ${c1}、读回$([ "$t1" = "${ALT}" ] && echo 一致 || echo 不一致)；切回 HTTP ${c2}、读回$([ "$t2" = "${TZ_T}" ] && echo 一致 || echo 不一致)"; fi
fi

# ---- T11（R7）：停用状态下观察推送和对时（只统计行数和关键字次数，不写日志内容；v2.14 建议改 6）----
code=$(api POST /v1/disable)
if [ "$DRY_RUN" = 1 ]; then
  printf 'QUERY: log show --last %ss --style compact --predicate process == "apsd" OR process == "timed"\n' "${R7_SECS}"
  res T11 跳过 "空跑"
else
  m4_log "停用，观察 ${R7_SECS} 秒推送服务和对时的系统日志（只统计）。"
  sleep "${R7_SECS}"
  L=$(log show --last "${R7_SECS}s" --style compact --predicate 'process == "apsd" OR process == "timed"' 2>/dev/null)
  res T11 记录 "R7：停用（HTTP ${code}）后 ${R7_SECS} 秒：apsd $(printf '%s\n' "$L" | grep -c ' apsd\[' || true) 行（含 connect $(printf '%s\n' "$L" | grep -ci 'connect' || true) 次），timed $(printf '%s\n' "$L" | grep -c ' timed\[' || true) 行（含 fail $(printf '%s\n' "$L" | grep -ci 'fail' || true) 次）"
fi
if [ "${M4_CAPTIVE:-0}" = 1 ] && [ "$DRY_RUN" != 1 ]; then
  m4_log "R16：请连上需要认证的 Wi-Fi，在弹出的认证页里登录，完成后按回车。"; read -r _ </dev/tty
  res T12 记录 "R16：认证后 status 的状态：$(jget "$(status_json)" state)，闸门 $(gate_open)"
else
  res T12 跳过 "R16 需要认证的 Wi-Fi：设 M4_CAPTIVE=1 并在有条件时人工做"
fi

# ---- T13：恢复（两步），网络和地区与快照逐项比对 ----
m4_log "恢复本机网络和地区。"
confirm_call POST /v1/restore '{}'
code=$RCODE
diffs=()
if [ "$DRY_RUN" != 1 ]; then
  while IFS=$'\t' read -r s mode; do
    now=$(networksetup -getinfo "$s" 2>/dev/null | awk -F': ' '/^IPv6: /{print $2}')
    [ "$now" = "$mode" ] || diffs+=("IPv6 不同")
  done <"${M4_STATE}/snap/ipv6"
  netstat -rn -f inet | awk '$4 ~ /^utun/ && ($1=="0/1" || $1=="128.0/1")' | grep -q . && diffs+=("还有经 utun 的 /1 路由")
  printf 'show %s\nquit\n' "${M4_DNS_KEY}" | scutil | grep -q 'No such key' || diffs+=("DNS 键还在")
  [ -z "$(/sbin/pfctl -a "${M4_ANCHOR}" -s rules 2>/dev/null)" ] || diffs+=("锚点不空")
  rg_current "${SUDO_USER}" "${SUID}" >"${M4_STATE}/region.after"
  rd=$(rg_diff "${M4_STATE}/snap/region" "${M4_STATE}/region.after" | tr '\n' ' ')
  [ -z "$rd" ] || diffs+=("地区与启用前不同：${rd}")
fi
if [ "$DRY_RUN" = 1 ]; then res T13 跳过 "空跑"
elif [ "$code" = 200 ] && [ ${#diffs[@]} -eq 0 ]; then res T13 通过 "abroadd 恢复后，路由、DNS、IPv6、锚点、时区、自动时区、语言、地区格式都与启用前一致"
else res T13 不通过 "HTTP ${code}；$(join_cn "${diffs[@]:-}")"; fi
if [ -n "${GW_BEFORE}" ] && [ "$DRY_RUN" != 1 ]; then
  as_user ping -c 2 -t 3 "${GW_BEFORE}" >/dev/null 2>&1 && m4_log "恢复后路由器可达" || m4_log "恢复后路由器不可达（请贴回日志）"
fi

# ---- 收尾：restore.sh（按脚本自己的快照再兜底一遍）----
m4_log "收尾：执行 restore.sh"
M4_RESTORED=1
if [ "$DRY_RUN" = 1 ]; then DRY_RUN=1 bash "${RESTORE}"; else bash "${RESTORE}"; fi
m4_log "结果汇总（${RESULTS}）："
cat "${RESULTS}" >&2
m4_log "请贴回：${M4_LOG}、${RESULTS}、${SAMPLES}，以及 scripts/m4/out/m4-abroadd-${TS}.log、m4-abroad-vm-${TS}.log"
