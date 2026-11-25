#!/usr/bin/env bash
# M6-3 解析锁的真机验证（R9、R18；m6-plan v2.4 第 8 条）：在“回国状态”（本项目没有接管网络）下写入解析锁，观察
# 系统解析器、curl、Claude Code、Claude 桌面版、浏览器（默认、开了加密 DNS）能不能连上 Claude，以及本机开着别的代理软件
# （TUN 或系统代理）时的变化。两种写法（default：nameserver 198.18.0.2；loopback：127.0.0.1 加端口）各跑一遍。
#
# 顺序：前提检查（同名文件已存在就停下，不覆盖）→ 无锁的基线（先用不带凭据的 curl 判断不经代理能不能到 Anthropic：
# 到不了 / 经本机代理或 TUN / 直连，判断不了按直连）→ 只锁测试域名的预演（不影响 Claude）→ 输入 yes 才锁真实域名
# （提示里写明两个方向的风险：锁有效时 Claude 连不上；锁无效时已登录的 Claude 会真的连上去，直连时就是从本机 IP）
# → 每种写法：启动死人开关 → 写入 → 先做不带凭据的几项（系统解析器、dig、curl）→ 再做已登录程序的手动观察 → 删除
# → 取消死人开关 → 结束时执行 restore.sh。
# 已登录程序的观察（Claude Code、桌面版、浏览器）要按出口单独确认：到不了不用；经代理或 TUN 每个场景输入 exit-trusted
# （脚本分不出隧道的出口在哪里，出口仍在国内就等同直连）；直连输入 real-ip-ok。不带凭据的几项已经显示锁无效时也默认跳过
# （直连时不能做；经代理时先确认出口，再输入 open-anyway 才做）。跳过的记“跳过：原因”，不算不通过。
# 结果文件里的“原始 …”行是每条只读命令输出里判断要用的字段（接口名、代理项、状态码、有无应答、地址个数和类别），不存地址。
# 结束、出错、中断（Ctrl+C、关掉终端）、死人开关到点，都会删掉自己写的锁文件（只删内容和自己写入一致的）。
#
# 只做两类事：写入、删除自己的锁文件（必要时建、删空的 /etc/resolver 目录），和只读查询。不改网络设置，不开关代理软件，
# 不发信号，不启动、不结束任何进程（Claude 也一样：要打开、要试的程序都由用户自己操作）。
# 锁文件内容和域名清单的唯一来源是 Go 包 internal/resolverlock，经 abroad-dev resolver-lock 取得。
#
#   sudo scripts/m6/resolver/run.sh                      # 真实运行（先预演，再问要不要锁真实域名）
#   sudo scripts/m6/resolver/run.sh --test-domain-only   # 只做测试域名的预演
#   scripts/m6/resolver/run.sh --dry-run                 # 空跑：只打印每一步会执行的命令，不写任何文件，不需要 sudo
#
# 环境变量：
#   M6R_DEADMAN_SECS  每一轮的死人开关（默认 900 秒 = 15 分钟；到点自动删掉这一轮的锁文件）
#   M6R_PORT          loopback 写法的端口（默认用 abroad-dev 的默认值）
#   M6R_GO            go 的路径（sudo 下找不到 go 时用，例如 sudo M6R_GO="$(command -v go)" scripts/m6/resolver/run.sh）
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "${ROOT}" || exit 1
TEST_ONLY=0
for a in "$@"; do
  case "$a" in
    --dry-run) DRY_RUN=1 ;;
    --test-domain-only) TEST_ONLY=1 ;;
    -h | --help) sed -n '2,24p' "$0"; exit 0 ;;
    *) echo "用法：sudo $0 [--test-domain-only] | $0 --dry-run" >&2; exit 2 ;;
  esac
done
DRY_RUN=${DRY_RUN:-0}
export DRY_RUN
. scripts/m6/resolver/lib.sh
m6r_require_mode
[ "${M6R_MODE}" = real ] || [ -n "${SUDO_USER:-}" ] || SUDO_USER=$(id -un)

RESTORE="${ROOT}/scripts/m6/resolver/restore.sh"
DEADMAN_SECS="${M6R_DEADMAN_SECS:-900}"
case "${DEADMAN_SECS}" in '' | *[!0-9]*) m6r_fatal "M6R_DEADMAN_SECS 要是秒数" ;; esac
[ "${DEADMAN_SECS}" -ge 1 ] && [ "${DEADMAN_SECS}" -le 3600 ] || m6r_fatal "M6R_DEADMAN_SECS 要在 1–3600 之间"
PORT_ARGS=""
if [ -n "${M6R_PORT:-}" ]; then
  case "${M6R_PORT}" in *[!0-9]*) m6r_fatal "M6R_PORT 要是端口号" ;; esac
  PORT_ARGS="--port ${M6R_PORT}"
fi
TS="$(date +%Y%m%d-%H%M%S)"
case "${M6R_MODE}" in
  real) OUT="${ROOT}/scripts/m6/resolver/out" ;;
  test) OUT="${M6R_TEST_DIR}/out" ;;
  dry) OUT="" ;;
esac
if [ -n "${OUT}" ]; then
  mkdir -p "${OUT}"
  M6R_LOG="${OUT}/run-${TS}.log"
  EVID="${OUT}/evidence-${TS}.txt"
  : >"${M6R_LOG}"
  : >"${EVID}"
else
  EVID=/dev/null
fi
export M6R_LOG

ev() { printf '%s\n' "$*" | m6r_scrub >>"${EVID}"; }
step() { m6r_log "== $* =="; ev ""; ev "## $*"; }
wait_line() { [ "$DRY_RUN" = 1 ] && printf 'WAIT: %s\n' "$*"; }
nap() { [ "${M6R_MODE}" = real ] && sleep "$1"; return 0; }
fail_at() { # 自测注入的错误（只在 M6R_TEST_DIR 下生效）
  [ "${M6R_MODE}" = test ] && [ "${M6R_TEST_FAIL_AT:-}" = "$1" ] && m6r_die "自测注入的错误：$1"
  return 0
}

# ---- 编出 abroad-dev，取得域名清单和锁文件内容（唯一来源 internal/resolverlock）----
BD=""
cleanup_build() {
  case "${BD}" in */claude-abroad-m6r.*) rm -rf -- "${BD:?}" ;; esac
  BD=""
}
trap 'cleanup_build' EXIT
if [ "${M6R_MODE}" = real ]; then
  BD=$(sudo -u "${SUDO_USER}" /usr/bin/mktemp -d /tmp/claude-abroad-m6r.XXXXXX) || m6r_die "建不了临时目录"
else
  BD=$(mktemp -d "${TMPDIR:-/tmp}/claude-abroad-m6r.XXXXXX") || m6r_die "建不了临时目录"
fi
if [ -n "${M6R_ABROAD_DEV:-}" ]; then
  AD="${M6R_ABROAD_DEV}" # 只有自测和空跑能用（lib.sh 在 root 下拒绝它）
else
  GO="${M6R_GO:-$(command -v go 2>/dev/null || true)}"
  [ -n "${GO}" ] && [ -x "${GO}" ] || m6r_die "找不到 go。请这样运行：sudo M6R_GO=\"\$(command -v go)\" scripts/m6/resolver/run.sh"
  m6r_log "编出 abroad-dev（以你自己的账号，不用 root）"
  if [ "${M6R_MODE}" = real ]; then
    sudo -u "${SUDO_USER}" "${GO}" build -o "${BD}/abroad-dev" ./cmd/abroad-dev || m6r_die "go build ./cmd/abroad-dev 失败"
  else
    "${GO}" build -o "${BD}/abroad-dev" ./cmd/abroad-dev || m6r_die "go build ./cmd/abroad-dev 失败"
  fi
  AD="${BD}/abroad-dev"
fi
"${AD}" resolver-lock domains >"${BD}/domains" || m6r_die "abroad-dev resolver-lock domains 失败"
"${AD}" resolver-lock content --form default >"${BD}/content.default" || m6r_die "取不到 default 写法的内容"
# shellcheck disable=SC2086 # PORT_ARGS 是“--port N”或空
"${AD}" resolver-lock content --form loopback ${PORT_ARGS} >"${BD}/content.loopback" || m6r_die "取不到 loopback 写法的内容"
DOMAINS=$(tr -d '\r' <"${BD}/domains" | grep . || true)
n=0
for d in ${DOMAINS}; do
  m6r_valid_domain "$d" || m6r_die "abroad-dev 给出的域名不对：$d"
  n=$((n + 1))
done
[ "$n" -ge 1 ] && [ "$n" -le 10 ] || m6r_die "abroad-dev 给出的域名数不对（${n} 个）"
[ "$(printf '%s\n' ${DOMAINS} | sort | uniq -d)" = "" ] || m6r_die "abroad-dev 给出的域名有重复"
for f in default loopback; do m6r_valid_content "${BD}/content.${f}" || m6r_die "abroad-dev 给出的 ${f} 写法内容不对"; done
grep -q '^port ' "${BD}/content.default" && m6r_die "default 写法不应带端口"
grep -q '^nameserver 127\.' "${BD}/content.loopback" && grep -q '^port ' "${BD}/content.loopback" || m6r_die "loopback 写法应是回环地址加端口"
# 测试域名：resolverlock.TestDomain（同一来源，经 abroad-dev 打印）。
TESTDOM=$("${AD}" resolver-lock test-domain)
m6r_valid_domain "${TESTDOM}" && case "${TESTDOM}" in *.invalid) true ;; *) false ;; esac || m6r_die "读不到测试域名（internal/resolverlock 的 TestDomain）"

# 写法的地址和端口（给 dig 用，看谁在替锁地址应答）。
ns_of() { sed -n 's/^nameserver //p' "${BD}/content.$1"; }
port_of() { sed -n 's/^port //p' "${BD}/content.$1"; }

# ---- 前提检查：同名文件已存在就停下来报告，不覆盖 ----
precheck() {
  local bad=0 d
  if [ -L "${M6R_RESOLVER_DIR}" ] || { [ -e "${M6R_RESOLVER_DIR}" ] && [ ! -d "${M6R_RESOLVER_DIR}" ]; }; then
    m6r_log "前提不满足：${M6R_RESOLVER_DIR} 不是普通目录"; bad=1
  fi
  for d in "${TESTDOM}" ${DOMAINS}; do
    if [ -e "${M6R_RESOLVER_DIR}/${d}" ] || [ -L "${M6R_RESOLVER_DIR}/${d}" ]; then
      m6r_log "前提不满足：${M6R_RESOLVER_DIR}/${d} 已经存在，不是这次写的，不覆盖。内容："
      sed 's/^/    /' "${M6R_RESOLVER_DIR}/${d}" 2>/dev/null | m6r_scrub >&2
      m6r_log "  如果是上一次运行留下的，先执行 sudo scripts/m6/resolver/restore.sh；如果是别的软件的，这台 Mac 不适合跑这个脚本。"
      bad=1
    fi
  done
  if [ -n "$(m6r_records 2>/dev/null)" ]; then
    m6r_log "前提不满足：状态目录里有上一次没清理的记录：$(m6r_records | tr '\n' ' ')。先执行 sudo scripts/m6/resolver/restore.sh"
    bad=1
  fi
  return $bad
}
if ! precheck; then
  if [ "$DRY_RUN" = 1 ]; then m6r_log "（空跑：前提不满足，继续打印后面的命令）"; else exit 3; fi
fi

# ---- 观察 ----
# 只做不带凭据的只读查询。每条查询的原始输出先经 raw_*（真机执行命令；自测时用 fake_* 造的样例，里面故意带地址），
# 再由 parse_* 只取判断要用的字段，写成“原始 …”行存进结果文件：接口名、哪几项代理开着、状态码、有没有应答、
# 解析出几个地址、地址属于哪一类（198.18/15、回环、其他）。不存具体地址。真机上判断错了，据这些行改解析代码。
# 每次 observe 之后，下面这些变量给出判断用的结果（自测时由 M6R_TEST_BASELINE、M6R_TEST_LOCK_LEAK 决定样例）。
OBS_LOCKED=0   # 这次观察时有没有锁
OBS_TUN=0      # 默认路由或半默认路由落在隧道接口（utun、tun、ppp、ipsec、wg）上，或解析、连接用到了 198.18/15 的假地址
OBS_SYSPROXY=0 # 系统代理开着（curl 和 Claude Code 不读它，浏览器、桌面版会读）
OBS_ENVPROXY=0 # 本脚本的环境里有代理变量（sudo 下一般没有）
OBS_RESOLVED=0 # 系统解析器给出了地址的名字数
OBS_DIG=0      # 直接问锁地址时有应答的名字数
OBS_HTTP=0     # curl 拿到 HTTP 应答（状态码不是 000）的个数：不带凭据，只说明“到得了”
OBS_ENVSIG=""  # 环境指纹（默认路由的接口 + 系统代理项），用来看用户是不是切换了代理
OBS_WHY=""     # 判断依据（写进结果文件）
TEST_BASE="${M6R_TEST_BASELINE:-unreachable}"
case "${TEST_BASE}" in unreachable | proxy | direct) ;; *) m6r_fatal "M6R_TEST_BASELINE 只能是 unreachable、proxy、direct" ;; esac
test_leak() { [ "${OBS_LOCKED}" = 0 ] || [ "${M6R_TEST_LOCK_LEAK:-0}" = 1 ]; } # 自测：这次“解析得出来”吗

# 地址分类（awk 函数，几个解析器共用）。
AWK_CLS='function cls(a) { if (a == "") return "无"; if (a ~ /^198\.1[89]\./) return "198.18/15"; if (a ~ /^127\./ || a == "::1") return "回环"; return "其他" }'

# ---- 自测用的样例输出（形状照真机的命令输出；故意带局域网地址、网关、代理地址，用来核对存档里不留地址）----
fake_proxy() {
  printf '<dictionary> {\n  ExceptionsList : <array> {\n    0 : 192.168.0.0/16\n  }\n'
  if [ "${TEST_BASE}" = proxy ]; then
    printf '  HTTPEnable : 1\n  HTTPPort : 7890\n  HTTPProxy : 127.0.0.1\n  HTTPSEnable : 1\n  HTTPSPort : 7890\n  HTTPSProxy : 127.0.0.1\n  SOCKSEnable : 0\n'
  else
    printf '  HTTPEnable : 0\n  HTTPSEnable : 0\n  SOCKSEnable : 0\n'
  fi
  printf '}\n'
}
fake_routes() {
  printf 'Routing tables\n\nInternet:\nDestination        Gateway            Flags               Netif Expire\n'
  if [ "${TEST_BASE}" = proxy ]; then
    printf '0/1                198.18.0.1         UGSc                utun9\n128.0/1            198.18.0.1         UGSc                utun9\n198.18/15          link#22            UCS                 utun9\n'
  fi
  printf 'default            192.168.1.1        UGScg                 en0\n127                127.0.0.1          UCS                   lo0\n192.168.1          link#11            UCS                   en0      !\n192.168.1.1/32     link#11            UCS                   en0      !\n'
}
fake_dns() {
  local d ns port n=2
  printf 'DNS configuration\n\nresolver #1\n  search domain[0] : corp.example\n  nameserver[0] : 192.168.1.1\n  if_index : 11 (en0)\n  flags    : Request A records\n  reach    : 0x00020002 (Reachable,Directly Reachable Address)\n'
  if [ "${OBS_LOCKED}" = 1 ]; then
    ns=$(ns_of "${OBS_FORM}"); port=$(port_of "${OBS_FORM}")
    for d in ${PHASE_DOMAINS}; do
      printf '\nresolver #%s\n  domain   : %s\n  nameserver[0] : %s\n' "$n" "$d" "$ns"
      [ -n "${port}" ] && printf '  port     : %s\n' "$port"
      printf '  timeout  : 1\n  flags    : Request A records, Request AAAA records\n  reach    : 0x00000003 (Reachable,Transient Connection)\n'
      n=$((n + 1))
    done
  fi
  printf '\nDNS configuration (for scoped queries)\n\nresolver #1\n  nameserver[0] : 192.168.1.1\n'
}
fake_lookup() { # $1 名字
  test_leak || return 0
  printf 'name: %s\n' "$1"
  if [ "${TEST_BASE}" = proxy ]; then printf 'ip_address: 198.18.0.7\n'; else printf 'ip_address: 160.79.104.10\nipv6_address: 2607:6bc0::10\n'; fi
}
fake_dig() { # $1 名字
  if [ "${M6R_TEST_LOCK_LEAK:-0}" = 1 ]; then
    printf ';; ->>HEADER<<- opcode: QUERY, status: NOERROR, id: 4242\n;; ANSWER SECTION:\n%s.\t\t60\tIN\tA\t160.79.104.10\n;; SERVER: 198.18.0.2#53(198.18.0.2)\n' "$1"
  else
    printf ';; communications error to 198.18.0.2#53: timed out\n;; no servers could be reached\n'
  fi
}
fake_curl() { # 输出“状态码 对端地址”，返回 curl 的退出码
  if [ "${TEST_BASE}" != unreachable ] && test_leak; then
    if [ "${TEST_BASE}" = proxy ]; then printf '403 198.18.0.7'; else printf '403 160.79.104.10'; fi
    return 0
  fi
  printf '000 '
  return 6
}

# ---- 原始输出（真机执行只读命令；自测用样例）----
raw_proxy() { if [ "${M6R_MODE}" = test ]; then fake_proxy; else scutil --proxy 2>/dev/null; fi; }
raw_routes() { if [ "${M6R_MODE}" = test ]; then fake_routes; else netstat -rn -f inet 2>/dev/null; fi; }
raw_dns() { if [ "${M6R_MODE}" = test ]; then fake_dns; else scutil --dns 2>/dev/null; fi; }
raw_lookup() { if [ "${M6R_MODE}" = test ]; then fake_lookup "$1"; else dscacheutil -q host -a name "$1" 2>/dev/null; fi; }
raw_dig() { # $1 锁地址 $2 端口（可空）$3 名字
  if [ "${M6R_MODE}" = test ]; then fake_dig "$3"; return; fi
  if [ -n "$2" ]; then dig +time=1 +tries=1 "@$1" -p "$2" "$3" A 2>&1; else dig +time=1 +tries=1 "@$1" "$3" A 2>&1; fi
}
raw_curl() { # $1 URL
  if [ "${M6R_MODE}" = test ]; then fake_curl; return; fi
  curl -sS -m 10 -o /dev/null -w '%{http_code} %{remote_ip}' "$1" 2>/dev/null
}

# ---- 只取判断要用的字段 ----
parse_proxy() { awk '$2 == ":" && $1 ~ /Enable$/ { printf "%s=%s ", $1, $3 }'; }
parse_routes() { # 目的地（只看默认、半默认、198.18/15 内）→ 接口（标志）；不存网关
  awk '$1 == "default" || $1 == "0/1" || $1 == "128.0/1" || $1 ~ /^198\.1[89]([.\/]|$)/ {
    d = $1; if (d ~ /^198/) d = "198.18/15内"
    printf "%s→%s(%s) ", d, $4, $3 }'
}
parse_dns() { # $1 要点名的域名（空格分隔）：每个解析器的域名（不在名单里的记“其他”）、地址类别、端口、可达性
  local keep
  keep=" $(printf '%s' "$1" | tr '\n\t' '  ') "
  awk -v keep="${keep}" "${AWK_CLS}"'
    /for scoped queries/ { exit }
    /^resolver #/ { if (r != "") print r; r = $1 $2; next }
    r == "" { next }
    $1 == "domain" { r = r " domain=" (index(keep, " " $3 " ") ? $3 : "其他") }
    $1 == "search" { r = r " search=有" }
    $1 ~ /^nameserver\[/ { r = r " ns=" cls($3) }
    $1 == "port" { r = r " port=" $3 }
    $1 == "reach" { s = $0; sub(/.*\(/, "", s); sub(/\).*/, "", s); r = r " reach=" s }
    END { if (r != "") print r }' | tr '\n' ';'
}
parse_lookup() { # 解析出几个地址、各属哪一类
  awk "${AWK_CLS}"'/_address:/ { n++; c = c cls($2) "," } END { if (n) printf "地址 %d 个（%s）", n, substr(c, 1, length(c) - 1); else printf "没有地址" }'
}
parse_dig() { # 状态、有没有应答、应答地址的类别
  awk "${AWK_CLS}"'
    /status:/ { s = $0; sub(/.*status: */, "", s); sub(/,.*/, "", s) }
    /timed out|no servers could be reached|connection refused/ { t = 1 }
    $4 == "A" && $3 == "IN" { n++; c = c cls($5) "," }
    END {
      if (n) printf "有应答 %d 条（%s），状态 %s", n, substr(c, 1, length(c) - 1), s
      else if (s != "") printf "无应答，状态 %s", s
      else if (t) printf "无应答（超时或连不上）"
      else printf "无应答"
    }'
}

env_summary() {
  local px rt
  if [ "$DRY_RUN" = 1 ]; then printf 'QUERY: scutil --proxy\nQUERY: netstat -rn -f inet\n'; return; fi
  px=$(raw_proxy | parse_proxy)
  ev "原始 scutil --proxy：${px:-（没有 Enable 项）}"
  px=$(printf '%s' "${px}" | tr ' ' '\n' | sed -n 's/=1$//p' | tr '\n' ' ')
  ev "环境：系统代理开着的项：${px:-无}"
  [ -n "${px}" ] && OBS_SYSPROXY=1
  rt=$(raw_routes | parse_routes)
  ev "原始 netstat -rn：${rt:-（没有默认路由）}"
  printf '%s' "${rt}" | grep -qE '(^|[ ])(default|0/1|128\.0/1)→(utun|tun|ppp|ipsec|wg)[0-9]+' && OBS_TUN=1
  OBS_ENVSIG="$(printf '%s' "${rt}" | tr ' ' '\n' | grep -E '^(default|0/1|128\.0/1)→' | sed 's/(.*//' | tr '\n' ' ')|${px}"
  px=$(env | awk -F= 'tolower($1) ~ /^(https?|all)_proxy$/ { printf "%s ", $1 }')
  [ -n "${px}" ] && OBS_ENVPROXY=1
  ev "环境：本脚本（sudo 下）的代理环境变量：${px:-无}（你自己终端里的变量 sudo 看不到，Claude Code 那一项以你实际观察为准）"
}
resolver_loaded() { # 系统有没有读到这一轮的锁文件（scutil --dns 里有没有这些域名的解析器）
  if [ "$DRY_RUN" = 1 ]; then printf 'QUERY: scutil --dns\n'; return; fi
  local all d
  all=$(raw_dns | parse_dns "${TESTDOM} ${DOMAINS}")
  ev "原始 scutil --dns：${all}"
  for d in "$@"; do
    case "${all}" in *"domain=${d} "*) ev "解析器 ${d}：已加载" ;; *) ev "解析器 ${d}：系统里没有" ;; esac
  done
}
lookup() { # $1 名字：系统解析器（dscacheutil，走 /etc/resolver）
  if [ "$DRY_RUN" = 1 ]; then printf 'QUERY: dscacheutil -q host -a name %s\n' "$1"; return; fi
  local raw r
  raw=$(raw_lookup "$1")
  r=$(printf '%s\n' "${raw}" | parse_lookup)
  ev "原始 dscacheutil $1：${r}"
  case "${r}" in 地址*) OBS_RESOLVED=$((OBS_RESOLVED + 1)) ;; esac
  case "${r}" in *198.18/15*) OBS_TUN=1 ;; esac
  return 0
}
ask_ns() { # $1 写法 $2 名字：直接问锁地址（看有没有别的程序替它应答，R18 的 TUN 劫持）
  local ns port r
  ns=$(ns_of "$1"); port=$(port_of "$1")
  if [ "$DRY_RUN" = 1 ]; then printf 'QUERY: dig +time=1 +tries=1 @%s %s%s A\n' "${ns}" "${port:+-p ${port} }" "$2"; return; fi
  r=$(raw_dig "${ns}" "${port}" "$2" | parse_dig)
  ev "原始 dig @锁地址（$1 写法） $2：${r}"
  case "${r}" in 有应答*) OBS_DIG=$((OBS_DIG + 1)) ;; esac
  return 0
}
probe_http() { # $1 URL：只记状态码、curl 的退出码（6 = 解析不了）和对端地址的类别；不记响应体，不带任何凭据
  if [ "$DRY_RUN" = 1 ]; then printf "QUERY: curl -sS -m 10 -o /dev/null -w '%%{http_code} %%{remote_ip}' %s\n" "$1"; return; fi
  local out rc code ipc
  out=$(raw_curl "$1"); rc=$?
  code=$(printf '%s' "${out}" | awk '{ print $1 }')
  ipc=$(printf '%s' "${out}" | awk "${AWK_CLS}"'{ print cls($2) }')
  ev "原始 curl $1：状态码 ${code:-000}，退出码 ${rc}$([ "$rc" = 6 ] && echo '（解析不了域名）')，对端地址类别 ${ipc:-无}"
  [ -n "${code}" ] && [ "${code}" != 000 ] && OBS_HTTP=$((OBS_HTTP + 1))
  [ "${ipc}" = 198.18/15 ] && OBS_TUN=1
  return 0
}
OBS_FORM=none
observe() { # $1 写法（none 表示没有锁）$2 域名……：只做不带凭据的几项（环境、系统解析器、dig、curl）
  local form=$1 d; shift
  OBS_TUN=0 OBS_SYSPROXY=0 OBS_ENVPROXY=0 OBS_RESOLVED=0 OBS_DIG=0 OBS_HTTP=0 OBS_ENVSIG=""
  OBS_FORM=${form}
  OBS_LOCKED=1; [ "${form}" = none ] && OBS_LOCKED=0
  env_summary
  [ "${form}" = none ] || resolver_loaded "$@"
  for d in "$@"; do
    lookup "$d"
    [ "${form}" = none ] || ask_ns "${form}" "$d"
  done
  case " $* " in *" anthropic.com "*) lookup api.anthropic.com ;; esac
  [ "$1" = "${TESTDOM}" ] || probe_http https://api.anthropic.com/
  probe_http https://claude.ai/
  OBS_WHY="curl 拿到应答 ${OBS_HTTP} 个；隧道接口或 198.18/15 的假地址：$([ "${OBS_TUN}" = 1 ] && echo 有 || echo 无)；系统代理：$([ "${OBS_SYSPROXY}" = 1 ] && echo 开着 || echo 无)；本脚本环境里的代理变量：$([ "${OBS_ENVPROXY}" = 1 ] && echo 有 || echo 无)"
}

# 出口判断：unreachable（到不了）、proxy（能到，经本机的代理或 TUN）、direct（能到，直连）。
# curl 不读系统代理，sudo 下也没有代理变量，所以 curl 到得了、又没有隧道的迹象，就是直连。判断不了一律按直连。
egress_now() {
  if [ "$DRY_RUN" = 1 ]; then echo direct; return; fi
  if [ "${OBS_HTTP}" -gt 0 ]; then
    if [ "${OBS_TUN}" = 1 ]; then echo proxy; else echo direct; fi
    return
  fi
  if [ "${OBS_LOCKED}" = 0 ]; then echo unreachable; return; fi
  # 有锁时 curl 到不了可能只是锁起了作用：看环境有没有变。
  if [ "${OBS_TUN}" = 1 ]; then echo proxy
  elif [ "${OBS_ENVSIG}" = "${BASE_ENVSIG}" ]; then echo "${BASE_EGRESS}"
  else echo direct; fi
}
PROXY_CAVEAT="脚本分不出这条隧道的出口在哪里；如果它的出口仍在国内（例如公司的 VPN），就等同直连"
egress_text() {
  case "$1" in
    unreachable) echo "到不了（不经任何代理时连不上 Anthropic）" ;;
    proxy) echo "能到，经本机的代理或 TUN（${PROXY_CAVEAT}）" ;;
    *) echo "能到，而且是直连（判断不了时也按直连处理）" ;;
  esac
}
# 锁对系统解析是否无效：系统解析器照常给出地址、锁地址有人应答、或 curl 连得上，任一项就算无效。
lock_ineffective() { [ "$DRY_RUN" != 1 ] && [ $((OBS_RESOLVED + OBS_DIG + OBS_HTTP)) -gt 0 ]; }

# 三种出口各要输入什么，才会请你打开已登录的 Claude：
#   到不了：不用额外确认；
#   经代理或 TUN：每个场景单独输入 exit-trusted（“我确认此刻的出口是我信任的”），不输入记“跳过：出口未确认”；
#   直连：单独输入 real-ip-ok（整个运行问一次），不输入记“跳过：直连可达”。
LOGIN_WORD=real-ip-ok
EXIT_WORD=exit-trusted
LOGIN_OK=0
LOGIN_ASKED=0
EXIT_ASKED=" "   # 问过 exit-trusted 的场景（写法/场景）
EXIT_OK=" "      # 确认过的场景
ask_login_direct() {
  [ "${LOGIN_OK}" = 1 ] && return 0
  [ "${LOGIN_ASKED}" = 1 ] && return 1
  LOGIN_ASKED=1
  if [ "$DRY_RUN" = 1 ]; then
    printf 'CONFIRM-LOGIN: 出口是直连时，输入 %s 才请你打开已登录的 Claude（整个运行问一次）；其他输入一律跳过这几项\n' "${LOGIN_WORD}"
    LOGIN_OK=1; return 0
  fi
  m6r_log "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"
  m6r_log "!! 此刻不经任何代理就能【直连】到 Anthropic。"
  m6r_log "!! 锁一旦无效，你按提示打开的 Claude Code、Claude 桌面版、浏览器里已登录的页面会真的连上去："
  m6r_log "!!"
  m6r_log "!!     【已登录的账号会从本机 IP 访问 Anthropic】"
  m6r_log "!!"
  m6r_log "!! 这正是本项目要避免的事。默认跳过这几项，只做不带凭据的观察（系统解析器、dig、curl）。"
  m6r_log "!! 确实要做，输入 ${LOGIN_WORD}（和前面的 yes 不同）；输入其他任何内容都跳过："
  m6r_log "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"
  m6r_read || REPLY=""
  if [ "${REPLY}" = "${LOGIN_WORD}" ]; then
    LOGIN_OK=1; ev "用户单独确认直连时也做已登录程序的观察：${LOGIN_WORD}"; return 0
  fi
  ev "用户没有单独确认：直连可达时跳过已登录程序的观察"
  return 1
}
ask_exit_trusted() { # $1 写法 $2 场景：出口经代理或 TUN 时，每个场景问一次
  local key="$1/$2"
  case "${EXIT_OK}" in *" ${key} "*) return 0 ;; esac
  case "${EXIT_ASKED}" in *" ${key} "*) return 1 ;; esac
  EXIT_ASKED="${EXIT_ASKED}${key} "
  if [ "$DRY_RUN" = 1 ]; then
    printf 'CONFIRM-EXIT: 出口经代理或 TUN 时（%s），这个场景输入 %s 才请你打开已登录的 Claude；其他输入跳过，记“跳过：出口未确认”\n' "${PROXY_CAVEAT}" "${EXIT_WORD}"
    EXIT_OK="${EXIT_OK}${key} "; return 0
  fi
  m6r_log "------------------------------------------------------------------"
  m6r_log "此刻能到 Anthropic，经本机的代理或 TUN（场景：$2）。"
  m6r_log "注意：${PROXY_CAVEAT}——锁无效时，已登录的账号会从那个出口访问 Anthropic。"
  m6r_log "你确认此刻的出口是你信任的，就输入 ${EXIT_WORD}（和 yes、${LOGIN_WORD}、open-anyway 都不同）；其他任何输入都跳过已登录程序的几项："
  m6r_log "------------------------------------------------------------------"
  m6r_read || REPLY=""
  if [ "${REPLY}" = "${EXIT_WORD}" ]; then
    EXIT_OK="${EXIT_OK}${key} "; ev "用户确认此刻的出口是信任的（写法=$1 场景=$2）：${EXIT_WORD}"; return 0
  fi
  ev "用户没有确认出口（写法=$1 场景=$2）"
  return 1
}
# 按出口要求对应的确认；不满足时返回 1，并把原因放进 GATE_REASON。
egress_gate() { # $1 出口 $2 写法 $3 场景
  GATE_REASON=""
  case "$1" in
    unreachable) return 0 ;;
    proxy) ask_exit_trusted "$2" "$3" && return 0; GATE_REASON="出口未确认"; return 1 ;;
    *) ask_login_direct && return 0; GATE_REASON="直连可达"; return 1 ;;
  esac
}

# 用户手动观察：y 能连上，n 连不上，s 跳过；后面可以跟一句备注（不要写个人信息）。
MANUAL_ITEMS="claude_code claude_desktop browser_default browser_doh"
manual_text() {
  case "$1" in
    claude_code) echo "Claude Code：在你平常用的终端里新开 claude，发一句话。能得到回复吗？" ;;
    claude_desktop) echo "Claude 桌面版：打开它（已经开着就发一句话）。能连上吗？" ;;
    browser_default) echo "浏览器（默认设置，建议用无痕窗口）打开 https://claude.ai/ 。能打开吗？（备注里写用的哪个浏览器）" ;;
    browser_doh) echo "浏览器开了“安全 DNS / 加密 DNS”后，用无痕窗口打开 https://claude.ai/ 。能打开吗？（不方便切换就输入 s）" ;;
  esac
}
skip_manual() { # $1 写法 $2 场景 $3 原因：不算不通过
  local it
  for it in ${MANUAL_ITEMS}; do ev "手动 写法=$1 场景=$2 ${it}=跳过：$3"; done
  m6r_log "已登录程序的几项跳过：$3"
}
# 已登录程序的几项（Claude Code、桌面版、浏览器里登录后的页面）：先按刚才不带凭据的观察判断能不能做。
manual() { # $1 写法 $2 场景
  local it ans note eg
  eg=$(egress_now)
  if [ "$1" != none ] && [ "$DRY_RUN" = 1 ]; then
    printf 'NOTE: 不带凭据的几项显示锁无效时，默认跳过下面的手动项（直连时不能做；经代理时先确认出口，再输入 open-anyway 才做）\n'
  elif [ "$1" != none ] && lock_ineffective; then
    m6r_log "锁对系统解析无效（系统解析出地址 ${OBS_RESOLVED} 个、锁地址有应答 ${OBS_DIG} 个、curl 连上 ${OBS_HTTP} 个），不建议再打开已登录的程序。"
    if [ "${eg}" = direct ]; then skip_manual "$1" "$2" "锁对系统解析无效，且直连可达"; return 0; fi
    if [ "${eg}" = proxy ] && ! ask_exit_trusted "$1" "$2"; then skip_manual "$1" "$2" "锁对系统解析无效，且出口未确认"; return 0; fi
    m6r_log "  此刻出口：$(egress_text "${eg}")。仍要做就输入 open-anyway；其他输入跳过："
    m6r_read || REPLY=""
    [ "${REPLY}" = open-anyway ] || { skip_manual "$1" "$2" "锁对系统解析无效"; return 0; }
    ev "用户在锁无效时仍要做已登录程序的观察（出口：${eg}）"
  fi
  if [ "$DRY_RUN" = 1 ]; then
    # 空跑不知道出口：两种确认都打印（命令核对要求请用户打开已登录 Claude 的提示都在它们之后）。
    ask_login_direct; ask_exit_trusted "$1" "$2"
  elif ! egress_gate "${eg}" "$1" "$2"; then
    skip_manual "$1" "$2" "${GATE_REASON}"; return 0
  fi
  for it in ${MANUAL_ITEMS}; do
    if [ "$DRY_RUN" = 1 ]; then wait_line "$(manual_text "$it")（y/n/s）"; continue; fi
    m6r_log "$(manual_text "$it")"
    m6r_log "  输入 y（能连上）、n（连不上）、s（跳过），后面可以空一格写备注（不要写个人信息），回车："
    m6r_read || REPLY=s
    ans=$(printf '%s' "${REPLY}" | cut -c1 | tr 'YNS' 'yns')
    note=$(printf '%s' "${REPLY}" | cut -c2- | sed 's/^ *//' | tr -d '\t' | cut -c1-200)
    case "${ans}" in y | n | s) ;; *) ans="s"; note="（输入不认识：${REPLY}）" ;; esac
    ev "手动 写法=$1 场景=$2 ${it}=${ans}${note:+ 备注：${note}}"
    [ "$1" = none ] || locks_intact || return 1
  done
  return 0
}

# 这一轮写的锁文件还在吗（死人开关到点会删掉）。
locks_intact() {
  [ "$DRY_RUN" = 1 ] && return 0
  local d
  for d in ${PHASE_DOMAINS}; do
    [ -f "${M6R_STATE}/lock.${d}" ] && [ -f "${M6R_RESOLVER_DIR}/${d}" ] || return 1
  done
  return 0
}

ROUND=0
PHASE_DOMAINS=""
DEADMAN_FIRED=0
# 一轮：启动死人开关 → 写入 → 观察 → 删除 → 取消死人开关。$1 写法 $2 是否要手动观察（1/0），其余是域名。
phase() {
  local form=$1 want_manual=$2 d scene="当前" fails=0; shift 2
  ROUND=$((ROUND + 1))
  PHASE_DOMAINS="$*"
  step "第 ${ROUND} 轮：写法 ${form}，锁 ${PHASE_DOMAINS}"
  ev "锁文件内容（${form}）：$(tr '\n' ';' <"${BD}/content.${form}")"
  m6r_state_init
  m6r_deadman_start "${DEADMAN_SECS}" "${RESTORE}" "${ROUND}"
  fail_at "after_deadman:${ROUND}"
  if [ "$DRY_RUN" = 1 ] || [ ! -d "${M6R_RESOLVER_DIR}" ]; then
    if [ "$DRY_RUN" = 1 ]; then
      printf 'NOTE: 下面这一条只在原来没有 %s 目录时执行，并记下 created_dir=1（还原时空了才删）\n' "${M6R_RESOLVER_DIR}"
      printf 'CHANGE: mkdir -m 755 %s\n' "${M6R_RESOLVER_DIR}"
    else
      printf '1\n' >"${M6R_STATE}/created_dir"
      mkdir -m 755 "${M6R_RESOLVER_DIR}" || m6r_die "建不了 ${M6R_RESOLVER_DIR}"
      m6r_log "已建目录 ${M6R_RESOLVER_DIR}（原来没有；还原时空了就删掉）"
    fi
  fi
  for d in "$@"; do
    m6r_write_lock "$d" "${BD}/content.${form}"
    fail_at "after_write:${ROUND}"
  done
  nap 3
  while :; do
    step "第 ${ROUND} 轮（${form}）观察：场景 ${scene}"
    observe "${form}" "$@"
    if [ "${want_manual}" = 1 ]; then
      if [ "$DRY_RUN" != 1 ]; then
        ev "判断 写法=${form} 场景=${scene}：锁对系统解析$(lock_ineffective && echo 无效 || echo 有效)（系统解析出地址 ${OBS_RESOLVED} 个，锁地址有应答 ${OBS_DIG} 个，curl 连上 ${OBS_HTTP} 个）；此刻出口：$(egress_text "$(egress_now)")；依据：${OBS_WHY}"
      fi
      manual "${form}" "${scene}" || { DEADMAN_FIRED=1; break; }
    fi
    locks_intact || { DEADMAN_FIRED=1; break; }
    [ "${want_manual}" = 1 ] || break
    if [ "$DRY_RUN" = 1 ]; then
      wait_line "要在别的代理状态下再观察一轮：你自己去开或关 Clash 的 TUN、系统代理等（脚本不替你开关），切好后输入场景名（如 tun、sysproxy、again）；直接回车结束这一种写法"
      break
    fi
    m6r_log "要在别的代理状态下再观察一轮吗？你自己去开或关 Clash 的 TUN、系统代理等（脚本不会替你开关），"
    m6r_log "  切好后在这里输入场景名（如 tun、sysproxy；想过一会儿再看一次就输入 again）回车；直接回车结束这一种写法："
    m6r_read || REPLY=""
    scene=$(printf '%s' "${REPLY}" | tr -cd 'A-Za-z0-9_-' | cut -c1-20)
    [ -n "${scene}" ] || break
  done
  if [ "${DEADMAN_FIRED}" = 1 ]; then
    m6r_log "这一轮的锁文件已经不在了（死人开关到点，或被别的程序删了）。不再继续后面的轮次。"
    ev "第 ${ROUND} 轮：锁文件在观察中途已经不在了（死人开关到点或被删），后面的轮次不做"
    return 1
  fi
  step "第 ${ROUND} 轮：删除锁文件"
  for d in "$@"; do m6r_remove_lock "$d" || fails=$((fails + 1)); done
  [ "${fails}" = 0 ] || m6r_die "有 ${fails} 个锁文件没删掉（见上面），交给 restore.sh 处理"
  m6r_deadman_cancel "${ROUND}"
  ev "第 ${ROUND} 轮：锁文件已删除"
  if [ "${want_manual}" = 1 ]; then
    nap 2
    step "第 ${ROUND} 轮（${form}）删锁之后"
    lookup claude.ai
    probe_http https://claude.ai/
  fi
  return 0
}

# ---- 开始 ----
ev "# 解析锁真机验证（R9、R18）证据 ${TS}"
ev "模式：${M6R_MODE}；macOS $(sw_vers -productVersion 2>/dev/null || echo '?')；死人开关每轮 ${DEADMAN_SECS} 秒"
ev "测试域名：${TESTDOM}；真实域名：$(printf '%s ' ${DOMAINS})"
ev "default 写法：$(tr '\n' ';' <"${BD}/content.default")；loopback 写法：$(tr '\n' ';' <"${BD}/content.loopback")"
m6r_log "这个脚本只写入、删除 ${M6R_RESOLVER_DIR} 下自己的锁文件，并做只读查询；不改网络设置，不会断网，不开关你的代理软件，不启动、不结束任何程序。"
m6r_log "每一轮写入前启动死人开关：${DEADMAN_SECS} 秒后自动删掉这一轮的锁文件。中途出事时手动恢复：sudo scripts/m6/resolver/restore.sh"
if [ "${M6R_MODE}" = real ]; then
  m6r_log "按回车开始（Ctrl+C 取消）。"
  m6r_read || exit 1
fi

step "无锁的基线（还没有写任何锁文件）：先判断不经任何代理能不能到 Anthropic"
observe none ${DOMAINS}
BASE_EGRESS=$(egress_now)
BASE_ENVSIG="${OBS_ENVSIG}"
if [ "$DRY_RUN" = 1 ]; then
  printf 'NOTE: 基线出口按上面两条 curl 和环境判断：到不了 / 经代理或 TUN / 直连（判断不了按直连）；空跑按直连打印后面的步骤\n'
else
  ev "基线出口：$(egress_text "${BASE_EGRESS}")；依据：${OBS_WHY}"
  m6r_log "基线出口：$(egress_text "${BASE_EGRESS}")"
fi

ab_arm_restore_trap "${RESTORE}"
trap 'rc=$?; cleanup_build; ab__on_exit "$rc"' EXIT
trap 'exit 129' HUP

phase default 0 "${TESTDOM}" || { M6R_RESTORED=0; exit 4; }
m6r_log "预演完成：测试域名的锁写入、系统加载、删除都正常（证据里“解析器 ${TESTDOM}”一行应是“已加载”）"

if [ "${TEST_ONLY}" = 0 ]; then
  CONFIRMED=0
  if [ "$DRY_RUN" = 1 ]; then
    printf 'CONFIRM: 输入 yes 才锁真实域名（%s）；提示里写明锁无效时已登录的 Claude 会真的连上去、此刻的出口\n' "$(printf '%s ' ${DOMAINS})"
    CONFIRMED=1
  else
    m6r_log "=================================================================="
    m6r_log "注意：下一步在 ${M6R_RESOLVER_DIR} 写入 $(printf '%s ' ${DOMAINS})的解析锁。两个方向的风险："
    m6r_log "1. 锁有效时：本机的 Claude（桌面版、Claude Code，可能包括正在运行的会话）可能连不上，"
    m6r_log "   直到脚本结束删掉锁文件（最迟 ${DEADMAN_SECS} 秒后由死人开关自动删掉）。不要在需要 Claude Code 会话的时候做。"
    m6r_log "2. 锁无效时：你按提示打开的 Claude（Claude Code、桌面版、浏览器里已登录的页面）会真的连上去。"
    m6r_log "   此刻它会从这里出去：$(egress_text "${BASE_EGRESS}")"
    if [ "${BASE_EGRESS}" = proxy ]; then
      m6r_log "   经代理或 TUN 时，每个场景请你打开已登录的 Claude 之前，还要单独输入 ${EXIT_WORD} 确认此刻的出口是你信任的。"
    fi
    if [ "${BASE_EGRESS}" = direct ]; then
      m6r_log "   !!!!!!!! 【直连】：锁无效时，已登录的账号会从本机 IP 访问 Anthropic !!!!!!!!"
      m6r_log "   所以已登录程序的观察默认跳过，只做不带凭据的几项；要做的话后面还要单独输入 ${LOGIN_WORD}。"
    fi
    m6r_log "每种写法都先做不带凭据的几项（系统解析器、dig、curl）；它们显示锁无效时，默认跳过已登录程序的观察。"
    m6r_log "输入 yes 继续；输入其他任何内容跳过真实域名这一段："
    m6r_log "=================================================================="
    m6r_read || REPLY=""
    [ "${REPLY}" = yes ] && CONFIRMED=1
  fi
  if [ "${CONFIRMED}" = 1 ]; then
    ev "用户确认锁真实域名：yes"
    step "无锁的基线：已登录程序的观察（对照用）"
    observe none ${DOMAINS}
    manual none baseline
    for form in default loopback; do
      # shellcheck disable=SC2086
      phase "${form}" 1 ${DOMAINS} || exit 4
    done
  else
    ev "用户没有确认，跳过真实域名"
    m6r_log "没有输入 yes，跳过真实域名"
  fi
fi

# ---- 收尾：restore.sh 再兜底一遍（这时应该已经没有要删的）----
M6R_RESTORED=1
m6r_log "收尾：执行 restore.sh"
rrc=0
if [ "$DRY_RUN" = 1 ]; then DRY_RUN=1 bash "${RESTORE}" || rrc=$?; else bash "${RESTORE}" || rrc=$?; fi
[ "${rrc}" = 0 ] || m6r_log "restore.sh 报告有没删掉的文件（退出码 ${rrc}），请按 README 的“应急”一节核对"
if [ -n "${OUT}" ]; then
  m6r_log "完成。请把这两个文件贴回：scripts/m6/resolver/out/$(basename "${EVID}")、scripts/m6/resolver/out/$(basename "${M6R_LOG}")"
fi
exit "${rrc}"
