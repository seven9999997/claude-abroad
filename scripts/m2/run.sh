#!/usr/bin/env bash
# M2 真机验证（M2-9c）：在这台 Mac 上以真实模式跑 abroadd，逐项验证断网保护、接管、恢复，并采集真机样本。
# 用法见 scripts/m2/README.md。会改动本机网络、需要 sudo，由用户执行；主会话只做语法检查、DRY_RUN=1 空跑和 --check-only。
#
#   sudo scripts/m2/run.sh                # 正式运行
#   scripts/m2/run.sh --check-only        # 只做前提检查（只读，不需要 root）
#   DRY_RUN=1 scripts/m2/run.sh           # 空跑：只打印会执行的改动命令（CHANGE:）和 pfctl 查询（QUERY:）
#
# 环境变量：M2_NODES（主代理节点的 Clash YAML，必填）、M2_NODE（选哪个节点，必填）、M2_EXIT_HOST（出口检查主机，
# 默认 www.gstatic.com）、M2_DEADMAN_SECS（默认 900）、M2_IPERF_SERVER（可选）、M2_INTERACTIVE=1（做睡眠唤醒、换 Wi-Fi 两项）、
# M2_LONGCONN（第 8 步的外部长连接目标 host:port，默认 www.baidu.com:80）。
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
. scripts/m2/lib.sh
exec 3>&1 # 空跑时 API 行打到这里，不混进 $(...) 拿到的状态码

CHECK_ONLY=0
case "${1:-}" in
  --check-only) CHECK_ONLY=1 ;;
  "") ;;
  *) echo "用法：sudo $0 [--check-only]" >&2; exit 2 ;;
esac

OUT="${ROOT}/scripts/m2/out"
TS="$(date +%Y%m%d-%H%M%S)"
mkdir -p "${OUT}"
M2_LOG="${OUT}/m2-${TS}.log"
SAMPLES="${OUT}/m2-samples-${TS}.txt"
RESULTS="${OUT}/m2-results-${TS}.txt"
: >"${M2_LOG}"
: >"${SAMPLES}"
: >"${RESULTS}"
BIN="${ROOT}/scripts/dev/out/bin"
EXIT_HOST="${M2_EXIT_HOST:-www.gstatic.com}"
DEADMAN_SECS="${M2_DEADMAN_SECS:-900}"
LONGCONN="${M2_LONGCONN:-www.baidu.com:80}"
API="${M2_STATE}/abroadd/api.sock"
VMDIR="${M2_STATE}/vm"
INSTBIN="${M2_INSTALL}/bin"
RESTORE="${ROOT}/scripts/m2/restore.sh"

# res <步骤> <通过|不通过|跳过> <说明>
res() { printf '%-4s %-6s %s\n' "$1" "$2" "$3" | tee -a "${RESULTS}" >&2; }
# sample <标题> <命令…>：把输出（脱敏后）写进采样文件。pfctl 经 m2_q（空跑时不执行）。
sample() {
  local title=$1; shift
  if [ "$DRY_RUN" = 1 ]; then # 空跑：不执行，m2_q、m2_do 自己打印 QUERY:/CHANGE:，其余命令打印成 QUERY:
    case "$1" in m2_q | m2_do) "$@" ;; *) printf 'QUERY: %s\n' "$*" ;; esac
    return 0
  fi
  { printf '\n==== %s ====\n$ %s\n' "$title" "$*"; "$@" 2>&1 | m2_redact; printf '（退出码 %s）\n' "${PIPESTATUS[0]}"; } >>"${SAMPLES}"
}
as_user() { if [ "$DRY_RUN" = 1 ]; then printf 'USER: %s\n' "$*"; return 0; fi; sudo -u "${SUDO_USER}" "$@"; }
api() { # api <方法> <路径> [请求体文件]：经本地接口调用 abroadd，输出 HTTP 状态码（应答在 api.out）
  if [ "$DRY_RUN" = 1 ]; then printf 'API: %s %s\n' "$1" "$2" >&3; echo 200; return 0; fi
  local data=()
  [ -n "${3:-}" ] && data=(--data-binary "@$3")
  curl -sS -m 180 --unix-socket "${API}" -X "$1" ${data[@]+"${data[@]}"} -o "${M2_STATE}/api.out" -w '%{http_code}' "http://abroadd$2" 2>/dev/null
}
# restore_api：两步恢复（m4-plan v2.3 决定 5）。第一次回 409，带预览和确认码；带确认码再调一次。最终状态码放在 RCODE。
# 确认码只写进 0600 的临时文件当请求体，用完就删，不打印。
restore_api() {
  local c cc f="${M2_STATE}/confirm.json"
  if [ "$DRY_RUN" = 1 ]; then
    api POST /v1/restore >/dev/null
    api POST /v1/restore "$f" >/dev/null
    RCODE=200
    return 0
  fi
  c=$(api POST /v1/restore)
  if [ "$c" = 409 ]; then
    cc=$(sed -n 's/.*"confirm":"\([0-9a-f]\{32\}\)".*/\1/p' "${M2_STATE}/api.out")
    if [ -n "$cc" ]; then
      (umask 077 && printf '{"confirm":"%s"}' "$cc" >"$f")
      c=$(api POST /v1/restore "$f")
      rm -f "$f"
    fi
  fi
  RCODE=$c
}

# ---- 前提检查（只读，不需要 root）----
precheck() {
  local reasons=() line
  # 1. 别的代理软件的 TUN：经虚拟网卡的公网或 fake-IP 段路由（与 internal/sysnet 的冲突定义一致）。
  local conf
  conf=$(netstat -rn -f inet 2>/dev/null | awk '
    $4 ~ /^(utun|ipsec|ppp|tun|tap|wg)[0-9]+$/ {
      d=$1
      if (d=="default") { print $4; next }
      if (d ~ /^(10|127)(\.|\/|$)/ || d ~ /^169\.254/ || d ~ /^192\.168/ || d ~ /^172\.(1[6-9]|2[0-9]|3[01])(\.|\/|$)/ || d ~ /^(22[4-9]|23[0-9]|24[0-9]|25[0-5])(\.|\/|$)/) next
      print $4
    }' | sort -u | tr '\n' ' ')
  [ -n "${conf}" ] && reasons+=("别的代理软件的 TUN 开着（接口：${conf% }）：请先在 Clash 等软件里关掉 TUN 模式")
  # 1b. 系统代理（HTTP、HTTPS、SOCKS、PAC、自动发现）：开着时启用会被 abroadd 的冲突检查拦下，那时程序、用户都已装好。
  m2_system_proxy_on && reasons+=("系统代理开着：请在“系统设置 → 网络 → 详细信息 → 代理”里全部关掉，或在代理软件里关掉“设为系统代理”")
  # 1c. 上一次的 abroadd 状态文件：留着的话 abroadd 一启动就会按它自动接管（上次中途失败或由死人开关还原时会留下）。
  [ -e "${M2_STATE}/abroadd/state.json" ] && reasons+=("上一次运行留下的 abroadd 状态文件还在（${M2_STATE}/abroadd/state.json）：请先执行 sudo scripts/m2/restore.sh")
  # 2. 主代理：节点文件、节点名；第一跳不能是本机回环。
  if [ -z "${M2_NODES:-}" ] || [ ! -f "${M2_NODES:-/nonexistent}" ] || [ -z "${M2_NODE:-}" ]; then
    reasons+=("没有主代理：请设置 M2_NODES（远端或局域网节点的 Clash YAML）和 M2_NODE（节点名）")
  elif [ -x "${BIN}/abroad-dev" ]; then
    local allow
    if ! allow=$("${BIN}/abroad-dev" apply-main-proxy --nodes "${M2_NODES}" --selected "${M2_NODE}" --print-allow 2>&1); then
      reasons+=("主代理节点不可用：$(printf '%s' "${allow}" | head -1 | m2_redact)")
    elif printf '%s\n' "${allow}" | grep -q '^127\.'; then
      reasons+=("主代理的第一跳是本机回环（本机代理软件当主代理会回环）：请换成远端或局域网节点")
    fi
  fi
  # 3. IPv6：有手动配置的网络服务时拒绝（脚本的还原只覆盖 Automatic、Off、Link-local）。
  while IFS= read -r line; do
    [ -n "${line}" ] || continue
    if networksetup -getinfo "${line}" 2>/dev/null | grep -q '^IPv6: Manual'; then
      reasons+=("网络服务的 IPv6 是手动配置：请先改成自动（系统设置 → 网络 → 详细信息 → TCP/IP）")
      break
    fi
  done < <(m2_services)
  # 4. 程序和镜像。
  for f in abroad-vm abroadd abroad-dev; do
    [ -x "${BIN}/${f}" ] || reasons+=("缺少 ${BIN}/${f}：见 README 的准备步骤")
  done
  [ -f guest/out/Image ] || reasons+=("缺少虚拟机镜像 guest/out：先跑一次 scripts/dev/vm-up.sh --no-panic")
  m2_log "提醒：运行期间会断网几次（见 README），请先结束 Claude Code 会话和其他要紧的网络任务。"
  if [ ${#reasons[@]} -gt 0 ]; then
    for r in "${reasons[@]}"; do m2_log "前提不满足：${r}"; done
    return 1
  fi
  m2_log "前提检查通过"
  return 0
}

if [ "${CHECK_ONLY}" = 1 ]; then m2_test_guard check; else m2_test_guard; fi
if [ "${CHECK_ONLY}" = 1 ]; then
  precheck
  exit $?
fi
if ! precheck; then
  if [ "$DRY_RUN" = 1 ]; then m2_log "（空跑：前提不满足，继续打印后面的命令）"; else exit 3; fi
fi

m2_require_root
[ "$DRY_RUN" = 1 ] && SUDO_USER=${SUDO_USER:-$(id -un)}

# ---- 先建状态目录（死人开关的日志和 PID 写在里面），紧接着启动死人开关；其余改动都在它之后 ----
m2_state_init
m2_deadman_start "${DEADMAN_SECS}" "${RESTORE}"
m2_arm_restore_trap "${RESTORE}" # 中途失败、Ctrl-C 时立刻还原，不用等死人开关
GW_BEFORE="$(m2_default_gw)"

# ---- 快照（restore.sh 只靠它还原，不依赖 abroadd）----
snapshot() {
  m2_do mkdir -p "${M2_STATE}/snap"
  if [ "$DRY_RUN" = 1 ]; then printf 'STATE: snap/ipv6、routes、dns、pf\n'; return; fi
  local s
  : >"${M2_STATE}/snap/ipv6"
  while IFS= read -r s; do
    [ -n "$s" ] || continue
    printf '%s\t%s\n' "$s" "$(networksetup -getinfo "$s" 2>/dev/null | awk -F': ' '/^IPv6: /{print $2}')" >>"${M2_STATE}/snap/ipv6"
  done < <(m2_services)
  netstat -rn -f inet >"${M2_STATE}/snap/routes" 2>&1
  printf 'show %s\nquit\n' "${M2_DNS_KEY}" | scutil >"${M2_STATE}/snap/dns" 2>&1
  pfctl -s info 2>/dev/null | head -1 >"${M2_STATE}/snap/pf"
}
# IPv6 关闭是持久的，而 /var/run 重启即清空：快照另存一份到重启不丢的目录，restore.sh 优先读它，还原成功后才删。
snapshot_persist() {
  m2_do mkdir -p "${M2_SNAP}"
  m2_do chmod 700 "${M2_SNAP}"
  m2_do cp -R "${M2_STATE}/snap/." "${M2_SNAP}/"
}
snapshot
snapshot_persist
m2_log "已拍快照：IPv6、路由表、DNS 键、pf 状态（另存一份到 ${M2_SNAP}，重启不丢）"

# ---- 安装程序、建测试用户、准备主代理 ----
m2_do mkdir -p "${INSTBIN}" "${M2_INSTALL}/image"
for f in abroad-vm abroadd abroad-dev; do m2_do cp "${BIN}/${f}" "${INSTBIN}/${f}"; done
m2_do cp -R guest/out/. "${M2_INSTALL}/image/"
m2_do chown -R root:wheel "${M2_INSTALL}"
m2_do chmod -R go-w "${M2_INSTALL}"
m2_user_create
m2_do mkdir -p "${VMDIR}"
m2_do chown "${M2_USER}" "${VMDIR}"
m2_do chmod 700 "${VMDIR}"
m2_do "${INSTBIN}/abroad-dev" apply-main-proxy --nodes "${M2_NODES:-<节点文件>}" --selected "${M2_NODE:-<节点名>}" --save-store "${M2_STATE}/store"
m2_do chmod -R go-rwx "${M2_STATE}/store" # 存储里有节点的凭据，只给 root 读
ALLOW=""; HOPS=""
if [ "$DRY_RUN" != 1 ]; then
  ALLOW=$("${INSTBIN}/abroad-dev" apply-main-proxy --nodes "${M2_NODES}" --selected "${M2_NODE}" --print-allow)
  HOPS=$("${INSTBIN}/abroad-dev" apply-main-proxy --nodes "${M2_NODES}" --selected "${M2_NODE}" --print-first-hop)
fi
VMARGS=(--image-dir "${M2_INSTALL}/image" --state-dir "${VMDIR}")
for a in ${ALLOW}; do VMARGS+=(--allow-outbound "$a"); done
for h in ${HOPS}; do VMARGS+=(--first-hop "$h"); done

# ---- 第 1 步：pfctl -n 语法检查生成的规则（只读）+ 启用前的 pf 样本 ----
RULES="${M2_STATE}/rules.conf"
if [ "$DRY_RUN" = 1 ]; then
  printf 'STATE: rules.conf（abroad-dev pf-render --user %s --utun utun99 --lan <局域网> --vmif bridge100）\n' "${M2_USER}"
else
  "${INSTBIN}/abroad-dev" pf-render --user "${M2_USER}" --utun utun99 --vmif bridge100 >"${RULES}"
fi
if out=$(m2_q /sbin/pfctl -n -f "${RULES}"); then res 1 通过 "pfctl -n 语法检查"; else res 1 不通过 "pfctl -n：$(printf '%s' "$out" | head -1)"; fi
sample "pfctl -n -v -o none -f（加载前的打印样本）" m2_q /sbin/pfctl -n -v -o none -f "${RULES}"
sample "pfctl -s info 首行（启用前）" m2_q /sbin/pfctl -s info
sample "pfctl -s rules（主规则集）" m2_q /sbin/pfctl -s rules
sample "pfctl -a 锚点 -s rules（启用前，应为空）" m2_q /sbin/pfctl -a "${M2_ANCHOR}" -s rules
sample "pfctl -a 锚点 -t abroad_lan -T show（表不存在）" m2_q /sbin/pfctl -a "${M2_ANCHOR}" -t abroad_lan -T show
# pfctl -E 在未启用、已启用两种情况下的输出；-X 已释放的 token。token 先记进状态再释放（中断也能清掉）。
pf_E_sample() {
  local o t
  if [ "$DRY_RUN" = 1 ]; then printf 'CHANGE: /sbin/pfctl -E\n'; return; fi
  o=$(/sbin/pfctl -E 2>&1); t=$(printf '%s\n' "$o" | awk -F': *' '/Token/{print $2}' | tr -d ' ')
  [ -n "$t" ] && m2_state_add pf_tokens "$t"
  { printf '\n==== pfctl -E（%s）====\n' "$1"; printf '%s\n' "$o"; } >>"${SAMPLES}"
  printf '%s' "$t"
}
if [ "$DRY_RUN" = 1 ]; then # 空跑：直接打印（放在 $(...) 里会被当成 token 吞掉）
  printf 'CHANGE: /sbin/pfctl -E\n'; printf 'CHANGE: /sbin/pfctl -E\n'; T1="<token1>"; T2="<token2>"
else
  T1=$(pf_E_sample "第一次")
  T2=$(pf_E_sample "已启用时再来一次")
fi
for t in "${T2:-<token2>}" "${T1:-<token1>}"; do
  if [ "$DRY_RUN" = 1 ]; then m2_do /sbin/pfctl -X "$t"; else m2_do /sbin/pfctl -X "$t" >>"${SAMPLES}" 2>&1; fi
done
sample "pfctl -X 已释放的 token" m2_do /sbin/pfctl -X "${T1:-<token1>}"
sample "pfctl -s info 首行（释放后）" m2_q /sbin/pfctl -s info
# sysnet 的真机样本（替换 internal/sysnet/testdata/*.constructed*）。
while IFS= read -r s; do [ -n "$s" ] && sample "networksetup -getinfo（启用前）" networksetup -getinfo "$s"; done < <(m2_services)
sample "route -n get 1.1.1.1（启用前，经物理网卡）" route -n get 1.1.1.1
sample "networksetup 出错时（不存在的服务）" networksetup -getinfo "claude-abroad-no-such-service"

# ---- 第 8 步准备：启用前建一条外部长连接和一条本机回环长连接 ----
LC="${M2_STATE}/longconn.py"
m2_do cp "${ROOT}/scripts/m2/longconn.py" "${LC}"
# 系统自带的 /usr/bin/python3 可能只是“未安装命令行工具”的占位：先试一下，不能用就把第 8 步记为跳过。
LC_OK=1
if [ "$DRY_RUN" != 1 ] && ! /usr/bin/python3 -c 'print(1)' >/dev/null 2>&1; then LC_OK=0; fi
lc_start() { # lc_start <名字> <模式> <目标>：python 自己写 PID 文件（不用 $!，那只是 sudo 的 PID）
  [ "$LC_OK" = 1 ] || return 0
  if [ "$DRY_RUN" = 1 ]; then printf 'USER: python3 %s %s %s <状态文件> %s\n' "${LC}" "$2" "$3" "${M2_STATE}/lc-$1.pid"; return; fi
  sudo -u "${SUDO_USER}" /usr/bin/python3 "${LC}" "$2" "$3" "${M2_STATE}/lc-$1.st" "${M2_STATE}/lc-$1.pid" >/dev/null 2>&1 &
  for _ in $(seq 1 20); do [ -s "${M2_STATE}/lc-$1.pid" ] && break; sleep 0.2; done
}
lc_check() { # lc_check <名字>：触发一次检查，回显最后一次结果
  [ "$DRY_RUN" = 1 ] && { echo ok; return; }
  local pid; pid=$(cat "${M2_STATE}/lc-$1.pid" 2>/dev/null)
  ps -p "${pid:-0}" -o command= 2>/dev/null | grep -qF "${LC}" || { echo "测试进程不在"; return; }
  kill -USR1 "$pid" 2>/dev/null; sleep 9; tail -1 "${M2_STATE}/lc-$1.st" 2>/dev/null
}
if [ "$DRY_RUN" != 1 ]; then chmod 755 "${M2_STATE}"; chmod 644 "${LC}" 2>/dev/null; fi
lc_start ext ext "${LONGCONN}"
lc_start lo lo 47998
sleep 2

# ---- 以测试用户启动 abroad-vm（R1：起不来就停下）----
if [ "$DRY_RUN" = 1 ]; then
  printf 'CHANGE: sudo -u %s %s/abroad-vm run %s\n' "${M2_USER}" "${INSTBIN}" "${VMARGS[*]}"
else
  # abroad-vm 自己把 PID 写进状态目录的 abroad-vm.pid（vmhost.StateDir.PIDFile）；restore.sh 按它结束（$! 只是 sudo 的 PID）。
  sudo -u "${M2_USER}" "${INSTBIN}/abroad-vm" run "${VMARGS[@]}" >>"${OUT}/m2-abroad-vm-${TS}.log" 2>&1 &
  ok=0
  for _ in $(seq 1 60); do
    curl -fsS -m 3 --unix-socket "${VMDIR}/ctl.sock" http://abroad-agent/v1/health >/dev/null 2>&1 && { ok=1; break; }
    sleep 1
  done
  if [ $ok != 1 ]; then
    res 2 不通过 "abroad-vm 以测试用户 ${M2_USER} 起不来（这是 R1 的结论：Virtualization.framework 不能在非登录用户下运行）。不再继续，请贴回日志。"
    M2_RESTORED=1
    bash "${RESTORE}"
    exit 4
  fi
fi
m2_do "${INSTBIN}/abroad-dev" apply-main-proxy --nodes "${M2_NODES:-<节点文件>}" --selected "${M2_NODE:-<节点名>}" --ctl-sock "${VMDIR}/ctl.sock"

# ---- 第 10 步：安装开发用 LaunchDaemon（KeepAlive）启动 abroadd ----
# 上一次留下的状态文件会让 abroadd 一启动就按它接管（前提检查查过一次，这里装之前再确认）。
[ -e "${M2_STATE}/abroadd/state.json" ] && m2_die "上一次的 abroadd 状态文件还在：先执行 sudo scripts/m2/restore.sh"
PL="${M2_STATE}/${M2_LABEL}.plist"
if [ "$DRY_RUN" = 1 ]; then
  printf 'STATE: %s（由 launchd.plist.tmpl 生成）\n' "${PL}"
else
  sed -e "s|@ABROADD@|${INSTBIN}/abroadd|" -e "s|@STATE@|${M2_STATE}/abroadd|" -e "s|@VMDIR@|${VMDIR}|" \
    -e "s|@STORE@|${M2_STATE}/store|" -e "s|@EXIT@|${EXIT_HOST}|" -e "s|@USER@|${M2_USER}|" -e "s|@LOG@|${OUT}/m2-abroadd-${TS}.log|" \
    -e "s|@LABEL@|${M2_LABEL}|" scripts/m2/launchd.plist.tmpl >"${PL}"
fi
m2_do cp "${PL}" "${M2_PLIST}"
m2_do chown root:wheel "${M2_PLIST}"
m2_do chmod 644 "${M2_PLIST}"
m2_do launchctl bootstrap system "${M2_PLIST}"
if [ "$DRY_RUN" != 1 ]; then
  for _ in $(seq 1 30); do [ -S "${API}" ] && break; sleep 1; done
  if [ -S "${API}" ]; then res 10 通过 "LaunchDaemon 已装、abroadd 已起（KeepAlive）"; else res 10 不通过 "30 秒内 abroadd 没起来，见 m2-abroadd-${TS}.log"; fi
else
  res 10 跳过 "空跑"
fi

# ---- 第 2 步：启用 ----
m2_log "第 2 步：启用。从这里开始，除了经主代理，本机上网会断开（预计 1–2 分钟内完成本步）。"
# 应用主代理（建立出口基准）：M4-3 起真实模式没有基准不能启用。abroadd 带 --dev-skip-region，不涉及时区和语言，
# 不需要确认码（m4-plan v2.5 建议改 3）。
m2_log "应用主代理（经主代理跑一轮出口检查，建立出口基准）。"
code=$(api POST /v1/main-proxy/apply)
[ "$code" = 200 ] || { [ "$DRY_RUN" != 1 ] && m2_redact <"${M2_STATE}/api.out" >>"${M2_LOG}"; m2_die "应用主代理返回 HTTP ${code}（主代理不可用、出口在大陆，或检查没有明确通过）"; }
code=$(api POST /v1/enable)
if [ "$code" = 200 ]; then
  ip=$(as_user curl -sS -m 15 https://api.ipify.org 2>/dev/null || true)
  fake=$(as_user dig +short +time=5 example.com 2>/dev/null | head -1 || true)
  v6=$(as_user curl -6 -sS -m 5 -o /dev/null -w '%{http_code}' https://ipv6.google.com 2>/dev/null || true)
  [ "$DRY_RUN" = 1 ] && { ip=x; fake=198.18.1.1; v6=000; }
  st2="出口 IP：$([ -n "$ip" ] && echo 拿到 || echo 没拿到)；dig：$(case "$fake" in 198.1[89].*) echo 假地址 ;; *) echo "不是假地址" ;; esac)；IPv6：$([ "${v6:-000}" = 000 ] && echo 不通 || echo "通了（${v6}）")"
  case "$fake" in 198.1[89].*) fk=1 ;; *) fk=0 ;; esac
  if [ -n "$ip" ] && [ $fk = 1 ] && [ "${v6:-000}" = 000 ]; then res 2 通过 "$st2"; else res 2 不通过 "$st2"; fi
else
  res 2 不通过 "启用返回 HTTP ${code}：见 ${M2_STATE}/api.out"
  [ "$DRY_RUN" != 1 ] && m2_redact <"${M2_STATE}/api.out" >>"${M2_LOG}"
fi
sample "pfctl -a 锚点 -s rules（启用后，实际加载的 -o none 打印）" m2_q /sbin/pfctl -a "${M2_ANCHOR}" -s rules
sample "pfctl -a 锚点 -t abroad_lan -T show（表存在）" m2_q /sbin/pfctl -a "${M2_ANCHOR}" -t abroad_lan -T show
sample "pfctl -s info 首行（启用后）" m2_q /sbin/pfctl -s info
sample "scutil show 我们的 DNS 键" bash -c "printf 'show ${M2_DNS_KEY}\nquit\n' | scutil"
sample "scutil --dns（我们的解析器是否首选）" scutil --dns
sample "route -n get 1.1.1.1（启用后，应经 utun）" route -n get 1.1.1.1
while IFS= read -r s; do [ -n "$s" ] && sample "networksetup -getinfo（启用后，IPv6 应为 Off）" networksetup -getinfo "$s"; done < <(m2_services)
sample "abroadd status" bash -c "curl -sS -m 10 --unix-socket '${API}' http://abroadd/v1/status"

# ---- 第 8 步：启用前的外部长连接被断开；本机回环长连接仍可用 ----
if [ "$LC_OK" != 1 ]; then
  res 8 跳过 "/usr/bin/python3 不可用（可能没装命令行工具），长连接测试没法做"
else
  e=$(lc_check ext); l=$(lc_check lo)
  [ "$DRY_RUN" = 1 ] && e=broken
  # 验证的是综合效果：启用后旧连接的状态被清（KillStates），同时路由切到 utun，两者都会让它断开。
  if [ "$e" = broken ] && [ "$l" = ok ]; then res 8 通过 "启用前的外部长连接已断开，本机回环长连接仍可用（lo0 无状态）"; else res 8 不通过 "外部长连接：${e:-无结果}；回环长连接：${l:-无结果}"; fi
fi

# ---- 第 4 步：清空锚点，5 秒内恢复 ----
m2_log "第 4 步：清空我们的锚点（断网保护会暂时失效，巡检应在 5 秒内重新加载）。"
m2_do /sbin/pfctl -a "${M2_ANCHOR}" -F all
back=-1
if [ "$DRY_RUN" != 1 ]; then
  for i in $(seq 1 15); do
    sleep 1
    [ -n "$(/sbin/pfctl -a "${M2_ANCHOR}" -s rules 2>/dev/null)" ] && { back=$i; break; }
  done
else back=3; fi
l=ok; [ "$LC_OK" = 1 ] && l=$(lc_check lo)
if [ $back -ge 0 ] && [ $back -le 6 ] && [ "$l" = ok ]; then res 4 通过 "${back} 秒内重新加载；回环长连接仍可用"; else res 4 不通过 "重新加载用时：${back} 秒（-1 表示 15 秒内没有）；回环长连接：${l:-无结果}"; fi

# ---- 第 3 步：kill -9 abroadd：断网；launchd 拉起后恢复 ----
m2_log "第 3 步：结束 abroadd（kill -9）。预计断网 10–30 秒，直到 launchd 把它拉起来。"
APID=""
[ "$DRY_RUN" != 1 ] && APID=$(launchctl print "system/${M2_LABEL}" 2>/dev/null | awk '/^\tpid = /{print $3}')
if [ "$DRY_RUN" != 1 ] && ! [[ "${APID}" =~ ^[0-9]+$ ]]; then
  res 3 不通过 "取不到 abroadd 的 PID（launchctl print 没给出），没有执行 kill"
  APID=skip
else
m2_do kill -9 "${APID:-<abroadd 的 PID>}"
fi
down=$(as_user curl -sS -m 5 -o /dev/null -w '%{http_code}' https://api.ipify.org 2>/dev/null || true)
up=-1
if [ "$DRY_RUN" != 1 ]; then
  for i in $(seq 1 60); do
    sleep 1
    if curl -fsS -m 3 --unix-socket "${API}" http://abroadd/v1/status 2>/dev/null | grep -q '"gate_open":true'; then up=$i; break; fi
  done
else down=000; up=5; fi
if [ "${APID}" = skip ]; then :
elif [ "${down:-000}" = 000 ] && [ $up -ge 0 ]; then res 3 通过 "结束后断网；${up} 秒后恢复（闸门重新打开）"; else res 3 不通过 "结束后外网：${down}；恢复用时：${up} 秒（-1 表示 60 秒内没恢复）"; fi

# ---- 第 5 步：局域网可达 ----
if [ -n "${GW_BEFORE}" ] || [ "$DRY_RUN" = 1 ]; then
  if as_user ping -c 2 -t 3 "${GW_BEFORE:-<网关>}" >/dev/null 2>&1; then res 5 通过 "路由器（默认网关）可达"; else res 5 不通过 "路由器不可达（检查“允许局域网”）"; fi
else
  res 5 跳过 "没有默认网关"
fi

# ---- 第 7 步：R6 性能（只记录数字）----
perf="${OUT}/m2-perf-${TS}.txt"
{
  if command -v iperf3 >/dev/null 2>&1 && [ -n "${M2_IPERF_SERVER:-}" ]; then
    echo "iperf3 → ${M2_IPERF_SERVER}"; as_user iperf3 -c "${M2_IPERF_SERVER}" -t 5 2>&1 | tail -4
  else echo "iperf3：跳过（没装或没设 M2_IPERF_SERVER）"; fi
  for u in https://www.example.com https://www.cloudflare.com; do
    for n in 1 2 3; do echo "$u 第 $n 次：$(as_user curl -sS -m 20 -o /dev/null -w 'connect=%{time_connect}s tls=%{time_appconnect}s total=%{time_total}s' "$u" 2>&1)"; done
  done
  if curl --version 2>/dev/null | grep -q HTTP3; then
    echo "HTTP/3（QUIC）首包：$(as_user curl --http3-only -sS -m 20 -o /dev/null -w 'connect=%{time_connect}s total=%{time_total}s' https://www.cloudflare.com 2>&1)"
  else echo "HTTP/3：跳过（这个 curl 不支持）"; fi
} >"${perf}" 2>&1
res 7 通过 "数字见 $(basename "${perf}")（只记录，不判通过与否）"

# ---- 第 9 步：睡眠唤醒、换 Wi-Fi（需要人工）；References 样本 ----
sample "pfctl -s References" m2_q /sbin/pfctl -s References
if [ "${M2_INTERACTIVE:-0}" = 1 ] && [ "$DRY_RUN" != 1 ]; then
  m2_log "第 9 步：请让电脑睡眠 30 秒后唤醒，然后按回车。"; read -r _
  sw=$(curl -sS -m 10 --unix-socket "${API}" http://abroadd/v1/status | grep -o '"gate_open":[a-z]*')
  m2_log "请换一个 Wi-Fi（或断开再连上），等 20 秒后按回车。"; read -r _
  wf=$(curl -sS -m 10 --unix-socket "${API}" http://abroadd/v1/status | grep -o '"gate_open":[a-z]*')
  res 9 通过 "睡眠唤醒后 ${sw}；换 Wi-Fi 后 ${wf}（看 status 里的事件）"
else
  res 9 跳过 "需要人工：设 M2_INTERACTIVE=1 重跑这一项"
fi

# ---- 第 6 步：恢复，与快照逐项比对 ----
m2_log "第 6 步：恢复本机网络。"
restore_api
code=$RCODE
diffs=()
if [ "$DRY_RUN" != 1 ]; then
  while IFS=$'\t' read -r s mode; do
    now=$(networksetup -getinfo "$s" 2>/dev/null | awk -F': ' '/^IPv6: /{print $2}')
    [ "$now" = "$mode" ] || diffs+=("IPv6 不同")
  done <"${M2_STATE}/snap/ipv6"
  netstat -rn -f inet | awk '$4 ~ /^utun/ && ($1=="0/1" || $1=="128.0/1")' | grep -q . && diffs+=("还有经 utun 的 /1 路由")
  printf 'show %s\nquit\n' "${M2_DNS_KEY}" | scutil | grep -q 'No such key' || diffs+=("DNS 键还在")
  [ -z "$(/sbin/pfctl -a "${M2_ANCHOR}" -s rules 2>/dev/null)" ] || diffs+=("锚点不空")
fi
if [ "$code" = 200 ] && [ ${#diffs[@]} -eq 0 ]; then res 6 通过 "路由、DNS、IPv6、锚点与启用前一致"; else res 6 不通过 "HTTP ${code}；${diffs[*]:-}"; fi
sample "pfctl -s info 首行（恢复后）" m2_q /sbin/pfctl -s info

# ---- 收尾：restore.sh（停进程和服务、清网络、清 pf、删用户、取消死人开关）----
m2_log "收尾：执行 restore.sh"
M2_RESTORED=1 # 正常走完，由这里执行还原，trap 不再执行
if [ "$DRY_RUN" = 1 ]; then DRY_RUN=1 bash "${RESTORE}"; else bash "${RESTORE}"; fi
m2_log "结果汇总（${RESULTS}）："
sort -n "${RESULTS}" >&2
m2_log "请贴回：${M2_LOG}、${RESULTS}、${SAMPLES}、${perf}，以及 scripts/m2/out/m2-abroadd-${TS}.log、m2-abroad-vm-${TS}.log"
