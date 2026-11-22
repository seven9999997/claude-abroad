#!/usr/bin/env bash
# M5 第二级走查的还原（M5-11）：不依赖 abroadd，也不依赖它的状态文件、待还原文件。沿用 M4 的顺序：
# 停服务和进程 → 清网络（路由、DNS、IPv6）→ 还原地区（按脚本自己的快照，逐项读回）→ 清 pf → 备份 abroadd 状态目录
# → 删共享 socket 目录和命令行链接（只删本脚本建的）→ 删测试用户 → 取消死人开关 → 删安装的程序和持久快照。
# 不执行 pfctl -d，不按名字结束进程，不碰用户的 Clash，不向任何 Claude 进程发信号，不动用户打开的应用。可以重复执行。
#
#   sudo scripts/m5/restore.sh
#   DRY_RUN=1 scripts/m5/restore.sh     # 空跑：只打印会执行的改动命令
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
. scripts/m5/lib.sh
. scripts/m4/region.sh
[ $# -eq 0 ] || { echo "用法：sudo $0" >&2; exit 2; }
m5_test_guard
m5_require_root
M5_LOG="${M5_LOG:-${ROOT}/scripts/m5/out/m5-restore-$(date +%Y%m%d-%H%M%S).log}"
mkdir -p "$(dirname "${M5_LOG}")" 2>/dev/null || true
DRY=$DRY_RUN
exists() { [ "$DRY" = 1 ] || [ -e "$1" ]; }
snapfile() { if [ -f "${M5_SNAP}/$1" ]; then echo "${M5_SNAP}/$1"; else echo "${M5_STATE}/snap/$1"; fi; }

m5_log "还原开始"

# ---- 1. 停服务和进程 ----
if exists "${M5_PLIST}"; then
  m5_do launchctl bootout system "${M5_PLIST}"
  m5_do rm -f "${M5_PLIST}"
  m5_log "已卸载开发用 LaunchDaemon（abroadd 随之停止）"
fi
m5_kill_pidfile "${M5_VMDIR}/abroad-vm.pid" "abroad-vm run"
m5_log "已停止本脚本启动的 abroad-vm"

# ---- 2. 清网络（同 M3、M4）----
for pair in "0.0.0.0/1 0.0.0.1" "128.0.0.0/1 128.0.0.1"; do
  set -- $pair
  via=$(route -n get "$2" 2>/dev/null | awk '/interface:/{print $2}')
  if [ "$DRY" = 1 ] || case "$via" in utun*) true ;; *) false ;; esac; then
    if [ "$DRY" = 1 ] || netstat -rn -f inet | awk -v d="${1%%/1}" '$4 ~ /^utun/ && ($1 == "0/1" && d == "0.0.0.0" || $1 == "128.0/1" && d == "128.0.0.0")' | grep -q .; then
      m5_do route -n delete -net "$1"
    fi
  fi
done
if [ "$DRY" = 1 ] || ! printf 'show %s\nquit\n' "${M5_DNS_KEY}" | scutil 2>/dev/null | grep -q 'No such key'; then
  if [ "$DRY" = 1 ]; then printf 'CHANGE: scutil <<remove %s\n' "${M5_DNS_KEY}"; else printf 'remove %s\nquit\n' "${M5_DNS_KEY}" | scutil; fi
  m5_log "已删除我们的 DNS 键"
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
      Automatic) m5_do networksetup -setv6automatic "$s" ;;
      Off) m5_do networksetup -setv6off "$s" ;;
      Link-local*) m5_do networksetup -setv6LinkLocal "$s" ;;
      *) m5_log "IPv6：${s} 原来是“${mode}”，脚本不还原（请手动核对）"; V6_OK=0; continue ;;
    esac
    now=$(networksetup -getinfo "$s" 2>/dev/null | awk -F': ' '/^IPv6: /{print $2}')
    [ "$now" = "$mode" ] || { m5_log "IPv6：${s} 还原后是“${now}”，不是“${mode}”"; V6_OK=0; }
  done <"${SNAP6}"
  m5_log "已按快照还原 IPv6"
else
  V6_OK=0
  m5_log "没有 IPv6 快照（run.sh 没走到拍快照这一步），跳过"
fi

# ---- 3. 还原地区（按脚本自己的快照，不依赖 abroadd；逐项读回）----
RG_OK=1
if [ "$DRY" = 1 ]; then rg_restore "$(snapfile region)"; rc=$?; bad=""; else bad=$(rg_restore "$(snapfile region)"); rc=$?; fi
case $rc in
  0) [ "$DRY" = 1 ] || m5_log "地区已按快照还原，读回与启用前一致" ;;
  3) RG_OK=0 ;;
  *) RG_OK=0; m5_log "地区还原后读回与快照不一致的项：$(printf '%s' "$bad" | tr '\n' ' ')（请手动核对：系统设置 → 通用 → 日期与时间、语言与地区）" ;;
esac

# ---- 4. 清 pf：清空我们的锚点，释放能读到的 token（不执行 pfctl -d）----
m5_do /sbin/pfctl -a "${M5_ANCHOR}" -F all
if [ "$DRY" = 1 ]; then
  toks="<token>"
else
  toks="$(m5_state_get pf_tokens) $(sed -n 's/.*"pf_tokens": *\[\([^]]*\)\].*/\1/p' "${M5_STATE}/abroadd/state.json" 2>/dev/null | tr -d '" ,' | tr ',' ' ')"
  toks=$(printf '%s' "${toks}" | tr -s ' \n' '\n' | grep -E '^[0-9]+$' | sort -u | tr '\n' ' ')
fi
for t in ${toks}; do
  if [ "$DRY" = 1 ]; then m5_do /sbin/pfctl -X "$t"; else m5_do /sbin/pfctl -X "$t" >/dev/null 2>&1 || true; fi
done
[ "$DRY" = 1 ] || rm -f "${M5_STATE}/pf_tokens"
m5_log "已清空锚点并释放能读到的启用引用（没有执行 pfctl -d）"
if exists "${M5_STATE}/abroadd"; then
  m5_do mv "${M5_STATE}/abroadd" "${M5_STATE}/abroadd.bak-$(date +%Y%m%d-%H%M%S)"
  m5_log "已把 abroadd 的状态目录改名备份"
fi

# ---- 5. 共享 socket 目录和命令行链接：只删本脚本建的 ----
if [ "$DRY" = 1 ] || [ "$(m5_state_get sockdir)" = "${M5_SOCKDIR}" ]; then
  m5_do rm -rf "${M5_SOCKDIR}"
  m5_do rm -f "${M5_STATE}/sockdir"
fi
if [ "$DRY" = 1 ] || { [ "$(m5_state_get cli_link)" = "${M5_CLI_LINK}" ] && [ "$(readlink "${M5_CLI_LINK}" 2>/dev/null)" = "${M5_INSTALL}/bin/abroad" ]; }; then
  m5_do rm -f "${M5_CLI_LINK}"
  m5_do rm -f "${M5_STATE}/cli_link"
fi

# ---- 6. 删测试用户 ----
m5_user_delete

# ---- 7. 取消死人开关，删安装的程序；网络和地区都还原了才删持久快照 ----
m5_deadman_stop
if exists "${M5_INSTALL}"; then m5_do rm -rf "${M5_INSTALL}"; fi
if [ "$DRY" = 1 ] || { [ "${V6_OK}" = 1 ] && [ "${RG_OK}" = 1 ] && [ -e "${M5_SNAP}" ]; }; then
  m5_do rm -rf "${M5_SNAP}"
elif [ -e "${M5_SNAP}" ]; then
  m5_log "IPv6 或地区没有全部还原，持久快照 ${M5_SNAP} 留着（重启后再执行一次 restore.sh）"
fi
m5_log "还原结束。应用如果还开着，可以直接退出（菜单栏图标 → 退出 Claude 出国）。"
