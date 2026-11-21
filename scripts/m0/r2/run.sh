#!/bin/bash
# M0 R2：pf 的 user 规则与"默认拦截所有接口"。说明见同目录 README.md。
#
# 用法：sudo scripts/m0/r2/run.sh [--anchor 250.abroadm0] [--deadman 360] [--dhcp] [--dry-run]
#
# 会做的系统改动（全部由 restore.sh 和死人开关撤销）：
#   建临时用户 _abroadm0；复制探测程序到 /Library/Application Support/claude-abroad-m0/；
#   往 pf 的 com.apple/ 下加载临时锚点并 pfctl -E。规则生效期间 Mac 断网（约 1–2 分钟）。
set -u
here="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../lib.sh
. "$here/../lib.sh"

ANCHOR_NAME=250.abroadm0
DEADMAN=360
DO_DHCP=0
while [ $# -gt 0 ]; do
  case "$1" in
    --anchor) ANCHOR_NAME=$2; shift 2 ;;
    --deadman) DEADMAN=$2; shift 2 ;;
    --dhcp) DO_DHCP=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    *) echo "未知参数 $1"; exit 2 ;;
  esac
done
ANCHOR="com.apple/$ANCHOR_NAME"
ORDER_A=com.apple/000.abroadm0order
ORDER_B=com.apple/999.abroadm0order
T_TCP=223.5.5.5:443
T_UDP=223.5.5.5:53
T_UDP2=223.6.6.6:53
T_V6=[2400:3200::1]:443

SRC_BIN="$here/out/m0-r2-probe"
BIN="$M0_INSTALL/m0-r2-probe"
OUT="$here/out"
STAMP=$(date +%Y%m%d-%H%M%S)
mkdir -p "$OUT"
M0_LOG="$OUT/r2-$STAMP.log"
TMP=$(mktemp -d)
cd /

m0_require_root
[ -x "$SRC_BIN" ] || m0_die "找不到 ${SRC_BIN}。先在仓库根目录执行：go build -o scripts/m0/r2/out/m0-r2-probe ./cmd/m0-r2-probe"

finish() {
  local rc=$?
  m0_log "脚本结束（退出码 ${rc}），执行还原"
  if [ "$DRY_RUN" != 1 ]; then
    /bin/bash "$here/restore.sh" 2>&1 | tee -a "$M0_LOG" >&2
  fi
  [ -n "${SUDO_USER:-}" ] && chown -R "$SUDO_USER" "$OUT" 2>/dev/null
  rm -rf "$TMP"
  m0_log "日志：$M0_LOG"
}

# ---------- 0. 清残留、预检 ----------
if [ "$DRY_RUN" != 1 ] && [ -d "$M0_STATE" ] && [ -n "$(ls -A "$M0_STATE" 2>/dev/null)" ]; then
  m0_log "发现上次残留的状态，先还原"
  /bin/bash "$here/restore.sh" 2>&1 | tee -a "$M0_LOG" >&2
fi
if dscl . -read "/Users/$M0_TEST_USER" >/dev/null 2>&1; then
  m0_die "测试用户 $M0_TEST_USER 已存在但没有状态记录，请手动确认后执行：sudo dscl . -delete /Users/$M0_TEST_USER"
fi

IFACE=$(m0_default_iface)
GW=$(m0_default_gw)
case "$IFACE" in en*) ;; *) m0_die "默认路由不在物理网卡上（${IFACE}）。请先关掉代理软件的 TUN / VPN" ;; esac
if netstat -rn -f inet | awk '$1=="0/1"||$1=="128.0/1"{f=1} END{exit !f}'; then
  if [ "$DRY_RUN" = 1 ]; then m0_log "[dry-run] 检测到接管路由（正式运行时会在这里停下，要求关掉 TUN）"
  else m0_die "检测到 0/1、128.0/1 接管路由（通常是 Clash/Mihomo TUN 或 VPN）。请先关掉再运行"; fi
fi
if scutil --proxy | grep -qE '(HTTP|HTTPS|SOCKS)Enable : 1'; then
  m0_log "提示：系统代理开着。本测试的探测程序不走系统代理，不影响结果，但建议关掉"
fi
asuser() { if [ "$(id -u)" = 0 ]; then sudo -u "$SUDO_USER" "$@"; else "$@"; fi; }
LAN=$(asuser "$SRC_BIN" lan -iface "$IFACE") || m0_die "取不到 $IFACE 的网段"
VMIFS=$(asuser "$SRC_BIN" vmifs)
m0_log "物理网卡 ${IFACE}，网关 ${GW}，网段 ${LAN}，虚拟机网桥 ${VMIFS:-无}，锚点 $ANCHOR"
[ "$DRY_RUN" = 1 ] || {
  echo "=== 环境 ==="
  sw_vers
  echo "应用防火墙：$(/usr/libexec/ApplicationFirewall/socketfilterfw --getglobalstate 2>&1)"
  echo "隐身模式：$(/usr/libexec/ApplicationFirewall/socketfilterfw --getstealthmode 2>&1)"
  echo "=== pf 状态 ==="
  pfctl -si 2>&1 | head -3
  echo "=== 主规则集 ==="
  pfctl -sr 2>&1
  echo "=== 所有锚点（递归）==="
  pfctl -vsA 2>&1
} >> "$M0_LOG" 2>&1
[ "$DRY_RUN" = 1 ] || for a in $(pfctl -vsA 2>/dev/null | awk '{print $1}' | grep '^com.apple'); do
  { echo "=== 锚点 $a 的规则 ==="; pfctl -a "$a" -sr 2>&1; } >> "$M0_LOG"
done

# ---------- 1. 记状态 → 死人开关 → 改系统 ----------
m0_state_init
[ "$DRY_RUN" = 1 ] || m0_state_set pf_status_before "$(pfctl -si 2>/dev/null | awk '/^Status:/{print $2}')"
m0_deadman_start "$DEADMAN" "$here/restore.sh"
trap finish EXIT
trap 'exit 130' INT TERM HUP

m0_user_create
m0_do mkdir -p "$M0_INSTALL" "$M0_STATE/sock"
m0_do install -o root -g wheel -m 755 "$SRC_BIN" "$BIN"
m0_do chmod 1777 "$M0_STATE/sock"

# ---------- 用例 ----------
# 以某个身份运行：root / user（当前登录用户）/ test（_abroadm0）
as() {
  local who=$1; shift
  case "$who" in
    root) "$@" ;;
    user) sudo -u "$SUDO_USER" "$@" ;;
    test) sudo -u "$M0_TEST_USER" "$@" ;;
  esac
}

PHASE=?
case_begin() { pfctl -a "$ANCHOR" -sl > "$TMP/before" 2>/dev/null || :; }
case_end() {
  local name=$1 who=$2 res=$3 diff
  pfctl -a "$ANCHOR" -sl > "$TMP/after" 2>/dev/null || :
  diff=$("$BIN" labeldiff "$TMP/before" "$TMP/after" 2>/dev/null || echo "?")
  printf 'CASE phase=%-8s %-30s who=%-4s | %s | pf=%s\n' "$PHASE" "$name" "$who" "$res" "$diff" | tee -a "$M0_LOG"
}

run_case() {
  local name=$1 who=$2; shift 2
  [ "$DRY_RUN" = 1 ] && { echo "[dry-run] case $name as $who: $*"; return; }
  case_begin
  local res; res=$(as "$who" "$@" 2>&1 | grep RESULT | tail -1)
  case_end "$name" "$who" "${res:-RESULT ok=false kind=error detail=没有输出}"
}

run_ping() {
  local name=$1 who=$2 target=$3
  [ "$DRY_RUN" = 1 ] && { echo "[dry-run] ping $target as $who"; return; }
  case_begin
  local res
  if as "$who" ping -c1 -t2 "$target" >/dev/null 2>&1; then res="RESULT ok=true kind=ok detail=ping 通"
  else res="RESULT ok=false kind=timeout detail=ping 不通"; fi
  case_end "$name" "$who" "$res"
}

# fd 传递：recv 身份监听，send 身份建连接后把 fd 传过去，由 recv 身份收发。
run_fd() {
  local name=$1 recv=$2 send=$3 netw=$4 target=$5 presend=${6:-}
  [ "$DRY_RUN" = 1 ] && { echo "[dry-run] fd $name recv=$recv send=$send $netw $target $presend"; return; }
  local sock="$M0_STATE/sock/$name.sock"
  case_begin
  as "$recv" "$BIN" fdrecv -sock "$sock" -wait 10s > "$TMP/recv.out" 2>&1 &
  local rpid=$!
  local i; for i in 1 2 3 4 5 6 7 8 9 10; do [ -S "$sock" ] && break; sleep 0.2; done
  local res
  res=$(as "$send" "$BIN" fdsend -sock "$sock" -net "$netw" -to "$target" -iface "$IFACE" $presend 2>&1 | grep RESULT | tail -1)
  wait "$rpid" 2>/dev/null
  [ -n "$res" ] || res=$(grep RESULT "$TMP/recv.out" | tail -1)
  case_end "$name" "recv=$recv,send=$send" "${res:-RESULT ok=false kind=error detail=$(tr '\n' ' ' < "$TMP/recv.out")}"
}

suite() {
  PHASE=$1
  m0_log "---- 用例组：$PHASE ----"
  run_case tcp_test_bound           test "$BIN" dial -net tcp -to "$T_TCP" -iface "$IFACE"
  run_case udp_test_bound           test "$BIN" dial -net udp -to "$T_UDP" -iface "$IFACE"
  run_case udpsendto_test_bound     test "$BIN" dial -net udp-sendto -to "$T_UDP,$T_UDP2" -iface "$IFACE"
  run_case tcp_test_unbound         test "$BIN" dial -net tcp -to "$T_TCP"
  run_case tcp_user                 user "$BIN" dial -net tcp -to "$T_TCP"
  run_case udp_user                 user "$BIN" dial -net udp -to "$T_UDP"
  run_ping icmp_user                user 223.5.5.5
  run_case tcp_root_bound           root "$BIN" dial -net tcp -to "$T_TCP" -iface "$IFACE"
  run_case udp_root_bound           root "$BIN" dial -net udp -to "$T_UDP" -iface "$IFACE"
  run_fd   fd_tcp_test2root         root test tcp "$T_TCP"
  run_fd   fd_udp_test2root_nopre   root test udp "$T_UDP"
  run_fd   fd_udp_test2root_presend root test udp "$T_UDP" -presend
  run_fd   fd_udp_root2test_nopre   test root udp "$T_UDP"
  run_fd   fd_udp_root2test_presend test root udp "$T_UDP" -presend
  [ -n "$GW" ] && run_ping lan_gateway_ping user "$GW"
  [ -n "$GW" ] && run_case lan_gateway_tcp80 user "$BIN" dial -net tcp -to "$GW:80" -sni -
  run_case loop_v4                  user "$BIN" listen -addr 127.0.0.1:0
  run_case loop_v6                  user "$BIN" listen -addr "[::1]:0"
  run_case tcp6_user                user "$BIN" dial -net tcp6 -to "$T_V6" -sni -
}

# ---------- 2. 基线（无规则）----------
suite baseline

# ---------- 3. 加载 6.1 规则 ----------
asuser "$SRC_BIN" rules -user "$M0_TEST_USER" -lan "$LAN,224.0.0.0/4,169.254.0.0/16" -vmif "$VMIFS" > "$TMP/rules.pf" \
  || m0_die "生成规则失败"
cp "$TMP/rules.pf" "$OUT/r2-$STAMP-rules.pf"
[ "$DRY_RUN" = 1 ] && { echo "[dry-run] 将加载的规则："; cat "$TMP/rules.pf"; }
m0_pf_enable
m0_log "加载规则到 ${ANCHOR}，开始断网"
m0_pf_load "$ANCHOR" "$TMP/rules.pf" || m0_die "加载规则失败"
{ echo "=== 加载后 $ANCHOR ==="; pfctl -a "$ANCHOR" -vvsr 2>&1; } >> "$M0_LOG"

suite rules

if [ "$DO_DHCP" = 1 ]; then
  if ipconfig getoption "$IFACE" server_identifier >/dev/null 2>&1; then
    PHASE=rules
    case_begin
    m0_log "触发 DHCP 续租：ipconfig set $IFACE DHCP"
    m0_do ipconfig set "$IFACE" DHCP
    sleep 8
    addr=$(ipconfig getifaddr "$IFACE" || true)
    case_end dhcp_renew root "RESULT ok=$([ -n "$addr" ] && echo true || echo false) kind=info detail=续租后地址$([ -n "$addr" ] && echo 存在 || echo 丢失)"
  else
    m0_log "$IFACE 不是 DHCP 获取地址，跳过 DHCP 用例"
  fi
fi

{ echo "=== 用例结束后 $ANCHOR 计数 ==="; pfctl -a "$ANCHOR" -vvsr 2>&1; } >> "$M0_LOG"
m0_do pfctl -a "$ANCHOR" -F all >/dev/null 2>&1
m0_log "规则已撤销，网络应已恢复"
m0_check_network

# ---------- 4. 锚点顺序 ----------
# A 排在所有 com.apple 子锚点最前，B 排在最后。两次交换 pass/block，
# 看是否"按名字排序、第一条命中的 quick 生效"。
PHASE=order
order_round() {
  local first=$1 second=$2 label=$3
  printf 'pass out quick proto tcp to %s port 443 label "m0_order_pass"\n' "${T_TCP%:*}" > "$TMP/pass.pf"
  printf 'block drop out quick proto tcp to %s port 443 label "m0_order_block"\n' "${T_TCP%:*}" > "$TMP/block.pf"
  m0_pf_load "$ORDER_A" "$TMP/$first.pf"
  m0_pf_load "$ORDER_B" "$TMP/$second.pf"
  [ "$DRY_RUN" = 1 ] && return
  local res; res=$(as user "$BIN" dial -net tcp -to "$T_TCP" -sni - | grep RESULT)
  local ca cb
  ca=$(pfctl -a "$ORDER_A" -sl 2>/dev/null | awk '{s+=$3} END{print s+0}')
  cb=$(pfctl -a "$ORDER_B" -sl 2>/dev/null | awk '{s+=$3} END{print s+0}')
  printf 'CASE phase=order    %-30s who=user | %s | A(000)=%s包 B(999)=%s包\n' "$label" "$res" "$ca" "$cb" | tee -a "$M0_LOG"
  pfctl -a "$ORDER_A" -F all >/dev/null 2>&1
  pfctl -a "$ORDER_B" -F all >/dev/null 2>&1
}
order_round pass block order_A_pass_B_block
order_round block pass order_A_block_B_pass

m0_log "全部用例完成"
exit 0
