# 真机验证脚本的公共函数（M2-9c 起用，M3-8 抽出共用；m3-plan v2.10 决定 1）。用 source 引入，不单独执行。
#
# 引入方（scripts/m2/lib.sh、scripts/m3/lib.sh）先设 AB_P（变量前缀，例如 M2）和下面这些以它为前缀的变量，
# 再 source 本文件，然后调用 ab_wrap 生成带前缀的函数名（m2_do、m3_do……）：
#   ${AB_P}_STATE      状态目录（root 所有；restore 只读这里和脚本自己拍的快照）
#   ${AB_P}_SNAP       重启不丢的快照目录
#   ${AB_P}_USER       测试用户（无 shell、无 home、隐藏）
#   ${AB_P}_LOG        日志文件（调用方在运行中设置）
#   ${AB_P}_RESTORED   =1 表示已经还原过，trap 不再执行
#   ${AB_P}_TEST_STATE 只在不用 root 的自测里覆盖状态目录（以 root 运行时禁止）
#   ${AB_P}_FROM_DEADMAN  =1 表示由死人开关触发（取消死人开关时不动自己）
#   ${AB_P}_TEST_PROXY_FILE  只在自测里代替 scutil --proxy
#
# 约定：
# - 改系统之前先启动死人开关（独立进程，到点执行 restore.sh）；只有“建状态目录”可以在它之前；
# - DRY_RUN=1 时：改动类命令只打印 "CHANGE: …"，需要 root 的查询只打印 "QUERY: …"，都不执行；
# - 只按程序自己写的 PID 文件结束进程（先核对命令行）；不按名字结束进程，不执行 pfctl -d，不碰用户的 Clash。

DRY_RUN=${DRY_RUN:-0}

# ab_v NAME：读 ${AB_P}_NAME。
ab_v() { local n="${AB_P}_$1"; printf '%s' "${!n:-}"; }
# ab_set NAME VALUE：写 ${AB_P}_NAME。
ab_set() { printf -v "${AB_P}_$1" '%s' "$2"; }
# 脚本目录名（m2、m3），用在给用户的提示里。
ab_tag() { printf '%s' "$AB_P" | tr '[:upper:]' '[:lower:]'; }

# 自测目录：以 root 运行时禁止；只能配空跑或 --check-only（ab_test_guard 在 run.sh、restore.sh 里再查一次）。
ab_apply_test_state() {
  local t; t=$(ab_v TEST_STATE)
  [ -n "$t" ] || return 0
  [ "$(id -u)" != 0 ] || { echo "${AB_P}_TEST_STATE 只能在不用 root 的自测里用" >&2; exit 1; }
  ab_set STATE "$t"
  ab_set SNAP "${t}-snap"
}
ab_test_guard() {
  [ -z "$(ab_v TEST_STATE)" ] && return 0
  [ "$DRY_RUN" = 1 ] || [ "${1:-}" = check ] || { echo "${AB_P}_TEST_STATE 只能配 DRY_RUN=1 或 --check-only" >&2; exit 1; }
}

ab_log() {
  local f; f=$(ab_v LOG)
  printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*" | tee -a "${f:-/dev/null}" >&2
}
ab_die() { ab_log "错误：$*"; exit 1; }

# 执行一条会改动系统的命令；空跑时只打印（检查脚本据此核对命令清单）。
ab_do() {
  if [ "$DRY_RUN" = 1 ]; then
    printf 'CHANGE: %s\n' "$*"
  else
    "$@"
  fi
}

# 需要 root 的只读查询；空跑时只打印。
ab_q() {
  if [ "$DRY_RUN" = 1 ]; then
    printf 'QUERY: %s\n' "$*"
  else
    "$@" 2>&1
  fi
}

ab_require_root() {
  [ "$DRY_RUN" = 1 ] && return 0
  [ "$(id -u)" = 0 ] || ab_die "需要用 sudo 运行"
  [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != root ] || ab_die "请用 sudo 从你自己的账号运行（需要 SUDO_USER）"
}

ab_state_init() {
  ab_do mkdir -p "$(ab_v STATE)"
  ab_do chmod 755 "$(ab_v STATE)"
}
ab_state_set() {
  if [ "$DRY_RUN" = 1 ]; then printf 'STATE: %s=%s\n' "$1" "$2"; return; fi
  printf '%s\n' "$2" > "$(ab_v STATE)/$1"
}
ab_state_get() { local s; s=$(ab_v STATE); [ -f "$s/$1" ] && cat "$s/$1" || true; }
ab_state_add() {
  if [ "$DRY_RUN" = 1 ]; then printf 'STATE: %s+=%s\n' "$1" "$2"; return; fi
  local s; s=$(ab_v STATE)
  grep -qxF "$2" "$s/$1" 2>/dev/null || printf '%s\n' "$2" >> "$s/$1"
}

# 死人开关：$1 秒后执行 $2（restore.sh 的绝对路径）。独立进程，关掉终端、脚本被结束都不影响它。
# 要求状态目录已经存在（日志和 PID 写在里面）；启动后核对 PID 文件和进程都在，不在就退出，不做任何后续改动。
# restore.sh 要 SUDO_USER，显式写进它的环境，不靠继承。
ab_deadman_start() {
  local secs=$1 restore=$2 st; st=$(ab_v STATE)
  if [ "$DRY_RUN" = 1 ]; then printf 'CHANGE: 死人开关 %s 秒后执行 %s\n' "$secs" "$restore"; return; fi
  [ -d "$st" ] || ab_die "状态目录不存在，死人开关起不来"
  nohup /usr/bin/env SUDO_USER="${SUDO_USER:-}" "${AB_P}_TEST_STATE=$(ab_v TEST_STATE)" "${AB_P}_FROM_DEADMAN=1" \
    /bin/bash -c "sleep $secs; exec /bin/bash '$restore'" >> "$st/deadman.log" 2>&1 < /dev/null &
  local pid=$!
  disown "$pid" 2>/dev/null || true
  ab_state_set deadman_pid "$pid"
  sleep 0.3
  if [ "$(ab_state_get deadman_pid)" != "$pid" ] || ! kill -0 "$pid" 2>/dev/null; then
    ab_die "死人开关没有起来（PID 文件或进程不在），不继续"
  fi
  ab_log "死人开关已启动：${secs} 秒后自动还原（pid ${pid}）"
}

# 取消死人开关：只结束记下的那个 PID（核对它的命令行里有 restore.sh），由死人开关自己触发时不动。
ab_deadman_stop() {
  local pid; pid=$(ab_state_get deadman_pid)
  [ "$DRY_RUN" = 1 ] && printf 'CHANGE: kill <死人开关的 PID>\n'
  if [ -n "$pid" ] && [ "$(ab_v FROM_DEADMAN)" != 1 ]; then
    if ps -p "$pid" -o command= 2>/dev/null | grep -q 'restore\.sh'; then
      ab_do kill "$pid"
    fi
  fi
  ab_do rm -f "$(ab_v STATE)/deadman_pid"
}

# 中途退出时自动还原（在死人开关之后调用）：收到 INT、TERM，或以非零状态退出时执行一次 $1（restore.sh）。
# 正常走完时调用方先把 ${AB_P}_RESTORED=1，trap 就不再执行。
ab_arm_restore_trap() {
  ab_set RESTORE_SCRIPT "$1"
  trap 'ab__on_exit $?' EXIT
  trap 'exit 130' INT TERM
}
ab__on_exit() {
  local rc=$1
  [ "$(ab_v RESTORED)" = 1 ] && return 0
  [ "$rc" = 0 ] && return 0
  ab_set RESTORED 1
  ab_log "中途退出（状态 ${rc}），自动执行还原"
  if [ "$DRY_RUN" = 1 ]; then DRY_RUN=1 /bin/bash "$(ab_v RESTORE_SCRIPT)"; else /bin/bash "$(ab_v RESTORE_SCRIPT)"; fi
}

# 按 PID 文件结束进程：$1 PID 文件，$2 完整命令行里必须含有的字符串（核对 ps 的 command，不看 comm：
# 系统自带 python 的 comm 未必以 python3 结尾，经 sudo 启动时 $! 也只是 sudo 的 PID）。先 TERM，10 秒后 KILL。
# PID 文件由程序自己写，不用 $!。
ab_kill_pidfile() {
  local f=$1 want=$2 pid i
  [ -f "$f" ] || { [ "$DRY_RUN" = 1 ] && printf 'CHANGE: kill -TERM <%s 里的 PID>\n' "$(basename "$f")"; return 0; }
  pid=$(cat "$f")
  case "$pid" in '' | *[!0-9]*) ab_do rm -f "$f"; return 0 ;; esac
  if ps -p "$pid" -o command= 2>/dev/null | grep -qF -- "$want"; then
    ab_do kill -TERM "$pid"
    for i in $(seq 1 10); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
    if kill -0 "$pid" 2>/dev/null && ps -p "$pid" -o command= 2>/dev/null | grep -qF -- "$want"; then
      ab_do kill -KILL "$pid"
    fi
  else
    ab_log "PID ${pid} 已经不是要找的程序（命令行里没有 ${want}），不动它"
  fi
  ab_do rm -f "$f"
}

# 在 400–499 里找一个用户和组都没被占用的 ID。
ab_free_id() {
  local id
  for id in $(seq 400 499); do
    if ! dscl . -search /Users UniqueID "$id" | grep -q . && ! dscl . -search /Groups PrimaryGroupID "$id" | grep -q .; then
      echo "$id"; return 0
    fi
  done
  return 1
}

# 建专用测试用户（无 shell、无 home、隐藏）：pf 只放行它的出站。
ab_user_create() {
  local name id; name=$(ab_v USER)
  if dscl . -read "/Users/$name" >/dev/null 2>&1; then
    ab_die "测试用户 $name 已存在，请先运行 sudo scripts/$(ab_tag)/restore.sh"
  fi
  id=$(ab_free_id) || ab_die "400–499 内没有空闲的 UID"
  ab_state_set user "$name"
  ab_do dscl . -create "/Groups/$name"
  ab_do dscl . -create "/Groups/$name" PrimaryGroupID "$id"
  ab_do dscl . -create "/Groups/$name" Password '*'
  ab_do dscl . -create "/Users/$name"
  ab_do dscl . -create "/Users/$name" UniqueID "$id"
  ab_do dscl . -create "/Users/$name" PrimaryGroupID "$id"
  ab_do dscl . -create "/Users/$name" UserShell /usr/bin/false
  ab_do dscl . -create "/Users/$name" NFSHomeDirectory /var/empty
  ab_do dscl . -create "/Users/$name" RealName "claude-abroad ${AB_P} test"
  ab_do dscl . -create "/Users/$name" Password '*'
  ab_do dscl . -create "/Users/$name" IsHidden 1
  ab_do dscacheutil -flushcache
  ab_log "已建测试用户 ${name}（UID ${id}）"
}

ab_user_delete() {
  local u want; u=$(ab_state_get user); want=$(ab_v USER)
  [ "$DRY_RUN" = 1 ] && u=$want
  [ "$u" = "$want" ] || { [ -n "$u" ] && ab_log "跳过非测试用户 $u"; return 0; }
  ab_do dscl . -delete "/Users/$u"
  ab_do dscl . -delete "/Groups/$u"
  ab_do dscacheutil -flushcache
  ab_do rm -f "$(ab_v STATE)/user"
  ab_log "已删除测试用户 $u"
}

# 脱敏：本机和局域网的 IPv4、全部 IPv6、MAC 换成占位；文档用地址、回环、我们自己的 198.18.0.0/15 逐个保留。
ab_redact() {
  perl -pe '
    s/\b(?:[0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}\b/<MAC>/g;
    s/(?<![\w:])(?=[0-9a-fA-F:]*::|(?:[0-9a-fA-F]{1,4}:){5})[0-9a-fA-F:]{3,}(?![\w:])/<IPv6>/g;
    s{\b((?:\d{1,3}\.){3}\d{1,3})\b}{ my $a=$1; ($a =~ /^(?:198\.1[89]\.|192\.0\.2\.|198\.51\.100\.|203\.0\.113\.|127\.|0\.0\.0\.0$|255\.255\.)/) ? $a : "<IPv4>" }ge;
  '
}

ab_default_iface() { route -n get default 2>/dev/null | awk '/interface:/{print $2}'; }
ab_default_gw() { route -n get default 2>/dev/null | awk '/gateway:/{print $2}'; }

# 网络服务列表（去掉首行说明和停用项）。
ab_services() { networksetup -listallnetworkservices 2>/dev/null | tail -n +2 | grep -v '^\*' || true; }

# 系统代理是否开着（HTTP、HTTPS、SOCKS、PAC、自动发现任一）。
ab_system_proxy_on() {
  local out f; f=$(ab_v TEST_PROXY_FILE)
  if [ -n "$f" ]; then out=$(cat "$f"); else out=$(scutil --proxy 2>/dev/null); fi
  printf '%s\n' "$out" | grep -qE '^\s*(HTTPEnable|HTTPSEnable|SOCKSEnable|ProxyAutoConfigEnable|ProxyAutoDiscoveryEnable) : 1\s*$'
}

# ab_wrap PREFIX：生成带前缀的函数名（例如 m2_do → ab_do）。
ab_wrap() {
  local p=$1 f
  for f in test_guard log die do q require_root state_init state_set state_get state_add deadman_start deadman_stop \
    arm_restore_trap kill_pidfile free_id user_create user_delete redact default_iface default_gw services system_proxy_on; do
    eval "${p}_${f}() { ab_${f} \"\$@\"; }"
  done
}
