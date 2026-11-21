# M0 验证脚本的公共函数。用 source 引入，不单独执行。
#
# 约定（见 docs/m0-plan.md"脚本安全约定"）：
# - 状态记在 root 所有的 ${M0_STATE}（/var/run/claude-abroad-m0），restore 只读这里；
# - 改系统之前先启动死人开关（nohup + disown 的独立进程），到点执行 restore；
# - 程序复制到 root 所有的 $M0_INSTALL 再运行；
# - DRY_RUN=1 时只打印会执行的改动命令。

M0_STATE=/var/run/claude-abroad-m0
M0_INSTALL="/Library/Application Support/claude-abroad-m0"
M0_TEST_USER=_abroadm0
DRY_RUN=${DRY_RUN:-0}

m0_log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*" | tee -a "${M0_LOG:-/dev/null}" >&2; }
m0_die() { m0_log "错误：$*"; exit 1; }

# 执行一条会改动系统的命令；dry-run 时只打印。
m0_do() {
  if [ "$DRY_RUN" = 1 ]; then
    printf '[dry-run] %s\n' "$*" >&2
  else
    "$@"
  fi
}

m0_require_root() {
  [ "$DRY_RUN" = 1 ] && return 0
  [ "$(id -u)" = 0 ] || m0_die "需要用 sudo 运行"
  [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != root ] || m0_die "请用 sudo 从你自己的账号运行（需要 SUDO_USER）"
}

m0_state_init() {
  m0_do mkdir -p "$M0_STATE"
  m0_do chmod 755 "$M0_STATE"
}

# 记一条状态：m0_state_set key value
m0_state_set() {
  if [ "$DRY_RUN" = 1 ]; then printf '[dry-run] state %s=%s\n' "$1" "$2" >&2; return; fi
  printf '%s\n' "$2" > "$M0_STATE/$1"
}
m0_state_get() { [ -f "$M0_STATE/$1" ] && cat "$M0_STATE/$1" || true; }
# 往列表型状态追加一行（去重）。
m0_state_add() {
  if [ "$DRY_RUN" = 1 ]; then printf '[dry-run] state %s+=%s\n' "$1" "$2" >&2; return; fi
  grep -qxF "$2" "$M0_STATE/$1" 2>/dev/null || printf '%s\n' "$2" >> "$M0_STATE/$1"
}

# 启动死人开关：$1 秒后执行 $2（restore 脚本的绝对路径）。
# 用 nohup + disown，关掉终端、脚本被 kill -9 都不影响它。
m0_deadman_start() {
  local secs=$1 restore=$2
  if [ "$DRY_RUN" = 1 ]; then printf '[dry-run] 死人开关：%s 秒后执行 %s\n' "$secs" "$restore" >&2; return; fi
  nohup /bin/bash -c "sleep $secs; M0_FROM_DEADMAN=1 /bin/bash '$restore'" \
    >> "$M0_STATE/deadman.log" 2>&1 < /dev/null &
  local pid=$!
  disown "$pid" 2>/dev/null || true
  m0_state_set deadman_pid "$pid"
  m0_log "死人开关已启动：${secs} 秒后自动还原（pid ${pid}）"
}

# 停掉死人开关（restore 正常结束时调用；由死人开关自己触发时不杀自己）。
m0_deadman_stop() {
  local pid; pid=$(m0_state_get deadman_pid)
  if [ -n "$pid" ] && [ "${M0_FROM_DEADMAN:-0}" != 1 ]; then
    pkill -P "$pid" 2>/dev/null || true
    kill "$pid" 2>/dev/null || true
  fi
  rm -f "$M0_STATE/deadman_pid"
}

# 在 400–499 里找一个用户和组都没被占用的 ID。
m0_free_id() {
  local id
  for id in $(seq 400 499); do
    if ! dscl . -search /Users UniqueID "$id" | grep -q . && \
       ! dscl . -search /Groups PrimaryGroupID "$id" | grep -q .; then
      echo "$id"; return 0
    fi
  done
  return 1
}

# 建临时测试用户（无 shell、无 home、隐藏），并记入状态。
m0_user_create() {
  local name=$M0_TEST_USER id
  if dscl . -read "/Users/$name" >/dev/null 2>&1; then
    m0_die "测试用户 $name 已存在，请先运行 restore.sh"
  fi
  id=$(m0_free_id) || m0_die "400–499 内没有空闲的 UID"
  m0_state_add users "$name"
  m0_do dscl . -create "/Groups/$name"
  m0_do dscl . -create "/Groups/$name" PrimaryGroupID "$id"
  m0_do dscl . -create "/Groups/$name" Password '*'
  m0_do dscl . -create "/Users/$name"
  m0_do dscl . -create "/Users/$name" UniqueID "$id"
  m0_do dscl . -create "/Users/$name" PrimaryGroupID "$id"
  m0_do dscl . -create "/Users/$name" UserShell /usr/bin/false
  m0_do dscl . -create "/Users/$name" NFSHomeDirectory /var/empty
  m0_do dscl . -create "/Users/$name" RealName "claude-abroad M0 test"
  m0_do dscl . -create "/Users/$name" Password '*'
  m0_do dscl . -create "/Users/$name" IsHidden 1
  m0_do dscacheutil -flushcache
  m0_log "已建临时用户 ${name}（UID ${id}）"
}

m0_users_delete() {
  local u
  for u in $(m0_state_get users) ; do
    case "$u" in _abroadm0) ;; *) m0_log "跳过非测试用户 $u"; continue ;; esac
    pkill -u "$u" 2>/dev/null || true
    dscl . -delete "/Users/$u" 2>/dev/null || true
    dscl . -delete "/Groups/$u" 2>/dev/null || true
    m0_log "已删除临时用户 $u"
  done
  rm -f "$M0_STATE/users"
  dscacheutil -flushcache
}

# 清空记录过的 pf 锚点，释放 pf 启用引用。
m0_pf_restore() {
  local a t
  for a in $(m0_state_get pf_anchors); do
    pfctl -a "$a" -F all >/dev/null 2>&1 || true
    m0_log "已清空锚点 $a"
  done
  rm -f "$M0_STATE/pf_anchors"
  for t in $(m0_state_get pf_tokens); do
    pfctl -X "$t" >/dev/null 2>&1 || true
    m0_log "已释放 pf 启用引用 $t"
  done
  rm -f "$M0_STATE/pf_tokens"
  local before; before=$(m0_state_get pf_status_before)
  local now; now=$(pfctl -si 2>/dev/null | awk '/^Status:/{print $2}')
  if [ -n "$before" ]; then
    m0_log "pf 状态：开始前 ${before}，现在 $now"
  fi
}

# 取得 pf 启用引用，token 记入状态。
m0_pf_enable() {
  if [ "$DRY_RUN" = 1 ]; then printf '[dry-run] pfctl -E\n' >&2; return; fi
  local out t
  out=$(pfctl -E 2>&1)
  t=$(printf '%s\n' "$out" | awk -F': *' '/Token/{print $2}' | tr -d ' ')
  [ -n "$t" ] || m0_die "pfctl -E 没有返回 token：$out"
  m0_state_add pf_tokens "$t"
  m0_log "pf 启用引用 token=$t"
}

# 加载规则到锚点，锚点名记入状态（先记再加载，中断也能清掉）。
m0_pf_load() {
  local anchor=$1 file=$2
  m0_state_add pf_anchors "$anchor"
  m0_do pfctl -a "$anchor" -f "$file"
}

m0_default_iface() { route -n get default 2>/dev/null | awk '/interface:/{print $2}'; }
m0_default_gw() { route -n get default 2>/dev/null | awk '/gateway:/{print $2}'; }

# 网络恢复检查：用网关和国内地址，不用需要代理才通的地址。
m0_check_network() {
  [ "$DRY_RUN" = 1 ] && { printf '[dry-run] 跳过网络检查\n' >&2; return; }
  local gw; gw=$(m0_default_gw)
  if [ -n "$gw" ] && ping -c1 -t2 "$gw" >/dev/null 2>&1; then
    m0_log "网络检查：网关 $gw 可达"
  else
    m0_log "网络检查：网关不可达（${gw:-无默认网关}）"
  fi
  if curl -s -o /dev/null -m 5 https://223.5.5.5/ 2>/dev/null || nc -z -G 3 223.5.5.5 443 2>/dev/null; then
    m0_log "网络检查：223.5.5.5:443 可达"
  else
    m0_log "网络检查：223.5.5.5:443 不可达"
  fi
}
