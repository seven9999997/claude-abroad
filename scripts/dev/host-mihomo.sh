#!/usr/bin/env bash
# 开发用：在本机单独起一个 mihomo（M2-3，设计 4.5），只听 127.0.0.1 上的 mixed-port，唯一出站是开发上游。
# 用来实测“本机代理软件当主代理时，UDP 中继经 192.168.127.254 能不能用”。
#
# 用法：scripts/dev/host-mihomo.sh start <开发上游端口>   # 成功时打印 "listening 127.0.0.1:<端口>"
#       scripts/dev/host-mihomo.sh stop
#       scripts/dev/host-mihomo.sh status
# 环境变量：HOST_MIHOMO_PORT（默认 47893）、HOST_MIHOMO_MANIFEST（默认 scripts/dev/host-mihomo.json）。
#
# 约束（architect 2026-11-18）：
#   - 以当前用户运行，root 时拒绝；不改本机网络；不开 TUN、DNS、external-controller；
#   - 只按本脚本写的 PID 文件停进程，停之前核对它的可执行文件就是本脚本下载的那一个；
#     绝不按进程名匹配（用户的 Clash 内核也叫这个名字，按名字结束等于让本机断网）；
#   - 只写仓库里的 scripts/dev/out/host-mihomo/，不读写用户主目录下的 mihomo 配置。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
umask 077

DIR="${ROOT}/scripts/dev/out/host-mihomo"
BINF="${DIR}/mihomo"
GZ="${DIR}/mihomo.gz"
PIDF="${DIR}/mihomo.pid"
CONF="${DIR}/config.yaml"
LOGF="${DIR}/mihomo.log"
PORT="${HOST_MIHOMO_PORT:-47893}"
MANIFEST="${HOST_MIHOMO_MANIFEST:-${ROOT}/scripts/dev/host-mihomo.json}"

die() { echo "host-mihomo：$*" >&2; exit 1; }
usage() { die "用法：$0 start <开发上游端口> | stop | status"; }

[[ "$(id -u)" != "0" ]] || die "不要以 root 运行"
[[ $# -ge 1 ]] || usage
CMD="$1"
shift
case "${CMD}" in
  start) [[ $# -eq 1 && "$1" =~ ^[0-9]{1,5}$ ]] || usage ;;
  stop | status) [[ $# -eq 0 ]] || usage ;;
  *) usage ;;
esac
[[ "${PORT}" =~ ^[0-9]{4,5}$ && ${PORT} -ge 1024 && ${PORT} -le 65535 ]] || die "HOST_MIHOMO_PORT 不合法"

mf() { python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))[sys.argv[2]])' "${MANIFEST}" "$1"; }
sha_of() { shasum -a 256 "$1" | cut -d' ' -f1; }

# our_pid：PID 文件里的进程还活着，且可执行文件就是本脚本下载的那个；是则回显 PID。
our_pid() {
  [[ -f "${PIDF}" ]] || return 1
  local pid exe
  pid="$(cat "${PIDF}")"
  [[ "${pid}" =~ ^[0-9]+$ ]] || return 1
  kill -0 "${pid}" 2>/dev/null || return 1
  exe="$(ps -p "${pid}" -o comm= 2>/dev/null || true)"
  [[ "${exe}" == "${BINF}" ]] || return 1
  echo "${pid}"
}

# fetch：取得经 sha256 校验的二进制。缓存的 .gz 校验不过就删掉重下；重下仍不过就删掉 .gz 和二进制并退出。
fetch() {
  mkdir -p "${DIR}"
  local want url
  want="$(mf sha256)"
  url="$(mf url)"
  [[ "${want}" =~ ^[0-9a-f]{64}$ && "${url}" == https://github.com/MetaCubeX/mihomo/releases/download/* ]] || die "清单不合法"
  if [[ -f "${GZ}" && "$(sha_of "${GZ}")" != "${want}" ]]; then
    rm -f "${GZ}" "${BINF}"
  fi
  if [[ ! -f "${GZ}" ]]; then
    curl -fsSL --max-time 300 -o "${GZ}.part" "${url}" || { rm -f "${GZ}.part"; die "下载失败"; }
    mv "${GZ}.part" "${GZ}"
  fi
  if [[ "$(sha_of "${GZ}")" != "${want}" ]]; then
    rm -f "${GZ}" "${BINF}"
    die "sha256 不符，已删除下载的文件"
  fi
  # 每次都从校验过的 .gz 解出，二进制不单独信任。
  gunzip -c "${GZ}" >"${BINF}.part"
  chmod 700 "${BINF}.part"
  mv "${BINF}.part" "${BINF}"
}

port_free() {
  python3 -I -c 'import socket,sys
p=int(sys.argv[1])
for t in (socket.SOCK_STREAM, socket.SOCK_DGRAM):
    s=socket.socket(socket.AF_INET, t)
    try: s.bind(("127.0.0.1", p))
    except OSError: sys.exit(1)
    finally: s.close()' "$1"
}

write_config() {
  local up=$1
  cat >"${CONF}" <<YAML
# 由 scripts/dev/host-mihomo.sh 生成（开发用，M2-3）。
mixed-port: ${PORT}
bind-address: 127.0.0.1
allow-lan: false
mode: rule
log-level: info
ipv6: false
unified-delay: false
geodata-mode: false
geo-auto-update: false
profile:
  store-selected: false
  store-fake-ip: false
tun:
  enable: false
dns:
  enable: false
proxies:
  - {name: up, type: socks5, server: 127.0.0.1, port: ${up}, udp: true}
rules:
  - MATCH,up
YAML
}

# check_config：自查生成的配置（验收 b），结果打印出来记入证据。
check_config() {
  python3 -I - "${CONF}" <<'PY'
import re, sys
t = open(sys.argv[1]).read()
body = "\n".join(l for l in t.splitlines() if not l.lstrip().startswith("#"))
checks = {
    "tun 关闭": re.search(r"^tun:\n  enable: false$", body, re.M) is not None,
    "bind-address 是 127.0.0.1": re.search(r"^bind-address: 127\.0\.0\.1$", body, re.M) is not None,
    "allow-lan 关闭": re.search(r"^allow-lan: false$", body, re.M) is not None,
    "没有 external-controller": "external-controller" not in body,
    "DNS 不监听": re.search(r"^dns:\n  enable: false$", body, re.M) is not None and "listen" not in body,
    "代理只有一个": len(re.findall(r"^  - \{name:", body.split("proxies:")[1].split("rules:")[0], re.M)) == 1,
    "规则只有 MATCH,up": body.split("rules:")[1].split() == ["-", "MATCH,up"],
    "全文没有 DIRECT": "DIRECT" not in t,
    "没有 geo 自动更新": re.search(r"^geo-auto-update: false$", body, re.M) is not None,
}
bad = [k for k, v in checks.items() if not v]
for k, v in checks.items():
    print(("✓ " if v else "✗ ") + "配置自查：" + k)
sys.exit(1 if bad else 0)
PY
}

listening() { lsof -nP -a -p "$1" -i 2>/dev/null | awk 'NR>1 {print $8, $9}' | sort -u; }

case "${CMD}" in
  start)
    UP="$1"
    if pid="$(our_pid)"; then die "已经在运行（pid ${pid}），先 stop"; fi
    rm -f "${PIDF}"
    port_free "${PORT}" || die "端口 127.0.0.1:${PORT} 已被占用（不换端口硬上）"
    fetch
    write_config "${UP}"
    check_config || die "生成的配置自查不过"
    : >"${LOGF}"
    "${BINF}" -d "${DIR}" -f "${CONF}" >>"${LOGF}" 2>&1 &
    PID=$!
    echo "${PID}" >"${PIDF}"
    for _ in $(seq 1 50); do
      if ! kill -0 "${PID}" 2>/dev/null; then rm -f "${PIDF}"; tail -n 20 "${LOGF}" >&2; die "mihomo 启动后退出了"; fi
      if lsof -nP -a -p "${PID}" -iTCP:"${PORT}" -sTCP:LISTEN >/dev/null 2>&1; then break; fi
      sleep 0.1
    done
    lsof -nP -a -p "${PID}" -iTCP:"${PORT}" -sTCP:LISTEN >/dev/null 2>&1 || { "$0" stop >/dev/null 2>&1 || true; die "5 秒内没有开始监听"; }
    echo "listening 127.0.0.1:${PORT}"
    ;;
  stop)
    if [[ ! -f "${PIDF}" ]]; then echo "没有在运行"; exit 0; fi
    if ! pid="$(our_pid)"; then
      # PID 文件过期，或那个 PID 已经不是我们的程序：只删 PID 文件，不碰任何进程。
      rm -f "${PIDF}"
      echo "PID 文件已过期，已删除（没有结束任何进程）"
      exit 0
    fi
    kill -TERM "${pid}" 2>/dev/null || true
    for _ in $(seq 1 50); do kill -0 "${pid}" 2>/dev/null || break; sleep 0.1; done
    if kill -0 "${pid}" 2>/dev/null && [[ "$(ps -p "${pid}" -o comm= 2>/dev/null || true)" == "${BINF}" ]]; then
      kill -KILL "${pid}" 2>/dev/null || true
    fi
    rm -f "${PIDF}"
    echo "已停止（pid ${pid}）"
    ;;
  status)
    if pid="$(our_pid)"; then
      echo "running pid ${pid}"
      listening "${pid}"
    else
      echo "not running"
      exit 1
    fi
    ;;
esac
