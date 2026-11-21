#!/usr/bin/env bash
# M3 真机验证的还原（同 M2-9c 决定 3）：不依赖 abroadd，也不依赖它的状态文件格式。
# 顺序：先停进程和服务 → 再清网络（路由、DNS、IPv6）→ 清 pf → 备份 abroadd 状态目录 → 删测试用户 → 取消死人开关。
# 不执行 pfctl -d，不按名字结束进程，不碰用户的 Clash。可以重复执行。
#
#   sudo scripts/m3/restore.sh
#   DRY_RUN=1 scripts/m3/restore.sh     # 空跑：只打印会执行的改动命令
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
. scripts/m3/lib.sh
[ $# -eq 0 ] || { echo "用法：sudo $0" >&2; exit 2; }
m3_test_guard
m3_require_root
M3_LOG="${M3_LOG:-${ROOT}/scripts/m3/out/m3-restore-$(date +%Y%m%d-%H%M%S).log}"
mkdir -p "$(dirname "${M3_LOG}")" 2>/dev/null || true
DRY=$DRY_RUN
exists() { [ "$DRY" = 1 ] || [ -e "$1" ]; }

m3_log "还原开始"

# ---- 1. 停进程和服务 ----
if exists "${M3_PLIST}"; then
  m3_do launchctl bootout system "${M3_PLIST}"
  m3_do rm -f "${M3_PLIST}"
  m3_log "已卸载开发用 LaunchDaemon（abroadd 随之停止）"
fi
# 都是程序自己写的 PID 文件（abroad-vm、两个开发上游）或脚本记下的 root 直接启动的进程（tcpdump），都核对完整命令行。
m3_kill_pidfile "${M3_VMDIR}/abroad-vm.pid" "abroad-vm-dev run"
for n in wg-a clash-b; do m3_kill_pidfile "${M3_UPDIR}/${n}.pid" "abroad-dev upstream --name ${n}"; done
for n in anth dns anth2; do m3_kill_pidfile "${M3_STATE}/tcpdump-${n}.pid" "/usr/sbin/tcpdump -i "; done
# Chrome：PID 文件没写上时（中途失败），从临时配置目录的锁文件取；核对命令行里有这个临时目录。
if [ "$DRY" != 1 ] && [ ! -f "${M3_STATE}/chrome.pid" ]; then
  l=$(readlink "${M3_CHROME_DIR}/SingletonLock" 2>/dev/null || true)
  p=${l##*-}
  case "$p" in '' | *[!0-9]*) ;; *) m3_state_set chrome.pid "$p" ;; esac
fi
m3_kill_pidfile "${M3_STATE}/chrome.pid" "--user-data-dir=${M3_CHROME_DIR}"
# 抓包的文本（53 端口的里有这段时间所有应用的查询名）：中途失败时也删。
for n in anth dns anth2; do
  if exists "${M3_STATE}/cap-${n}.txt"; then m3_do rm -f "${M3_STATE}/cap-${n}.txt"; fi
done
if exists "${M3_CHROME_DIR}"; then m3_do rm -rf "${M3_CHROME_DIR}"; fi
m3_log "已停止本脚本启动的 abroad-vm、开发上游、抓包和 Chrome，删除了抓包文本和 Chrome 的临时配置"

# ---- 2. 清网络（同 M2）----
for pair in "0.0.0.0/1 0.0.0.1" "128.0.0.0/1 128.0.0.1"; do
  set -- $pair
  via=$(route -n get "$2" 2>/dev/null | awk '/interface:/{print $2}')
  if [ "$DRY" = 1 ] || case "$via" in utun*) true ;; *) false ;; esac; then
    if [ "$DRY" = 1 ] || netstat -rn -f inet | awk -v d="${1%%/1}" '$4 ~ /^utun/ && ($1 == "0/1" && d == "0.0.0.0" || $1 == "128.0/1" && d == "128.0.0.0")' | grep -q .; then
      m3_do route -n delete -net "$1"
    fi
  fi
done
if [ "$DRY" = 1 ] || ! printf 'show %s\nquit\n' "${M3_DNS_KEY}" | scutil 2>/dev/null | grep -q 'No such key'; then
  if [ "$DRY" = 1 ]; then
    printf 'CHANGE: scutil <<remove %s\n' "${M3_DNS_KEY}"
  else
    printf 'remove %s\nquit\n' "${M3_DNS_KEY}" | scutil
  fi
  m3_log "已删除我们的 DNS 键"
fi
SNAP="${M3_SNAP}/ipv6"
[ -f "${SNAP}" ] || SNAP="${M3_STATE}/snap/ipv6"
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
      Automatic) m3_do networksetup -setv6automatic "$s" ;;
      Off) m3_do networksetup -setv6off "$s" ;;
      Link-local*) m3_do networksetup -setv6LinkLocal "$s" ;;
      *) m3_log "IPv6：${s} 原来是“${mode}”，脚本不还原（请手动核对）"; V6_OK=0; continue ;;
    esac
    now=$(networksetup -getinfo "$s" 2>/dev/null | awk -F': ' '/^IPv6: /{print $2}')
    [ "$now" = "$mode" ] || { m3_log "IPv6：${s} 还原后是“${now}”，不是“${mode}”"; V6_OK=0; }
  done <"${SNAP}"
  m3_log "已按快照还原 IPv6（快照：${SNAP}）"
else
  V6_OK=0
  m3_log "没有 IPv6 快照（run.sh 没走到拍快照这一步），跳过"
fi

# ---- 3. 清 pf：清空我们的锚点，释放能读到的 token（不执行 pfctl -d）----
m3_do /sbin/pfctl -a "${M3_ANCHOR}" -F all
toks=""
if [ "$DRY" = 1 ]; then
  toks="<token>"
else
  toks="$(m3_state_get pf_tokens) $(sed -n 's/.*"pf_tokens": *\[\([^]]*\)\].*/\1/p' "${M3_STATE}/abroadd/state.json" 2>/dev/null | tr -d '" ,' | tr ',' ' ')"
  toks=$(printf '%s' "${toks}" | tr -s ' \n' '\n' | grep -E '^[0-9]+$' | sort -u | tr '\n' ' ')
fi
for t in ${toks}; do
  if [ "$DRY" = 1 ]; then m3_do /sbin/pfctl -X "$t"; else m3_do /sbin/pfctl -X "$t" >/dev/null 2>&1 || true; fi
done
[ "$DRY" = 1 ] || rm -f "${M3_STATE}/pf_tokens"
m3_log "已清空锚点并释放能读到的启用引用（没有执行 pfctl -d）"
if exists "${M3_STATE}/abroadd"; then
  m3_do mv "${M3_STATE}/abroadd" "${M3_STATE}/abroadd.bak-$(date +%Y%m%d-%H%M%S)"
  m3_log "已把 abroadd 的状态目录改名备份"
fi

# ---- 4. 删测试用户 ----
m3_user_delete

# ---- 5. 取消死人开关，删安装的程序和持久快照 ----
m3_deadman_stop
if exists "${M3_INSTALL}"; then m3_do rm -rf "${M3_INSTALL}"; fi
if [ "$DRY" = 1 ] || { [ "${V6_OK}" = 1 ] && [ -e "${M3_SNAP}" ]; }; then
  m3_do rm -rf "${M3_SNAP}"
elif [ -e "${M3_SNAP}" ]; then
  m3_log "IPv6 没有全部还原，持久快照 ${M3_SNAP} 留着"
fi
m3_log "还原结束。状态目录 ${M3_STATE} 留着（快照和日志），确认无误后可手动删除。若仍上不了网，重启电脑后 pf 规则会自动消失。"
