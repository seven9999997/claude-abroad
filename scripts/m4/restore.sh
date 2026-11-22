#!/usr/bin/env bash
# M4 真机验证的还原（M4-9，m4-plan v2.13 验收 d）：不依赖 abroadd，也不依赖它的状态文件、待还原文件。
# 顺序：停进程和服务 → 撤销人为制造的泄漏 → 清网络（路由、DNS、IPv6）→ 还原地区（按脚本自己的快照，逐项读回）
#   → 清 pf → 备份 abroadd 状态目录 → 删真实 IP 临时文件 → 删测试用户 → 取消死人开关。
# 不执行 pfctl -d，不按名字结束进程，不碰用户的 Clash，不向任何 Claude 进程发信号。可以重复执行。
#
#   sudo scripts/m4/restore.sh
#   DRY_RUN=1 scripts/m4/restore.sh     # 空跑：只打印会执行的改动命令
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
. scripts/m4/lib.sh
. scripts/m4/region.sh
[ $# -eq 0 ] || { echo "用法：sudo $0" >&2; exit 2; }
m4_test_guard
m4_require_root
M4_LOG="${M4_LOG:-${ROOT}/scripts/m4/out/m4-restore-$(date +%Y%m%d-%H%M%S).log}"
mkdir -p "$(dirname "${M4_LOG}")" 2>/dev/null || true
DRY=$DRY_RUN
exists() { [ "$DRY" = 1 ] || [ -e "$1" ]; }
snapfile() { if [ -f "${M4_SNAP}/$1" ]; then echo "${M4_SNAP}/$1"; else echo "${M4_STATE}/snap/$1"; fi; }

m4_log "还原开始"

# ---- 1. 停进程和服务 ----
if exists "${M4_PLIST}"; then
  m4_do launchctl bootout system "${M4_PLIST}"
  m4_do rm -f "${M4_PLIST}"
  m4_log "已卸载开发用 LaunchDaemon（abroadd 随之停止）"
fi
# 都是程序自己写的 PID 文件（abroad-vm）或脚本记下的、核对过完整命令行的进程（tcpdump、假 Claude 进程）。
m4_kill_pidfile "${M4_VMDIR}/abroad-vm.pid" "abroad-vm run"
for n in down-wg down-ovpn down-sub dns53; do m4_kill_pidfile "${M4_STATE}/tcpdump-${n}.pid" "/usr/sbin/tcpdump -i "; done
# 假 Claude 进程（v2.14 必须改 2）：是脚本自己启动的 /bin/sleep 副本，核对命令行里有临时目录的路径才结束。
m4_kill_pidfile "${M4_STATE}/fake.pid" "${M4_FAKE_BIN} 900"
for n in down-wg down-ovpn down-sub dns53; do
  if exists "${M4_STATE}/cap-${n}.txt"; then m4_do rm -f "${M4_STATE}/cap-${n}.txt"; fi
done
m4_log "已停止本脚本启动的 abroad-vm、抓包和假 Claude 进程，删除了抓包文本"

# ---- 2. 撤销人为制造的泄漏（IPv6 在第 3 步按快照兜底）----
PSNAP=$(snapfile proxy)
if [ "$DRY" = 1 ] || grep -qx proxy "${M4_STATE}/leaks" 2>/dev/null; then
  svc=$(sed -n 's/^service=//p' "${PSNAP}" 2>/dev/null); srv=$(sed -n 's/^Server=//p' "${PSNAP}" 2>/dev/null); port=$(sed -n 's/^Port=//p' "${PSNAP}" 2>/dev/null)
  [ "$DRY" = 1 ] && svc=${svc:-'<主服务>'}
  if [ -n "${svc}" ]; then
    if [ -n "${srv}" ] && [ "${port:-0}" != 0 ]; then m4_do networksetup -setsecurewebproxy "${svc}" "${srv}" "${port}"
    else m4_do networksetup -setsecurewebproxy "${svc}" "" 0; fi # 原来为空：清掉脚本写进去的 127.0.0.1:9
    if [ "$DRY" = 1 ] || [ "$(sed -n 's/^Enabled=//p' "${PSNAP}")" != Yes ]; then m4_do networksetup -setsecurewebproxystate "${svc}" off; fi
    if [ "$DRY" != 1 ]; then
      . scripts/m4/judge.sh
      why=$(judge_proxy_restored "${PSNAP}" "$(networksetup -getsecurewebproxy "${svc}" 2>/dev/null)") && m4_log "已按快照还原 HTTPS 代理设置：${why}" ||
        m4_log "HTTPS 代理没有完全还原：${why}（请在“系统设置 → 网络 → ${svc} → 详细信息 → 代理 → 安全网页代理（HTTPS）”里手动核对）"
    fi
  fi
fi
if exists "${M4_FAKE_ROOT}"; then m4_do rm -rf "${M4_FAKE_ROOT}"; fi
if exists "${M4_STATE}/leaks"; then m4_do rm -f "${M4_STATE}/leaks"; fi

# ---- 3. 清网络（同 M3）----
for pair in "0.0.0.0/1 0.0.0.1" "128.0.0.0/1 128.0.0.1"; do
  set -- $pair
  via=$(route -n get "$2" 2>/dev/null | awk '/interface:/{print $2}')
  if [ "$DRY" = 1 ] || case "$via" in utun*) true ;; *) false ;; esac; then
    if [ "$DRY" = 1 ] || netstat -rn -f inet | awk -v d="${1%%/1}" '$4 ~ /^utun/ && ($1 == "0/1" && d == "0.0.0.0" || $1 == "128.0/1" && d == "128.0.0.0")' | grep -q .; then
      m4_do route -n delete -net "$1"
    fi
  fi
done
if [ "$DRY" = 1 ] || ! printf 'show %s\nquit\n' "${M4_DNS_KEY}" | scutil 2>/dev/null | grep -q 'No such key'; then
  if [ "$DRY" = 1 ]; then printf 'CHANGE: scutil <<remove %s\n' "${M4_DNS_KEY}"; else printf 'remove %s\nquit\n' "${M4_DNS_KEY}" | scutil; fi
  m4_log "已删除我们的 DNS 键"
fi
SNAP6=$(snapfile ipv6)
V6_OK=1
if [ "$DRY" = 1 ]; then
  printf 'CHANGE: networksetup -setv6automatic <服务>（按快照，原来是 Automatic 的）\n'
  printf 'CHANGE: networksetup -setv6off <服务>（按快照，原来是 Off 的）\n'
  printf 'CHANGE: networksetup -setv6LinkLocal <服务>（按快照，原来是 Link-local 的）\n'
elif [ -f "${SNAP6}" ]; then
  while IFS=$'\t' read -r s mode; do
    [ -n "$s" ] || continue
    now=$(networksetup -getinfo "$s" 2>/dev/null | awk -F': ' '/^IPv6: /{print $2}')
    [ "$now" = "$mode" ] && continue
    case "$mode" in
      Automatic) m4_do networksetup -setv6automatic "$s" ;;
      Off) m4_do networksetup -setv6off "$s" ;;
      Link-local*) m4_do networksetup -setv6LinkLocal "$s" ;;
      *) m4_log "IPv6：${s} 原来是“${mode}”，脚本不还原（请手动核对）"; V6_OK=0; continue ;;
    esac
    now=$(networksetup -getinfo "$s" 2>/dev/null | awk -F': ' '/^IPv6: /{print $2}')
    [ "$now" = "$mode" ] || { m4_log "IPv6：${s} 还原后是“${now}”，不是“${mode}”"; V6_OK=0; }
  done <"${SNAP6}"
  m4_log "已按快照还原 IPv6"
else
  V6_OK=0
  m4_log "没有 IPv6 快照（run.sh 没走到拍快照这一步），跳过"
fi

# ---- 4. 还原地区（按脚本自己的快照，不依赖 abroadd；逐项读回）----
RG_OK=1
if [ "$DRY" = 1 ]; then rg_restore "$(snapfile region)"; rc=$?; bad=""; else bad=$(rg_restore "$(snapfile region)"); rc=$?; fi
case $rc in
  0) [ "$DRY" = 1 ] || m4_log "地区已按快照还原，读回与启用前一致" ;;
  3) RG_OK=0 ;;
  *) RG_OK=0; m4_log "地区还原后读回与快照不一致的项：$(printf '%s' "$bad" | tr '\n' ' ')（请手动核对：系统设置 → 通用 → 日期与时间、语言与地区）" ;;
esac

# ---- 5. 清 pf：清空我们的锚点，释放能读到的 token（不执行 pfctl -d）----
m4_do /sbin/pfctl -a "${M4_ANCHOR}" -F all
toks=""
if [ "$DRY" = 1 ]; then
  toks="<token>"
else
  toks="$(m4_state_get pf_tokens) $(sed -n 's/.*"pf_tokens": *\[\([^]]*\)\].*/\1/p' "${M4_STATE}/abroadd/state.json" 2>/dev/null | tr -d '" ,' | tr ',' ' ')"
  toks=$(printf '%s' "${toks}" | tr -s ' \n' '\n' | grep -E '^[0-9]+$' | sort -u | tr '\n' ' ')
fi
for t in ${toks}; do
  if [ "$DRY" = 1 ]; then m4_do /sbin/pfctl -X "$t"; else m4_do /sbin/pfctl -X "$t" >/dev/null 2>&1 || true; fi
done
[ "$DRY" = 1 ] || rm -f "${M4_STATE}/pf_tokens"
m4_log "已清空锚点并释放能读到的启用引用（没有执行 pfctl -d）"
if exists "${M4_STATE}/abroadd"; then
  m4_do mv "${M4_STATE}/abroadd" "${M4_STATE}/abroadd.bak-$(date +%Y%m%d-%H%M%S)"
  m4_log "已把 abroadd 的状态目录改名备份"
fi

# ---- 6. 删真实 IP 和出口配置的临时文件（0700 子目录）----
if exists "${M4_SECRET}"; then m4_do rm -rf "${M4_SECRET}"; fi

# ---- 7. 删测试用户 ----
m4_user_delete

# ---- 8. 取消死人开关，删安装的程序；网络和地区都还原了才删持久快照 ----
m4_deadman_stop
if exists "${M4_INSTALL}"; then m4_do rm -rf "${M4_INSTALL}"; fi
if [ "$DRY" = 1 ] || { [ "${V6_OK}" = 1 ] && [ "${RG_OK}" = 1 ] && [ -e "${M4_SNAP}" ]; }; then
  m4_do rm -rf "${M4_SNAP}"
elif [ -e "${M4_SNAP}" ]; then
  m4_log "IPv6 或地区没有全部还原，持久快照 ${M4_SNAP} 留着（重启后再执行一次 restore.sh）"
fi
m4_log "还原结束。状态目录 ${M4_STATE} 留着（快照和日志），确认无误后可手动删除。若仍上不了网，重启电脑后 pf 规则会自动消失，然后再执行一次 restore.sh。"
