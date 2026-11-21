#!/usr/bin/env bash
# 直连拨号模块的会话内真实拨号（m3-plan M3-4 验收 h）。以当前用户运行，不需要 sudo，不改宿主机网络：
#   1. 采一次只读的用户环境快照（同 vm-up：mihomo/clash 进程、1.1.1.1 的出口接口、系统代理设置）；
#   2. 以当前用户起 abroad-vm direct（socket 放在 scripts/dev/out/ 下）；
#   3. 经它按 IP、按域名各连一次公网 443（目标可配置），报告是否连上、本端地址属于哪块网卡；
#   4. 停掉服务，再采一次快照比对。
# 证明的是“走的是绑物理网卡的拨号”，不是目标可达；连不上时如实记录。证据里不写地址。
#
#   scripts/dev/direct-probe.sh [--ip 1.1.1.1] [--host one.one.one.one] [--port 443]
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
[ "$(id -u)" != 0 ] || { echo "不要用 root 运行" >&2; exit 2; }
IP=1.1.1.1 HOST=one.one.one.one PORT=443
while [ $# -gt 0 ]; do
  case "$1" in
    --ip) IP="$2"; shift 2 ;; --host) HOST="$2"; shift 2 ;; --port) PORT="$2"; shift 2 ;;
    *) echo "不认识的参数 $1" >&2; exit 2 ;;
  esac
done
OUT="${ROOT}/scripts/dev/out"
WORK="${OUT}/direct-probe"
mkdir -p "${WORK}"
LOG="${OUT}/direct-probe-$(date +%Y%m%d-%H%M%S).txt"
SOCK="${WORK}/d.sock"
PIDF="${WORK}/direct.pid"

env_snap() {
  echo "procs: $(ps -axo pid=,comm= | awk 'tolower($0) ~ /mihomo|clash/ {print $1}' | sort -n | tr '\n' ' ')"
  echo "route-1.1.1.1: $(route -n get 1.1.1.1 2>/dev/null | awk '/interface:/ {print $2}')"
  echo "proxy-sha: $(scutil --proxy | shasum -a 256 | cut -d' ' -f1)"
}
cleanup() {
  if [ -f "${PIDF}" ]; then kill "$(cat "${PIDF}")" 2>/dev/null || true; rm -f "${PIDF}"; fi
}
trap cleanup EXIT

{
  echo "# 直连拨号模块的会话内真实拨号，$(date '+%F %T')，macOS $(sw_vers -productVersion)"
  ENV0="$(env_snap)"
  echo "## 开始时的用户环境"; echo "${ENV0}"
  go build -o "${WORK}/abroad-vm" ./cmd/abroad-vm
  go build -o "${WORK}/abroad-dev" ./cmd/abroad-dev
  # 启用前记下的本机 DNS：这里取 scutil --dns 的第一个 nameserver（只读）。只记它属于哪一类，不写地址。
  DNS="$(scutil --dns | awk '/nameserver\[0\]/ {print $3; exit}')"
  case "${DNS}" in
    198.18.*|198.19.*) KIND="fake 段（本机有别的代理软件接管了 DNS，按设计直连域名请求会回 resolve_failed）" ;;
    127.*) KIND="回环（按设计直连域名请求会回 resolve_failed）" ;;
    10.*|192.168.*|172.1[6-9].*|172.2[0-9].*|172.3[01].*) KIND="私有网段" ;;
    "") KIND="没有"; DNS=192.0.2.1 ;;
    *) KIND="公网" ;;
  esac
  echo "## 本机 DNS：${KIND}"
  "${WORK}/abroad-vm" direct --sock "${SOCK}" --dns "${DNS}" >"${WORK}/direct.log" 2>&1 &
  echo $! >"${PIDF}"
  for _ in $(seq 1 50); do [ -S "${SOCK}" ] && break; sleep 0.1; done
  [ -S "${SOCK}" ] || { echo "直连拨号模块没有起来"; cat "${WORK}/direct.log"; exit 1; }
  echo "## socket 权限：$(stat -f '%Sp' "${SOCK}")"
  echo "## 按 IP 连 ${IP}:${PORT}"
  "${WORK}/abroad-dev" direct-probe --sock "${SOCK}" --ip "${IP}" --port "${PORT}" || true
  echo "## 按域名连 ${HOST}:${PORT}"
  "${WORK}/abroad-dev" direct-probe --sock "${SOCK}" --host "${HOST}" --port "${PORT}" || true
  echo "## 置顶域名（应回 pinned，不解析不拨号）"
  "${WORK}/abroad-dev" direct-probe --sock "${SOCK}" --host claude.ai --port 443 || true
  echo "## Anthropic 地址段（应回 pinned）"
  "${WORK}/abroad-dev" direct-probe --sock "${SOCK}" --ip 160.79.104.10 --port 443 || true
  IFACE="$(route -n get default 2>/dev/null | awk '/interface:/ {print $2}')"
  echo "## 对照：系统自带的 nc 绑定 ${IFACE} 连 ${IP}:${PORT}（与直连拨号模块同样用 IP_BOUND_IF；不改任何设置）"
  if nc -z -G 3 -b "${IFACE}" "${IP}" "${PORT}" 2>&1 | sed -E 's/[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/<地址>/g'; then echo "nc：连上"; else echo "nc：没连上"; fi
  echo "## 对照：不绑网卡的 nc（走系统路由，可能经别的代理软件的 TUN）"
  if nc -z -G 3 "${IP}" "${PORT}" >/dev/null 2>&1; then echo "nc：连上"; else echo "nc：没连上"; fi
  echo "## 当前物理网卡（只读）：$(route -n get default 2>/dev/null | awk '/interface:/ {print $2}')（默认路由的接口；有 TUN 接管时它是 utun，直连仍绑物理网卡）"
  cleanup
  sleep 0.3
  echo "## 停止后的计数（abroad-vm direct 的日志最后一行）"
  tail -1 "${WORK}/direct.log" | sed -E 's/[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/<地址>/g'
  [ ! -e "${SOCK}" ] && echo "socket 已删除" || echo "socket 没有删除"
  ENV1="$(env_snap)"
  echo "## 结束时的用户环境"; echo "${ENV1}"
  if [ "${ENV0}" = "${ENV1}" ]; then echo "用户环境没变"; else echo "用户环境变了"; fi
} 2>&1 | tee "${LOG}"
echo "证据：${LOG}"
