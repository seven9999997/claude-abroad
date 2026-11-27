#!/usr/bin/env bash
# M6-8b 开发安装的卸载（真机）：先走正常途径（命令行 abroad uninstall：结束 Claude、恢复本机网络、删解析锁和托管设置），
# 再按安装脚本的记录撤掉它装上的东西（两个 LaunchDaemon、_abroad、目录、应用、命令行链接），最后和安装前的快照逐项对照。
# 正常途径走不通时（abroadd 不在、卸载出错），按 docs/emergency.md 的步骤兜底：只撤 Claude 出国自己加的东西。
# 可以重复执行。会话里只做空跑和自测。
#
#   sudo scripts/m6/install/uninstall.sh [--yes] [--kill-browsers]
#   DRY_RUN=1 scripts/m6/install/uninstall.sh
#
# --yes（v0.71）：正常途径执行 abroad --yes uninstall、不读终端（应用经管理员授权运行时没有终端；确认已经在应用里做过）；
# --kill-browsers：原样传给 abroad uninstall（同时强制退出确认过的浏览器）。
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "${ROOT}" || exit 1
. scripts/m6/install/lib.sh

YES=0 KILLB=0
while [ $# -gt 0 ]; do
  case "$1" in
    --yes) YES=1 ;;
    --kill-browsers) KILLB=1 ;;
    *) echo "用法：sudo $0 [--yes] [--kill-browsers]" >&2; exit 2 ;;
  esac
  shift
done
# --yes（v0.71c）：应用经管理员授权运行时没有终端、也没有 SUDO_USER（确认已经在应用里做过）；仍要求 root。
[ "${YES}" = 1 ] && M6I_FROM=1
UARGS=()
[ "${YES}" = 1 ] && UARGS+=(--yes)
UARGS+=(uninstall)
[ "${KILLB}" = 1 ] && UARGS+=(--kill-browsers)
m6i_require_mode
INST="${M6I_INSTALL}"
STATE="${INST}/state"
SNAPD="$(P "${M6I_SNAP}")"
OUT="${ROOT}/scripts/m6/install/out"
TS="$(date +%Y%m%d-%H%M%S)"
if [ "${M6I_MODE}" = real ]; then mkdir -p "${OUT}"; M6I_LOG="${OUT}/uninstall-${TS}.log"; : >"${M6I_LOG}"; fi
fails=0
warn() { m6i_log "注意：$*"; fails=$((fails + 1)); }

if [ "${M6I_MODE}" != dry ] && [ ! -d "${SNAPD}" ]; then
  m6i_log "没有安装记录（${M6I_SNAP} 不在）：这台 Mac 上没有本脚本装的开发安装，什么都不做。应急恢复见 docs/emergency.md。"
  exit 0
fi
rec() { [ "${M6I_MODE}" = dry ] && return 0; [ -n "$(m6i_rec_get "$1")" ]; }
# 应用那一行只在安装记录里有它时参与对照（v0.71 风险 8：应用由用户自己放进“应用程序”、或由构建脚本替换时，装卸前后它在不在、
# 内容是什么不归脚本管）。下面第 4 段会删掉这条记录，所以先记下来。
APP_REC=0
rec app && APP_REC=1

# 只剩快照、安装目录已经不在（上一次卸载做完了、只是对照没过，architect 2026-11-27）：只重做对照，对上了就删快照，不做别的。
only_snap=0
# 安装中途失败时自动调用的卸载（M6I_FROM_INSTALL=1）不走这条：那时安装目录可能还没建，但别的步骤已经做了。
[ "${M6I_MODE}" != dry ] && [ "${M6I_FROM_INSTALL:-0}" != 1 ] && [ ! -e "$(P "${INST}")" ] && only_snap=1

if [ "${only_snap}" = 1 ]; then
  m6i_log "安装目录已经不在，只剩安装前的快照：只重做对照（对上了就删快照），不做别的"
else
# ---- 1. 正常途径：abroad uninstall（安装中途失败时跳过：那时还没有启用过）----
normal=0
if [ "${M6I_FROM_INSTALL:-0}" != 1 ]; then
  if [ "${M6I_MODE}" = dry ]; then
    m6i_say WAIT "在提示时输入 uninstall 确认（结束 Claude、恢复本机网络、删除解析锁和托管设置）"
    m6i_do "${INST}/bin/abroad" "${UARGS[@]}"
    m6i_say NOTE "下面第 3 段的兜底只在正常途径没走完时执行（空跑一并打印）"
  elif [ "${M6I_MODE}" = test ]; then
    m6i_do sudo "${INST}/bin/abroad" "${UARGS[@]}"
    [ "${M6I_TEST_NORMAL_FAILS:-0}" = 1 ] || normal=1
  elif [ -S "${M6I_SOCK}/api.sock" ] && [ -x "${INST}/bin/abroad" ]; then
    if [ "${YES}" = 1 ]; then
      m6i_log "先走正常途径：abroad ${UARGS[*]}（已经确认过，不再询问）。"
      "${INST}/bin/abroad" "${UARGS[@]}" </dev/null && normal=1
    else
      m6i_log "先走正常途径：abroad uninstall。提示时输入 uninstall 确认。"
      "${INST}/bin/abroad" "${UARGS[@]}" </dev/tty && normal=1
    fi
    if [ "${normal}" = 1 ]; then :; else m6i_log "abroad uninstall 没有走完，接下来按应急步骤兜底"; fi
  else
    m6i_log "后台服务不在，按应急步骤兜底"
  fi
fi

# ---- 2. 停掉后台服务（先 abroadd，再 guard；guard 是一次性的，通常已经退出）----
for l in "${M6I_LABEL_D}" "${M6I_LABEL_G}"; do
  if rec "launchd.${l}"; then
    m6i_do launchctl bootout "system/${l}" || true
    m6i_do rm -f "${SNAPD}/launchd.${l}"
  fi
done

# ---- 3. 兜底（正常途径没走完时；可以重复执行，只撤自己的）----
if [ "${normal}" != 1 ]; then
  m6i_log "应急步骤：撤断网规则、DNS 设置，恢复 IPv6，删自己写的解析锁和托管设置"
  m6i_do pfctl -a "${M6I_ANCHOR}" -F all || true
  case "${M6I_MODE}" in
    real) printf 'remove %s\nquit\n' "${M6I_DNS_KEY}" | scutil || true ;;
    *) m6i_do scutil remove "${M6I_DNS_KEY}" ;;
  esac
  [ "${M6I_MODE}" = dry ] && m6i_do networksetup -setv6automatic "<快照里原来是自动、现在不是的网络服务>"
  if [ -f "${SNAPD}/system.before" ] && [ "${M6I_MODE}" = real ]; then
    while IFS=$'\t' read -r k svc was; do
      [ "$k" = ipv6 ] || continue
      now=$(networksetup -getinfo "$svc" 2>/dev/null | awk -F': ' '/^IPv6: /{print $2}')
      if [ "$was" = Automatic ] && [ "$now" != Automatic ]; then m6i_do networksetup -setv6automatic "$svc" || warn "恢复 ${svc} 的 IPv6 失败"; fi
    done <"${SNAPD}/system.before"
  fi
fi
# 解析锁和托管设置：按 abroadd 的记录，只删内容和记录一致的（正常途径已经删过时这里什么都不做）。
ours() { # ours <记录文件> <resolver|cc> <目标目录>：打印要删的文件（每行一个）；目录是记录里写着“我们建的”时最后一行是 DIR <目录>
  /usr/bin/python3 -I - "$@" <<'PY'
import json, os, sys
rec, kind, d = sys.argv[1], sys.argv[2], sys.argv[3]
try:
    r = json.load(open(rec, encoding="utf-8"))
except Exception:
    sys.exit(0)
names = r.get("domains", []) if kind == "resolver" else ["claude-abroad.json"]
for n in names:
    if not n or "/" in n or n.startswith("."):
        continue
    p = os.path.join(d, n)
    try:
        if open(p, encoding="utf-8").read() == r.get("content"):
            print(p)
    except Exception:
        pass
if r.get("created_dir"):
    print("DIR " + d)
PY
}
for k in resolver cc; do
  case $k in
    resolver) recf="$(P "${STATE}/abroadd")/resolver-lock.json"; dir="$(P "${M6I_RESOLVER}")" ;;
    cc) recf="$(P "${STATE}/abroadd")/claude-code-settings.json"; dir="$(P "${M6I_CC_DIR}")" ;;
  esac
  if [ "${M6I_MODE}" = dry ]; then
    m6i_do rm -f "${dir}/<记录里的文件>（只删内容和记录一致的）"
    continue
  fi
  [ -f "${recf}" ] || continue
  mkdir_created=""
  while IFS= read -r line; do
    case "$line" in
      "DIR "*) mkdir_created=${line#DIR } ;;
      *) m6i_do rm -f "${line:?}" || warn "删除 ${line} 失败" ;;
    esac
  done < <(ours "${recf}" "$k" "${dir}")
  if [ -n "${mkdir_created}" ] && [ -d "${mkdir_created}" ] && [ -z "$(ls -A "${mkdir_created}")" ]; then
    m6i_do rmdir "${mkdir_created:?}" || true
  fi
done

# ---- 4. 安装脚本装上的东西（按记录）----
for l in "${M6I_LABEL_D}" "${M6I_LABEL_G}"; do
  if rec "plist.${l}"; then m6i_do rm -f "$(P "${M6I_PLISTDIR}/${l}.plist")"; m6i_do rm -f "${SNAPD}/plist.${l}"; fi
done
if rec user; then
  m6i_do dscl . -delete "/Users/${M6I_USER}" || true
  m6i_do dscl . -delete "/Groups/${M6I_USER}" || true
  m6i_do dscacheutil -flushcache
  m6i_do rm -f "${SNAPD}/user"
fi
if rec cli_link; then
  link="$(P "${M6I_CLI_LINK}")"
  if [ "${M6I_MODE}" = dry ] || { [ -L "${link}" ] && [ "$(readlink "${link}")" = "${INST}/bin/abroad" ]; }; then
    m6i_do rm -f "${link:?}"
  else
    warn "${M6I_CLI_LINK} 已经不是安装时放的链接，没有删"
  fi
  m6i_do rm -f "${SNAPD}/cli_link"
fi
if rec app; then
  app="$(P "${M6I_APP}")"
  if [ "${M6I_MODE}" = dry ] || [ -f "${app}/Contents/Info.plist" ]; then
    m6i_do rm -rf "${app:?}"
  fi
  m6i_do rm -f "${SNAPD}/app"
fi
if rec sockdir; then m6i_do rm -rf "$(P "${M6I_SOCK}")"; m6i_do rm -f "${SNAPD}/sockdir"; fi
if rec logdir; then m6i_do rm -rf "$(P "${M6I_LOGDIR}")"; m6i_do rm -f "${SNAPD}/logdir"; fi
if rec install_dir; then m6i_do rm -rf "$(P "${INST}")"; m6i_do rm -f "${SNAPD}/install_dir"; fi

# ---- 5. 和安装前的快照对照 ----
fi
if [ "${M6I_MODE}" = dry ]; then
  m6i_say STATE "对照 snap/files、snap/system（安装前 / 卸载后）"
  m6i_do rm -rf "$(P "${M6I_SNAP}")（对照一致时）"
  exit 0
fi
m6i_snap_files "${SNAPD}/files.after"
m6i_snap_system "${SNAPD}/system.after"
m6i_pf_info "${SNAPD}/pf.after"
appf() { if [ "${APP_REC}" = 1 ]; then cat "$1"; else grep -v "^${M6I_APP}"$'\t' "$1"; fi; }
diffs=$(diff <(appf "${SNAPD}/files.before") <(appf "${SNAPD}/files.after"); [ -f "${SNAPD}/system.before" ] && m6i_sys_cmp "${SNAPD}/system.before" "${SNAPD}/system.after" "${SNAPD}/pf.after")
if [ -n "${diffs}" ]; then
  m6i_log "和安装前的快照不一致（快照留在 ${M6I_SNAP}，可以重试卸载或按 docs/emergency.md 处理）："
  printf '%s\n' "${diffs}" >&2
  [ "${M6I_LOG}" = /dev/null ] || printf '%s\n' "${diffs}" >>"${M6I_LOG}"
  exit 1
fi
m6i_do rm -rf "${SNAPD:?}"
if [ "${fails}" != 0 ]; then m6i_log "卸载完成，但有 ${fails} 处要注意（见上）"; exit 1; fi
m6i_log "卸载完成：和安装前的快照逐项一致"
# 应用不在安装记录里（用户自己放的，或升级时交给了用户）：卸载不删它
if [ "${APP_REC}" = 0 ] && [ -d "$(P "${M6I_APP}")" ]; then
  m6i_log "应用 ${M6I_APP} 留着，不需要的话可以自己移到废纸篓。"
fi
exit 0
