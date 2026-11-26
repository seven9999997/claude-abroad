#!/usr/bin/env bash
# M6-8b 开发安装（真机，设计 11.4 的 M6-8a 段）：装到固定位置、建 _abroad、注册开发用 LaunchDaemon（abroadd 和开机时的
# guard），由 abroadd 自己做首次准备。只装、不启用：装完是“已回国”状态，不加载断网规则，不改本机网络。
# 会话里只做空跑、--check-only 和自测；真机安装由用户执行（M6-9 的端到端也从这里开始）。用法见 README.md。
#
#   scripts/dev/integrity-build.sh --dev && scripts/dev/app-bundle.sh   # 先以自己的身份构建（不要 sudo）
#   sudo scripts/m6/install/install.sh              # 安装
#   scripts/m6/install/install.sh --check-only      # 只做前提检查（只读，不需要 root）
#   DRY_RUN=1 scripts/m6/install/install.sh         # 空跑
#
# 安装失败时自动执行 uninstall.sh，按安装前的快照撤掉已经装上的部分。
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "${ROOT}" || exit 1
. scripts/m6/install/lib.sh

CHECK_ONLY=0
case "${1:-}" in
  --check-only) CHECK_ONLY=1 ;;
  "") ;;
  *) echo "用法：sudo $0 [--check-only]" >&2; exit 2 ;;
esac

SRC="${M6I_TEST_SRC:-${ROOT}/scripts/dev/out/integrity}"
APPSRC="${M6I_TEST_APP:-${ROOT}/app/.build/bundle/ClaudeAbroad.app}"
EXIT_HOST="${M6I_EXIT_HOST:-www.gstatic.com}"
INST="${M6I_INSTALL}"
STATE="${INST}/state"
OUT="${ROOT}/scripts/m6/install/out"
TS="$(date +%Y%m%d-%H%M%S)"
UNINSTALL="${ROOT}/scripts/m6/install/uninstall.sh"
if [ "${M6I_MODE}" = real ]; then mkdir -p "${OUT}"; M6I_LOG="${OUT}/install-${TS}.log"; : >"${M6I_LOG}"; fi

precheck() {
  local reasons=() f
  for f in bin/abroad bin/abroad-rescue bin/abroadd bin/abroad-guard bin/abroad-vm integrity.json image/Image image/initramfs.gz image/modloop image/rootfs.img; do
    [ -f "${SRC}/${f}" ] || reasons+=("缺少 ${SRC}/${f}：先以自己的身份执行 scripts/dev/integrity-build.sh --dev（要先有虚拟机镜像 guest/out）")
  done
  if [ -x "${SRC}/bin/abroadd" ] && [ "${M6I_MODE}" != test ]; then
    "${SRC}/bin/abroadd" -h 2>&1 | grep -q -- '-dev-fake-claude-dir' ||
      reasons+=("${SRC}/bin/abroadd 不是开发构建（认不出 --dev-fake-claude-dir）：请用 scripts/dev/integrity-build.sh --dev 构建")
  fi
  [ -d "${APPSRC}/Contents/MacOS" ] || reasons+=("缺少应用包 ${APPSRC}：先执行 scripts/dev/app-bundle.sh")
  [ -e "$(P "${INST}")" ] && reasons+=("${INST} 已经存在：已经装过，请先执行 sudo scripts/m6/install/uninstall.sh")
  [ -e "$(P "${M6I_SNAP}")" ] && reasons+=("${M6I_SNAP} 已经存在：上一次安装或卸载没有走完，请先执行 sudo scripts/m6/install/uninstall.sh")
  for f in "${M6I_PLISTDIR}/${M6I_LABEL_D}.plist" "${M6I_PLISTDIR}/${M6I_LABEL_G}.plist" "${M6I_SOCK}"; do
    [ -e "$(P "$f")" ] && reasons+=("${f} 已经存在（别的安装或上一次的走查脚本留下的）：请先清理")
  done
  m6i_user_exists && reasons+=("用户 ${M6I_USER} 已经存在：不是这次安装建的，不能接着用。请确认后删除它（或先执行卸载）")
  for t in m2 m3 m4 m5; do
    [ -e "$(P "/var/run/claude-abroad-${t}")" ] && reasons+=("${t} 走查脚本的状态还在：请先执行 sudo scripts/${t}/restore.sh")
  done
  if [ ${#reasons[@]} -gt 0 ]; then
    for f in "${reasons[@]}"; do m6i_log "前提不满足：${f}"; done
    return 1
  fi
  m6i_log "前提检查通过"
}

if [ "${CHECK_ONLY}" = 1 ]; then precheck; exit $?; fi
m6i_require_mode
if ! precheck; then
  if [ "${M6I_MODE}" = dry ]; then m6i_log "（空跑：前提不满足，继续打印后面的命令）"; else exit 3; fi
fi
if [ "${M6I_MODE}" = real ] && [ "${M6I_YES:-0}" != 1 ]; then
  m6i_log "将安装到 ${INST}，注册 ${M6I_LABEL_D}、${M6I_LABEL_G}，建用户 ${M6I_USER}。不会启用、不会断网。按回车开始（Ctrl+C 取消）。"
  read -r _ </dev/tty
fi

# ---- 快照（先于一切改动；卸载据此逐项对照）----
m6i_do mkdir -p "$(P "${M6I_SNAP}")"
m6i_do chmod 700 "$(P "${M6I_SNAP}")"
if [ "${M6I_MODE}" = dry ]; then
  m6i_say STATE "snap/files、snap/system（安装前）"
else
  m6i_snap_files "$(P "${M6I_SNAP}")/files.before"
  m6i_snap_system "$(P "${M6I_SNAP}")/system.before"
fi
m6i_rec_set installing 1

# 安装中途失败：自动卸载（按记录只撤这次装上的）。
on_exit() {
  local rc=$?
  [ "${rc}" = 0 ] && return 0
  [ "${M6I_MODE}" = dry ] && return 0
  m6i_log "安装中途退出（状态 ${rc}），自动执行卸载，撤掉已经装上的部分"
  M6I_FROM_INSTALL=1 M6I_YES=1 bash "${UNINSTALL}"
}
trap on_exit EXIT
trap 'exit 130' INT TERM HUP

# ---- 程序和镜像 ----
m6i_do mkdir -p "$(P "${INST}/bin")" "$(P "${INST}/image")"
m6i_rec_set install_dir 1
for f in abroad abroad-rescue abroadd abroad-guard abroad-vm; do m6i_do cp "${SRC}/bin/${f}" "$(P "${INST}/bin/${f}")" || m6i_die "复制 ${f} 失败"; done
for f in Image initramfs.gz modloop rootfs.img; do m6i_do cp "${SRC}/image/${f}" "$(P "${INST}/image/${f}")" || m6i_die "复制镜像 ${f} 失败"; done
m6i_do cp "${SRC}/integrity.json" "$(P "${INST}/integrity.json")" || m6i_die "复制完整性清单失败"
m6i_do chown -R root:wheel "$(P "${INST}")"
m6i_do chmod -R go-w "$(P "${INST}")"

# ---- 专用用户（虚拟机以它运行，pf 只放行它的出站）----
id=$(m6i_free_id) || m6i_die "400–499 内没有空闲的 UID"
m6i_rec_set user "${M6I_USER}"
m6i_do dscl . -create "/Groups/${M6I_USER}"
m6i_do dscl . -create "/Groups/${M6I_USER}" PrimaryGroupID "${id}"
m6i_do dscl . -create "/Groups/${M6I_USER}" Password '*'
m6i_do dscl . -create "/Users/${M6I_USER}"
m6i_do dscl . -create "/Users/${M6I_USER}" UniqueID "${id}"
m6i_do dscl . -create "/Users/${M6I_USER}" PrimaryGroupID "${id}"
m6i_do dscl . -create "/Users/${M6I_USER}" UserShell /usr/bin/false
m6i_do dscl . -create "/Users/${M6I_USER}" NFSHomeDirectory /var/empty
m6i_do dscl . -create "/Users/${M6I_USER}" RealName "claude-abroad"
m6i_do dscl . -create "/Users/${M6I_USER}" Password '*'
m6i_do dscl . -create "/Users/${M6I_USER}" IsHidden 1
m6i_do dscacheutil -flushcache

# ---- 状态目录、日志目录、本地接口目录 ----
m6i_do mkdir -p "$(P "${STATE}/abroadd")" "$(P "${STATE}/vm")" "$(P "${STATE}/store")" "$(P "${M6I_LOGDIR}")"
m6i_do chmod 700 "$(P "${STATE}/abroadd")" "$(P "${STATE}/vm")" "$(P "${STATE}/store")"
# 开发构建的假 Claude 目录：回国结束 Claude 和第三层只针对这里的进程（端到端脚本放测试进程进来），本机真实的 Claude 不受影响。
m6i_do mkdir -p "$(P "${INST}/fake-claude")"
m6i_do chmod 755 "$(P "${INST}/fake-claude")"
m6i_do chown "${M6I_USER}" "$(P "${STATE}/vm")"
m6i_rec_set logdir 1
m6i_do mkdir -p "$(P "${M6I_SOCK}")"
m6i_do chown root:wheel "$(P "${M6I_SOCK}")"
m6i_do chmod 755 "$(P "${M6I_SOCK}")"
m6i_rec_set sockdir 1

# ---- 应用和命令行（已经有同名的就不覆盖）----
if [ -e "$(P "${M6I_APP}")" ]; then
  m6i_log "${M6I_APP} 已经存在，不覆盖：请从 ${APPSRC} 打开应用"
else
  m6i_do cp -R "${APPSRC}" "$(P "${M6I_APP}")" || m6i_die "复制应用失败"
  m6i_rec_set app 1
fi
if [ -e "$(P "${M6I_CLI_LINK}")" ] || [ -L "$(P "${M6I_CLI_LINK}")" ]; then
  m6i_log "${M6I_CLI_LINK} 已经存在，不覆盖：命令行请用 ${INST}/bin/abroad"
else
  m6i_do mkdir -p "$(P "$(dirname "${M6I_CLI_LINK}")")"
  m6i_do ln -s "${INST}/bin/abroad" "$(P "${M6I_CLI_LINK}")"
  m6i_rec_set cli_link 1
fi

# ---- LaunchDaemon（开发用）----
plist() { # plist <label> <是否常驻 true|false> <日志> <参数…>
  local label=$1 keep=$2 log=$3 a
  shift 3
  printf '<?xml version="1.0" encoding="UTF-8"?>\n<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">\n'
  printf '<!-- 开发安装（M6-8b）：scripts/m6/install/install.sh 安装，uninstall.sh 移除。 -->\n<plist version="1.0">\n<dict>\n'
  printf '  <key>Label</key><string>%s</string>\n  <key>ProgramArguments</key>\n  <array>\n' "${label}"
  for a in "$@"; do printf '    <string>%s</string>\n' "$a"; done
  printf '  </array>\n  <key>RunAtLoad</key><true/>\n'
  if [ "${keep}" = true ]; then printf '  <key>KeepAlive</key><true/>\n'; else printf '  <key>KeepAlive</key><false/>\n  <key>LaunchOnlyOnce</key><true/>\n'; fi
  printf '  <key>StandardErrorPath</key><string>%s</string>\n  <key>StandardOutPath</key><string>%s</string>\n</dict>\n</plist>\n' "${log}" "${log}"
}
for p in "${INST}" "${EXIT_HOST}"; do
  [[ "$p" != *[\<\>\&\"\']* ]] || m6i_die "路径或主机名里不能有 XML 特殊字符：$p"
done
GUARD_ARGS=("${INST}/bin/abroad-guard" --state-dir "${STATE}/abroadd" --run-dir "${M6I_SOCK}"
  --integrity-manifest "${INST}/integrity.json" --integrity-base "${INST}")
D_ARGS=("${INST}/bin/abroadd" --state-dir "${STATE}/abroadd" --vm-dir "${STATE}/vm" --main-proxy-store "${STATE}/store"
  --exit-check-host "${EXIT_HOST}" --pf-user "${M6I_USER}" --socket-dir "${M6I_SOCK}"
  --vm-exec "${INST}/bin/abroad-vm" --vm-user "${M6I_USER}" --vm-arg="--image-dir=${INST}/image"
  --vm-arg="--direct-sock=${STATE}/vm/direct.sock" --vm-arg=--direct-peer-uid=0
  --integrity-manifest "${INST}/integrity.json" --integrity-base "${INST}"
  --launch-control --dev-fake-claude-dir "${INST}/fake-claude" --dev-resolver-test-domain)
if [ "${M6I_MODE}" = dry ]; then
  m6i_say STATE "${M6I_LABEL_G}.plist：${GUARD_ARGS[*]}"
  m6i_say STATE "${M6I_LABEL_D}.plist：${D_ARGS[*]}"
else
  plist "${M6I_LABEL_G}" false "${M6I_LOGDIR}/guard.log" "${GUARD_ARGS[@]}" >"$(P "${M6I_SNAP}")/${M6I_LABEL_G}.plist"
  plist "${M6I_LABEL_D}" true "${M6I_LOGDIR}/abroadd.log" "${D_ARGS[@]}" >"$(P "${M6I_SNAP}")/${M6I_LABEL_D}.plist"
fi
for l in "${M6I_LABEL_G}" "${M6I_LABEL_D}"; do
  m6i_do cp "$(P "${M6I_SNAP}")/${l}.plist" "$(P "${M6I_PLISTDIR}/${l}.plist")" || m6i_die "写 ${l}.plist 失败"
  m6i_do chown root:wheel "$(P "${M6I_PLISTDIR}/${l}.plist")"
  m6i_do chmod 644 "$(P "${M6I_PLISTDIR}/${l}.plist")"
  m6i_rec_set "plist.${l}" 1
  m6i_do launchctl bootstrap system "$(P "${M6I_PLISTDIR}/${l}.plist")" || m6i_die "注册 ${l} 失败"
  m6i_rec_set "launchd.${l}" 1
done

# ---- 等 abroadd 起来，核对首次准备和完整性 ----
API="${M6I_SOCK}/api.sock"
if [ "${M6I_MODE}" = real ]; then
  for _ in $(seq 1 30); do [ -S "${API}" ] && break; sleep 1; done
  [ -S "${API}" ] || m6i_die "30 秒内 abroadd 没起来，见 ${M6I_LOGDIR}/abroadd.log"
  for _ in $(seq 1 15); do
    st=$(curl -sS -m 10 --unix-socket "${API}" http://abroadd/v1/status 2>/dev/null || echo '{}')
    chk=$(printf '%s' "${st}" | /usr/bin/python3 -I -c 'import json,sys
d=json.load(sys.stdin); p=d.get("prepare") or {}; i=d.get("integrity") or {}
print(d.get("state",""), "prepare=%s" % p.get("done"), "integrity=%s/%s" % (i.get("checked"), i.get("ok")))' 2>/dev/null)
    [ "${chk}" = "RESTORED prepare=True integrity=True/True" ] && break
    sleep 1
  done
  m6i_log "abroadd 已起来：${chk}"
  case "${chk}" in
    "RESTORED prepare=True integrity=True/True") ;;
    *) m6i_die "abroadd 的状态不对（应为 RESTORED、首次准备完成、完整性校验通过）：${chk}" ;;
  esac
else
  m6i_say QUERY "curl --unix-socket ${API} http://abroadd/v1/status（应为 RESTORED、prepare.done、integrity.ok）"
fi
m6i_do rm -f "$(P "${M6I_SNAP}")/installing"
trap - EXIT
m6i_log "安装完成（开发构建：不结束 Claude 进程、解析锁只锁测试域名、不写 Claude Code 的托管设置）。"
m6i_say WAIT "以你自己的身份（不要 sudo）打开应用：open ${M6I_APP}；卸载：sudo scripts/m6/install/uninstall.sh"
exit 0
