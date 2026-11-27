#!/bin/bash
# M0 R1：Virtualization.framework 能否在 launchd 的系统级后台服务里以非登录用户运行。
# 说明和判定表见同目录 README.md。设计文档第 15 节 R1、第 3.1 节。
#
# 用法：sudo scripts/m0/r1/run.sh [--deadman 900]
#
# 会做的系统改动（全部由 restore.sh 和死人开关撤销，重启后也不会残留服务）：
#   建临时用户 _abroadm0；把 abroad-vm 和镜像复制到 /Library/Application Support/claude-abroad-m0/r1/；
#   用 launchctl bootstrap system 加载临时服务（plist 不放 /Library/LaunchDaemons）。
# 不改 pf、路由、网卡、DNS；虚拟机不挂网卡，**不会断网**。
#
# 时长上限（死人开关必须大于它）：每个用例最多
#   bootstrap 10 秒 + 等 health 90 秒 + 不通时读系统日志最多 60 秒 + bootout 后等退出 30 秒 ≈ 190 秒；
#   最多 3 个用例 570 秒，加准备约 30 秒，合计约 600 秒 < 死人开关默认 900 秒。
set -u
here="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$here/../../.." && pwd)"
# shellcheck source=../lib.sh
. "$here/../lib.sh"

DEADMAN=900
while [ $# -gt 0 ]; do
  case "$1" in
    --deadman) DEADMAN=$2; shift 2 ;;
    *) echo "未知参数 $1"; exit 2 ;;
  esac
done

R1DIR="$M0_INSTALL/r1"
SRC_BIN="$ROOT/scripts/dev/out/bin/abroad-vm"
SRC_IMG="$ROOT/guest/out"
IMAGES="Image initramfs.gz rootfs.img modloop"
LABEL_USER=com.claude-abroad.m0.r1-user
LABEL_ROOT=com.claude-abroad.m0.r1-root
LABEL_HOME=com.claude-abroad.m0.r1-userhome
HEALTH_WAIT=90
OUT="$here/out"
STAMP=$(date +%Y%m%d-%H%M%S)
M0_LOG="$OUT/r1-$STAMP.log"
cd /

# 日志目录尽早交还给发起 sudo 的用户（预检失败时也一样）。
chown_out() { [ -n "${SUDO_USER:-}" ] && chown -R "$SUDO_USER" "$OUT" 2>/dev/null; return 0; }
mkdir -p "$OUT"
trap chown_out EXIT

m0_require_root
chown_out

# ---------- 0. 预检（任何系统改动之前） ----------
for f in $IMAGES; do
  [ -f "$SRC_IMG/$f" ] || m0_die "找不到 $SRC_IMG/$f。先以普通身份（不用 sudo）在仓库根目录执行：scripts/dev/vm-up.sh --no-panic"
done
[ -x "$SRC_BIN" ] || m0_die "找不到 ${SRC_BIN}。先以普通身份执行：scripts/dev/vm-up.sh --no-panic"
codesign -d --entitlements - "$SRC_BIN" 2>/dev/null | grep -q com.apple.security.virtualization ||
  m0_die "$SRC_BIN 没有虚拟化授权。先以普通身份执行：scripts/dev/sign.sh $SRC_BIN"
if [ -e "$M0_STATE/pf_anchors" ] || [ -e "$M0_STATE/pf_tokens" ]; then
  m0_die "发现 R2 的残留状态（pf 规则可能还在）。请先执行 sudo scripts/m0/r2/restore.sh"
fi
if [ -d "$M0_STATE" ] && [ -n "$(ls -A "$M0_STATE" 2>/dev/null)" ]; then
  m0_die "发现上次残留的状态 ${M0_STATE}（R1，或 R2 跑完留下的记录）。请先执行 sudo scripts/m0/r1/restore.sh"
fi
if dscl . -read "/Users/$M0_TEST_USER" >/dev/null 2>&1; then
  m0_die "测试用户 $M0_TEST_USER 已存在（上次没还原干净）。请先执行 sudo scripts/m0/r1/restore.sh"
fi

vm_procs() {
  ps -axo user=,pid=,lstart=,comm= | grep -E "abroad-vm|com.apple.Virtualization.VirtualMachine" | grep -v grep || true
}

finish() {
  local rc=$?
  m0_log "脚本结束（退出码 ${rc}），执行还原"
  /bin/bash "$here/restore.sh" 2>&1 | tee -a "$M0_LOG" >&2
  m0_log "还原后的 Virtualization / abroad-vm 进程（与开始前对照，不应多出本测试的进程）："
  vm_procs | tee -a "$M0_LOG" >&2
  chown_out
  m0_log "日志：$M0_LOG"
}
trap finish EXIT
trap 'exit 130' INT TERM

m0_log "R1 开始：macOS $(sw_vers -productVersion)，$(uname -m)"
m0_log "仓库 $ROOT；abroad-vm 来自 $SRC_BIN"
m0_log "开始前已有的 Virtualization 进程（不是本测试的，供对照）："
vm_procs | tee -a "$M0_LOG" >&2

# ---------- 1. 状态、死人开关、临时用户（任一步失败即终止并还原） ----------
m0_state_init || m0_die "建状态目录失败"
m0_state_set r1 "$STAMP" || m0_die "写状态失败"
m0_deadman_start "$DEADMAN" "$here/restore.sh"
m0_user_create
id -u "$M0_TEST_USER" >/dev/null 2>&1 || m0_die "建临时用户 $M0_TEST_USER 失败"
m0_log "临时用户：$(id "$M0_TEST_USER")"

# ---------- 2. 复制程序和镜像（root 所有） ----------
m0_state_set r1_dir "$R1DIR" || m0_die "写状态失败"
mkdir -p "$R1DIR/img" || m0_die "建 $R1DIR 失败"
chown root:wheel "$M0_INSTALL" "$R1DIR" "$R1DIR/img" || m0_die "改属主失败"
chmod 755 "$M0_INSTALL" "$R1DIR" "$R1DIR/img" || m0_die "改权限失败"
install -o root -g wheel -m 0755 "$SRC_BIN" "$R1DIR/abroad-vm" || m0_die "复制 abroad-vm 失败"
for f in $IMAGES; do
  install -o root -g wheel -m 0644 "$SRC_IMG/$f" "$R1DIR/img/$f" || m0_die "复制 $f 失败"
done
# 虚拟机以临时用户运行，要能读镜像（构建出来的镜像是 0600；2026-11-27 真机以 _abroad 拉起时挂载 rootfs.img 报
# Operation not supported）。上面 install -m 0644 已经给了读权限，这里和安装脚本一致再兜一次，只作用于镜像目录。
chmod -R go-w,a+rX "$R1DIR/img" || m0_die "改镜像权限失败"
codesign -d --entitlements - "$R1DIR/abroad-vm" 2>/dev/null | grep -q com.apple.security.virtualization ||
  m0_die "复制后的 abroad-vm 丢了虚拟化授权"
m0_log "已复制 abroad-vm 和镜像到 $R1DIR（授权仍在）："
codesign -dv "$R1DIR/abroad-vm" 2>&1 | tee -a "$M0_LOG" >&2

# 状态目录：用户用例归 _abroadm0，root 用例归 root，都是 0700。服务的日志预先建好。
install -d -o "$M0_TEST_USER" -g "$M0_TEST_USER" -m 0700 "$R1DIR/state-user" || m0_die "建 state-user 失败"
install -d -o root -g wheel -m 0700 "$R1DIR/state-root" || m0_die "建 state-root 失败"
# new_log <路径> <属主>
new_log() { : >"$1" && chown "$2:$2" "$1" 2>/dev/null || chown "$2" "$1"; chmod 600 "$1"; }
new_log "$R1DIR/user-stderr.log" "$M0_TEST_USER" || m0_die "建日志失败"
: >"$R1DIR/root-stderr.log" && chown root:wheel "$R1DIR/root-stderr.log" && chmod 600 "$R1DIR/root-stderr.log" || m0_die "建日志失败"

# 以测试用户的身份确认它够得着需要的文件（不通过就是脚本准备的问题，不是 R1 的结论）。
sudo -u "$M0_TEST_USER" /bin/test -x "$R1DIR/abroad-vm" || m0_die "$M0_TEST_USER 不能执行 $R1DIR/abroad-vm"
for f in $IMAGES; do
  sudo -u "$M0_TEST_USER" /bin/test -r "$R1DIR/img/$f" || m0_die "$M0_TEST_USER 不能读 $R1DIR/img/$f"
done
sudo -u "$M0_TEST_USER" /bin/test -w "$R1DIR/state-user" || m0_die "$M0_TEST_USER 不能写 $R1DIR/state-user"
sudo -u "$M0_TEST_USER" /bin/test -w "$R1DIR/user-stderr.log" || m0_die "$M0_TEST_USER 不能写日志"
m0_log "准备完成：$M0_TEST_USER 能执行 abroad-vm、读镜像、写状态目录"

# write_plist <plist 路径> <标签> <状态目录> <stderr 日志> [运行用户]；失败返回非 0。
write_plist() {
  local plist=$1 label=$2 state=$3 errlog=$4 user=${5:-}
  {
    cat <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>$label</string>
	<key>ProgramArguments</key>
	<array>
		<string>$R1DIR/abroad-vm</string>
		<string>run</string>
		<string>--image-dir</string><string>$R1DIR/img</string>
		<string>--state-dir</string><string>$state</string>
	</array>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><false/>
	<key>ExitTimeOut</key><integer>30</integer>
	<key>WorkingDirectory</key><string>/var/empty</string>
	<key>StandardErrorPath</key><string>$errlog</string>
	<key>StandardOutPath</key><string>$errlog</string>
EOF
    if [ -n "$user" ]; then
      printf '\t<key>UserName</key><string>%s</string>\n\t<key>GroupName</key><string>%s</string>\n' "$user" "$user"
    fi
    printf '</dict>\n</plist>\n'
  } >"$plist" || return 1
  chown root:wheel "$plist" && chmod 644 "$plist" || return 1
  plutil -lint "$plist" >/dev/null || return 1
}

# run_case <名称> <标签> <状态目录> <stderr 日志> [运行用户]
# 结果写到 CASE_RESULT：通过 / 不通过 / 未能执行（原因）。
# “未能执行”是脚本或 launchd 层面的问题，不能用来下 R1 的结论。
run_case() {
  local name=$1 label=$2 state=$3 errlog=$4 user=${5:-}
  local plist="$R1DIR/$label.plist" pid="" t0 deadline ok=0 health="" out rc i
  m0_log "========== 用例 ${name}（${user:-root}） =========="
  if ! write_plist "$plist" "$label" "$state" "$errlog" "$user"; then
    CASE_RESULT="未能执行（生成 plist 失败）"
    m0_log "用例 ${name}：${CASE_RESULT}"
    return
  fi
  m0_state_add labels "$label"
  t0=$(date +%s)
  out=$(launchctl bootstrap system "$plist" 2>&1)
  rc=$?
  m0_log "launchctl bootstrap system $plist → 退出码 ${rc} ${out}"
  if [ "$rc" != 0 ]; then
    CASE_RESULT="未能执行（launchctl bootstrap 返回 ${rc}：${out}）"
    launchctl bootout "system/$label" >/dev/null 2>&1 || true
    m0_log "用例 ${name}：${CASE_RESULT}"
    return
  fi

  deadline=$((t0 + HEALTH_WAIT))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if health=$(curl -fsS --max-time 3 --unix-socket "$state/ctl.sock" http://abroad-agent/v1/health 2>/dev/null) && [ -n "$health" ]; then
      ok=1
      break
    fi
    sleep 0.5
  done
  [ -f "$state/abroad-vm.pid" ] && pid=$(cat "$state/abroad-vm.pid") && m0_state_add pids "$pid"
  if [ "$ok" = 1 ]; then
    m0_log "✓ health 可达（启动后 $(($(date +%s) - t0)) 秒）：$health"
  else
    m0_log "✗ ${HEALTH_WAIT} 秒内 health 不通"
  fi
  m0_log "进程（用户、pid、启动时间、程序）："
  vm_procs | tee -a "$M0_LOG" >&2
  m0_log "launchctl print system/$label（节选）："
  launchctl print "system/$label" 2>&1 | grep -E "state =|pid =|last exit|username|program =" | tee -a "$M0_LOG" >&2
  m0_log "abroad-vm 日志（$errlog）："
  tail -n 40 "$errlog" 2>/dev/null | tee -a "$M0_LOG" >&2
  m0_log "虚拟机控制台末尾 40 行："
  tail -n 40 "$state/console.log" 2>/dev/null | tee -a "$M0_LOG" >&2
  if [ "$ok" != 1 ]; then
    m0_log "系统日志（最近 3 分钟，abroad-vm 和 Virtualization，只读）："
    perl -e 'alarm shift; exec @ARGV or die "exec: $!"' 60 log show --last 3m --style compact \
      --predicate 'process == "abroad-vm" OR process == "com.apple.Virtualization.VirtualMachine"' 2>&1 |
      tail -n 80 | tee -a "$M0_LOG" >&2
  fi

  m0_log "launchctl bootout system/$label"
  launchctl bootout "system/$label" 2>&1 | tee -a "$M0_LOG" >&2
  if [ -n "$pid" ]; then
    for i in $(seq 1 60); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.5
    done
    if kill -0 "$pid" 2>/dev/null; then
      m0_log "✗ abroad-vm（pid $pid）30 秒内没有退出（restore 会强制结束）"
    else
      m0_log "✓ abroad-vm（pid $pid）已退出"
    fi
  fi
  m0_log "停止后 abroad-vm 日志末尾（应看到收到 terminated、已停止）："
  tail -n 8 "$errlog" 2>/dev/null | tee -a "$M0_LOG" >&2
  [ -e "$state/abroad-vm.pid" ] && m0_log "注意：pidfile 没有删除（bootout 可能用了 SIGKILL）"
  [ -e "$state/ctl.sock" ] && m0_log "注意：ctl.sock 没有删除"
  if [ "$ok" = 1 ]; then CASE_RESULT="通过"; else CASE_RESULT="不通过"; fi
  m0_log "用例 ${name}：${CASE_RESULT}"
}

# ---------- 3. 用例依次跑（一个的结果不影响另一个） ----------
run_case "_abroadm0" "$LABEL_USER" "$R1DIR/state-user" "$R1DIR/user-stderr.log" "$M0_TEST_USER"
RESULT_USER=$CASE_RESULT
run_case "root 对照" "$LABEL_ROOT" "$R1DIR/state-root" "$R1DIR/root-stderr.log"
RESULT_ROOT=$CASE_RESULT

# 用户用例不通过时加跑：同一个用户，但给一个真实可写的 home，
# 用来区分“非登录用户不行”和“只是缺 home”。
RESULT_HOME="未执行（_abroadm0 用例没有不通过）"
if [ "$RESULT_USER" = "不通过" ]; then
  m0_log "_abroadm0 用例不通过，加跑“_abroadm0 + 真实 home”"
  if install -d -o "$M0_TEST_USER" -g "$M0_TEST_USER" -m 0700 "$R1DIR/home" &&
    install -d -o "$M0_TEST_USER" -g "$M0_TEST_USER" -m 0700 "$R1DIR/state-userhome" &&
    new_log "$R1DIR/userhome-stderr.log" "$M0_TEST_USER" &&
    dscl . -create "/Users/$M0_TEST_USER" NFSHomeDirectory "$R1DIR/home" &&
    dscacheutil -flushcache; then
    m0_log "home 已改为 $R1DIR/home"
    run_case "_abroadm0 + home" "$LABEL_HOME" "$R1DIR/state-userhome" "$R1DIR/userhome-stderr.log" "$M0_TEST_USER"
    RESULT_HOME=$CASE_RESULT
  else
    RESULT_HOME="未能执行（准备 home 失败）"
  fi
fi

m0_log "========== 结果 =========="
m0_log "以 _abroadm0 运行：        $RESULT_USER"
m0_log "以 root 运行：             $RESULT_ROOT"
m0_log "_abroadm0 + 真实 home：    $RESULT_HOME"
m0_log "判定见 scripts/m0/r1/README.md 的判定表；请把本日志贴回给开发会话"
