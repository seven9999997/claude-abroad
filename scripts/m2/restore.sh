#!/usr/bin/env bash
# M2 真机验证的还原（M2-9c 决定 3）：不依赖 abroadd，也不依赖它的状态文件格式（token 能读到就释放，读不到跳过）。
# 顺序：先停进程和服务 → 再清网络（路由、DNS、IPv6）→ 最后清 pf → 删测试用户 → 取消死人开关。
# 不执行 pfctl -d，不按名字结束进程，不碰用户的 Clash。可以重复执行。
#
#   sudo scripts/m2/restore.sh
#   DRY_RUN=1 scripts/m2/restore.sh     # 空跑：只打印会执行的改动命令
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
. scripts/m2/lib.sh
[ $# -eq 0 ] || { echo "用法：sudo $0" >&2; exit 2; }
m2_test_guard
m2_require_root
M2_LOG="${M2_LOG:-${ROOT}/scripts/m2/out/m2-restore-$(date +%Y%m%d-%H%M%S).log}"
mkdir -p "$(dirname "${M2_LOG}")" 2>/dev/null || true
DRY=$DRY_RUN
exists() { [ "$DRY" = 1 ] || [ -e "$1" ]; }

m2_log "还原开始"

# ---- 1. 停进程和服务 ----
if exists "${M2_PLIST}"; then
  m2_do launchctl bootout system "${M2_PLIST}"
  m2_do rm -f "${M2_PLIST}"
  m2_log "已卸载开发用 LaunchDaemon（abroadd 随之停止）"
fi
# abroad-vm 自己写的 PID 文件（vmhost.StateDir.PIDFile）；长连接测试程序也是自己写的 PID。都核对完整命令行。
m2_kill_pidfile "${M2_STATE}/vm/abroad-vm.pid" "abroad-vm run"
for n in ext lo; do m2_kill_pidfile "${M2_STATE}/lc-${n}.pid" "${M2_STATE}/longconn.py"; done
m2_log "已停止本脚本启动的 abroad-vm 和长连接测试进程"

# ---- 2. 清网络：两条 /1 路由（先查后删，只删经 utun 的）、我们的 DNS 键、按快照还原 IPv6 ----
for pair in "0.0.0.0/1 0.0.0.1" "128.0.0.0/1 128.0.0.1"; do
  set -- $pair
  via=$(route -n get "$2" 2>/dev/null | awk '/interface:/{print $2}')
  if [ "$DRY" = 1 ] || case "$via" in utun*) true ;; *) false ;; esac; then
    # 只在这条网段确实经 utun（而且是我们加的那条）时删；经物理网卡的默认路由不会匹配到 /1。
    if [ "$DRY" = 1 ] || netstat -rn -f inet | awk -v d="${1%%/1}" '$4 ~ /^utun/ && ($1 == "0/1" && d == "0.0.0.0" || $1 == "128.0/1" && d == "128.0.0.0")' | grep -q .; then
      m2_do route -n delete -net "$1"
    fi
  fi
done
if [ "$DRY" = 1 ] || ! printf 'show %s\nquit\n' "${M2_DNS_KEY}" | scutil 2>/dev/null | grep -q 'No such key'; then
  if [ "$DRY" = 1 ]; then
    printf 'CHANGE: scutil <<remove %s\n' "${M2_DNS_KEY}"
  else
    printf 'remove %s\nquit\n' "${M2_DNS_KEY}" | scutil
  fi
  m2_log "已删除我们的 DNS 键"
fi
# 优先读重启不丢的持久快照（/var/run 重启即清空，而 IPv6 关闭是持久的）。
SNAP="${M2_SNAP}/ipv6"
[ -f "${SNAP}" ] || SNAP="${M2_STATE}/snap/ipv6"
V6_OK=1
if [ "$DRY" = 1 ]; then
  printf 'CHANGE: networksetup -setv6automatic <服务>（按快照，原来是 Automatic 的）\n'
  printf 'CHANGE: networksetup -setv6off <服务>（按快照，原来是 Off 的）\n'
  printf 'CHANGE: networksetup -setv6LinkLocal <服务>（按快照，原来是 Link-local 的）\n'
elif [ -f "${SNAP}" ]; then
  while IFS=$'\t' read -r s mode; do
    [ -n "$s" ] || continue
    now=$(networksetup -getinfo "$s" 2>/dev/null | awk -F': ' '/^IPv6: /{print $2}')
    [ "$now" = "$mode" ] && continue
    case "$mode" in
      Automatic) m2_do networksetup -setv6automatic "$s" ;;
      Off) m2_do networksetup -setv6off "$s" ;;
      Link-local*) m2_do networksetup -setv6LinkLocal "$s" ;;
      *) m2_log "IPv6：${s} 原来是“${mode}”，脚本不还原（请手动核对）"; V6_OK=0; continue ;;
    esac
    now=$(networksetup -getinfo "$s" 2>/dev/null | awk -F': ' '/^IPv6: /{print $2}')
    [ "$now" = "$mode" ] || { m2_log "IPv6：${s} 还原后是“${now}”，不是“${mode}”"; V6_OK=0; }
  done <"${SNAP}"
  m2_log "已按快照还原 IPv6（快照：${SNAP}）"
else
  V6_OK=0
  m2_log "没有 IPv6 快照（run.sh 没走到拍快照这一步），跳过"
fi

# ---- 3. 清 pf：清空我们的锚点，释放能读到的 token（不执行 pfctl -d）----
m2_do /sbin/pfctl -a "${M2_ANCHOR}" -F all
toks=""
if [ "$DRY" = 1 ]; then
  toks="<token>"
else
  toks="$(m2_state_get pf_tokens) $(sed -n 's/.*"pf_tokens": *\[\([^]]*\)\].*/\1/p' "${M2_STATE}/abroadd/state.json" 2>/dev/null | tr -d '" ,' | tr ',' ' ')"
  toks=$(printf '%s' "${toks}" | tr -s ' \n' '\n' | grep -E '^[0-9]+$' | sort -u | tr '\n' ' ')
fi
for t in ${toks}; do
  if [ "$DRY" = 1 ]; then m2_do /sbin/pfctl -X "$t"; else m2_do /sbin/pfctl -X "$t" >/dev/null 2>&1 || true; fi
done
[ "$DRY" = 1 ] || rm -f "${M2_STATE}/pf_tokens"
m2_log "已清空锚点并释放能读到的启用引用（没有执行 pfctl -d；pf 若因别的软件保持启用，属正常）"
# abroadd 的状态目录改名备份：留着的话下一次 run.sh 装上 LaunchDaemon 时 abroadd 会按它自动接管。
if exists "${M2_STATE}/abroadd"; then
  m2_do mv "${M2_STATE}/abroadd" "${M2_STATE}/abroadd.bak-$(date +%Y%m%d-%H%M%S)"
  m2_log "已把 abroadd 的状态目录改名备份"
fi

# ---- 4. 删测试用户 ----
m2_user_delete

# ---- 5. 取消死人开关，删安装的程序 ----
m2_deadman_stop
if exists "${M2_INSTALL}"; then m2_do rm -rf "${M2_INSTALL}"; fi
# 持久快照：IPv6 全部还原成功之后才删（否则留着，重启后再执行 restore.sh 还能用）。
if [ "$DRY" = 1 ] || { [ "${V6_OK}" = 1 ] && [ -e "${M2_SNAP}" ]; }; then
  m2_do rm -rf "${M2_SNAP}"
elif [ -e "${M2_SNAP}" ]; then
  m2_log "IPv6 没有全部还原，持久快照 ${M2_SNAP} 留着"
fi
m2_log "还原结束。状态目录 ${M2_STATE} 留着（快照和日志），确认无误后可手动删除。若仍上不了网，重启电脑后 pf 规则会自动消失。"
