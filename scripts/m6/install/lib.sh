# M6-8b 开发安装、卸载脚本的公共部分（设计 11.4 的 M6-8a 段、m6-plan M6-8）。用 source 引入，不单独执行。
#
# 三种模式：
#   真实模式  sudo 运行：装到固定位置、注册开发用 LaunchDaemon、建 _abroad。以 root 运行时拒绝任何 M6I_TEST_* 变量。
#   空跑      DRY_RUN=1：只打印改动（CHANGE:）、只读查询（QUERY:）、要用户做的事（WAIT:），不写任何文件，不需要 sudo。
#   自测      M6I_TEST_DIR=<自己的临时目录>，不用 root：所有目标路径前面加上这个目录（文件真的写在里面，便于核对）；
#             系统命令（dscl、dscacheutil、launchctl、chown、pfctl、scutil、networksetup）不执行，只记一行 SYS: 到日志。
#
# 安装只装文件、注册服务；不启用、不改本机网络（新装的 abroadd 状态是“已回国”，不加载断网规则）。
# 默认是开发构建：abroadd 带 --launch-control（打开启动管控）、--dev-fake-claude-dir（回国结束 Claude 和第三层只针对
# 安装目录下 fake-claude 里的测试进程，本机真实的 Claude 不受影响）和 --dev-resolver-test-domain
# （首次准备只锁测试域名、不写 Claude Code 的托管设置），本机真实的 Claude、Claude Code 不受影响。

M6I_INSTALL="/Library/Application Support/claude-abroad"
M6I_SNAP="/Library/Application Support/claude-abroad-install-snap"
M6I_SOCK=/var/run/claude-abroad
M6I_LOGDIR=/var/log/claude-abroad
M6I_PLISTDIR=/Library/LaunchDaemons
M6I_LABEL_D=com.claude-abroad.dev.abroadd
M6I_LABEL_G=com.claude-abroad.dev.guard
M6I_USER=_abroad
M6I_APP=/Applications/ClaudeAbroad.app
M6I_CLI_LINK=/usr/local/bin/abroad
M6I_RESOLVER=/etc/resolver
M6I_CC_DIR="/Library/Application Support/ClaudeCode/managed-settings.d"
M6I_ANCHOR=com.apple/250.abroad
# M6-11b：开发安装只管测试目标，回国拦截用测试段（文档保留地址，和 internal/pf/rbset_dev.go 一致；不出现真实的地址段）。
M6I_RB_NETS="192.0.2.0/24,2001:db8:abd::/48"
M6I_RB_RULES="block return out quick inet from any to 192.0.2.0/24
block return out quick inet6 from any to 2001:db8:abd::/48"
M6I_DNS_KEY=State:/Network/Service/claude-abroad/DNS
DRY_RUN=${DRY_RUN:-0}

m6i_fatal() { echo "$*" >&2; exit 1; }

# ---- 模式 ----
M6I_TEST_VARS=$(env | sed -n 's/^\(M6I_TEST_[A-Za-z0-9_]*\)=.*/\1/p' | tr '\n' ' ')
if [ "$(id -u)" = 0 ]; then
  for v in ${M6I_TEST_VARS}; do
    [ -z "${!v:-}" ] || m6i_fatal "以 root 运行时不接受 ${v}（它只给不用 root 的自测用）。请去掉它再运行。"
  done
fi
if [ -n "${M6I_TEST_DIR:-}" ]; then
  case "${M6I_TEST_DIR}" in
    /) m6i_fatal "M6I_TEST_DIR 不能是 /" ;;
    /*) ;;
    *) m6i_fatal "M6I_TEST_DIR 必须是绝对路径" ;;
  esac
  [ -d "${M6I_TEST_DIR}" ] && [ -O "${M6I_TEST_DIR}" ] || m6i_fatal "M6I_TEST_DIR 必须是自己拥有的、已经存在的目录"
  [ "${DRY_RUN}" = 1 ] && m6i_fatal "自测和空跑不能同时用"
  M6I_MODE=test
  M6I_ROOT="${M6I_TEST_DIR}"
else
  for v in ${M6I_TEST_VARS}; do
    [ -z "${!v:-}" ] || m6i_fatal "${v} 只能配 M6I_TEST_DIR 用（自测）"
  done
  M6I_MODE=real
  [ "${DRY_RUN}" = 1 ] && M6I_MODE=dry
  M6I_ROOT=""
fi

# P <绝对路径>：自测时加上自测目录。
P() { printf '%s%s' "${M6I_ROOT}" "$1"; }

m6i_require_mode() {
  case "${M6I_MODE}" in
    real)
      [ "$(id -u)" = 0 ] || m6i_fatal "需要用 sudo 运行（空跑：DRY_RUN=1）"
      # --from（应用经管理员授权运行时，v0.71）没有 SUDO_USER；仍要求 root。
      [ "${M6I_FROM:-0}" = 1 ] || { [ -n "${SUDO_USER:-}" ] && [ "${SUDO_USER}" != root ]; } || m6i_fatal "请用 sudo 从你自己的账号运行（需要 SUDO_USER）"
      ;;
    dry | test) [ "$(id -u)" != 0 ] || m6i_fatal "空跑和自测不要用 sudo" ;;
  esac
}

M6I_LOG=/dev/null
m6i_log() {
  local l; l=$(printf '[%s] %s' "$(date +%H:%M:%S)" "$*")
  printf '%s\n' "$l" >&2
  [ "${M6I_LOG}" = /dev/null ] || printf '%s\n' "$l" >>"${M6I_LOG}"
}
m6i_die() { m6i_log "错误：$*"; exit 1; }

# m6i_do <命令…>：会改动系统的命令。空跑打印；自测里系统命令只记 SYS:，文件命令（路径已经由 P 加了前缀）真的执行。
m6i_do() {
  case "${M6I_MODE}" in
    dry) printf 'CHANGE: %s\n' "$*" ;;
    test)
      case "$1" in
        dscl | dscacheutil | launchctl | chown | pfctl | scutil | networksetup | sudo) printf 'SYS: %s\n' "$*" >>"${M6I_TEST_DIR}/sys.log" ;;
        *) "$@" ;;
      esac
      ;;
    real) "$@" ;;
  esac
}
# m6i_q <命令…>：只读查询（要 root 的）。空跑打印；自测返回空。
m6i_q() {
  case "${M6I_MODE}" in
    dry) printf 'QUERY: %s\n' "$*" ;;
    test) printf 'SYS-QUERY: %s\n' "$*" >>"${M6I_TEST_DIR}/sys.log" ;;
    real) "$@" 2>&1 ;;
  esac
}
# m6i_say <种类> <文字>：给核对用的标记行（NOTE:、WAIT:、STATE:），空跑和自测都打印到标准输出。
m6i_say() { printf '%s: %s\n' "$1" "$2"; }

# 专用用户是否存在（自测：M6I_TEST_USER_EXISTS=1 模拟已存在）。
m6i_user_exists() {
  case "${M6I_MODE}" in
    test) [ "${M6I_TEST_USER_EXISTS:-0}" = 1 ] ;;
    dry) return 1 ;;
    real) dscl . -read "/Users/${M6I_USER}" >/dev/null 2>&1 ;;
  esac
}
m6i_free_id() {
  local id
  [ "${M6I_MODE}" = real ] || { echo 450; return 0; }
  for id in $(seq 400 499); do
    if ! dscl . -search /Users UniqueID "$id" | grep -q . && ! dscl . -search /Groups PrimaryGroupID "$id" | grep -q .; then
      echo "$id"; return 0
    fi
  done
  return 1
}

# 记录（安装脚本建了什么，卸载只撤这些）：放在快照目录里，卸载最后才删。
m6i_rec_set() {
  [ "${M6I_MODE}" = dry ] && { m6i_say STATE "$1=$2"; return; }
  printf '%s\n' "$2" >"$(P "${M6I_SNAP}")/$1"
}
m6i_rec_get() { local f; f="$(P "${M6I_SNAP}")/$1"; [ -f "$f" ] && cat "$f" || true; }

# 快照里要比对的路径（存在与否、类型、内容摘要）：安装前拍一份，卸载后再拍一份，两份逐行相同才算还原。
m6i_snap_paths() {
  printf '%s\n' "${M6I_INSTALL}" "${M6I_SOCK}" "${M6I_LOGDIR}" "${M6I_PLISTDIR}/${M6I_LABEL_D}.plist" \
    "${M6I_PLISTDIR}/${M6I_LABEL_G}.plist" "${M6I_APP}" "${M6I_CLI_LINK}" "${M6I_RESOLVER}" "${M6I_CC_DIR}"
}
# m6i_snap_files <输出文件>：每个路径一行：路径、类型、内容摘要（目录列出一层的名字的摘要）。
m6i_snap_files() {
  local out=$1 p rp kind sum
  : >"$out"
  while IFS= read -r p; do
    rp=$(P "$p")
    if [ -L "$rp" ]; then kind=link; sum=$(readlink "$rp" | shasum -a 256 | cut -c1-16)
    elif [ -d "$rp" ]; then kind=dir; sum=$(ls -A "$rp" 2>/dev/null | LC_ALL=C sort | shasum -a 256 | cut -c1-16)
    elif [ -f "$rp" ]; then kind=file; sum=$(shasum -a 256 <"$rp" | cut -c1-16)
    else kind=none; sum=-; fi
    printf '%s\t%s\t%s\n' "$p" "$kind" "$sum" >>"$out"
  done < <(m6i_snap_paths)
}
# m6i_sys_cmp <安装前> <卸载后> [pf 引用信息]：系统快照的对照。pf 是否启用有一边无法判断时，这一项不参与对照，另记一条说明。
# 2026-11-27 真机（architect）：卸载期间系统的 pfd 取得了一个 pf 启用引用，安装前 Disabled、卸载后 Enabled。下面四条都成立时
# pf.enabled 这一项不算不一致，另记说明：①安装前 Disabled、卸载后 Enabled；②我们的锚点规则和 nat 都空；③References 读得出，
# 且没有进程名是 pfctl（我们用 pfctl -E 取引用，记的名字就是 pfctl）、abroadd、abroad-guard、abroad-rescue 的 token；
# 其余情况（含 References 读不出、格式认不出）照严格对照。
m6i_sys_cmp() {
  if grep -q '^pf\.enabled\.unknown' "$1" "$2"; then
    m6i_log "pf 是否启用：无法判断（pfctl -s info 第一行认不出，原样：$(grep -h '^pf\.enabled' "$1" "$2" | tr '\t\n' '  ')），不参与对照"
    diff <(grep -v '^pf\.enabled' "$1") <(grep -v '^pf\.enabled' "$2")
    return
  fi
  local info=${3:-} holders
  if [ -n "${info}" ] && [ -f "${info}" ] &&
    grep -qx $'pf.enabled\tDisabled' "$1" && grep -qx $'pf.enabled\tEnabled' "$2" &&
    grep -qx $'anchor.rules\t0' "${info}" && grep -qx $'anchor.nat\t0' "${info}" && grep -qx $'refs.ok\tyes' "${info}" &&
    ! grep -qE $'^ref\t(pfctl|abroadd|abroad-guard|abroad-rescue)$' "${info}"; then
    holders=$(sed -n $'s/^ref\t//p' "${info}" | sort -u | tr '\n' ' ')
    m6i_log "pf 仍启用：由别的程序持有引用（进程名：${holders% }），不是 Claude 出国留下的；我们的锚点和引用都已清除"
    diff <(grep -v '^pf\.enabled' "$1") <(grep -v '^pf\.enabled' "$2")
    return
  fi
  diff "$1" "$2"
}
# m6i_pf_refs_parse：把 pfctl -s References 的输出（标准输入）转成 refs.ok 和每个 token 的进程名（ref 行）。
# 认得出 TOKENS 表头才算读得出；进程名取 PID 之后的那一列。
m6i_pf_refs_parse() {
  awk -F'[ \t]+' '
    /^TOKENS:/ { t = 1; next }
    t && /^PID[ \t]/ { h = 1; next }
    h && $1 ~ /^[0-9]+$/ && NF >= 3 { print "ref\t" $2 }
    END { print "refs.ok\t" (h ? "yes" : "no") }'
}
# m6i_pf_info <输出文件>：卸载对照用的只读 pf 信息（真实模式）：我们锚点的规则数、nat 数、References 里每个 token 的进程名。
m6i_pf_info() {
  local out=$1
  [ "${M6I_MODE}" = test ] && [ -n "${M6I_TEST_PF_REFS:-}" ] && {
    { printf 'anchor.rules\t%s\nanchor.nat\t%s\n' "${M6I_TEST_PF_ANCHOR:-0}" "${M6I_TEST_PF_ANCHOR:-0}"; m6i_pf_refs_parse <"${M6I_TEST_PF_REFS}"; } >"$out"
    return 0
  }
  [ "${M6I_MODE}" = real ] || { : >"$out"; return 0; }
  {
    printf 'anchor.rules\t%s\n' "$(pfctl -a "${M6I_ANCHOR}" -s rules 2>/dev/null | grep -c . || true)"
    printf 'anchor.nat\t%s\n' "$(pfctl -a "${M6I_ANCHOR}" -s nat 2>/dev/null | grep -c . || true)"
    pfctl -s References 2>/dev/null | m6i_pf_refs_parse
  } >"$out"
}
# 系统部分的快照（真实模式；自测、空跑跳过）：专用用户、两个 LaunchDaemon 是否已注册、pf 锚点里的规则数、pf 是否启用
# （M6-11b：装上后回国拦截会启用 pf，卸载后要复原）、DNS 键、各网络服务的 IPv6。
m6i_snap_system() {
  local out=$1
  # 自测：M6I_TEST_SYS_SNAP 给出“这一刻”的系统快照内容（对照逻辑的自测用；真实模式下 M6I_TEST_* 一律被拒）
  [ "${M6I_MODE}" = test ] && [ -n "${M6I_TEST_SYS_SNAP:-}" ] && { cp "${M6I_TEST_SYS_SNAP}" "$out"; return 0; }
  [ "${M6I_MODE}" = real ] || { : >"$out"; return 0; }
  {
    printf 'user\t%s\n' "$(m6i_user_exists && echo yes || echo no)"
    printf 'launchd.d\t%s\n' "$(launchctl print "system/${M6I_LABEL_D}" >/dev/null 2>&1 && echo yes || echo no)"
    printf 'launchd.g\t%s\n' "$(launchctl print "system/${M6I_LABEL_G}" >/dev/null 2>&1 && echo yes || echo no)"
    printf 'pf.anchor\t%s\n' "$(pfctl -a "${M6I_ANCHOR}" -s rules 2>/dev/null | grep -c . || true)"
    # 第一行的格式没在真机核对过：认不出 Enabled/Disabled 时记“无法判断”并原样保存那一行（对照时不当成不一致）
    local pfl; pfl=$(pfctl -s info 2>/dev/null | awk 'NR==1')
    case "$(printf '%s' "${pfl}" | awk '{print $2}')" in
      Enabled | Disabled) printf 'pf.enabled\t%s\n' "$(printf '%s' "${pfl}" | awk '{print $2}')" ;;
      *) printf 'pf.enabled.unknown\t%s\n' "${pfl}" ;;
    esac
    printf 'dns.key\t%s\n' "$(printf 'show %s\nquit\n' "${M6I_DNS_KEY}" | scutil 2>/dev/null | grep -q 'No such key' && echo no || echo yes)"
    networksetup -listallnetworkservices 2>/dev/null | tail -n +2 | grep -v '^\*' | while IFS= read -r s; do
      printf 'ipv6\t%s\t%s\n' "$s" "$(networksetup -getinfo "$s" 2>/dev/null | awk -F': ' '/^IPv6: /{print $2}')"
    done
  } >"$out"
}

# ---- 原地升级（v0.71，docs/app-install-plan.md 第 2 节）----

# m6i_status：取 /v1/status（只读）；后台服务不响应时没有输出、返回 1。
# 自测：M6I_TEST_STATUS 指一份假的状态（文件不在、没给时当作不响应）；其中的 build、state_format 按安装目录里的
# build.json 改写（模拟“后台服务报告的是它自己的版本”；没有 build.json 时去掉 build，模拟加版本号之前的老安装）。
# M6I_STATUS_PHASE=after（升级后重启时）且给了 M6I_TEST_STATUS_BAD=version|integrity|state 时，按它改坏应答。
m6i_status() {
  case "${M6I_MODE}" in
    real) curl -sS -m 10 --unix-socket "${M6I_SOCK}/api.sock" http://abroadd/v1/status 2>/dev/null ;;
    dry) return 1 ;;
    test)
      [ -n "${M6I_TEST_STATUS:-}" ] && [ -f "${M6I_TEST_STATUS}" ] || return 1
      local bad=""
      [ "${M6I_STATUS_PHASE:-}" = after ] && bad="${M6I_TEST_STATUS_BAD:-}"
      /usr/bin/python3 -I - "${M6I_TEST_STATUS}" "$(P "${M6I_INSTALL}")/build.json" "${bad}" <<'PY'
import json, os, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))
bj, bad = sys.argv[2], sys.argv[3]
if os.path.exists(bj):
    b = json.load(open(bj, encoding="utf-8"))
    d["build"] = {"commit": b["commit"], "build_time": b["build_time"], "dirty": b["dirty"]}
    d["state_format"] = b["state_format"]
else:
    d.pop("build", None)
if bad == "version":
    d["build"] = {"commit": "0000000", "build_time": "20000101T000000Z", "dirty": False}
elif bad == "integrity":
    d["integrity"] = {"checked": True, "ok": False}
elif bad == "state":
    d["state"] = "ENABLED"
print(json.dumps(d))
PY
      ;;
  esac
}

# m6i_upgrade_decide <状态接口的应答（可以是空）> <新包的 build.json> <状态文件> <允许版本不明 0|1>。
# 输出 key=value 行：code（0 继续、0 且 same=1 什么都不做、10 状态不允许、11 不降级、13 状态格式不同、14 版本不明要确认、
# 15 忙（调用方重查））、msg、old、new、before_state、before_prepare、before_rb、old_known。
m6i_upgrade_decide() {
  /usr/bin/python3 -I - "$@" <<'PY'
import json, re, sys
raw, newp, statep, accept = sys.argv[1].strip(), sys.argv[2], sys.argv[3], sys.argv[4] == "1"
def out(code, msg="", **k):
    print("code=%d" % code)
    print("msg=" + msg.replace("\n", " "))
    for a, b in k.items():
        print("%s=%s" % (a, b))
    sys.exit(0)
try:
    d = json.loads(raw) if raw else None
except Exception:
    d = None
if not isinstance(d, dict):
    out(10, "后台服务没有响应，不能原地升级。网络有异常时按 docs/emergency.md 用应急程序 sudo abroad-rescue 恢复；"
        "也可以执行卸载脚本后重新安装。")
nb = json.load(open(newp, encoding="utf-8"))
new = "%s（构建于 %s%s）" % (nb["commit"], nb["build_time"], "，有未提交的改动" if nb.get("dirty") else "")
st = d.get("state", "")
un = d.get("uninstall") or {}
if d.get("corrupt"):
    out(10, "后台服务的状态文件读不出来，不能原地升级。请执行卸载脚本后重新安装。")
if st == "ENABLED":
    out(10, "已启用时不能升级：请先停用（sudo abroad down），升级后再启用。")
if st in ("ENABLING", "RESTORING"):
    out(10, "正在%s，等它完成再升级。" % ("启用" if st == "ENABLING" else "回国"))
if st not in ("RESTORED", "DISABLED"):
    out(10, "后台服务的状态是 %s，只有已回国或已停用时才能升级。" % (st or "未知"))
if un.get("running"):
    out(10, "正在一键卸载，不能升级。")
if un.get("done"):
    out(10, "已经一键卸载过：请执行卸载脚本（sudo scripts/m6/install/uninstall.sh）后重新安装，不能原地升级。")
if d.get("borrow"):
    out(10, "正在借用虚拟机（应用主代理或检查主代理），等它完成再升级。")
if d.get("busy"):
    out(15, "正在进行别的操作，等它完成再升级。")
sf = d.get("state_format")
if sf is None:
    try:
        sf = json.load(open(statep, encoding="utf-8")).get("version")
    except Exception:
        sf = None
    if sf != 1:
        out(13, "后台服务没有报告状态格式，状态文件的版本也读不出或不是 1：不能原地升级，请先卸载再重装（主代理和设置要重新配置）。")
if sf != nb.get("state_format"):
    out(13, "状态格式变了（旧 %s、新 %s），不能原地升级：请先卸载再重装（主代理和设置要重新配置）。不做迁移，是为了升级失败退回旧版本时"
        "旧版本还能读懂状态。" % (sf, nb.get("state_format")))
pr = (d.get("prepare") or {}).get("done")
rb = d.get("return_block") or {}
before = dict(before_state=st, before_prepare=pr, before_rb="%s/%s" % (rb.get("on"), rb.get("active")), new=new)
b = d.get("build") or {}
known = bool(re.fullmatch(r"[0-9a-f]{7,40}", str(b.get("commit", "")))) and bool(re.fullmatch(r"[0-9]{8}T[0-9]{6}Z", str(b.get("build_time", ""))))
if not known:
    if not accept:
        out(14, "后台服务的版本不明（是加版本号之前装的）。", old="版本不明", old_known=0, **before)
    out(0, "", old="版本不明", old_known=0, old_commit="", old_time="", **before)
old = "%s（构建于 %s%s）" % (b["commit"], b["build_time"], "，有未提交的改动" if b.get("dirty") else "")
k = dict(old=old, old_known=1, old_commit=b["commit"], old_time=b["build_time"], **before)
if b["build_time"] == nb["build_time"] and b["commit"] == nb["commit"]:
    out(0, "已经是这个版本，什么都不做。", same=1, **k)
if nb["build_time"] <= b["build_time"]:
    out(11, "后台服务（%s）不比要装的（%s）旧，不降级。" % (old, new), **k)
out(0, "", **k)
PY
}

# m6i_upgrade_verify <重启后的状态应答> <升级前的状态 before_state> <before_prepare> <before_rb> <期望的提交号> <期望的构建时间>：期望的提交号为空时不核对版本（退回到版本不明的旧版本时）。输出不满足的原因（满足时没有输出）。
m6i_upgrade_verify() {
  /usr/bin/python3 -I - "$@" <<'PY'
import json, sys
raw, st0, pr0, rb0, commit, btime = sys.argv[1:7]
raw = raw.strip()
try:
    d = json.loads(raw) if raw else None
except Exception:
    d = None
if not isinstance(d, dict):
    print("后台服务没有响应"); sys.exit(0)
b = d.get("build") or {}
if commit and (b.get("commit") != commit or b.get("build_time") != btime):
    print("后台服务的版本不对（%s %s，应为 %s %s）" % (b.get("commit"), b.get("build_time"), commit, btime)); sys.exit(0)
i = d.get("integrity") or {}
if commit and not (i.get("checked") and i.get("ok")):
    print("完整性校验没有通过"); sys.exit(0)
if d.get("corrupt") or d.get("state") != st0:
    print("状态和升级前不一样（%s → %s）" % (st0, d.get("state"))); sys.exit(0)
if str((d.get("prepare") or {}).get("done")) != pr0:
    print("首次准备的结果和升级前不一样"); sys.exit(0)
rb = d.get("return_block") or {}
if "%s/%s" % (rb.get("on"), rb.get("active")) != rb0:
    print("回国拦截和升级前不一样"); sys.exit(0)
PY
}

# m6i_inject <步骤>：自测的注入点（只在自测模式生效）。M6I_TEST_FAIL_AT=<步骤,…> 让这一步失败；M6I_TEST_KILL_AT=<步骤>
# 在这一步模拟被中断（和收到 INT/TERM 时一样以 130 退出）。
m6i_inject() {
  [ "${M6I_MODE}" = test ] || return 0
  if [ "${M6I_TEST_KILL_AT:-}" = "$1" ]; then m6i_log "（自测：在 $1 模拟被中断）"; exit 130; fi
  case ",${M6I_TEST_FAIL_AT:-}," in *",$1,"*) m6i_log "（自测：在 $1 注入失败）"; return 1 ;; esac
  return 0
}

# m6i_wait_api <up|down> <秒>：真实模式下等状态接口响应或不再响应；空跑、自测直接返回成功。
m6i_wait_api() {
  [ "${M6I_MODE}" = real ] || return 0
  local i
  for i in $(seq 1 "$2"); do
    if [ "$1" = up ]; then m6i_status >/dev/null && [ -S "${M6I_SOCK}/api.sock" ] && return 0
    else m6i_status >/dev/null || return 0; fi
    sleep 1
  done
  return 1
}
