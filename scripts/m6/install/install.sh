#!/usr/bin/env bash
# M6-8b 开发安装（真机，设计 11.4 的 M6-8a 段）：装到固定位置、建 _abroad、注册开发用 LaunchDaemon（abroadd 和开机时的
# guard），由 abroadd 自己做首次准备。只装、不启用：装完是“已回国”状态，不加载断网规则，不改本机网络。
# v0.71 起还有原地升级（--upgrade）：只换程序、镜像、清单、plist（变了才换）和安装目录里的脚本，状态目录整个保留。
# 会话里只做空跑、--check-only 和自测；真机安装、升级由用户执行（M6-9 的端到端也从这里开始）。用法见 README.md。
#
#   scripts/dev/integrity-build.sh --dev && scripts/dev/payload.sh && scripts/dev/app-bundle.sh   # 先以自己的身份构建
#   sudo scripts/m6/install/install.sh              # 安装
#   sudo scripts/m6/install/install.sh --upgrade    # 原地升级（只在已回国或已停用、没有正在进行的操作时）
#   scripts/m6/install/install.sh --check-only [--upgrade]   # 只做前提检查（只读，不需要 root）
#   DRY_RUN=1 scripts/m6/install/install.sh [--upgrade]      # 空跑
#
# 其他参数：--from <payload 目录>（默认 scripts/dev/out/payload；给了就不复制应用、日志写 /var/log/claude-abroad，
# 应用经管理员授权运行时用）、--yes（不问回车）、--accept-unknown-version（后台服务版本不明时也升级）。
# 安装失败时自动执行 uninstall.sh，按安装前的快照撤掉已经装上的部分；升级失败时把旧版本放回去、重启服务。
#
# 升级的退出码：0 完成（或已经是这个版本）、10 状态不允许、11 不降级、13 状态格式不同、14 版本不明要确认、
# 20 升级失败已退回旧版本、21 退回也没有完成、3 前提不满足、2 用法不对。
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "${ROOT}" || exit 1

CHECK_ONLY=0 UPGRADE=0 FROM="" ACCEPT_UNKNOWN=0 YES=0
usage() { echo "用法：sudo $0 [--upgrade] [--from <payload 目录>] [--yes] [--accept-unknown-version] | $0 --check-only [--upgrade]" >&2; exit 2; }
while [ $# -gt 0 ]; do
  case "$1" in
    --check-only) CHECK_ONLY=1 ;;
    --upgrade) UPGRADE=1 ;;
    --from) [ $# -ge 2 ] && [ -n "$2" ] || usage; FROM=$2; shift ;;
    --yes) YES=1 ;;
    --accept-unknown-version) ACCEPT_UNKNOWN=1 ;;
    *) usage ;;
  esac
  shift
done
M6I_FROM=0
[ -n "${FROM}" ] && M6I_FROM=1
. scripts/m6/install/lib.sh
. scripts/lib/common.sh # ab_dev_flags_missing（M2–M5 共用，自测 scripts/lib/buildcheck-selftest.sh）
[ "${YES}" = 1 ] && M6I_YES=1

if [ -n "${FROM}" ]; then
  case "${FROM}" in /*) ;; *) echo "--from 要给绝对路径" >&2; exit 2 ;; esac
  SRC="${FROM}"
else
  SRC="${M6I_TEST_SRC:-${ROOT}/scripts/dev/out/payload}"
fi
APPSRC="${M6I_TEST_APP:-${ROOT}/app/.build/bundle/ClaudeAbroad.app}"
EXIT_HOST="${M6I_EXIT_HOST:-www.gstatic.com}"
INST="${M6I_INSTALL}"
STATE="${INST}/state"
PREV="${INST}/prev"
OUT="${ROOT}/scripts/m6/install/out"
TS="$(date +%Y%m%d-%H%M%S)"
UNINSTALL="${ROOT}/scripts/m6/install/uninstall.sh"
# 程序、镜像、清单、版本号：安装和升级都从 payload 里复制这些；升级时整项替换。
BINS=(abroad abroad-rescue abroadd abroad-guard abroad-vm)
IMAGES=(Image initramfs.gz modloop rootfs.img)
# 安装目录里给“应用里的卸载”（阶段 B）用的脚本：和 payload、仓库里相同的相对位置。
INST_SCRIPTS=(scripts/m6/install/uninstall.sh scripts/m6/install/lib.sh scripts/lib/common.sh)
ITEMS=(bin image integrity.json build.json scripts)
if [ "${M6I_MODE}" = real ] && [ "${CHECK_ONLY}" != 1 ]; then
  if [ "${M6I_FROM}" = 1 ]; then
    # 从 payload 运行时（应用经管理员授权）日志写日志目录；全新安装时日志目录还没建，先只写标准输出。
    [ -d "${M6I_LOGDIR}" ] && M6I_LOG="${M6I_LOGDIR}/$([ "${UPGRADE}" = 1 ] && echo upgrade || echo install)-${TS}.log"
  else
    mkdir -p "${OUT}"; M6I_LOG="${OUT}/$([ "${UPGRADE}" = 1 ] && echo upgrade || echo install)-${TS}.log"
  fi
  [ "${M6I_LOG}" = /dev/null ] || : >"${M6I_LOG}"
fi

# 安装目录里的脚本从哪里复制：payload 里有就用 payload 的，否则用正在运行的这一份所在的仓库。
script_src() { if [ -f "${SRC}/$1" ]; then printf '%s' "${SRC}/$1"; else printf '%s' "${ROOT}/$1"; fi; }

# payload 的检查（安装和升级共用）：文件齐、逐文件摘要对得上（有 payload.sha256 时；--from 时必须有）、
# abroadd --version 和 build.json 一致、是开发构建。
payload_check() {
  local f
  for f in "${BINS[@]/#/bin/}" "${IMAGES[@]/#/image/}" integrity.json build.json; do
    [ -f "${SRC}/${f}" ] || reasons+=("缺少 ${SRC}/${f}：先以自己的身份执行 scripts/dev/integrity-build.sh --dev 和 scripts/dev/payload.sh（要先有虚拟机镜像 guest/out）")
  done
  if [ -f "${SRC}/payload.sha256" ]; then
    (cd "${SRC}" && shasum -a 256 -s -c payload.sha256) || reasons+=("${SRC}/payload.sha256 和里面的文件对不上：请重新执行 scripts/dev/payload.sh")
  elif [ "${M6I_FROM}" = 1 ]; then
    reasons+=("${SRC} 里没有 payload.sha256")
  fi
  # 2026-11-27 真机：构建时漏开 cgo，编出了起不了虚拟机的 abroad-vm。只用 grep 核对它链接了虚拟化框架（动态链接的框架路径
  # 明文写在 Mach-O 的加载命令里），不依赖 go 和 otool（经管理员授权运行时 PATH 里没有 go；没装命令行工具时 otool 会弹安装框）。
  if [ -f "${SRC}/bin/abroad-vm" ] && ! /usr/bin/grep -q "${M6I_VZ_MARK}" "${SRC}/bin/abroad-vm"; then
    reasons+=("${SRC}/bin/abroad-vm 没有链接虚拟化框架（构建时没开 cgo）：请装好 Xcode 命令行工具后重新构建（scripts/dev/integrity-build.sh --dev）")
  fi
  if [ -x "${SRC}/bin/abroadd" ] && [ "${M6I_MODE}" != test ]; then
    # 用公共函数（先存下输出再判断）：abroadd -h 的退出码是 2，直接接管道在 pipefail 下会误报“不是开发构建”
    ab_dev_flags_missing "${SRC}/bin/abroadd" --dev-fake-claude-dir --dev-resolver-test-domain >/dev/null ||
      reasons+=("${SRC}/bin/abroadd 不是开发构建（认不出 --dev-fake-claude-dir）：请用 scripts/dev/integrity-build.sh --dev 构建")
    [ -f "${SRC}/build.json" ] && [ "$("${SRC}/bin/abroadd" --version 2>/dev/null)" = "$(cat "${SRC}/build.json")" ] ||
      reasons+=("${SRC}/bin/abroadd --version 和 build.json 不一致：请重新构建")
  fi
}

# LaunchDaemon（开发用）的内容：安装和升级共用（升级时内容变了才换）。
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

precheck() {
  local reasons=() f
  payload_check
  [ "${M6I_FROM}" = 1 ] || [ -d "${APPSRC}/Contents/MacOS" ] || reasons+=("缺少应用包 ${APPSRC}：先执行 scripts/dev/app-bundle.sh")
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

# place_scripts：安装目录的 scripts/（给“应用里的卸载”用：属主 root，和 payload 相同的相对位置）。
place_scripts() {
  local f
  m6i_do mkdir -p "$(P "${INST}/scripts/m6/install")" "$(P "${INST}/scripts/lib")" || return 1
  for f in "${INST_SCRIPTS[@]}"; do m6i_do cp "$(script_src "$f")" "$(P "${INST}/$f")" || return 1; done
}

# settle_note：结尾等后台服务的启动检查结束（busy、starting 都为假，最多 30 秒；只读查询）。2026-11-27 真机：升级后马上
# 在应用里操作被回“忙”。等不到不算失败，只提示。
settle_note() {
  m6i_wait_settled 30 || m6i_log "注意：后台服务还在做启动检查，几秒后再操作（现在操作可能提示“忙”）。"
}

# ================= 原地升级（v0.71，docs/app-install-plan.md 第 2 节）=================
# 只换程序、镜像、清单、版本号、安装目录里的脚本、两份 plist（内容变了才换）；状态目录、日志目录、_abroad 一个字节都不碰。

UPG_MARK=upgrading # 快照目录里的标记：上一次升级没走完时下一次 --upgrade 先接着退回

# upgrade_status_check：经只读状态接口决定能不能升级。设 U_* 变量；返回 0 继续（U_SAME=1 是“已经是这个版本”），
# 否则返回要用的退出码。
upgrade_status_check() {
  local i out
  U_SAME=0
  for i in 1 2 3 4 5; do
    out=$(m6i_upgrade_decide "$(m6i_status)" "${SRC}/build.json" "$(P "${STATE}")/abroadd/state.json" "${ACCEPT_UNKNOWN}")
    U_CODE=$(sed -n 's/^code=//p' <<<"${out}")
    [ "${U_CODE}" = 15 ] || break
    [ "${M6I_MODE}" = test ] || sleep 1
  done
  [ "${U_CODE}" = 15 ] && U_CODE=10
  U_MSG=$(sed -n 's/^msg=//p' <<<"${out}")
  U_OLD=$(sed -n 's/^old=//p' <<<"${out}")
  U_NEW=$(sed -n 's/^new=//p' <<<"${out}")
  U_OLD_KNOWN=$(sed -n 's/^old_known=//p' <<<"${out}")
  U_OLD_COMMIT=$(sed -n 's/^old_commit=//p' <<<"${out}")
  U_OLD_TIME=$(sed -n 's/^old_time=//p' <<<"${out}")
  U_ST=$(sed -n 's/^before_state=//p' <<<"${out}")
  U_PR=$(sed -n 's/^before_prepare=//p' <<<"${out}")
  U_RB=$(sed -n 's/^before_rb=//p' <<<"${out}")
  grep -q '^same=1$' <<<"${out}" && U_SAME=1
  U_NEW_COMMIT=$(/usr/bin/python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["commit"])' "${SRC}/build.json" 2>/dev/null)
  U_NEW_TIME=$(/usr/bin/python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["build_time"])' "${SRC}/build.json" 2>/dev/null)
  return "${U_CODE:-10}"
}

upgrade_precheck() {
  local reasons=() f
  payload_check
  [ -d "$(P "${INST}")" ] && [ -d "$(P "${M6I_SNAP}")" ] ||
    reasons+=("没有装过（${INST} 或 ${M6I_SNAP} 不在）：请用 sudo scripts/m6/install/install.sh 安装")
  [ -e "$(P "${M6I_SNAP}")/installing" ] && reasons+=("上一次安装没有走完：请先执行 sudo scripts/m6/install/uninstall.sh")
  for f in "${M6I_PLISTDIR}/${M6I_LABEL_D}.plist" "${M6I_PLISTDIR}/${M6I_LABEL_G}.plist"; do
    [ -f "$(P "$f")" ] || reasons+=("${f} 不在：这份安装不完整，请先卸载再安装")
  done
  if [ ${#reasons[@]} -gt 0 ]; then
    for f in "${reasons[@]}"; do m6i_log "前提不满足：${f}"; done
    return 1
  fi
}

# upgrade_rollback <原因>：把 prev/ 里的旧版放回去、重启服务、核对。成功退出 20；没有完成退出 21（prev/ 和标记留着）。
upgrade_rollback() {
  local why=$1 it orig="" bad="" l
  trap - EXIT
  trap '' INT TERM HUP
  M6I_STATUS_PHASE=rollback
  m6i_log "升级没有完成（${why}），退回旧版本"
  m6i_do launchctl bootout "system/${M6I_LABEL_D}" || true
  [ -f "$(P "${PREV}")/orig-items" ] && orig=$(cat "$(P "${PREV}")/orig-items")
  for it in "${ITEMS[@]}"; do
    if grep -qx "${it}" <<<"${orig}"; then
      # 原来就有：已经搬进 prev 的放回去（还没搬的原样在安装目录里，不动）
      if [ -e "$(P "${PREV}/${it}")" ]; then
        { m6i_do rm -rf "$(P "${INST}/${it}")" && m6i_do mv "$(P "${PREV}/${it}")" "$(P "${INST}/${it}")"; } || bad="放回 ${it} 失败"
      fi
    elif [ -f "$(P "${PREV}")/orig-items" ]; then
      m6i_do rm -rf "$(P "${INST}/${it}")" || bad="删掉新放的 ${it} 失败" # 原来没有（老安装没有版本号、脚本）：删掉新放的
    fi
  done
  for l in "${M6I_LABEL_D}" "${M6I_LABEL_G}"; do
    if [ -f "$(P "${PREV}")/plists/${l}.plist" ] && ! cmp -s "$(P "${PREV}")/plists/${l}.plist" "$(P "${M6I_PLISTDIR}/${l}.plist")"; then
      m6i_do cp "$(P "${PREV}")/plists/${l}.plist" "$(P "${M6I_PLISTDIR}/${l}.plist")" || bad="放回 ${l}.plist 失败"
      if [ "${l}" = "${M6I_LABEL_G}" ]; then
        m6i_do launchctl bootout "system/${M6I_LABEL_G}" || true
        m6i_do launchctl bootstrap system "$(P "${M6I_PLISTDIR}/${M6I_LABEL_G}.plist")" || bad="重新注册 guard 失败"
      fi
    fi
  done
  if [ -d "$(P "${PREV}")/app.app" ]; then
    { m6i_do rm -rf "$(P "${M6I_APP}")" && m6i_do mv "$(P "${PREV}")/app.app" "$(P "${M6I_APP}")"; } || bad="放回应用失败"
  fi
  if [ -z "${bad}" ]; then
    { m6i_inject rollback && m6i_do launchctl bootstrap system "$(P "${M6I_PLISTDIR}/${M6I_LABEL_D}.plist")"; } || bad="重新启动后台服务失败"
  fi
  if [ -z "${bad}" ]; then
    m6i_wait_api up 60 || bad="60 秒内后台服务没有起来，见 ${M6I_LOGDIR}/abroadd.log"
  fi
  if [ -z "${bad}" ] && [ "${M6I_MODE}" != dry ]; then
    local c="" t=""
    if [ "${U_OLD_KNOWN:-0}" = 1 ]; then c=${U_OLD_COMMIT}; t=${U_OLD_TIME}; fi
    bad=$(m6i_upgrade_verify "$(m6i_status)" "${U_ST}" "${U_PR}" "${U_RB}" "$c" "$t")
  fi
  if [ -n "${bad}" ]; then
    m6i_log "升级失败（${why}），退回旧版本也没有完成：${bad}。旧版本留在 ${PREV}。网络有异常时按 docs/emergency.md 用 sudo abroad-rescue；"
    m6i_log "再执行一次 sudo scripts/m6/install/install.sh --upgrade 会先接着退回；也可以执行 sudo scripts/m6/install/uninstall.sh 卸载。"
    exit 21
  fi
  m6i_do rm -rf "$(P "${PREV}")"
  m6i_do rm -f "$(P "${M6I_SNAP}")/${UPG_MARK}"
  m6i_log "升级失败，已退回旧版本：${why}"
  exit 20
}

on_upgrade_exit() {
  local rc=$?
  [ "${rc}" = 0 ] && return 0
  [ "${M6I_MODE}" = dry ] && return 0
  upgrade_rollback "“${UPG_STEP}”这一步失败或被中断（状态 ${rc}）"
}

do_upgrade() {
  local it l code f why
  m6i_require_mode
  # 上一次升级没走完：先接着退回（升级前的状态记在标记里）
  if [ "${M6I_MODE}" != dry ] && [ -f "$(P "${M6I_SNAP}")/${UPG_MARK}" ]; then
    # shellcheck disable=SC1090
    . "$(P "${M6I_SNAP}")/${UPG_MARK}"
    upgrade_rollback "上一次升级没有走完"
  fi
  if ! upgrade_precheck; then
    if [ "${M6I_MODE}" = dry ]; then m6i_log "（空跑：前提不满足，继续打印后面的命令）"; else exit 3; fi
  fi
  # ---- 1. 确认状态（只读）----
  if [ "${M6I_MODE}" = dry ]; then
    m6i_say QUERY "curl --unix-socket ${M6I_SOCK}/api.sock http://abroadd/v1/status（已回国或已停用、没有正在进行的操作、状态格式相同、版本更新）"
    U_ST=RESTORED U_PR=True U_RB=True/True U_OLD="（空跑）" U_NEW="（空跑）"
  else
    upgrade_status_check; code=$?
    if [ "${code}" = 14 ] && [ "${M6I_MODE}" = real ] && [ "${M6I_YES:-0}" != 1 ]; then
      m6i_log "${U_MSG}仍要升级时按回车继续（Ctrl+C 取消）。"
      read -r _ </dev/tty
      ACCEPT_UNKNOWN=1
      upgrade_status_check; code=$?
    fi
    if [ "${code}" != 0 ]; then
      m6i_log "不能原地升级：${U_MSG}"
      [ "${code}" = 14 ] && m6i_log "确认要升级时加 --accept-unknown-version 重新运行。"
      m6i_log "什么都没有改动。"
      exit "${code}"
    fi
    if [ "${U_SAME}" = 1 ]; then m6i_log "${U_MSG}（${U_OLD}）"; exit 0; fi
  fi
  if [ "${M6I_MODE}" = real ] && [ "${M6I_YES:-0}" != 1 ]; then
    m6i_log "将把后台服务从 ${U_OLD} 升级到 ${U_NEW}，主代理和设置保留；期间后台服务会停一下，不会断网也不会接管网络。按回车开始（Ctrl+C 取消）。"
    read -r _ </dev/tty
  fi
  # ---- 2. 标记；此后任何失败或中断都退回 ----
  if [ "${M6I_MODE}" = dry ]; then
    m6i_say STATE "snap/${UPG_MARK}（升级前的状态和版本）"
  else
    { printf 'U_ST=%q\nU_PR=%q\nU_RB=%q\n' "${U_ST}" "${U_PR}" "${U_RB}"
      printf 'U_OLD_KNOWN=%q\nU_OLD_COMMIT=%q\nU_OLD_TIME=%q\n' "${U_OLD_KNOWN}" "${U_OLD_COMMIT}" "${U_OLD_TIME}"; } >"$(P "${M6I_SNAP}")/${UPG_MARK}" ||
      { m6i_log "写升级标记失败，什么都没有改动"; exit 1; }
  fi
  UPG_STEP="确认状态"
  trap on_upgrade_exit EXIT
  trap 'exit 130' INT TERM HUP

  # ---- 3. 停服务（它管理的虚拟机随之停；pf 不动）----
  UPG_STEP="停后台服务"
  m6i_inject stop || m6i_die "停后台服务失败"
  m6i_do launchctl bootout "system/${M6I_LABEL_D}" || m6i_die "停后台服务失败"
  m6i_wait_api down 30 || m6i_die "30 秒内后台服务没有停下"

  # ---- 4. 旧版搬进 prev/ ----
  UPG_STEP="把旧版本搬进 prev"
  { m6i_do mkdir -p "$(P "${PREV}/plists")" && m6i_do chmod 700 "$(P "${PREV}")"; } || m6i_die "建 prev 失败"
  if [ "${M6I_MODE}" != dry ]; then
    for it in "${ITEMS[@]}"; do [ -e "$(P "${INST}/${it}")" ] && echo "${it}"; done >"$(P "${PREV}")/orig-items"
  fi
  for l in "${M6I_LABEL_D}" "${M6I_LABEL_G}"; do
    m6i_do cp "$(P "${M6I_PLISTDIR}/${l}.plist")" "$(P "${PREV}")/plists/${l}.plist" || m6i_die "备份 ${l}.plist 失败"
  done
  for it in "${ITEMS[@]}"; do
    [ "${M6I_MODE}" = dry ] || [ -e "$(P "${INST}/${it}")" ] || continue
    m6i_inject move || m6i_die "搬 ${it} 失败"
    m6i_do mv "$(P "${INST}/${it}")" "$(P "${PREV}/${it}")" || m6i_die "搬 ${it} 失败"
  done

  # ---- 5. 放新文件 ----
  UPG_STEP="放新文件"
  m6i_do mkdir -p "$(P "${INST}/bin")" "$(P "${INST}/image")" || m6i_die "建目录失败"
  for f in "${BINS[@]}"; do m6i_do cp "${SRC}/bin/${f}" "$(P "${INST}/bin/${f}")" || m6i_die "复制 ${f} 失败"; done
  m6i_inject place || m6i_die "复制镜像失败"
  for f in "${IMAGES[@]}"; do m6i_do cp "${SRC}/image/${f}" "$(P "${INST}/image/${f}")" || m6i_die "复制镜像 ${f} 失败"; done
  m6i_do cp "${SRC}/integrity.json" "$(P "${INST}/integrity.json")" || m6i_die "复制完整性清单失败"
  m6i_do cp "${SRC}/build.json" "$(P "${INST}/build.json")" || m6i_die "复制版本号失败"
  place_scripts || m6i_die "复制安装目录里的脚本失败"
  # 只改新放的这几项的属主和权限（状态目录里 vm/ 属 _abroad，不能整个安装目录 chown -R）
  for it in "${ITEMS[@]}"; do
    m6i_do chown -R root:wheel "$(P "${INST}/${it}")" || m6i_die "改属主失败"
    # a+rX：虚拟机以 _abroad 运行，要能读镜像（构建出来的镜像是 0600；2026-11-27 真机：挂载 rootfs.img 报 Operation not supported）
    m6i_do chmod -R go-w,a+rX "$(P "${INST}/${it}")" || m6i_die "改权限失败"
  done

  # ---- 6. plist：内容变了才换（guard 换了时像开机一样运行一次）----
  UPG_STEP="换 plist"
  m6i_inject plist || m6i_die "生成 plist 失败"
  if [ "${M6I_MODE}" = dry ]; then
    m6i_say STATE "${M6I_LABEL_G}.plist、${M6I_LABEL_D}.plist：按新参数生成，和现有的不一样才换"
  else
    plist "${M6I_LABEL_G}" false "${M6I_LOGDIR}/guard.log" "${GUARD_ARGS[@]}" >"$(P "${M6I_SNAP}")/${M6I_LABEL_G}.plist" || m6i_die "生成 plist 失败"
    plist "${M6I_LABEL_D}" true "${M6I_LOGDIR}/abroadd.log" "${D_ARGS[@]}" >"$(P "${M6I_SNAP}")/${M6I_LABEL_D}.plist" || m6i_die "生成 plist 失败"
  fi
  for l in "${M6I_LABEL_G}" "${M6I_LABEL_D}"; do
    if [ "${M6I_MODE}" != dry ] && cmp -s "$(P "${M6I_SNAP}")/${l}.plist" "$(P "${M6I_PLISTDIR}/${l}.plist")"; then continue; fi
    [ "${M6I_MODE}" = dry ] || m6i_log "${l}.plist 的内容变了，换成新的"
    if [ "${l}" = "${M6I_LABEL_G}" ]; then m6i_do launchctl bootout "system/${l}" || true; fi
    m6i_do cp "$(P "${M6I_SNAP}")/${l}.plist" "$(P "${M6I_PLISTDIR}/${l}.plist")" || m6i_die "写 ${l}.plist 失败"
    m6i_do chown root:wheel "$(P "${M6I_PLISTDIR}/${l}.plist")"
    m6i_do chmod 644 "$(P "${M6I_PLISTDIR}/${l}.plist")"
    if [ "${l}" = "${M6I_LABEL_G}" ]; then
      m6i_do launchctl bootstrap system "$(P "${M6I_PLISTDIR}/${l}.plist")" || m6i_die "重新注册 guard 失败"
    fi
  done

  # ---- 7. 应用的交接（过渡，风险 8；顾问 2026-11-27 定）：老安装脚本以 root 身份放进“应用程序”的那份（记录里有 app）
  # 换成仓库里的新应用包，属主改成执行 sudo 的用户，升级成功后从安装记录里删掉 app——之后应用归用户管：卸载不删它、
  # 快照对照不管它。拿不到执行 sudo 的用户（没有或是 root）时不替换应用，只提示。
  APP_HANDOVER=0
  if [ "${M6I_FROM}" != 1 ] && [ -d "${APPSRC}/Contents/MacOS" ] &&
    { [ "${M6I_MODE}" = dry ] || { [ -n "$(m6i_rec_get app)" ] && [ -d "$(P "${M6I_APP}")" ]; }; }; then
    case "${M6I_MODE}" in
      real) owner=${SUDO_USER:-} ;;
      test) owner=${M6I_TEST_SUDO_USER:-} ;;
      dry) owner="<SUDO_USER>" ;;
    esac
    if [ -z "${owner}" ] || [ "${owner}" = root ]; then
      m6i_log "注意：${M6I_APP} 是以前的安装脚本以管理员身份放的；这次拿不到执行 sudo 的用户（请用 sudo 从你自己的账号运行），应用没有替换，也没有交给你。"
    else
      UPG_STEP="换应用"
      m6i_do mv "$(P "${M6I_APP}")" "$(P "${PREV}")/app.app" || m6i_die "搬旧应用失败"
      m6i_do cp -R "${APPSRC}" "$(P "${M6I_APP}")" || m6i_die "复制应用失败"
      m6i_do chown -R "${owner}" "$(P "${M6I_APP}")" || m6i_die "把应用交给 ${owner} 失败"
      APP_HANDOVER=1
    fi
  fi

  # ---- 8. 启动服务，核对 ----
  UPG_STEP="启动后台服务"
  m6i_inject start || m6i_die "启动后台服务失败"
  m6i_do launchctl bootstrap system "$(P "${M6I_PLISTDIR}/${M6I_LABEL_D}.plist")" || m6i_die "启动后台服务失败"
  m6i_wait_api up 60 || m6i_die "60 秒内后台服务没有起来，见 ${M6I_LOGDIR}/abroadd.log"
  UPG_STEP="核对"
  M6I_STATUS_PHASE=after
  if [ "${M6I_MODE}" = dry ]; then
    m6i_say QUERY "curl --unix-socket ${M6I_SOCK}/api.sock http://abroadd/v1/status（版本是新的、完整性校验通过、状态和升级前一样）"
  else
    why=$(m6i_upgrade_verify "$(m6i_status)" "${U_ST}" "${U_PR}" "${U_RB}" "${U_NEW_COMMIT}" "${U_NEW_TIME}")
    m6i_inject verify || why="（自测注入）核对失败"
    [ -z "${why}" ] || m6i_die "${why}"
  fi

  # ---- 9. 完成 ----
  m6i_do rm -rf "$(P "${PREV}")"
  if [ "${APP_HANDOVER}" = 1 ]; then
    m6i_do rm -f "$(P "${M6I_SNAP}")/app"
    m6i_log "应用 ${M6I_APP} 已换成新的并交给 ${owner}：之后归你管，卸载不会删它。"
  fi
  m6i_do rm -f "$(P "${M6I_SNAP}")/${UPG_MARK}"
  trap - EXIT
  settle_note
  m6i_log "升级完成：${U_OLD} → ${U_NEW}，主代理和设置已保留。"
  exit 0
}

if [ "${UPGRADE}" = 1 ]; then
  if [ "${CHECK_ONLY}" = 1 ]; then
    upgrade_precheck || exit 3
    [ "${M6I_MODE}" = dry ] && exit 0
    upgrade_status_check; code=$?
    if [ "${code}" = 0 ]; then
      if [ "${U_SAME}" = 1 ]; then m6i_log "${U_MSG}"; else m6i_log "可以升级：${U_OLD} → ${U_NEW}"; fi
    else
      m6i_log "不能原地升级：${U_MSG}"
    fi
    exit "${code}"
  fi
  do_upgrade
fi

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
for f in "${BINS[@]}"; do m6i_do cp "${SRC}/bin/${f}" "$(P "${INST}/bin/${f}")" || m6i_die "复制 ${f} 失败"; done
for f in "${IMAGES[@]}"; do m6i_do cp "${SRC}/image/${f}" "$(P "${INST}/image/${f}")" || m6i_die "复制镜像 ${f} 失败"; done
m6i_do cp "${SRC}/integrity.json" "$(P "${INST}/integrity.json")" || m6i_die "复制完整性清单失败"
m6i_do cp "${SRC}/build.json" "$(P "${INST}/build.json")" || m6i_die "复制版本号失败"
place_scripts || m6i_die "复制安装目录里的脚本失败"
m6i_do chown -R root:wheel "$(P "${INST}")"
# a+rX：虚拟机以 _abroad 运行，要能读镜像（构建出来的镜像是 0600；2026-11-27 真机）。这时安装目录里还没有状态目录。
m6i_do chmod -R go-w,a+rX "$(P "${INST}")"

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
if [ "${M6I_FROM}" = 1 ]; then
  m6i_log "从 payload 安装：不复制应用（应用就是 payload 的来源）"
elif [ -e "$(P "${M6I_APP}")" ]; then
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
  # M6-11b：装上即生效——回国拦截已加载（开发构建只管测试目标：测试段），只读核对，不改任何东西
  rb=$(printf '%s' "${st}" | /usr/bin/python3 -I -c 'import json,sys
r=(json.load(sys.stdin).get("return_block") or {}); print("on=%s active=%s test=%s nets=%s" % (r.get("on"), r.get("active"), r.get("test"), ",".join(r.get("nets") or [])))' 2>/dev/null)
  m6i_log "回国拦截：${rb}"
  [ "${rb}" = "on=True active=True test=True nets=${M6I_RB_NETS}" ] || m6i_die "回国拦截应已生效且是测试段（${M6I_RB_NETS}）：${rb}"
  anchor=$(pfctl -a "${M6I_ANCHOR}" -s rules 2>/dev/null)
  m6i_log "锚点里的规则：${anchor}"
  [ "${anchor}" = "${M6I_RB_RULES}" ] || m6i_die "锚点里应恰好是测试段的两条回国拦截：${anchor}"
else
  m6i_say QUERY "curl --unix-socket ${API} http://abroadd/v1/status（应为 RESTORED、prepare.done、integrity.ok）"
  m6i_say QUERY "curl --unix-socket ${API} http://abroadd/v1/status（return_block 应为 on、active、test，nets=${M6I_RB_NETS}）"
  m6i_q pfctl -a "${M6I_ANCHOR}" -s rules
fi
m6i_do rm -f "$(P "${M6I_SNAP}")/installing"
trap - EXIT
settle_note
m6i_log "安装完成（开发构建：不结束 Claude 进程、解析锁只锁测试域名、不写 Claude Code 的托管设置、回国拦截只拦测试段）。"
m6i_say WAIT "以你自己的身份（不要 sudo）打开应用：open ${M6I_APP}；卸载：sudo scripts/m6/install/uninstall.sh"
exit 0
