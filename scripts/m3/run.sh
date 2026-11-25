#!/usr/bin/env bash
# M3 真机验证（M3-8）：在这台 Mac 上以真实模式跑 abroadd，验证分流（多出口、直连、置顶域名不泄漏）、R3 的 root 部分、
# 在途 fd、直连拨号模块的 pf 放行、停用后的 offline_allow、R15。用法见 scripts/m3/README.md。
# 会改动本机网络、需要 sudo，由用户执行；主会话只做语法检查、DRY_RUN=1 空跑、--check-only 和自测。
#
#   sudo scripts/m3/run.sh                # 正式运行
#   scripts/m3/run.sh --check-only        # 只做前提检查（只读，不需要 root）
#   DRY_RUN=1 scripts/m3/run.sh           # 空跑：只打印会执行的改动命令（CHANGE:）和查询（QUERY:）
#
# 环境变量：
#   M3_NODES、M3_NODE        主代理节点的 Clash YAML 和节点名（必填，同 M2）
#   M3_M2_PASSED=1           确认 M2 的真机脚本已经通过（必填）
#   M3_DIRECT_HOST           直连测试用的域名（默认 www.apple.com；要能从本机网络直接连上 443）
#   M3_DIRECT_TARGET         在途 fd 检查的目标 IPv4:端口（默认 1.1.1.1:443）
#   M3_DIRECT_TLS_NAME       它的 TLS 服务器名（默认 one.one.one.one）
#   M3_EXIT_HOST             出口检查主机（默认 www.gstatic.com）
#   M3_DEADMAN_SECS          死人开关（默认 1500 秒）
#   M3_INTERACTIVE=1         额外做一次微信（需要你手动发一条消息）
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
. scripts/m3/lib.sh
. scripts/m3/judge.sh

exec 3>&1
CHECK_ONLY=0
case "${1:-}" in
  --check-only) CHECK_ONLY=1 ;;
  "") ;;
  *) echo "用法：sudo $0 [--check-only]" >&2; exit 2 ;;
esac

OUT="${ROOT}/scripts/m3/out"
TS="$(date +%Y%m%d-%H%M%S)"
mkdir -p "${OUT}"
M3_LOG="${OUT}/m3-${TS}.log"
SAMPLES="${OUT}/m3-samples-${TS}.txt"
RESULTS="${OUT}/m3-results-${TS}.txt"
: >"${M3_LOG}"
: >"${SAMPLES}"
: >"${RESULTS}"
BIN="${ROOT}/scripts/dev/out/bin"
EXIT_HOST="${M3_EXIT_HOST:-www.gstatic.com}"
DEADMAN_SECS="${M3_DEADMAN_SECS:-1500}"
DIRECT_HOST="${M3_DIRECT_HOST:-www.apple.com}"
DIRECT_TARGET="${M3_DIRECT_TARGET:-1.1.1.1:443}"
DIRECT_TLS_NAME="${M3_DIRECT_TLS_NAME:-one.one.one.one}"
RAW_IP443="1.1.1.1"
API="${M3_STATE}/abroadd/api.sock"
INSTBIN="${M3_INSTALL}/bin"
RESTORE="${ROOT}/scripts/m3/restore.sh"
CHROME_APP="Google Chrome"

# res <步骤> <通过|不通过|跳过|记录> <说明>
res() { printf '%-4s %-6s %s\n' "$1" "$2" "$3" | tee -a "${RESULTS}" >&2; }
# res_judge <步骤> <判定函数的返回码> <说明>
res_judge() { case "$2" in 0) res "$1" 通过 "$3" ;; 2) res "$1" 跳过 "$3" ;; *) res "$1" 不通过 "$3" ;; esac; }
sample() {
  local title=$1; shift
  if [ "$DRY_RUN" = 1 ]; then
    case "$1" in m3_q | m3_do) "$@" ;; *) printf 'QUERY: %s\n' "$*" ;; esac
    return 0
  fi
  { printf '\n==== %s ====\n$ %s\n' "$title" "$*"; "$@" 2>&1 | m3_redact; printf '（退出码 %s）\n' "${PIPESTATUS[0]}"; } >>"${SAMPLES}"
}
# 以运行 sudo 的用户执行；空跑时把命令打到 3 号描述符（调用处常把输出丢掉或放进 $(...)，空跑清单里仍要看得到）。
as_user() { if [ "$DRY_RUN" = 1 ]; then printf 'USER: %s\n' "$*" >&3; return 0; fi; sudo -u "${SUDO_USER}" "$@"; }
api() { # api <方法> <路径> [请求体文件]：经本地接口调用 abroadd，输出 HTTP 状态码
  # 空跑：API 行打到 3 号描述符（脚本开头复制的标准输出），不混进 $(...) 拿到的状态码。
  if [ "$DRY_RUN" = 1 ]; then printf 'API: %s %s\n' "$1" "$2" >&3; echo 200; return 0; fi
  local data=()
  [ -n "${3:-}" ] && data=(--data-binary "@$3")
  curl -sS -m 180 --unix-socket "${API}" -X "$1" ${data[@]+"${data[@]}"} -o "${M3_STATE}/api.out" -w '%{http_code}' "http://abroadd$2" 2>/dev/null
}
# restore_api：两步恢复（m4-plan v2.3 决定 5）。第一次回 409，带预览和确认码；带确认码再调一次。最终状态码放在 RCODE。
# 确认码只写进 0600 的临时文件当请求体，用完就删，不打印。
restore_api() {
  local c cc f="${M3_STATE}/confirm.json"
  if [ "$DRY_RUN" = 1 ]; then
    api POST /v1/restore >/dev/null
    api POST /v1/restore "$f" >/dev/null
    RCODE=200
    return 0
  fi
  c=$(api POST /v1/restore)
  if [ "$c" = 409 ]; then
    cc=$(sed -n 's/.*"confirm":"\([0-9a-f]\{32\}\)".*/\1/p' "${M3_STATE}/api.out")
    if [ -n "$cc" ]; then
      (umask 077 && printf '{"confirm":"%s"}' "$cc" >"$f")
      c=$(api POST /v1/restore "$f")
      rm -f "$f"
    fi
  fi
  RCODE=$c
}
status_json() { # abroadd 的 status（只取计数用）
  [ "$DRY_RUN" = 1 ] && { echo '{}'; return; }
  curl -sS -m 10 --unix-socket "${API}" http://abroadd/v1/status 2>/dev/null || echo '{}'
}
vm_netstats() { # abroad-vm 的计数（直连拨号模块的各种应答码）
  [ "$DRY_RUN" = 1 ] && { echo '{}'; return; }
  curl -sS -m 10 --unix-socket "${M3_VMDIR}/host.sock" http://abroad-vm/v1/netstats 2>/dev/null || echo '{}'
}
mark() { od -An -N4 -tx1 /dev/urandom | tr -d ' \n'; }

# ---- 前提检查（只读，不需要 root）----
precheck() {
  local reasons=() line conf
  # 1. 别的代理软件的 TUN（与 M2 相同）。
  conf=$(netstat -rn -f inet 2>/dev/null | awk '
    $4 ~ /^(utun|ipsec|ppp|tun|tap|wg)[0-9]+$/ {
      d=$1
      if (d=="default") { print $4; next }
      if (d ~ /^(10|127)(\.|\/|$)/ || d ~ /^169\.254/ || d ~ /^192\.168/ || d ~ /^172\.(1[6-9]|2[0-9]|3[01])(\.|\/|$)/ || d ~ /^(22[4-9]|23[0-9]|24[0-9]|25[0-5])(\.|\/|$)/) next
      print $4
    }' | sort -u | tr '\n' ' ')
  [ -n "${conf}" ] && reasons+=("别的代理软件的 TUN 开着（接口：${conf% }）：请先在 Clash 等软件里关掉 TUN 模式")
  m3_system_proxy_on && reasons+=("系统代理开着：请在“系统设置 → 网络 → 详细信息 → 代理”里全部关掉，或在代理软件里关掉“设为系统代理”")
  [ -e "${M3_STATE}/abroadd/state.json" ] && reasons+=("上一次运行留下的 abroadd 状态文件还在（${M3_STATE}/abroadd/state.json）：请先执行 sudo scripts/m3/restore.sh")
  [ -e /var/run/claude-abroad-m2/abroadd/state.json ] && reasons+=("M2 脚本留下的 abroadd 状态文件还在：请先执行 sudo scripts/m2/restore.sh")
  # 2. M2 的真机脚本已经通过（M3 建立在它验证过的断网保护、接管、恢复之上）。
  [ "${M3_M2_PASSED:-0}" = 1 ] || reasons+=("没有确认 M2 的真机脚本已经通过：先执行 scripts/m2（见 PROGRESS 清单第 5 步），通过后设 M3_M2_PASSED=1 再运行")
  # 3. 主代理。
  if [ -z "${M3_NODES:-}" ] || [ ! -f "${M3_NODES:-/nonexistent}" ] || [ -z "${M3_NODE:-}" ]; then
    reasons+=("没有主代理：请设置 M3_NODES（远端或局域网节点的 Clash YAML）和 M3_NODE（节点名）")
  elif [ -x "${BIN}/abroad-dev" ]; then
    local allow
    if ! allow=$("${BIN}/abroad-dev" apply-main-proxy --nodes "${M3_NODES}" --selected "${M3_NODE}" --print-allow 2>&1); then
      reasons+=("主代理节点不可用：$(printf '%s' "${allow}" | head -1 | m3_redact)")
    elif printf '%s\n' "${allow}" | grep -q '^127\.'; then
      reasons+=("主代理的第一跳是本机回环（本机代理软件当主代理会回环）：请换成远端或局域网节点")
    fi
  fi
  # 4. IPv6 手动配置（同 M2）。
  while IFS= read -r line; do
    [ -n "${line}" ] || continue
    if networksetup -getinfo "${line}" 2>/dev/null | grep -q '^IPv6: Manual'; then
      reasons+=("网络服务的 IPv6 是手动配置：请先改成自动（系统设置 → 网络 → 详细信息 → TCP/IP）")
      break
    fi
  done < <(m3_services)
  # 5. 程序和镜像：开发标签构建的 abroad-vm（开发用多出口要它）。
  for f in abroad-vm-dev abroadd abroad-dev; do
    [ -x "${BIN}/${f}" ] || reasons+=("缺少 ${BIN}/${f}：见 README 的准备步骤")
  done
  miss=""
  [ -x "${BIN}/abroadd" ] && miss=$(m3_dev_flags_missing "${BIN}/abroadd" --dev-skip-claude-kill --dev-skip-region)
  if [ -n "${miss}" ]; then
    reasons+=("${BIN}/abroadd 认不出要传的开发参数（$(echo ${miss})）：请按 README 用 go build -tags abroaddev 重新编出（开发参数只在开发构建里有）")
  fi
  miss=""
  [ -x "${BIN}/abroad-vm-dev" ] && miss=$(m3_dev_flags_missing "${BIN}/abroad-vm-dev" --dev-agent)
  if [ -n "${miss}" ]; then
    reasons+=("${BIN}/abroad-vm-dev 认不出 --dev-agent：请按 README 用 go build -tags abroaddev 重新编出（开发参数只在开发构建里有）")
  fi
  [ -f guest/out/Image ] || reasons+=("缺少虚拟机镜像 guest/out：先跑一次 scripts/dev/vm-up.sh --no-panic")
  # 6. Chrome 正在运行：已有进程里的连接、新开的窗口会混进判定，请先退出（只读查询进程名，不结束它）。
  if pgrep -x "${CHROME_APP}" >/dev/null 2>&1; then
    reasons+=("Chrome 正在运行：请先完全退出 Chrome（⌘Q），脚本会用单独的临时配置启动它")
  fi
  m3_log "提醒：运行期间会断网几次（见 README），请先结束 Claude Code 会话和其他要紧的网络任务。"
  if [ ${#reasons[@]} -gt 0 ]; then
    for r in "${reasons[@]}"; do m3_log "前提不满足：${r}"; done
    return 1
  fi
  m3_log "前提检查通过"
  return 0
}

if [ "${CHECK_ONLY}" = 1 ]; then m3_test_guard check; else m3_test_guard; fi
if [ "${CHECK_ONLY}" = 1 ]; then
  precheck
  exit $?
fi
if ! precheck; then
  if [ "$DRY_RUN" = 1 ]; then m3_log "（空跑：前提不满足，继续打印后面的命令）"; else exit 3; fi
fi

m3_require_root
[ "$DRY_RUN" = 1 ] && SUDO_USER=${SUDO_USER:-$(id -un)}

# ---- 先建状态目录，紧接着启动死人开关；其余改动都在它之后 ----
m3_state_init
m3_deadman_start "${DEADMAN_SECS}" "${RESTORE}"
m3_arm_restore_trap "${RESTORE}"
GW_BEFORE="$(m3_default_gw)"
IF_BEFORE="$(m3_default_iface)" # 物理网卡（启用前的默认路由出口），抓包用
[ "$DRY_RUN" = 1 ] && IF_BEFORE=${IF_BEFORE:-en0}

# ---- 快照（restore.sh 只靠它还原，不依赖 abroadd；同 M2）----
snapshot() {
  m3_do mkdir -p "${M3_STATE}/snap"
  if [ "$DRY_RUN" = 1 ]; then printf 'STATE: snap/ipv6、routes、dns、pf\n'; return; fi
  local s
  : >"${M3_STATE}/snap/ipv6"
  while IFS= read -r s; do
    [ -n "$s" ] || continue
    printf '%s\t%s\n' "$s" "$(networksetup -getinfo "$s" 2>/dev/null | awk -F': ' '/^IPv6: /{print $2}')" >>"${M3_STATE}/snap/ipv6"
  done < <(m3_services)
  netstat -rn -f inet >"${M3_STATE}/snap/routes" 2>&1
  printf 'show %s\nquit\n' "${M3_DNS_KEY}" | scutil >"${M3_STATE}/snap/dns" 2>&1
  pfctl -s info 2>/dev/null | head -1 >"${M3_STATE}/snap/pf"
}
snapshot
m3_do mkdir -p "${M3_SNAP}"
m3_do chmod 700 "${M3_SNAP}"
m3_do cp -R "${M3_STATE}/snap/." "${M3_SNAP}/"
m3_log "已拍快照：IPv6、路由表、DNS 键、pf 状态（另存一份到 ${M3_SNAP}，重启不丢）"

# ---- 安装程序、建测试用户、准备主代理 ----
m3_do mkdir -p "${INSTBIN}" "${M3_INSTALL}/image"
for f in abroad-vm-dev abroadd abroad-dev; do m3_do cp "${BIN}/${f}" "${INSTBIN}/${f}"; done
m3_do cp -R guest/out/. "${M3_INSTALL}/image/"
m3_do chown -R root:wheel "${M3_INSTALL}"
m3_do chmod -R go-w "${M3_INSTALL}"
m3_user_create
TUID=0
[ "$DRY_RUN" != 1 ] && TUID=$(id -u "${M3_USER}")
m3_do mkdir -p "${M3_VMDIR}" "${M3_UPDIR}"
m3_do chown "${M3_USER}" "${M3_VMDIR}" "${M3_UPDIR}"
m3_do chmod 700 "${M3_VMDIR}" "${M3_UPDIR}"
m3_do "${INSTBIN}/abroad-dev" apply-main-proxy --nodes "${M3_NODES:-<节点文件>}" --selected "${M3_NODE:-<节点名>}" --save-store "${M3_STATE}/store"
m3_do chmod -R go-rwx "${M3_STATE}/store"
ALLOW=""; HOPS=""
if [ "$DRY_RUN" != 1 ]; then
  ALLOW=$("${INSTBIN}/abroad-dev" apply-main-proxy --nodes "${M3_NODES}" --selected "${M3_NODE}" --print-allow)
  HOPS=$("${INSTBIN}/abroad-dev" apply-main-proxy --nodes "${M3_NODES}" --selected "${M3_NODE}" --print-first-hop)
fi

# ---- 两个开发上游（出口 A、B）：以测试用户运行，在本机应答、不出站，只用来区分路径（m3-plan v2.11 偏差 1）----
UP_A=""; UP_B=""
start_up() { # start_up <名字> <变量名>：程序自己写 PID 文件；端口从它打印的 listening 行读
  local n=$1 out="${M3_STATE}/up-$1.out" a
  if [ "$DRY_RUN" = 1 ]; then
    printf 'CHANGE: sudo -u %s %s/abroad-dev upstream --name %s --log %s/%s.jsonl --pid-file %s/%s.pid\n' \
      "${M3_USER}" "${INSTBIN}" "$n" "${M3_UPDIR}" "$n" "${M3_UPDIR}" "$n"
    printf -v "$2" '%s' 40000
    return 0
  fi
  sudo -u "${M3_USER}" "${INSTBIN}/abroad-dev" upstream --name "$n" --log "${M3_UPDIR}/$n.jsonl" --pid-file "${M3_UPDIR}/$n.pid" >"${out}" 2>&1 &
  for _ in $(seq 1 50); do grep -q '^listening ' "${out}" 2>/dev/null && break; sleep 0.2; done
  a=$(awk '/^listening /{print $2; exit}' "${out}")
  [ -n "$a" ] || m3_die "开发上游 ${n} 没有起来：$(head -3 "${out}")"
  printf -v "$2" '%s' "${a##*:}"
}
start_up wg-a UP_A
start_up clash-b UP_B
m3_log "开发上游已起：出口 A（wg-a）、出口 B（clash-b），都在本机回环上应答"

VMARGS=(--image-dir "${M3_INSTALL}/image" --state-dir "${M3_VMDIR}" --dev-agent
  --direct-sock "${M3_VMDIR}/direct.sock" --direct-peer-uid 0
  --allow-outbound "127.0.0.1:${UP_A}" --allow-outbound "127.0.0.1:${UP_B}")
for a in ${ALLOW}; do VMARGS+=(--allow-outbound "$a"); done
for h in ${HOPS}; do VMARGS+=(--first-hop "$h"); done

# ---- 以测试用户启动开发版 abroad-vm（带直连拨号模块）----
if [ "$DRY_RUN" = 1 ]; then
  printf 'CHANGE: sudo -u %s %s/abroad-vm-dev run %s\n' "${M3_USER}" "${INSTBIN}" "${VMARGS[*]}"
else
  sudo -u "${M3_USER}" "${INSTBIN}/abroad-vm-dev" run "${VMARGS[@]}" >>"${OUT}/m3-abroad-vm-${TS}.log" 2>&1 &
  ok=0
  for _ in $(seq 1 90); do
    curl -fsS -m 3 --unix-socket "${M3_VMDIR}/ctl.sock" http://abroad-agent/v1/health >/dev/null 2>&1 && { ok=1; break; }
    sleep 1
  done
  [ $ok = 1 ] || m3_die "abroad-vm 以测试用户 ${M3_USER} 起不来，见 m3-abroad-vm-${TS}.log"
fi
m3_do "${INSTBIN}/abroad-dev" apply-main-proxy --nodes "${M3_NODES:-<节点文件>}" --selected "${M3_NODE:-<节点名>}" --ctl-sock "${M3_VMDIR}/ctl.sock"
DEVX="$(printf '{"exits":[{"Tag":"wg-a","Port":%s,"Server":"192.168.127.254"},{"Tag":"clash-b","Port":%s,"Server":"192.168.127.254"}]}' "${UP_A}" "${UP_B}")"
if [ "$DRY_RUN" = 1 ]; then
  printf 'API: PUT abroad-agent /v1/dev-exits（出口 wg-a、clash-b）\n'
else
  curl -fsS -m 30 --unix-socket "${M3_VMDIR}/ctl.sock" -X PUT -d "${DEVX}" http://abroad-agent/v1/dev-exits >/dev/null ||
    m3_die "设置开发出口失败"
fi

# ---- 安装开发用 LaunchDaemon 启动 abroadd ----
[ -e "${M3_STATE}/abroadd/state.json" ] && m3_die "上一次的 abroadd 状态文件还在：先执行 sudo scripts/m3/restore.sh"
PL="${M3_STATE}/${M3_LABEL}.plist"
if [ "$DRY_RUN" = 1 ]; then
  printf 'STATE: %s（由 launchd.plist.tmpl 生成）\n' "${PL}"
else
  sed -e "s|@ABROADD@|${INSTBIN}/abroadd|" -e "s|@STATE@|${M3_STATE}/abroadd|" -e "s|@VMDIR@|${M3_VMDIR}|" \
    -e "s|@STORE@|${M3_STATE}/store|" -e "s|@EXIT@|${EXIT_HOST}|" -e "s|@USER@|${M3_USER}|" -e "s|@LOG@|${OUT}/m3-abroadd-${TS}.log|" \
    -e "s|@LABEL@|${M3_LABEL}|" scripts/m3/launchd.plist.tmpl >"${PL}"
fi
m3_do cp "${PL}" "${M3_PLIST}"
m3_do chown root:wheel "${M3_PLIST}"
m3_do chmod 644 "${M3_PLIST}"
m3_do launchctl bootstrap system "${M3_PLIST}"
if [ "$DRY_RUN" != 1 ]; then
  for _ in $(seq 1 30); do [ -S "${API}" ] && break; sleep 1; done
  [ -S "${API}" ] || m3_die "30 秒内 abroadd 没起来，见 m3-abroadd-${TS}.log"
fi

# ---- 启用、写规则 ----
m3_log "启用。从这里开始，除了经主代理、开发出口和直连拨号模块，本机上网会断开。"
# 应用主代理（建立出口基准）：M4-3 起真实模式没有基准不能启用。abroadd 带 --dev-skip-region，不涉及时区和语言，
# 不需要确认码（m4-plan v2.5 建议改 3）。
m3_log "应用主代理（经主代理跑一轮出口检查，建立出口基准）。"
code=$(api POST /v1/main-proxy/apply)
[ "$code" = 200 ] || { [ "$DRY_RUN" != 1 ] && m3_redact <"${M3_STATE}/api.out" >>"${M3_LOG}"; m3_die "应用主代理返回 HTTP ${code}（主代理不可用、出口在大陆，或检查没有明确通过）"; }
code=$(api POST /v1/enable)
[ "$code" = 200 ] || { [ "$DRY_RUN" != 1 ] && m3_redact <"${M3_STATE}/api.out" >>"${M3_LOG}"; m3_die "启用返回 HTTP ${code}"; }
RULES_FILE="${M3_STATE}/rules.yaml"
write_rules() { # write_rules <Chrome 的动作>
  if [ "$DRY_RUN" = 1 ]; then printf 'STATE: rules.yaml（Chrome → %s）\n' "$1"; return; fi
  {
    echo "rules:"
    echo "  - {app: com.google.chrome, action: \"$1\"}"
    echo "  - {app: \"path:/usr/bin/curl\", action: \"proxy:clash-b\"}"
    echo "  - {app: \"path:/usr/bin/nc\", action: direct}"
    [ "${M3_INTERACTIVE:-0}" = 1 ] && echo "  - {app: com.tencent.xinwechat, action: direct}"
    echo "offline_allow: [\"path:/usr/bin/nc\"]"
  } >"${RULES_FILE}"
}
put_rules() { # put_rules <Chrome 的动作>：drop_affected=1，换规则时断开判定结果变了的在途连接
  write_rules "$1"
  local c; c=$(api PUT "/v1/rules?drop_affected=1" "${RULES_FILE}")
  [ "$c" = 200 ] || { [ "$DRY_RUN" != 1 ] && m3_redact <"${M3_STATE}/api.out" >>"${M3_LOG}"; m3_die "写规则返回 HTTP ${c}"; }
}
put_rules "proxy:wg-a"
sample "abroadd status（写规则后）" bash -c "curl -sS -m 10 --unix-socket '${API}' http://abroadd/v1/status"

# ---- Chrome：以用户自己的身份、用单独的临时配置启动（m3-plan v2.11 必须改 2）----
m3_do mkdir -p "${M3_CHROME_DIR}"
m3_do chown "${SUDO_USER}" "${M3_CHROME_DIR}"
m3_do chmod 700 "${M3_CHROME_DIR}"
HAVE_CHROME=1
[ "$DRY_RUN" != 1 ] && [ ! -d "/Applications/${CHROME_APP}.app" ] && HAVE_CHROME=0
chrome_open() { # chrome_open <URL>
  m3_do sudo -u "${SUDO_USER}" open -na "${CHROME_APP}" --args "--user-data-dir=${M3_CHROME_DIR}" --no-first-run --no-default-browser-check "$1"
  [ "$DRY_RUN" = 1 ] && return 0
  # 从锁文件取主进程的 PID（SingletonLock 是指向“主机名-PID”的符号链接），写进 PID 文件，restore 按它结束。
  local l p
  for _ in $(seq 1 25); do
    l=$(readlink "${M3_CHROME_DIR}/SingletonLock" 2>/dev/null) && break
    sleep 0.2
  done
  p=${l##*-}
  case "$p" in '' | *[!0-9]*) ;; *) m3_state_set chrome.pid "$p" ;; esac
}
wait_marker() { # wait_marker <日志> <标记> <秒>
  [ "$DRY_RUN" = 1 ] && return 0
  local i
  for i in $(seq 1 "$3"); do grep -qF "\"host\":\"$2" "$1" 2>/dev/null && return 0; sleep 1; done
  return 1
}

# ---- S1：Chrome 经出口 A ----
MARK="m3-$(mark)"
if [ "${HAVE_CHROME}" = 1 ]; then
  chrome_open "http://${MARK}-a.m3.test/whoami"
  wait_marker "${M3_UPDIR}/wg-a.jsonl" "${MARK}-a" 20
  if [ "$DRY_RUN" = 1 ]; then res S1 跳过 "空跑"; else
    why=$(judge_marker "${M3_UPDIR}/wg-a.jsonl" "${M3_UPDIR}/clash-b.jsonl" "${MARK}-a"); res_judge S1 $? "Chrome → 出口 A：${why}"
  fi
else
  res S1 跳过 "没装 Chrome（/Applications/${CHROME_APP}.app）"
fi

# ---- S2：curl 经出口 B ----
body=$(as_user curl -sS -m 15 "http://${MARK}-b.m3.test/whoami" 2>/dev/null || true)
if [ "$DRY_RUN" = 1 ]; then res S2 跳过 "空跑"; else
  why=$(judge_marker "${M3_UPDIR}/clash-b.jsonl" "${M3_UPDIR}/wg-a.jsonl" "${MARK}-b"); rc=$?
  [ "${body}" = clash-b ] || { rc=1; why="${why}；响应体不是 clash-b"; }
  res_judge S2 $rc "curl → 出口 B：${why}"
fi

# ---- S3：直连的应用（nc）按域名访问，经直连拨号模块（m3-plan v2.11 必须改 1：用域名，不用裸 IP）----
st0=$(status_json)
as_user /bin/bash -c "sleep 4 | /usr/bin/nc -w 6 '${DIRECT_HOST}' 443" >/dev/null 2>&1
sleep 1
st1=$(status_json)
if [ "$DRY_RUN" = 1 ]; then res S3 跳过 "空跑"; else
  why=$(judge_direct_domain "$(delta "$(jget "$st0" stack.direct_ok)" "$(jget "$st1" stack.direct_ok)")"); res_judge S3 $? "nc 按域名直连：${why}"
fi
# S3b：nc 连裸 IP 的 443、不发数据：等不到首包，按 port443_timeout 改走主代理，直连计数不变。
as_user /bin/bash -c "sleep 4 | /usr/bin/nc -w 6 ${RAW_IP443} 443" >/dev/null 2>&1
sleep 1
st2=$(status_json)
if [ "$DRY_RUN" = 1 ]; then res S3b 跳过 "空跑"; else
  why=$(judge_rawip443 "$(delta "$(jget "$st1" stack.direct_ok)" "$(jget "$st2" stack.direct_ok)")" \
    "$(delta "$(jget "$st1" stack.reasons.port443_timeout)" "$(jget "$st2" stack.reasons.port443_timeout)")")
  res_judge S3b $? "nc 连裸 IP 的 443：${why}"
fi

# ---- S4：Chrome 设成本机网络后访问 claude.ai（四条同时成立，m3-plan v2.10 决定 4）----
# 两个只读抓包：物理网卡上发往 Anthropic 地址段的包、53 端口的查询。只统计行数；53 端口的文本只放在 root 才能读的
# 状态目录里，统计完立刻删除（中途失败时 restore.sh 也删）。tcpdump 由 root 直接启动，$! 就是它自己的 PID。
cap_start() { # cap_start <名字> <-q 或空> <过滤式…>
  local n=$1 q=$2; shift 2
  if [ "$DRY_RUN" = 1 ]; then printf 'CHANGE: /usr/sbin/tcpdump -i %s -n -l %s%s\n' "${IF_BEFORE}" "${q:+$q }" "$*"; return; fi
  # 抓包文本只给 root 读（53 端口的那份里有这段时间所有应用查询的域名；状态目录是 755）：先建成 600 再写。
  ( umask 077; : >"${M3_STATE}/cap-$n.txt" )
  chmod 600 "${M3_STATE}/cap-$n.txt"
  if [ -n "$q" ]; then
    /usr/sbin/tcpdump -i "${IF_BEFORE}" -n -l "$q" "$@" >>"${M3_STATE}/cap-$n.txt" 2>/dev/null &
  else
    /usr/sbin/tcpdump -i "${IF_BEFORE}" -n -l "$@" >>"${M3_STATE}/cap-$n.txt" 2>/dev/null &
  fi
  m3_state_set "tcpdump-$n.pid" "$!"
}
cap_stop() { m3_kill_pidfile "${M3_STATE}/tcpdump-$1.pid" "/usr/sbin/tcpdump -i ${IF_BEFORE}"; }
cap_drop() { m3_do rm -f "${M3_STATE}/cap-$1.txt"; }
if [ "${HAVE_CHROME}" = 1 ]; then
  put_rules direct
  cap_start anth -q dst net 160.79.104.0/21
  cap_start dns "" port 53
  [ "$DRY_RUN" != 1 ] && sleep 1
  st0=$(status_json)
  chrome_open "https://claude.ai/"
  [ "$DRY_RUN" != 1 ] && sleep 20
  st1=$(status_json)
  cap_stop anth
  cap_stop dns
  PK=$(count_packets "${M3_STATE}/cap-anth.txt"); DQ=$(count_dns_queries "${M3_STATE}/cap-dns.txt")
  cap_drop anth
  cap_drop dns
  if [ "$DRY_RUN" = 1 ]; then res S4 跳过 "空跑"; else
    pin=$(($(delta "$(jget "$st0" stack.reasons.pinned)" "$(jget "$st1" stack.reasons.pinned)") + \
      $(delta "$(jget "$st0" stack.reasons.pinned_resolved)" "$(jget "$st1" stack.reasons.pinned_resolved)")))
    # 主代理出口自己的开流计数（M4-8：status.opens.main.tcp；原来的 stack.tcp_proxied 也算分流出口的连接）
    main=$(delta "$(jget "$st0" opens.main.tcp)" "$(jget "$st1" opens.main.tcp)")
    why=$(judge_pinned_case "$pin" "$main" "${PK:-0}" "${DQ:-0}"); res_judge S4 $? "Chrome 设成本机网络后访问 claude.ai：${why}"
  fi
  put_rules "proxy:wg-a"
else
  res S4 跳过 "没装 Chrome"
fi

# ---- S5：R3 的 root 部分：系统进程的识别抽查 + r3-accuracy.sh ----
if [ "$DRY_RUN" = 1 ]; then
  printf 'QUERY: %s/abroad-dev ident-sample\n' "${INSTBIN}"
  printf 'QUERY: bash scripts/m3/r3-accuracy.sh\n'
  res S5 跳过 "空跑"
else
  IDS=$("${INSTBIN}/abroad-dev" ident-sample 2>&1)
  printf '\n==== ident-sample（只有计数和 system: 名单）====\n%s\n' "${IDS}" >>"${SAMPLES}"
  bash scripts/m3/r3-accuracy.sh >>"${M3_LOG}" 2>&1 || true
  res S5 记录 "识别率 $(jget "${IDS}" rate_pct)%（条目 $(jget "${IDS}" with_pid)，系统进程 $(jget "${IDS}" kinds.system)），r3-accuracy 的结果见 scripts/m3/out/r3-curl-*.txt"
fi

# ---- S6：在途 fd：从 root 一侧经直连拨号模块拨 300 次，每次做完整 TLS 握手（m3-plan v2.11 偏差 2）----
ns0=$(vm_netstats); st0=$(status_json)
if [ "$DRY_RUN" = 1 ]; then
  printf 'QUERY: %s/abroad-dev direct-probe --sock %s/direct.sock --server-uid <测试用户> --own-config --ip %s --port %s --tls-name %s --count 300 --concurrency 10\n' \
    "${INSTBIN}" "${M3_VMDIR}" "${DIRECT_TARGET%:*}" "${DIRECT_TARGET##*:}" "${DIRECT_TLS_NAME}"
  res S6 跳过 "空跑"
else
  BJ=$("${INSTBIN}/abroad-dev" direct-probe --sock "${M3_VMDIR}/direct.sock" --server-uid "${TUID}" --own-config \
    --ip "${DIRECT_TARGET%:*}" --port "${DIRECT_TARGET##*:}" --tls-name "${DIRECT_TLS_NAME}" --count 300 --concurrency 10 --timeout 15s 2>&1)
  printf '\n==== 在途 fd：direct-probe 批量（不含地址）====\n%s\n' "${BJ}" >>"${SAMPLES}"
  why=$(judge_batch "${BJ}"); S6RC=$?; res_judge S6 $S6RC "在途 fd：${why}"
fi
# S6b：探针带了自己的配置；abroadd 发起的下一条直连应收到 stale_config、自动补发、照常成功（v2.9 的自愈路径）。
as_user /bin/bash -c "sleep 4 | /usr/bin/nc -w 6 '${DIRECT_HOST}' 443" >/dev/null 2>&1
sleep 1
ns1=$(vm_netstats); st1=$(status_json)
if [ "$DRY_RUN" = 1 ]; then res S6b 跳过 "空跑"; else
  why=$(judge_selfheal "$(delta "$(jget "$ns0" direct.stale_config)" "$(jget "$ns1" direct.stale_config)")" \
    "$(delta "$(jget "$st0" stack.direct_ok)" "$(jget "$st1" stack.direct_ok)")")
  res_judge S6b $? "探针之后直连自愈：${why}"
fi

# ---- S7：直连拨号模块以测试用户运行时，出站被 pf 放行（对照 R2）----
if [ "$DRY_RUN" = 1 ]; then res S7 跳过 "空跑"
elif grep -qE '^S3 +通过' "${RESULTS}" || [ "${S6RC:-1}" = 0 ]; then
  res S7 通过 "启用状态下（pf 只放行测试用户）直连拨号模块的出站连上了（见 S3、S6），与 R2 的结论对照"
else
  res S7 不通过 "S3、S6 都没有连上：可能是 pf 的 user 规则没放行测试用户（对照 R2），请贴回日志"
fi

# ---- S9：R15：Docker、OrbStack（在运行时各做一次容器内访问，只记录）----
if [ "$DRY_RUN" = 1 ]; then
  printf 'USER: docker run --rm busybox wget -q -T 10 -O /dev/null http://www.example.com\n'
  res S9 跳过 "空跑"
elif as_user docker info >/dev/null 2>&1; then
  ctx=$(as_user docker context show 2>/dev/null || echo default)
  st0=$(status_json)
  as_user docker run --rm busybox wget -q -T 10 -O /dev/null http://www.example.com >/dev/null 2>&1; drc=$?
  st1=$(status_json)
  res S9 记录 "Docker（上下文 ${ctx}）容器内访问退出码 ${drc}；经代理 +$(delta "$(jget "$st0" stack.tcp_proxied)" "$(jget "$st1" stack.tcp_proxied)")、直连 +$(delta "$(jget "$st0" stack.direct_ok)" "$(jget "$st1" stack.direct_ok)")"
else
  res S9 跳过 "没有在运行的 Docker / OrbStack"
fi

# ---- 微信（M3_INTERACTIVE=1，只看计数）----
if [ "${M3_INTERACTIVE:-0}" = 1 ] && [ "$DRY_RUN" != 1 ]; then
  st0=$(status_json)
  m3_log "请打开微信给任意联系人（例如文件传输助手）发一条消息，然后按回车。"; read -r _
  st1=$(status_json)
  res WX 记录 "微信设成直连后：直连 +$(delta "$(jget "$st0" stack.direct_ok)" "$(jget "$st1" stack.direct_ok)")"
else
  res WX 跳过 "需要人工：设 M3_INTERACTIVE=1 重跑这一项"
fi

# ---- S8：停用后，offline_allow 的应用（nc）访问普通域名能通、访问 claude.ai 不通 ----
code=$(api POST /v1/disable)
st0=$(status_json)
as_user /bin/bash -c "sleep 4 | /usr/bin/nc -w 6 '${DIRECT_HOST}' 443" >/dev/null 2>&1
sleep 1
st1=$(status_json)
cap_start anth2 -q dst net 160.79.104.0/21
[ "$DRY_RUN" != 1 ] && sleep 1
as_user /bin/bash -c "sleep 4 | /usr/bin/nc -w 6 claude.ai 443" >/dev/null 2>&1
sleep 1
st2=$(status_json)
cap_stop anth2
PK=$(count_packets "${M3_STATE}/cap-anth2.txt")
cap_drop anth2
if [ "$DRY_RUN" = 1 ]; then res S8 跳过 "空跑"
elif [ "$code" != 200 ]; then res S8 不通过 "停用返回 HTTP ${code}"
else
  why=$(judge_offline "$(delta "$(jget "$st0" stack.direct_ok)" "$(jget "$st1" stack.direct_ok)")" \
    "$(delta "$(jget "$st1" stack.direct_ok)" "$(jget "$st2" stack.direct_ok)")" "${PK:-0}")
  res_judge S8 $? "停用后：${why}"
fi

# ---- S10：恢复，与快照逐项比对（同 M2 第 6 步）----
m3_log "恢复本机网络。"
restore_api
code=$RCODE
diffs=()
if [ "$DRY_RUN" != 1 ]; then
  while IFS=$'\t' read -r s mode; do
    now=$(networksetup -getinfo "$s" 2>/dev/null | awk -F': ' '/^IPv6: /{print $2}')
    [ "$now" = "$mode" ] || diffs+=("IPv6 不同")
  done <"${M3_STATE}/snap/ipv6"
  netstat -rn -f inet | awk '$4 ~ /^utun/ && ($1=="0/1" || $1=="128.0/1")' | grep -q . && diffs+=("还有经 utun 的 /1 路由")
  printf 'show %s\nquit\n' "${M3_DNS_KEY}" | scutil | grep -q 'No such key' || diffs+=("DNS 键还在")
  [ -z "$(/sbin/pfctl -a "${M3_ANCHOR}" -s rules 2>/dev/null)" ] || diffs+=("锚点不空")
fi
if [ "$DRY_RUN" = 1 ]; then res S10 跳过 "空跑"
elif [ "$code" = 200 ] && [ ${#diffs[@]} -eq 0 ]; then res S10 通过 "路由、DNS、IPv6、锚点与启用前一致"; else res S10 不通过 "HTTP ${code}；${diffs[*]:-}"; fi
if [ -n "${GW_BEFORE}" ] && [ "$DRY_RUN" != 1 ]; then
  as_user ping -c 2 -t 3 "${GW_BEFORE}" >/dev/null 2>&1 && m3_log "恢复后路由器可达" || m3_log "恢复后路由器不可达（请贴回日志）"
fi

# ---- 收尾：restore.sh ----
m3_log "收尾：执行 restore.sh"
M3_RESTORED=1
if [ "$DRY_RUN" = 1 ]; then DRY_RUN=1 bash "${RESTORE}"; else bash "${RESTORE}"; fi
m3_log "结果汇总（${RESULTS}）："
cat "${RESULTS}" >&2
m3_log "请贴回：${M3_LOG}、${RESULTS}、${SAMPLES}，scripts/m3/out/m3-abroadd-${TS}.log、m3-abroad-vm-${TS}.log，以及 scripts/m3/out/r3-curl-*.txt（最新的一份）"
