# M6-3 解析锁真机脚本（R9、R18，m6-plan v2.4 第 8 条）的公共部分。用 source 引入，不单独执行。
#
# 三种模式：
#   真实模式  sudo 运行；锁文件目录固定是 /etc/resolver，状态目录固定在 /Library/Application Support（重启不丢：
#             锁文件跨重启保留，记录也要跨重启保留，restore.sh 才知道哪些是自己写的）。
#             以 root 运行时，任何 M6R_TEST_*、M6R_ABROAD_DEV 环境变量都会被拒绝（不能把锁写到别处，也不能用来历不明的程序）。
#   空跑      --dry-run 或 DRY_RUN=1：只打印会执行的命令（CHANGE:）、查询（QUERY:）、要用户做的事（WAIT:），不写任何文件，不需要 sudo。
#   自测      M6R_TEST_DIR=<自己的临时目录>，不用 root：锁文件目录换成 <临时目录>/resolver，状态目录换成 <临时目录>/state，
#             基线出口和“锁无效”由 M6R_TEST_BASELINE（unreachable|proxy|direct）、M6R_TEST_LOCK_LEAK=1 模拟，
#             真实的查询（DNS、curl）不做，要用户输入的地方从标准输入读。
#
# 本脚本组只做两类事：写入、删除自己的锁文件（以及必要时建、删空的 /etc/resolver 目录），和只读查询。
# 不改网络设置，不发信号，不启动、不结束任何进程（死人开关是自己起的计时进程，取消时它自己退出，不靠信号）。
# 公共函数来自 scripts/lib/common.sh，只用其中的日志、空跑、状态文件、还原 trap、脱敏、系统代理判断。

M6R_REAL_RESOLVER_DIR=/etc/resolver
M6R_REAL_STATE="/Library/Application Support/claude-abroad-m6r"
M6R_RESTORED=0

AB_P=M6R
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/../../lib" && pwd)/common.sh"

m6r_fatal() { echo "$*" >&2; exit 1; }

# ---- 模式 ----
# 所有 M6R_TEST_* 变量（M6R_TEST_DIR、M6R_TEST_FAIL_AT、M6R_TEST_BASELINE、M6R_TEST_LOCK_LEAK……）按前缀一起处理，新加的也跑不掉。
M6R_TEST_VARS=$(env | sed -n 's/^\(M6R_TEST_[A-Za-z0-9_]*\)=.*/\1/p' | tr '\n' ' ')
if [ "$(id -u)" = 0 ]; then
  for v in ${M6R_TEST_VARS} M6R_TEST_DIR M6R_ABROAD_DEV M6R_CC_SRC_DIR; do
    [ -z "${!v:-}" ] || m6r_fatal "以 root 运行时不接受 ${v}（它只给不用 root 的自测用）。请去掉它再运行。"
  done
fi
if [ -n "${M6R_TEST_DIR:-}" ]; then
  case "${M6R_TEST_DIR}" in
    /etc | /etc/* | /private/etc | /private/etc/*) m6r_fatal "M6R_TEST_DIR 不能在 /etc 下" ;;
    /*) ;;
    *) m6r_fatal "M6R_TEST_DIR 必须是绝对路径" ;;
  esac
  [ -d "${M6R_TEST_DIR}" ] && [ -O "${M6R_TEST_DIR}" ] || m6r_fatal "M6R_TEST_DIR 必须是自己拥有的、已经存在的目录"
  M6R_MODE=test
  M6R_RESOLVER_DIR="${M6R_TEST_DIR}/resolver"
  M6R_STATE="${M6R_TEST_DIR}/state"
else
  M6R_MODE=real
  M6R_RESOLVER_DIR="${M6R_REAL_RESOLVER_DIR}"
  M6R_STATE="${M6R_REAL_STATE}"
  for v in ${M6R_TEST_VARS}; do
    [ -z "${!v:-}" ] || m6r_fatal "${v} 只能配 M6R_TEST_DIR 用（自测）"
  done
fi
[ "$DRY_RUN" = 1 ] && [ "${M6R_MODE}" = real ] && M6R_MODE=dry

# 真实模式要 root（经 sudo，从自己的账号）；空跑、自测不要。
m6r_require_mode() {
  case "${M6R_MODE}" in
    real) ab_require_root ;;
    dry | test) [ "$(id -u)" != 0 ] || m6r_fatal "空跑和自测不要用 sudo" ;;
  esac
}

# ---- 脱敏和日志：证据和输出里不写出口 IP、局域网地址、用户名、主目录路径 ----
m6r_scrub() {
  M6R_SCRUB_USER="${SUDO_USER:-$(id -un)}" perl -pe '
    BEGIN { $u = $ENV{M6R_SCRUB_USER} // "" }
    s{/Users/[^/\s]+}{~}g; s{/var/root}{~}g;
    if (length($u) >= 2) { s/(?<![\w.-])\Q$u\E(?![\w-])/<user>/g }
  ' | ab_redact
}
m6r_log() {
  local m; m=$(printf '%s' "$*" | m6r_scrub)
  ab_log "$m"
}
m6r_die() { m6r_log "错误：$*"; exit 1; }

# ---- 输入：真实模式从终端读；自测从标准输入读。读不到（EOF）返回 1 ----
m6r_read() {
  REPLY=""
  if [ "${M6R_MODE}" = test ]; then read -r REPLY || return 1; else read -r REPLY </dev/tty || return 1; fi
  return 0
}

# ---- 域名和锁文件内容的校验（内容来自 abroad-dev，这里只防坏输出）----
m6r_valid_domain() {
  printf '%s\n' "$1" | grep -qE '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$'
}
m6r_valid_content() { # $1 文件
  [ -s "$1" ] && [ "$(tail -c 1 "$1" | od -An -c | tr -d ' ')" = '\n' ] || return 1
  awk 'NR == 1 && /^nameserver [0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/ { n++; next }
       NR == 2 && /^port [0-9]+$/ { next }
       /^timeout [0-9]+$/ { t++; next }
       { bad = 1 }
       END { exit !(n == 1 && t == 1 && !bad) }' "$1"
}

# ---- 状态目录（root 所有、700；只放记录：lock.<域名> 是写入的内容，deadman_gen 是死人开关的轮次）----
m6r_state_init() {
  ab_do mkdir -p "${M6R_STATE}"
  ab_do chmod 700 "${M6R_STATE}"
}
m6r_records() { # 列出记录里的域名
  local f
  for f in "${M6R_STATE}"/lock.*; do
    [ -f "$f" ] || continue
    printf '%s\n' "${f##*/lock.}"
  done
}

# ---- 写入、删除一个锁文件 ----
# 写入：先把内容记进状态目录（记录在写入之前，中途断掉 restore 也知道），再确认目标不存在才写；不覆盖任何已有文件。
m6r_write_lock() { # $1 域名 $2 内容文件
  local d=$1 src=$2 rec
  m6r_valid_domain "$d" || m6r_die "域名不对：$d"
  rec="${M6R_STATE:?}/lock.${d}"
  if [ "$DRY_RUN" = 1 ]; then
    printf 'STATE: lock.%s（内容同 %s）\n' "$d" "$(basename "$src")"
    printf 'CHANGE: install -m 644 %s/lock.%s %s/%s\n' "${M6R_STATE}" "$d" "${M6R_RESOLVER_DIR}" "$d"
    return 0
  fi
  if [ -e "${M6R_RESOLVER_DIR:?}/${d}" ] || [ -L "${M6R_RESOLVER_DIR:?}/${d}" ]; then
    m6r_die "${M6R_RESOLVER_DIR}/${d} 已经存在（不是这次写的），不覆盖"
  fi
  cp "$src" "$rec" && chmod 600 "$rec" || m6r_die "记录写不进状态目录"
  install -m 644 "$rec" "${M6R_RESOLVER_DIR:?}/${d}" || m6r_die "写不进 ${M6R_RESOLVER_DIR}/${d}"
  cmp -s "$rec" "${M6R_RESOLVER_DIR:?}/${d}" || m6r_die "写入后读回的内容和要写的不一致：${M6R_RESOLVER_DIR}/${d}"
  m6r_log "已写入锁文件 ${M6R_RESOLVER_DIR}/${d}"
}

# 删除：只删内容和记录一致的普通文件；不一致就不动，返回 1。
m6r_remove_lock() { # $1 域名
  local d=$1 rec
  m6r_valid_domain "$d" || { m6r_log "记录里有不认识的域名（${d}），不动"; return 1; }
  rec="${M6R_STATE:?}/lock.${d}"
  if [ "$DRY_RUN" = 1 ]; then
    printf 'CHANGE: rm -f %s/%s\n' "${M6R_RESOLVER_DIR}" "$d"
    printf 'CHANGE: rm -f %s/lock.%s\n' "${M6R_STATE}" "$d"
    return 0
  fi
  [ -f "$rec" ] || { m6r_log "没有 ${d} 的记录，不动 ${M6R_RESOLVER_DIR}/${d}"; return 1; }
  if [ ! -e "${M6R_RESOLVER_DIR:?}/${d}" ] && [ ! -L "${M6R_RESOLVER_DIR:?}/${d}" ]; then
    m6r_log "${M6R_RESOLVER_DIR}/${d} 已经不在"
    rm -f -- "${M6R_STATE:?}/lock.${d}"
    return 0
  fi
  if [ -L "${M6R_RESOLVER_DIR:?}/${d}" ] || [ ! -f "${M6R_RESOLVER_DIR:?}/${d}" ] || ! cmp -s "$rec" "${M6R_RESOLVER_DIR:?}/${d}"; then
    m6r_log "${M6R_RESOLVER_DIR}/${d} 的内容和这次写入的不一致（可能被别的程序改过），不删，请人工核对"
    return 1
  fi
  rm -f -- "${M6R_RESOLVER_DIR:?}/${d}"
  if [ -e "${M6R_RESOLVER_DIR:?}/${d}" ]; then m6r_log "删不掉 ${M6R_RESOLVER_DIR}/${d}"; return 1; fi
  rm -f -- "${M6R_STATE:?}/lock.${d}"
  m6r_log "已删除锁文件 ${M6R_RESOLVER_DIR}/${d}"
}

# ---- 死人开关（不发信号、不结束任何进程）----
# 独立的计时进程（nohup，关掉终端也在）每秒核对一次轮次文件：轮次变了或文件没了（取消、换了新一轮）就自己退出；
# 到点还是同一轮，就执行 restore.sh --deadman <轮次>，restore 再核对一次轮次才动手。
m6r_deadman_start() { # $1 秒 $2 restore.sh 的绝对路径 $3 轮次
  local secs=$1 restore=$2 gen=$3 pid
  if [ "$DRY_RUN" = 1 ]; then
    printf 'CHANGE: 死人开关 %s 秒后执行 <仓库>/%s（第 %s 轮）\n' "$secs" "scripts/${restore##*/scripts/}" "$gen"
    return 0
  fi
  [ -d "${M6R_STATE}" ] || m6r_die "状态目录不存在，死人开关起不来"
  printf '%s\n' "$gen" >"${M6R_STATE}/deadman_gen" || m6r_die "轮次文件写不进去"
  nohup /usr/bin/env SUDO_USER="${SUDO_USER:-}" M6R_TEST_DIR="${M6R_TEST_DIR:-}" /bin/bash -c '
    secs=$1 genf=$2 gen=$3 restore=$4 i=0
    while [ "$i" -lt "$secs" ]; do
      sleep 1
      [ "$(cat "$genf" 2>/dev/null)" = "$gen" ] || exit 0
      i=$((i + 1))
    done
    exec /bin/bash "$restore" --deadman "$gen"' m6r-deadman "$secs" "${M6R_STATE}/deadman_gen" "$gen" "$restore" \
    >>"${M6R_STATE}/deadman.log" 2>&1 </dev/null &
  pid=$!
  disown "$pid" 2>/dev/null || true
  sleep 0.3
  ps -p "$pid" >/dev/null 2>&1 || m6r_die "死人开关没有起来，不继续"
  m6r_log "死人开关已启动（第 ${gen} 轮）：${secs} 秒后自动删除这一轮写入的锁文件"
}
# 取消：删掉轮次文件，计时进程一秒内自己退出。
m6r_deadman_cancel() { # $1 轮次
  if [ "$DRY_RUN" = 1 ]; then printf 'CHANGE: 取消死人开关（第 %s 轮）\n' "$1"; return 0; fi
  if [ "$(ab_state_get deadman_gen)" = "$1" ]; then rm -f -- "${M6R_STATE:?}/deadman_gen"; fi
}
