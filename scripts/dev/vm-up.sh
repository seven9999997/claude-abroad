#!/usr/bin/env bash
# 阶段 A：构建镜像和 abroad-vm，启动虚拟机，验证 abroad-agent 的控制面可达，然后关掉。
# 设计文档 14（M1 验收），docs/m1-plan.md M1-3。
#
# 约束：以当前用户运行（不用 sudo）；不改本机网络设置（虚拟机的网卡接 abroad-vm 进程内的用户态网络，
# 出站只到公网 IPv4 并绑定物理网卡；4f 会向公共 DNS 查 example.com）；只写仓库内的 scripts/dev/out/ 和 guest/out/；
# 只按自己记下的 pid 结束自己的进程；退出时（包括失败、Ctrl-C）一定停掉虚拟机。
#
# 用法：scripts/dev/vm-up.sh            # 常规轮 + panic 探测轮
#       scripts/dev/vm-up.sh --no-panic # 只跑常规轮
#       scripts/dev/vm-up.sh --precheck-only # 只跑前置检查（绑物理网卡的连接），不起虚拟机
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
umask 077

OUT="scripts/dev/out"
BIN="${OUT}/bin"
TS="$(date +%Y%m%d-%H%M%S)"
EVID="${OUT}/stage-a-${TS}.txt"
PANIC=1
[[ "${1:-}" == "--no-panic" ]] && PANIC=0

mkdir -p "${BIN}"
: >"${EVID}"

log() { echo "[$(date +%H:%M:%S)] $*" | tee -a "${EVID}"; }
section() { printf '\n==== %s ====\n' "$*" | tee -a "${EVID}"; }
fail() {
  log "✗ 失败：$*"
  exit 1
}
ok() { log "✓ $*"; }

# 带超时运行（macOS 没有 timeout 命令；perl 的 alarm 到点发 SIGALRM 终止命令）。
with_timeout() {
  local secs="$1"
  shift
  perl -e 'alarm shift; exec @ARGV or die "exec: $!"' "${secs}" "$@"
}

now_ms() { perl -MTime::HiRes=time -e 'printf "%d\n", time*1000'; }

VM_PID=""
VM_LOG=""
VM_STATE=""

# 只结束自己启动的 abroad-vm：核对 pid 对应的程序名，先 TERM，15 秒后 KILL。
stop_vm() {
  [[ -n "${VM_PID}" ]] || return 0
  if kill -0 "${VM_PID}" 2>/dev/null; then
    local name
    name="$(ps -p "${VM_PID}" -o comm= 2>/dev/null || true)"
    if [[ "${name}" == *abroad-vm || "${name}" == *abroad-vm-dev ]]; then
      kill -TERM "${VM_PID}" 2>/dev/null || true
      for _ in $(seq 1 15); do
        kill -0 "${VM_PID}" 2>/dev/null || break
        sleep 1
      done
      if kill -0 "${VM_PID}" 2>/dev/null; then
        log "abroad-vm（pid ${VM_PID}）15 秒内没有退出，发 SIGKILL"
        kill -KILL "${VM_PID}" 2>/dev/null || true
      fi
    fi
  fi
  wait "${VM_PID}" 2>/dev/null || true
  VM_PID=""
}

VM_RUNS=0
LISTENER_PIDS=()
# 本机临时监听器（只绑 127.0.0.1）：start_listener <名字>，端口写到 ${OUT}/lst-<名字>.port，
# 收到的连接数写到 ${OUT}/lst-<名字>.count。脚本退出时按记下的 pid 结束。
start_listener() {
  local name=$1
  rm -f "${OUT}/lst-${name}.port" "${OUT}/lst-${name}.count"
  echo 0 >"${OUT}/lst-${name}.count"
  python3 -I -c 'import socket,sys
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.bind(("127.0.0.1", 0)); s.listen(16)
open(sys.argv[1], "w").write(str(s.getsockname()[1]))
n=0
while True:
    c,_=s.accept(); n+=1; open(sys.argv[2], "w").write(str(n)); c.close()' "${OUT}/lst-${name}.port" "${OUT}/lst-${name}.count" &
  LISTENER_PIDS+=($!)
  for _ in $(seq 1 40); do [[ -s "${OUT}/lst-${name}.port" ]] && break; sleep 0.1; done
  [[ -s "${OUT}/lst-${name}.port" ]] || fail "本机临时监听器 ${name} 没有起来"
}
lst_port() { cat "${OUT}/lst-$1.port"; }
lst_count() { cat "${OUT}/lst-$1.count"; }
stop_listeners() {
  local p
  for p in "${LISTENER_PIDS[@]:-}"; do
    if [[ -n "${p}" ]]; then
      kill "${p}" 2>/dev/null || true
      wait "${p}" 2>/dev/null || true # 被结束的进程退出码非 0，不能让 set -e 退出脚本
    fi
  done
  LISTENER_PIDS=()
  return 0
}

HMP="${HOST_MIHOMO_PORT:-47893}" # 阶段 D 本机独立 mihomo 的端口（scripts/dev/host-mihomo.sh）
cleanup() {
  local rc=$?
  stop_vm
  stop_listeners
  scripts/dev/host-mihomo.sh stop >/dev/null 2>&1 || true # 只按它自己的 PID 文件停，不按名字
  if [[ -n "${VM_LOG}" && -f "${VM_LOG}" ]]; then
    section "abroad-vm 日志（${VM_LOG}）"
    tail -n 80 "${VM_LOG}" >>"${EVID}" || true
  fi
  if [[ ${rc} -ne 0 ]]; then
    echo "阶段 A 未通过，证据：${EVID}" >&2
  fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# 用户环境快照（M2-3 验收 c）：除本脚本启动的之外，名字含 mihomo 或 clash 的进程 PID 集合、
# route -n get 1.1.1.1 的接口、scutil --proxy 的输出。只读查询；开始和结束各采一次比较。
env_snap() {
  local ours=""
  [[ -f scripts/dev/out/host-mihomo/mihomo.pid ]] && ours="$(cat scripts/dev/out/host-mihomo/mihomo.pid)"
  echo "procs: $(ps -axo pid=,comm= | awk -v o="${ours}" 'tolower($0) ~ /mihomo|clash/ && $1 != o {print $1}' | sort -n | tr '\n' ' ')"
  echo "route-1.1.1.1: $(route -n get 1.1.1.1 2>/dev/null | awk '/interface:/ {print $2}')"
  echo "proxy-sha: $(scutil --proxy | shasum -a 256 | cut -d' ' -f1)"
}
ENV0="$(env_snap)"

# 前置检查（architect 审 M3-4；2026-11-22 判 W-2a 后改成局域网目标）：本机若拒绝“绑物理网卡”的连接，
# 虚拟机出站和直连都会以难懂的方式失败。用系统自带的 nc 绑定物理网卡试连（只是连接尝试，不改设置）：
#   - 局域网目标（必须通过）：VMUP_LAN_TARGET=<IPv4>:<端口>，不给时用默认网关上第一个能连上的 TCP 端口（不用 443）；
#   - 公网 1.1.1.1:443（只记录）：连不上时 PUBLIC_OK=0，后面要经物理网卡到公网的几项按“被本机环境挡住”记录，不算失败。
# 证据里不写局域网目标的地址，只写它是怎么选出来的。
phys_iface() {
  local ifc
  ifc="$(route -n get default 2>/dev/null | awk '/interface:/ {print $2}')"
  case "${ifc}" in utun*|"") ifc="$(for i in en0 en1 en2 en3; do ipconfig getifaddr "$i" >/dev/null 2>&1 && { echo "$i"; break; }; done)" ;; esac
  echo "${ifc}"
}
bound_ok() { nc -z -G 3 -b "$1" "$2" "$3" >/dev/null 2>&1; } # bound_ok <网卡> <地址> <端口>
lan_target() { # 输出 “地址:端口 来源”；找不到时输出空
  local ifc=$1 gw p
  if [[ -n "${VMUP_LAN_TARGET:-}" ]]; then echo "${VMUP_LAN_TARGET} 环境变量 VMUP_LAN_TARGET"; return; fi
  gw="$(route -n get -ifscope "${ifc}" default 2>/dev/null | awk '/gateway:/ {print $2}')"
  [[ "${gw}" =~ ^(10|172|192)\. ]] || return 0
  for p in 80 53 22 8080 8000 8443 23 5000; do
    if bound_ok "${ifc}" "${gw}" "${p}"; then echo "${gw}:${p} 默认网关的 TCP ${p} 端口"; return; fi
  done
}
section "前置检查：绑物理网卡的连接"
IFC="$(phys_iface)"
LT="$(lan_target "${IFC}")"
LAN_TARGET="${LT%% *}"
if [[ -z "${LAN_TARGET}" ]]; then
  log "找不到可用的局域网目标：默认网关上 80、53、22、8080、8000、8443、23、5000 都连不上。请用 VMUP_LAN_TARGET=<局域网 IPv4>:<端口（不是 443）> 指定一个能连上的。"
  fail "前置检查：没有局域网目标"
fi
[[ "${LAN_TARGET##*:}" != 443 ]] || fail "前置检查：局域网目标的端口不能是 443（9c 的直连要先发非 TLS 的字节）"
if bound_ok "${IFC}" "${LAN_TARGET%:*}" "${LAN_TARGET##*:}"; then
  ok "绑物理网卡（${IFC}）连局域网目标正常（目标：${LT#* }）"
else
  log "本机环境拒绝绑物理网卡（${IFC}）连局域网目标（${LT#* }）。（只读检查，没有做任何改动）"
  [[ "${VMUP_IGNORE_BOUND_CHECK:-}" == 1 ]] || fail "前置检查：本机环境拒绝绑物理网卡的连接"
fi
if bound_ok "${IFC}" 1.1.1.1 443; then
  PUBLIC_OK=1
  ok "绑物理网卡（${IFC}）连公网正常"
else
  PUBLIC_OK=0
  log "注意：本机环境拒绝绑物理网卡连公网（可能是 VPN 或代理软件的“所有流量走隧道”）。经物理网卡到公网的几项（g、4g-i、4g-iii-a、4g-iii-c）按“被本机环境挡住”记录，只核对策略放行、没有向外拨号成功；不改任何设置。"
fi
[[ "${1:-}" == "--precheck-only" ]] && exit 0

# start_vm <镜像目录> <状态目录>
start_vm() { # start_vm <镜像目录> <状态目录> [额外参数…]
  local img=$1 st=$2
  shift 2
  VM_STATE="${st}"
  VM_LOG="${OUT}/abroad-vm-${TS}-$(basename "${st}")-$((++VM_RUNS)).log"
  "${BIN}/abroad-vm" run --image-dir "${img}" --state-dir "${st}" ${@+"$@"} 2>"${VM_LOG}" &
  VM_PID=$!
  log "abroad-vm 已启动（pid ${VM_PID}，状态目录 ${st}）"
}

health() {
  curl -fsS --max-time 3 --unix-socket "${VM_STATE}/ctl.sock" http://abroad-agent/v1/health 2>/dev/null
}

# wait_health <秒>：等到 health 返回 200，把应答写到 $HEALTH。abroad-vm 提前退出立即失败。
wait_health() {
  local deadline=$(($(date +%s) + $1))
  while (($(date +%s) < deadline)); do
    if ! kill -0 "${VM_PID}" 2>/dev/null; then
      tail -n 30 "${VM_LOG}" | tee -a "${EVID}"
      fail "abroad-vm 提前退出"
    fi
    if HEALTH="$(health)" && [[ -n "${HEALTH}" ]]; then
      return 0
    fi
    sleep 0.5
  done
  return 1
}

json() { python3 -I -c "import json,sys; d=json.loads(sys.argv[1]); print(eval(sys.argv[2]))" "$1" "$2"; }

section "环境"
log "仓库：${ROOT}"
log "系统：$(sw_vers -productVersion) $(uname -m)；Go：$(go version | awk '{print $3}')"

section "1. 构建 abroad-agent 和镜像"
with_timeout 600 env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o guest/out/abroad-agent ./cmd/abroad-agent ||
  fail "构建 abroad-agent"
with_timeout 600 go run ./cmd/abroad-imagebuild build -agent guest/out/abroad-agent 2>&1 | tee -a "${EVID}"
[[ ${PIPESTATUS[0]} -eq 0 ]] || fail "构建镜像"
with_timeout 120 go run ./cmd/abroad-imagebuild verify 2>&1 | tee -a "${EVID}"
[[ ${PIPESTATUS[0]} -eq 0 ]] || fail "镜像检查"
KVER="$(json "$(cat guest/out/build.json)" 'd["kernel_version"]')"
ok "镜像就绪，内核 ${KVER}"

section "2. 构建并签名 abroad-vm"
with_timeout 600 env CGO_ENABLED=1 go build -o "${BIN}/abroad-vm" ./cmd/abroad-vm || fail "构建 abroad-vm（需要 Xcode 命令行工具）"
scripts/dev/sign.sh "${BIN}/abroad-vm" | tee -a "${EVID}"
with_timeout 600 go build -o "${BIN}/abroad-dev" ./cmd/abroad-dev || fail "构建 abroad-dev"
ok "abroad-vm 已签名"

section "3. 启动虚拟机，等 health"
STATE="${OUT}/state"
rm -f "${STATE}/console.log"
T0="$(now_ms)"
# 阶段 A 的 4f 要测任意公网目标的出站，用开发用的开放模式（M2-1a 起默认收紧）。
start_vm guest/out "${STATE}" --outbound-open
wait_health 90 || fail "90 秒内 health 不通"
T1="$(now_ms)"
ok "health 可达，从启动 abroad-vm 到 health 返回用时 $((T1 - T0)) 毫秒"
python3 -I -c 'import json,sys; print(json.dumps(json.loads(sys.argv[1]), ensure_ascii=False, indent=2))' "${HEALTH}" | tee -a "${EVID}"

section "4. 检查 health 内容"
[[ "$(json "${HEALTH}" 'd["kernel"]')" == "${KVER}" ]] || fail "kernel 与 build.json 不一致"
ok "kernel = ${KVER}"
BAD="$(json "${HEALTH}" '",".join(m["name"] for m in d["modules"] if not m["ok"])')"
[[ -z "${BAD}" ]] || fail "有模块加载失败：${BAD}"
for m in vsock vmw_vsock_virtio_transport_common vmw_vsock_virtio_transport; do
  [[ "$(json "${HEALTH}" "any(x['name']=='${m}' and x['ok'] for x in d['modules'])")" == "True" ]] || fail "模块 ${m} 不在或没加载成功"
done
ok "模块全部加载成功：$(json "${HEALTH}" '",".join(m["name"] for m in d["modules"])')"
[[ "$(json "${HEALTH}" 'd["net"]["ok"]')" == "True" ]] || fail "eth0 应已配好：$(json "${HEALTH}" 'd["net"]')"
ok "net.ok=true（eth0 已配好，接用户态网络）"
for _ in $(seq 1 40); do # sing-box 可能还在启动
  [[ "$(json "${HEALTH}" 'd["engines"]["sing-box"]["state"]')" == "running" ]] && break
  sleep 0.25
  HEALTH="$(health)"
done
[[ "$(json "${HEALTH}" 'd["engines"]["sing-box"]["state"]')" == "running" ]] ||
  fail "sing-box 应处于 running，实际：$(json "${HEALTH}" 'd["engines"]["sing-box"]')"
ok "sing-box running"
[[ "$(json "${HEALTH}" '"data" in d and "sessions" in d["data"]')" == "True" ]] || fail "health 应有 data 段（数据面已监听）"
ok "数据面已监听：$(json "${HEALTH}" 'd["data"]')"
BOOT1="$(json "${HEALTH}" 'd.get("boot_id","")')"
[[ ${#BOOT1} -eq 32 ]] || fail "health 里应有 32 位的 boot_id，实际“${BOOT1}”"
ok "boot_id = ${BOOT1}"

section "4e-1. 数据面：主代理下发之前"
PROBE_COUNT="${OUT}/probe-count-${TS}"
: >"${PROBE_COUNT}"
probe() { # 经 data.sock 开一条流；输出一行 JSON。在 $(...) 里调用，所以计数写文件
  echo . >>"${PROBE_COUNT}"
  "${BIN}/abroad-dev" probe --data-sock "${STATE}/data.sock" "$@"
}
probes_since_boot() { wc -l <"${PROBE_COUNT}" | tr -d ' '; }
RES="$(probe --exit main --net tcp --target 127.0.0.1:1090)" || fail "probe 失败"
log "main（mihomo 未运行） → ${RES}"
[[ "$(json "${RES}" 'd.get("code")')" == "exit_down" ]] || fail "主代理没下发时应回 exit_down"
ok "a. exit_down"
RES="$(probe --exit nope --net tcp --target 127.0.0.1:1090)" || fail "probe 失败"
log "nope → ${RES}"
[[ "$(json "${RES}" 'd.get("code")')" == "unknown_exit" ]] || fail "不认识的出口应回 unknown_exit"
ok "b. unknown_exit"

section "4b. 主代理下发（PUT /v1/main-proxy）与二次校验"
ctl() { curl -sS --max-time 25 --unix-socket "${STATE}/ctl.sock" "$@"; }
# mcfg <proxies 等块（可含 \n）> <MATCH 目标> [nameserver 的 # 目标]：生成一份能通过二次校验的配置（设计 7.2）。
mcfg() {
  local blocks=$1 match=$2 ns=${3:-$2} rules='"MATCH,REJECT"'
  # 规则：MATCH,<出口> 加 MATCH,REJECT 兜底（出口不支持 UDP 时 mihomo 会跳过前一条，没有兜底就落到 DIRECT）。
  [[ "${match}" == "REJECT" ]] || rules="\"MATCH,${match}\", ${rules}"
  printf 'mixed-port: 7890\nbind-address: 127.0.0.1\nallow-lan: false\nmode: rule\nipv6: false\nlog-level: warning\ndns:\n  enable: true\n  ipv6: false\n  use-hosts: false\n  use-system-hosts: false\n  default-nameserver: ["192.168.127.1"]\n  proxy-server-nameserver: ["192.168.127.1"]\n  nameserver: ["https://1.1.1.1/dns-query#%s"]\n%brules: [%s]\n' "${ns}" "${blocks}" "${rules}"
}
# nodecfg <服务器> <端口>：单个 socks5 节点 + 组 CAGE + MATCH,CAGE/MATCH,REJECT（把目标当成“节点”，测虚拟机出站）。
nodecfg() {
  mcfg "proxies:\n  - {name: n, type: socks5, server: $1, port: $2}\nproxy-groups:\n  - {name: CAGE, type: select, proxies: [n]}\n" CAGE
}
putcfg() { # 输出 HTTP 状态码；应答原文写进证据文件
  printf '%s' "$1" | ctl -o /dev/stderr -w '%{http_code}' -X PUT --data-binary @- http://abroad-agent/v1/main-proxy 2>>"${EVID}"
  echo >>"${EVID}"
}
log "坏配置："
BAD="$(putcfg 'proxies: [oops')"
[[ "${BAD}" == "422" ]] || fail "坏配置应回 422，实际 ${BAD}"
ok "坏配置 → 422"
# 最小配置：无节点、无凭据，只有 MATCH,REJECT（nameserver 也指向 REJECT）；不需要联网。
MINCFG="$(mcfg "" REJECT)"
log "最小配置："
GOOD="$(putcfg "${MINCFG}")"
[[ "${GOOD}" == "200" ]] || fail "最小配置应回 200，实际 ${GOOD}"
ok "最小配置 → 200"
HEALTH="$(health)"
python3 -I -c 'import json,sys; d=json.loads(sys.argv[1]); print(json.dumps({"engines":d["engines"],"main_proxy":d["main_proxy"]}, ensure_ascii=False, indent=2))' "${HEALTH}" | tee -a "${EVID}"
[[ "$(json "${HEALTH}" 'd["engines"]["mihomo"]["state"]')" == "running" ]] || fail "下发后 mihomo 应为 running"
[[ "$(json "${HEALTH}" 'd["main_proxy"]["configured"]')" == "True" ]] || fail "下发后 main_proxy.configured 应为 true"
ok "mihomo running（127.0.0.1:7890 可连），main_proxy.configured=true"
ctl -fsS "http://abroad-agent/v1/logs?engine=mihomo&tail=50" >/dev/null || fail "/v1/logs 应可用"
# 二次校验：这些配置会从本机真实 IP 出去或在虚拟机里解析目标域名，一律 422，引擎状态不变。
NODE="$(nodecfg 198.51.100.1 1080)"
MSINCE="$(json "$(health)" 'd["engines"]["mihomo"]["since"]')"
declare -a DANGER_NAMES=("MATCH,DIRECT" "mode: direct" "空规则" "带 tun" "带 IP-CIDR 规则" "组成员 Proxies: [DIRECT]" "nameserver 不带 #"
  "空格名节点 + MATCH, DIRECT" "名字 %44IRECT + nameserver #%44IRECT" "只有 MATCH,CAGE（没有 MATCH,REJECT 兜底）")
declare -a DANGER_CFGS=(
  "$(mcfg "" DIRECT REJECT)"
  "${MINCFG/mode: rule/mode: direct}"
  "$(printf '%s\n' "${MINCFG}" | sed 's/^rules: .*/rules: []/')"
  "${MINCFG/ipv6: false
log-level/ipv6: false
tun:
  enable: false
log-level}"
  "$(printf '%s\n' "${MINCFG}" | sed 's|^rules: .*|rules: ["IP-CIDR,1.1.1.1/32,REJECT", "MATCH,REJECT"]|')"
  "$(printf '%s\n' "${NODE}" | sed 's/proxies: \[n\]}/proxies: [n], Proxies: [DIRECT]}/')"
  "$(printf '%s\n' "${NODE}" | sed 's/#CAGE//')"
  "$(printf '%s\n' "${NODE}" | sed -e 's/name: n,/name: " DIRECT",/' -e 's/proxies: \[n\]}/proxies: [" DIRECT"]}/' -e 's/"MATCH,CAGE"/"MATCH, DIRECT"/')"
  "$(printf '%s\n' "${NODE}" | sed -e 's/name: n,/name: "%44IRECT",/' -e 's/proxies: \[n\]}/proxies: ["%44IRECT"]}/' -e 's/#CAGE/#%44IRECT/')"
  "$(printf '%s\n' "${NODE}" | sed 's/^rules: .*/rules: ["MATCH,CAGE"]/')"
)
for i in "${!DANGER_NAMES[@]}"; do
  [[ "${DANGER_CFGS[$i]}" != "${MINCFG}" && "${DANGER_CFGS[$i]}" != "${NODE}" ]] || fail "脚本错误：危险配置 ${DANGER_NAMES[$i]} 没有改出来"
  CODE="$(putcfg "${DANGER_CFGS[$i]}")"
  [[ "${CODE}" == "422" ]] || fail "危险配置（${DANGER_NAMES[$i]}）应回 422，实际 ${CODE}"
  H="$(health)"
  [[ "$(json "${H}" 'd["engines"]["mihomo"]["state"]')" == "running" && "$(json "${H}" 'd["main_proxy"]["configured"]')" == "True" &&
    "$(json "${H}" 'd["engines"]["mihomo"]["since"]')" == "${MSINCE}" ]] || fail "危险配置（${DANGER_NAMES[$i]}）被拒后 mihomo 不应重启"
  ok "危险配置（${DANGER_NAMES[$i]}）→ 422，mihomo 未重启"
done

section "4c. 对时（PUT /v1/clock）"
host_ms() { perl -MTime::HiRes=time -e 'printf "%d\n", time*1000'; }
vm_skew() { # 虚拟机时间 − 宿主时间（毫秒）
  local h
  h="$(health)" || return 1
  echo $(($(json "${h}" 'd["clock"]["now_unix_ms"]') - $(host_ms)))
}
abs() { local v=$1; echo $((v < 0 ? -v : v)); }
SYNCED=0
for _ in $(seq 1 40); do
  if [[ "$(json "$(health)" 'd.get("clock",{}).get("synced")')" == "True" ]]; then SYNCED=1; break; fi
  sleep 0.25
done
[[ ${SYNCED} -eq 1 ]] || fail "health 通过后 10 秒内 clock.synced 应为 true"
SKEW="$(vm_skew)"
(($(abs "${SKEW}") < 2000)) || fail "自动对时后虚拟机时间应与宿主相差小于 2 秒，实际 ${SKEW} 毫秒"
ok "a. 自动对时完成，偏差 ${SKEW} 毫秒；clock：$(json "$(health)" 'd["clock"]')"
putclock() { ctl -w ' %{http_code}' -X PUT --data-binary "{\"unix_ms\":$1}" http://abroad-agent/v1/clock; }
RES="$(putclock $(($(host_ms) + 3600000)))"
log "PUT 快 1 小时 → ${RES}"
[[ "${RES}" == *'"stepped":true'*' 200' ]] || fail "快 1 小时应回 200 且 stepped=true"
SKEW="$(vm_skew)"
(($(abs $((SKEW - 3600000))) < 5000)) || fail "虚拟机时间应约快 3600 秒，实际偏差 ${SKEW} 毫秒"
ok "b. 虚拟机时间快了约 1 小时（偏差 ${SKEW} 毫秒）"
RES="$(putclock "$(host_ms)")"
log "PUT 正确时间 → ${RES}"
[[ "${RES}" == *'"stepped":true'*' 200' ]] || fail "调回正确时间应回 200 且 stepped=true"
OFF="$(echo "${RES% *}" | python3 -I -c 'import json,sys; print(json.load(sys.stdin)["offset_ms"])')"
(($(abs $((OFF + 3600000))) < 5000)) || fail "offset_ms 应约为 −3600000，实际 ${OFF}"
SKEW="$(vm_skew)"
(($(abs "${SKEW}") < 2000)) || fail "调回后应相差小于 2 秒，实际 ${SKEW} 毫秒"
ok "c. 调回正确时间（offset_ms ${OFF}，偏差 ${SKEW} 毫秒）"
RES="$(putclock 946684800000)"
log "PUT 2000 年 → ${RES}"
[[ "${RES}" == *' 400' ]] || fail "2000 年的时间应回 400"
SKEW="$(vm_skew)"
(($(abs "${SKEW}") < 2000)) || fail "被拒后时钟不应改变，实际偏差 ${SKEW} 毫秒"
ok "d. 越界时间被拒，时钟不变"

section "4e-2. 数据面：经真实的 sing-box 和 mihomo（主代理 MATCH,REJECT）"
# DIRECT 已被二次校验禁止，主代理用 MATCH,REJECT。经 mihomo 到回环的 TCP、UDP 正例（原 d、g）
# 在 M1-6c 的阶段 C（开发用本机上游）恢复。
CODE="$(putcfg "${MINCFG}")"
[[ "${CODE}" == "200" ]] || fail "下发 MATCH,REJECT 配置应回 200，实际 ${CODE}"
for tgt in 127.0.0.1:9 203.0.113.1:443; do
  RES="$(probe --exit main --net tcp --target "${tgt}")" || fail "probe 失败"
  log "TCP → ${tgt}（MATCH,REJECT）→ ${RES}"
  # 设计 4.4：ok 只表示已交给出口引擎；被拒或不可达表现为 ok 之后零字节 EOF。
  [[ "$(json "${RES}" 'd["ok"] and d["recv_hex"]=="" and d["eof"] and not d["timeout"]')" == "True" ]] ||
    fail "${tgt} 应表现为 ok 之后零字节 EOF（设计 4.4）"
  ok "e/f. ${tgt} → ok 后零字节 EOF（$(json "${RES}" 'd.get("elapsed_ms")') 毫秒）"
done
for _ in $(seq 1 20); do
  D="$(json "$(health)" 'd["data"]')"
  [[ "$(json "$(health)" 'd["data"]["active_streams"]')" == "0" && "$(json "$(health)" 'd["data"]["sessions"]')" == "0" ]] && break
  sleep 0.25
done
log "data 段：${D}"
PROBES="$(probes_since_boot)"
[[ "$(json "$(health)" 'd["data"]["total_streams"]')" == "${PROBES}" ]] || fail "total_streams 应等于探测次数 ${PROBES}"
[[ "$(json "$(health)" '"exit_down" in d["data"]["rejects"] and "unknown_exit" in d["data"]["rejects"]')" == "True" ]] || fail "rejects 里应有 exit_down 和 unknown_exit"
[[ "$(json "$(health)" 'd["data"]["active_streams"]')" == "0" && "$(json "$(health)" 'd["data"]["sessions"]')" == "0" ]] || fail "探测结束后活动流和会话应回到 0"
ok "h. 计数正确（total_streams=${PROBES}），活动流和会话已归零"
log "mihomo 日志（节选）："
ctl "http://abroad-agent/v1/logs?engine=mihomo&tail=30" | python3 -I -c 'import json,sys; print("\n".join(json.load(sys.stdin)["lines"]))' | tee -a "${EVID}"
log "sing-box 日志（节选）："
ctl "http://abroad-agent/v1/logs?engine=sing-box&tail=30" | python3 -I -c 'import json,sys; print("\n".join(json.load(sys.stdin)["lines"]))' | tee -a "${EVID}"

section "4f. 虚拟机出站（经用户态网络、出站策略、绑定物理网卡）"
# DIRECT 被禁止后，用“把目标当成主代理节点”来测出站：mihomo 去连这个“节点”，连接经虚拟机网卡 →
# 用户态网络 → 出站策略 → 绑定物理网卡的拨号器。握手必然失败，probe 得到零字节即可，看的是计数。
# 只测 TCP 出站；UDP 出站的真机覆盖在 135116 那份证据和单测里，M1-6c 的阶段 C 补回。
# 网关 DNS 回 REFUSED（原 i）同理由 usernet 单测和 135116 证据覆盖。
netstats() { curl -fsS --max-time 3 --unix-socket "${STATE}/host.sock" http://abroad-vm/v1/netstats; }
stat_of() { json "$1" "d['$2']"; }
# st_delta <前> <后> <字段>；ext_delta <前> <后>：绑物理网卡的向外拨号增量（dial_ok − dial_ok_loopback）。
st_delta() { echo $(($(stat_of "$2" "$3") - $(stat_of "$1" "$3"))); }
ext_delta() { echo $(($(st_delta "$1" "$2" dial_ok) - $(st_delta "$1" "$2" dial_ok_loopback))); }
delta_log() {
  log "$1 netstats 增量：dial_ok +$(st_delta "$2" "$3" dial_ok)，dial_ok_loopback +$(st_delta "$2" "$3" dial_ok_loopback)，policy_denied +$(st_delta "$2" "$3" policy_denied)，hop_denied +$(st_delta "$2" "$3" hop_denied)，bind_failed +$(st_delta "$2" "$3" bind_failed)，resolver_dial_ok +$(st_delta "$2" "$3" resolver_dial_ok)"
}
# viaNode <描述> <节点服务器> <端口> <policy_denied 最小增量（0 表示必须为 0）> <dial_ok 最小增量（0 表示必须为 0）>
viaNode() {
  local desc=$1 srv=$2 port=$3 wantDeny=$4 wantOK=$5 b a dd dk code
  code="$(putcfg "$(nodecfg "${srv}" "${port}")")"
  [[ "${code}" == "200" ]] || fail "${desc}：下发节点配置应回 200，实际 ${code}"
  b="$(netstats)"
  RES="$(probe --exit main --net tcp --target 198.51.100.1:443 --read-timeout 5s)" || fail "probe 失败"
  sleep 0.5
  a="$(netstats)"
  dd=$(($(stat_of "${a}" policy_denied) - $(stat_of "${b}" policy_denied)))
  dk=$(($(stat_of "${a}" dial_ok) - $(stat_of "${b}" dial_ok)))
  log "${desc}：${RES}；policy_denied +${dd}，dial_ok +${dk}，bind_failed $(stat_of "${a}" bind_failed)，网卡 $(stat_of "${a}" interface)"
  [[ "$(json "${RES}" 'd["recv_hex"]')" == "" ]] || fail "${desc} 应收不到任何字节"
  if ((wantDeny < 0)); then :
  elif ((wantDeny == 0)); then ((dd == 0)) || fail "${desc} 的 policy_denied 增量应为 0（实际 ${dd}）"
  else ((dd >= wantDeny)) || fail "${desc} 的 policy_denied 增量应 ≥ ${wantDeny}（实际 ${dd}）"; fi
  if ((wantOK < 0)); then : # 本机环境对公网时通时不通：不判 dial_ok，由调用方按结果记录
  elif ((wantOK == 0)); then ((dk == 0)) || fail "${desc} 的 dial_ok 增量必须为 0（实际 ${dk}）"
  else ((dk >= wantOK)) || fail "${desc} 的 dial_ok 增量应 ≥ ${wantOK}（实际 ${dk}）"; fi
  LAST_DK=${dk}
  [[ "$(stat_of "${a}" bind_failed)" == "0" ]] || fail "bind_failed 应为 0"
}
S0="$(netstats)"; log "netstats 起点：${S0}"
PUB=223.5.5.5
if [[ ${PUBLIC_OK} -eq 1 ]]; then
  viaNode "g. 节点 ${PUB}:53（公网 IP）" "${PUB}" 53 0 1
else # 本机环境限制公网：策略必须放行（policy_denied +0）；拨号成不成功取决于本机环境，按结果记录
  viaNode "g. 节点 ${PUB}:53（公网 IP；本机环境限制公网）" "${PUB}" 53 0 -1
fi
IFACE="$(stat_of "$(netstats)" interface)"
[[ -n "${IFACE}" ]] || fail "出站后 interface 应非空"
if [[ ${PUBLIC_OK} -eq 1 ]]; then ok "g. 虚拟机 TCP 经绑定物理网卡（${IFACE}）出站"
elif ((LAST_DK >= 1)); then ok "g. 虚拟机 TCP 经绑定物理网卡（${IFACE}）出站（本机环境这次放行了这个目标）"
else log "g. 被本机环境挡住：策略放行、拨号绑的是物理网卡（${IFACE}），本机拒绝了到公网的连接（不算失败）"; fi
# 原 h（节点域名经直连 DoH 解析）：M2-1b 起节点域名只经网关 DNS 解析，开放模式下网关 DNS 一律 REFUSED，
# 改由 6c 的 4g-iii 在收紧模式下覆盖。
viaNode "j1. 节点 10.0.0.1:80（私有）" 10.0.0.1 80 1 0
viaNode "j2. 节点 192.168.127.1:80（网关非 53 端口）" 192.168.127.1 80 1 0
viaNode "j3. 节点 169.254.169.254:80（链路本地，在策略之前被丢）" 169.254.169.254 80 0 0
# 宿主局域网地址：用拨号器当前绑定的网卡名去取（开着 Clash TUN 时默认路由是 utun，不能用 route get default）。
# 局域网地址通常在 192.168/10/172.16 网段，被静态网段拦下；“本机持有公网地址”（isOwn）只有单测覆盖。
LAN="$(ipconfig getifaddr "${IFACE}" 2>/dev/null || true)"
[[ -n "${LAN}" ]] || fail "取不到网卡 ${IFACE} 的局域网地址"
viaNode "j4. 节点 宿主局域网地址:80" "${LAN}" 80 1 0
ok "j. 四个负例都没有出站（dial_ok 增量 0），策略计数符合预期"
# j5：没有回环白名单时，网关上没有 192.168.127.254 这个虚拟地址，虚拟机连不到本机 127.0.0.1。
start_listener P
start_listener Q
PP="$(lst_port P)"; QP="$(lst_port Q)"
viaNode "j5. 节点 192.168.127.254:${PP}（无白名单）" 192.168.127.254 "${PP}" -1 0
[[ "$(lst_count P)" == "0" ]] || fail "j5. 本机监听器不应收到连接（实际 $(lst_count P)）"
ok "j5. 无白名单时虚拟机连不到本机回环（监听器 0 条连接）"
log "netstats 终点：$(netstats)"

section "5. 经 ctl.sock 请求关机，验证自动重启"
sleep 3 # 让 uptime 增长，便于比较
UP1="$(json "$(health)" 'd["uptime_s"]')"
CODE="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 3 --unix-socket "${STATE}/ctl.sock" -X POST http://abroad-agent/v1/shutdown)"
[[ "${CODE}" == "202" ]] || fail "POST /v1/shutdown 返回 ${CODE}"
ok "POST /v1/shutdown → 202（关机前 uptime ${UP1} 秒）"
T2="$(now_ms)"
DOWN=0
for _ in $(seq 1 40); do
  if ! health >/dev/null; then
    DOWN=1
    break
  fi
  sleep 0.25
done
[[ ${DOWN} -eq 1 ]] || fail "关机请求后 health 一直可达"
T3="$(now_ms)"
ok "health 已不通（$((T3 - T2)) 毫秒）"
wait_health 90 || fail "关机后 90 秒内没有自动重启"
T4="$(now_ms)"
UP2="$(json "${HEALTH}" 'd["uptime_s"]')"
((UP2 < UP1)) || fail "重启后 uptime（${UP2}）应小于重启前（${UP1}）"
BOOT2="$(json "${HEALTH}" 'd.get("boot_id","")')"
[[ ${#BOOT2} -eq 32 && "${BOOT2}" != "${BOOT1}" ]] || fail "重启后 boot_id 应改变（${BOOT1} → ${BOOT2}）"
ok "重启后 boot_id 已改变：${BOOT1} → ${BOOT2}"
[[ "$(json "${HEALTH}" 'd["main_proxy"]["configured"]')" == "False" ]] || fail "重启后 main_proxy.configured 应回到 false（配置只在 tmpfs）"
[[ "$(json "${HEALTH}" 'd["engines"]["mihomo"]["state"]')" == "stopped" ]] || fail "重启后 mihomo 应为 stopped"
ok "重启后主代理配置已消失（configured=false，mihomo stopped）：配置只在内存"
SYNCED=0
for _ in $(seq 1 40); do
  if [[ "$(json "$(health)" 'd.get("clock",{}).get("synced")')" == "True" ]]; then SYNCED=1; break; fi
  sleep 0.25
done
[[ ${SYNCED} -eq 1 ]] || fail "重启后 10 秒内应重新对时"
ok "e. 重启后重新对时（OnReady 重新触发）"
RES="$(probe --exit main --net tcp --target 127.0.0.1:1090)" || fail "probe 失败"
log "重启后 main → ${RES}"
[[ "$(json "${RES}" 'd.get("code")')" == "exit_down" ]] || fail "重启后（主代理配置已消失）应回 exit_down"
ok "4e-i. 重启后 data.sock 的新连接到了新虚拟机（exit_down）"
viaNode "重启后 g. 节点 ${PUB}:53" "${PUB}" 53 0 1
ok "4f. 重启后虚拟机出站仍然可用（新的 socketpair）"
ok "虚拟机已自动重启，health 恢复（距关机请求 $((T4 - T2)) 毫秒，uptime ${UP1} → ${UP2}）"

section "6. SIGTERM 停止 abroad-vm"
PID="${VM_PID}"
[[ "$(cat "${STATE}/abroad-vm.pid")" == "${PID}" ]] || fail "pidfile 内容与进程不一致"
T5="$(now_ms)"
kill -TERM "${PID}"
RC=0
for _ in $(seq 1 80); do
  kill -0 "${PID}" 2>/dev/null || break
  sleep 0.25
done
kill -0 "${PID}" 2>/dev/null && fail "SIGTERM 后 20 秒内没有退出"
wait "${PID}" || RC=$?
VM_PID=""
T6="$(now_ms)"
[[ ${RC} -eq 0 ]] || fail "abroad-vm 退出码 ${RC}"
ok "abroad-vm 用 $((T6 - T5)) 毫秒退出，退出码 0"
[[ ! -e "${STATE}/abroad-vm.pid" ]] || fail "pidfile 没有删除"
[[ ! -e "${STATE}/ctl.sock" ]] || fail "ctl.sock 没有删除"
[[ ! -e "${STATE}/host.sock" ]] || fail "host.sock 没有删除"
[[ ! -e "${STATE}/data.sock" ]] || fail "data.sock 没有删除"
ps -p "${PID}" >/dev/null 2>&1 && fail "进程 ${PID} 仍存在"
ok "4e-j. 进程已不存在，pidfile、ctl.sock、data.sock 已清理"
READY="$(grep -c "虚拟机已就绪" "${VM_LOG}" || true)"
[[ "${READY}" -eq 2 ]] || fail "abroad-vm 日志里“虚拟机已就绪”应出现 2 次（两次开机各一次），实际 ${READY}"
grep "虚拟机已就绪" "${VM_LOG}" | tee -a "${EVID}"
ok "每次开机各有一条“已就绪”"

section "控制台日志（${STATE}/console.log，节选）"
grep -E "abroad-agent|Power down|reboot|panic" "${STATE}/console.log" | tail -n 60 | tee -a "${EVID}" || true
for want in "boot：根盘" "切换到根盘" "serve 已启动" "控制面已在 vsock 1081 上监听"; do
  grep -q "${want}" "${STATE}/console.log" || fail "控制台日志里没有“${want}”"
done
ok "控制台日志包含认盘、切换根盘、serve 启动、控制面监听"

section "6b. 回环白名单（--allow-outbound 127.0.0.1:P）"
# 本机其他代理软件的端口当主代理（设计 4.5）：网关虚拟地址 192.168.127.254 经 NAT 映射到本机 127.0.0.1，
# 由 abroad-vm 不绑网卡代为连接；只放行白名单里的精确端口。监听器只绑 127.0.0.1，脚本结束时结束。
start_vm guest/out "${STATE}" --allow-outbound "127.0.0.1:${PP}"
wait_health 90 || fail "带白名单启动后 90 秒内 health 不通"
[[ "$(stat_of "$(netstats)" allow_count)" == "1" ]] || fail "netstats 的 allow_count 应为 1"
ok "带白名单启动（allow_count=1，日志里只写条数）"
for _ in $(seq 1 40); do [[ "$(json "$(health)" 'd["engines"]["sing-box"]["state"]')" == "running" ]] && break; sleep 0.25; done
k1() {
  local before after
  before="$(lst_count P)"
  viaNode "$1. 节点 192.168.127.254:${PP}（白名单）" 192.168.127.254 "${PP}" 0 1
  after="$(lst_count P)"
  ((after >= before + 1)) || fail "$1. 本机监听器 P 应收到连接（${before} → ${after}）"
  ok "$1. 虚拟机经 192.168.127.254 连到本机 127.0.0.1:${PP}（监听器 ${before} → ${after}）"
}
k1 k1
QB="$(lst_count Q)"
viaNode "k2. 节点 192.168.127.254:${QP}（不在白名单）" 192.168.127.254 "${QP}" 1 0
[[ "$(lst_count Q)" == "${QB}" ]] || fail "k2. 监听器 Q 不应收到连接"
ok "k2. 白名单之外的本机端口被拒（监听器 Q 0 条连接）"
PB="$(lst_count P)"; QB="$(lst_count Q)"
viaNode "k3. 节点 127.0.0.1:${PP}（虚拟机自己的回环）" 127.0.0.1 "${PP}" -1 0
[[ "$(lst_count P)" == "${PB}" && "$(lst_count Q)" == "${QB}" ]] || fail "k3. 本机监听器的连接数不应增加"
ok "k3. 节点写 127.0.0.1 时连的是虚拟机自己的回环，不会到本机"
KB="$(json "$(health)" 'd["boot_id"]')"
CODE="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 3 --unix-socket "${STATE}/ctl.sock" -X POST http://abroad-agent/v1/shutdown)"
[[ "${CODE}" == "202" ]] || fail "k4. 请求关机应回 202"
DOWN=0
for _ in $(seq 1 40); do
  if ! health >/dev/null; then DOWN=1; break; fi
  sleep 0.25
done
[[ ${DOWN} -eq 1 ]] || fail "k4. 关机请求后 health 一直可达"
wait_health 90 || fail "k4. 关机后没有自动重启"
KA="$(json "${HEALTH}" 'd["boot_id"]')"
[[ -n "${KA}" && "${KA}" != "${KB}" ]] || fail "k4. 重启后 boot_id 应改变（${KB} → ${KA}）"
ok "k4. 虚拟机已重启（boot_id ${KB} → ${KA}）"
for _ in $(seq 1 40); do [[ "$(json "$(health)" 'd["engines"]["sing-box"]["state"]')" == "running" ]] && break; sleep 0.25; done
k1 k4
PID="${VM_PID}"
kill -TERM "${PID}"
RC=0
for _ in $(seq 1 80); do kill -0 "${PID}" 2>/dev/null || break; sleep 0.25; done
kill -0 "${PID}" 2>/dev/null && fail "6b. SIGTERM 后 20 秒内没有退出"
wait "${PID}" || RC=$?
VM_PID=""
[[ ${RC} -eq 0 ]] || fail "6b. abroad-vm 退出码 ${RC}"
ok "6b. abroad-vm 收到 SIGTERM 后以 0 退出"
stop_listeners
[[ ! -e "${STATE}/host.sock" ]] || fail "host.sock 没有删除"
ok "6b. 回环白名单一轮完成，abroad-vm 和本机监听器都已停止"

section "6c. 出站收紧（默认模式，--first-hop；设计 4.5，M2-1a）"
# 虚拟机只能连白名单和路线第一跳；第一跳是域名时由网关 DNS 代为解析（答出的地址才放行）。
# 网关 DNS 的真机验证（4g-iii）要让 mihomo 用网关解析节点域名，随 M2-1b 一起做。
start_vm guest/out "${STATE}" --first-hop 223.5.5.5:443 --first-hop dns.google:443
wait_health 90 || fail "收紧模式启动后 90 秒内 health 不通"
for _ in $(seq 1 40); do [[ "$(json "$(health)" 'd["engines"]["sing-box"]["state"]')" == "running" ]] && break; sleep 0.25; done
G0="$(netstats)"; log "收紧模式 netstats 起点：${G0}"
[[ "$(stat_of "${G0}" outbound_mode)" == "first_hop" && "$(stat_of "${G0}" first_hop_count)" == "2" ]] || fail "4g. netstats 应为 outbound_mode=first_hop、first_hop_count=2"
ok "4g. 出站模式 first_hop，第一跳 2 项（日志里只写条数）"
# hopcase <描述> <节点服务器> <端口>：把目标当成主代理节点（同 4f），探测前后读 netstats 到 HB、HA。
# 每次下发配置都会重启 mihomo，DNS 缓存随之清空，节点域名会重新经网关解析。
hopcase() {
  local desc=$1 srv=$2 port=$3 code
  code="$(putcfg "$(nodecfg "${srv}" "${port}")")"
  [[ "${code}" == "200" ]] || fail "${desc}：下发节点配置应回 200，实际 ${code}"
  HB="$(netstats)"
  RES="$(probe --exit main --net tcp --target 198.51.100.1:443 --read-timeout 5s)" || fail "probe 失败"
  sleep 0.5
  HA="$(netstats)"
  log "${desc}：${RES}"
  delta_log "${desc}" "${HB}" "${HA}"
  log "${desc} 网关 DNS 增量：dns_answered +$(st_delta "${HB}" "${HA}" dns_answered)，dns_refused +$(st_delta "${HB}" "${HA}" dns_refused)，dns_failed +$(st_delta "${HB}" "${HA}" dns_failed)"
  (($(st_delta "${HB}" "${HA}" policy_denied) == 0)) || fail "${desc}：公网目标不应计入 policy_denied"
}
dh() { st_delta "${HB}" "${HA}" "$1"; }
# viaHop <描述> <节点服务器> <端口> <yes|no>：字面量节点，只看向外拨号和 hop_denied。
viaHop() {
  hopcase "$1" "$2" "$3"
  if [[ "$4" == "yes" ]]; then
    (($(ext_delta "${HB}" "${HA}") >= 1 && $(dh hop_denied) == 0)) || fail "$1：应向外拨号成功，hop_denied +0"
  else
    (($(ext_delta "${HB}" "${HA}") == 0 && $(dh hop_denied) >= 1)) || fail "$1：不应向外拨号，hop_denied 应增长"
  fi
}
if [[ ${PUBLIC_OK} -eq 1 ]]; then
  viaHop "4g-i. 节点 223.5.5.5:443（字面量第一跳）" 223.5.5.5 443 yes
  ok "4g-i. 第一跳放行"
else
  hopcase "4g-i. 节点 223.5.5.5:443（字面量第一跳；本机环境限制公网）" 223.5.5.5 443
  (($(dh hop_denied) == 0)) || fail "4g-i. 第一跳不应被 hop_denied 拒绝"
  if (($(ext_delta "${HB}" "${HA}") >= 1)); then ok "4g-i. 第一跳放行，向外拨号成功（本机环境这次放行了这个目标）"
  else log "4g-i. 被本机环境挡住：第一跳没有被收紧策略拒绝（hop_denied +0），本机拒绝了到公网的连接（不算失败）"; fi
fi
viaHop "4g-ii. 节点 223.5.5.5:53（端口不符）" 223.5.5.5 53 no
viaHop "4g-ii. 节点 119.29.29.29:443（不在路线上）" 119.29.29.29 443 no
ok "4g-ii. 端口不符、不在路线上的公网目标都被拒（hop_denied 增长，没有向外拨号）"
hopcase "4g-iii-b. 节点 dns.alidns.com:443（域名没登记）" dns.alidns.com 443
(($(dh dns_refused) >= 1 && $(dh resolver_dial_ok) == 0 && $(ext_delta "${HB}" "${HA}") == 0)) ||
  fail "4g-iii-b. 没登记的域名应 REFUSED（dns_refused 增长），abroad-vm 不去解析，也没有向外拨号"
ok "4g-iii-b. 没登记的节点域名被网关 DNS 拒绝，没有解析、没有向外拨号"
term_wait_vm() {
  local rc=0
  kill -TERM "${VM_PID}" 2>/dev/null || true
  for _ in $(seq 1 80); do kill -0 "${VM_PID}" 2>/dev/null || break; sleep 0.25; done
  kill -0 "${VM_PID}" 2>/dev/null && fail "abroad-vm 20 秒内没有退出"
  wait "${VM_PID}" || rc=$?
  VM_PID=""
  [[ ${rc} -eq 0 ]] || fail "abroad-vm 退出码 ${rc}"
}
term_wait_vm
# 第二轮：只登记域名第一跳 dns.alidns.com:443（没有字面量第一跳，向外拨号只能来自网关答过的地址）。
start_vm guest/out "${STATE}" --first-hop dns.alidns.com:443
wait_health 90 || fail "4g-iii：90 秒内 health 不通"
for _ in $(seq 1 40); do [[ "$(json "$(health)" 'd["engines"]["sing-box"]["state"]')" == "running" ]] && break; sleep 0.25; done
if [[ ${PUBLIC_OK} -eq 1 ]]; then
  hopcase "4g-iii-a. 节点 dns.alidns.com:443（登记的域名第一跳）" dns.alidns.com 443
  (($(dh dns_answered) >= 1 && $(dh resolver_dial_ok) >= 1 && $(ext_delta "${HB}" "${HA}") >= 1 && $(dh hop_denied) == 0)) ||
    fail "4g-iii-a. 网关应答（dns_answered 增长）、abroad-vm 经 DoH 解析（resolver_dial_ok 增长）、向外拨号成功、hop_denied +0"
  ok "4g-iii-a. mihomo 用网关（裸 IP 的 UDP nameserver）解析节点域名，答出的地址放行（实测 a 成立）"
  hopcase "4g-iii-c. 节点 dns.alidns.com:853（端口不符）" dns.alidns.com 853
  (($(dh dns_answered) >= 1 && $(dh hop_denied) >= 1 && $(ext_delta "${HB}" "${HA}") == 0)) ||
    fail "4g-iii-c. 域名照样应答，但端口不属于第一跳：hop_denied 增长，没有向外拨号"
  ok "4g-iii-c. 答过的地址只在第一跳的端口上放行"
else # 网关代解析要经物理网卡做 DoH（公网），被本机环境挡住：只核对登记的域名没有被拒绝、没有向外拨号成功
  hopcase "4g-iii-a. 节点 dns.alidns.com:443（登记的域名第一跳；本机环境限制公网）" dns.alidns.com 443
  (($(dh dns_refused) == 0 && $(dh hop_denied) == 0)) || fail "4g-iii-a. 登记的域名不应被网关拒绝（dns_refused +0），不应有 hop_denied"
  if (($(dh dns_answered) >= 1)); then
    ok "4g-iii-a. 网关应答了登记的域名（本机环境这次放行了 DoH），向外拨号 +$(ext_delta "${HB}" "${HA}")"
    hopcase "4g-iii-c. 节点 dns.alidns.com:853（端口不符）" dns.alidns.com 853
    (($(dh hop_denied) >= 1 && $(ext_delta "${HB}" "${HA}") == 0)) || fail "4g-iii-c. 端口不属于第一跳：hop_denied 增长，没有向外拨号"
    ok "4g-iii-c. 答过的地址只在第一跳的端口上放行"
  else
    log "4g-iii-a. 被本机环境挡住：登记的域名被网关接受（dns_refused +0），abroad-vm 的 DoH 解析经物理网卡到公网被本机拒绝（dns_failed +$(dh dns_failed)），不算失败"
    log "4g-iii-c. 被本机环境挡住：依赖 4g-iii-a 的解析结果，跳过"
  fi
fi
term_wait_vm
ok "6c. 收紧模式一轮完成，abroad-vm 以 0 退出"

section "7. 阶段 B、C（主代理 = 本机回环上的开发上游）"
# 开发上游（abroad-dev upstream）充当“本机其他代理软件”：固定应答、绝不向外拨号，只绑 127.0.0.1。
# 链路：curl → abroad-dev socks → data.sock → 虚拟机数据面 → sing-box → mihomo → 节点 192.168.127.254:P
#       → 用户态网络 NAT → 本机 127.0.0.1:P（开发上游）。真实远端节点的验证在 M2 验收前补（进度单）。
# bg_wait_line <输出文件> <前缀>：等后台进程打出 "<前缀> <地址>"，回显地址。
bg_wait_line() {
  local f=$1 pre=$2 i
  for i in $(seq 1 50); do
    if grep -q "^${pre} " "${f}" 2>/dev/null; then grep "^${pre} " "${f}" | head -1 | cut -d' ' -f2; return 0; fi
    sleep 0.1
  done
  return 1
}
# term_wait <pid> <名字>：TERM 后等它退出，断言退出码 0。
term_wait() {
  local pid=$1 name=$2 rc=0
  kill -TERM "${pid}" 2>/dev/null || true
  for _ in $(seq 1 60); do kill -0 "${pid}" 2>/dev/null || break; sleep 0.25; done
  kill -0 "${pid}" 2>/dev/null && fail "${name} 15 秒内没有退出"
  wait "${pid}" || rc=$?
  [[ ${rc} -eq 0 ]] || fail "${name} 退出码 ${rc}"
}
UPLOG="${OUT}/upstream-${TS}.jsonl"
: >"${UPLOG}"
"${BIN}/abroad-dev" upstream --log "${UPLOG}" >"${OUT}/upstream-${TS}.out" 2>&1 &
UP_PID=$!
LISTENER_PIDS+=("${UP_PID}")
UP_ADDR="$(bg_wait_line "${OUT}/upstream-${TS}.out" listening)" || fail "开发上游没有起来"
UPP="${UP_ADDR##*:}"
NODES="${OUT}/dev-nodes-${TS}.yaml"
printf 'proxies:\n - {name: exit, type: socks5, server: 127.0.0.1, port: %s, udp: true}\n' "${UPP}" >"${NODES}"
BADNODES="${OUT}/dev-nodes-bad-${TS}.yaml"
printf 'proxies:\n - {name: exit, type: socks5, server: localhost, port: %s}\n' "${UPP}" >"${BADNODES}"
ALLOW="$("${BIN}/abroad-dev" apply-main-proxy --nodes "${NODES}" --selected exit --print-allow)" || fail "--print-allow 失败"
[[ "${ALLOW}" == "127.0.0.1:${UPP}" ]] || fail "白名单应为 127.0.0.1:${UPP}，实际 ${ALLOW}"
ok "开发上游已在 127.0.0.1:${UPP}（TCP、UDP 同端口）；白名单 ${ALLOW}"
# 阶段 D 的本机独立 mihomo 也在白名单里（它在阶段 D 才启动；之前连它会被拒绝连接）。
start_vm guest/out "${STATE}" --allow-outbound "${ALLOW}" --allow-outbound "127.0.0.1:${HMP}"
wait_health 90 || fail "阶段 B：90 秒内 health 不通"
for _ in $(seq 1 40); do [[ "$(json "$(health)" 'd["engines"]["sing-box"]["state"]')" == "running" ]] && break; sleep 0.25; done
APPLIED="$("${BIN}/abroad-dev" apply-main-proxy --nodes "${NODES}" --selected exit --ctl-sock "${STATE}/ctl.sock")" || fail "B1. 下发主代理失败"
log "B1. ${APPLIED}"
[[ "${APPLIED}" == "applied udp=false" ]] || fail "B1. 第一跳是本机回环时默认不转 UDP，应为 applied udp=false"
H="$(health)"
[[ "$(json "${H}" 'd["main_proxy"]["configured"]')" == "True" && "$(json "${H}" 'd["engines"]["mihomo"]["state"]')" == "running" ]] || fail "B1. 下发后应 configured=true、mihomo running"
ok "B1. 主代理已下发（VMRuntime：第一跳 127.0.0.1 改写为 192.168.127.254，udp=false），mihomo running"
MSINCE="$(json "${H}" 'd["engines"]["mihomo"]["since"]')"
if "${BIN}/abroad-dev" apply-main-proxy --nodes "${BADNODES}" --selected exit --ctl-sock "${STATE}/ctl.sock" 2>>"${EVID}"; then
  fail "B2. 第一跳 localhost 应被拒"
fi
H="$(health)"
[[ "$(json "${H}" 'd["main_proxy"]["configured"]')" == "True" && "$(json "${H}" 'd["engines"]["mihomo"]["since"]')" == "${MSINCE}" ]] || fail "B2. 被拒后主代理状态不应改变"
ok "B2. 第一跳 localhost 被拒（提示改写成 127.0.0.1），主代理状态不变"

C0="$(netstats)"; log "阶段 C 开始时 netstats：${C0}"
[[ "$(stat_of "${C0}" outbound_mode)" == "first_hop" ]] || fail "阶段 B、C 应在默认的收紧模式下跑"
"${BIN}/abroad-dev" socks --data-sock "${STATE}/data.sock" --listen 127.0.0.1:0 >"${OUT}/socks-${TS}.out" 2>&1 &
SOCKS_PID=$!
LISTENER_PIDS+=("${SOCKS_PID}")
SOCKS_ADDR="$(bg_wait_line "${OUT}/socks-${TS}.out" listening)" || fail "abroad-dev socks 没有起来"
c1() {
  local code
  code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 20 -x "socks5h://${SOCKS_ADDR}" http://generate.test/generate_204 || true)"
  log "$1. curl -x socks5h://${SOCKS_ADDR} http://generate.test/generate_204 → ${code}"
  [[ "${code}" == "204" ]] || fail "$1. 应拿到 204，实际 ${code}"
  grep -q '"cmd":"connect","atyp":3,"host":"generate.test","port":80,"result":"204"' "${UPLOG}" || fail "$1. 开发上游应收到域名形式的 CONNECT"
  ok "$1. curl 经整条链路拿到 204；开发上游收到的是域名 generate.test（ATYP=3），本机和虚拟机都没有解析它"
}
R0="$(netstats)"
c1 c1
delta_log c1 "${R0}" "$(netstats)"
DNSQ_A=123401000001000000000000076578616d706c6503636f6d0000010001 # A example.com，ID 0x1234
GETHEX="$(python3 -I -c 'print(b"GET /generate_204 HTTP/1.1\r\nHost: generate.test\r\n\r\n".hex())')"
R0="$(netstats)"
RES="$(probe --exit main --net tcp --target 198.51.100.7:80 --send-hex "${GETHEX}")" || fail "probe 失败"
log "c2. TCP → 198.51.100.7:80 → ${RES}"
delta_log c2 "${R0}" "$(netstats)"
[[ "$(json "${RES}" 'd["recv_hex"]')" == "$(python3 -I -c 'print(b"HTTP/1.1 204".hex())')"* ]] || fail "c2. 应收到 HTTP/1.1 204"
ok "c2. TCP 正例（原 4e-2 的 d）：经 mihomo 到开发上游，收到 204"
if [[ "${REPRO_UDP_LEAK:-0}" == "1" ]]; then
  # 修复前的复现（architect 2026-11-18）：节点不支持 UDP 时 mihomo 会跳过 MATCH,CAGE 落到 DIRECT。
  R0="$(netstats)"
  RES="$(probe --exit main --net udp --target 223.5.5.5:53 --send-hex "${DNSQ_A}")" || fail "probe 失败"
  R1="$(netstats)"
  log "复现：UDP → 223.5.5.5:53（DNS 查询）→ ${RES}"
  log "复现：netstats ${R0} → ${R1}"
  log "复现：dial_ok-dial_ok_loopback 增量 $(( ($(stat_of "${R1}" dial_ok) - $(stat_of "${R1}" dial_ok_loopback)) - ($(stat_of "${R0}" dial_ok) - $(stat_of "${R0}" dial_ok_loopback)) ))，policy_denied 增量 $(($(stat_of "${R1}" policy_denied) - $(stat_of "${R0}" policy_denied)))"
  log "复现结束（REPRO_UDP_LEAK=1 只跑到这里）"
  exit 0
fi
# c3a：路线不转 UDP（第一跳本机回环，默认）。MATCH,REJECT 兜底，UDP 不能落到 DIRECT 从本机真实 IP 出去。
# 目标用真实的公网 DNS（223.5.5.5:53）：修复前这里会拿到真实应答（证据 stage-a-20261118-150341.txt）。
R0="$(netstats)"
RES="$(probe --exit main --net udp --target 223.5.5.5:53 --send-hex "${DNSQ_A}" --read-timeout 3s)" || fail "probe 失败"
sleep 0.5
R1="$(netstats)"
log "c3a. UDP → 223.5.5.5:53（DNS 查询，路线 udp=false）→ ${RES}"
delta_log c3a "${R0}" "${R1}"
[[ "$(json "${RES}" 'd["recv_hex"]')" == "" ]] || fail "c3a. 不应收到任何应答（收到说明 UDP 直连出去了）"
(($(ext_delta "${R0}" "${R1}") == 0 && $(st_delta "${R0}" "${R1}" policy_denied) == 0)) ||
  fail "c3a. 不应有绑物理网卡的向外拨号，也不应有被策略拦下的拨号"
ok "c3a. 路线不转 UDP 时，UDP 被 MATCH,REJECT 拒绝：没有应答，dial_ok−dial_ok_loopback +0，policy_denied +0"

# c3b：--loopback-udp 打开后，路线转 UDP，经 mihomo 的 UDP ASSOCIATE 到开发上游并回显。
APPLIED="$("${BIN}/abroad-dev" apply-main-proxy --nodes "${NODES}" --selected exit --loopback-udp --ctl-sock "${STATE}/ctl.sock")" || fail "c3b. 下发失败"
log "c3b. ${APPLIED}"
[[ "${APPLIED}" == "applied udp=true" ]] || fail "c3b. --loopback-udp 时应为 applied udp=true"
UPN="$(wc -l <"${UPLOG}" | tr -d ' ')"
R0="$(netstats)"
RES="$(probe --exit main --net udp --target 198.51.100.7:53 --send-hex abcd)" || fail "probe 失败"
sleep 0.5
R1="$(netstats)"
log "c3b. UDP → 198.51.100.7:53（路线 udp=true）→ ${RES}"
delta_log c3b "${R0}" "${R1}"
if ! [[ "$(json "${RES}" 'd["ok"]')" == "True" && "$(json "${RES}" 'd["recv_hex"]')" == "abcd" ]] ||
  ! tail -n +"$((UPN + 1))" "${UPLOG}" | grep -q '"cmd":"udp"'; then
  log "c3b 诊断：开发上游新增的请求："
  tail -n +"$((UPN + 1))" "${UPLOG}" | tee -a "${EVID}"
  log "c3b 诊断：mihomo 日志："
  ctl "http://abroad-agent/v1/logs?engine=mihomo&tail=40" | python3 -I -c 'import json,sys; print("\n".join(json.load(sys.stdin)["lines"]))' | tee -a "${EVID}"
  log "c3b 诊断：sing-box 日志："
  ctl "http://abroad-agent/v1/logs?engine=sing-box&tail=40" | python3 -I -c 'import json,sys; print("\n".join(json.load(sys.stdin)["lines"]))' | tee -a "${EVID}"
  log "c3b 诊断：data 段 $(json "$(health)" 'd["data"]')"
  fail "c3b. UDP 应经开发上游原样回显，且开发上游记一条 udp"
fi
(($(ext_delta "${R0}" "${R1}") == 0)) || fail "c3b. dial_ok 增量应等于 dial_ok_loopback 增量（只拨本机回环）"
ok "c3b. UDP 正例：经 mihomo 的 UDP ASSOCIATE 到开发上游并回显；开发上游记了 udp；只拨了本机回环"
UPN="$(wc -l <"${UPLOG}" | tr -d ' ')"
R0="$(netstats)"
RES="$(probe --exit main --net udp --target udp.test:53 --send-hex abcd)" || fail "probe 失败"
sleep 0.5
R1="$(netstats)"
log "c4（路线 udp=true）. UDP → udp.test:53（域名目标）→ ${RES}"
delta_log c4 "${R0}" "${R1}"
log "c4 期间开发上游新增的请求："
tail -n +"$((UPN + 1))" "${UPLOG}" | tee -a "${EVID}"
# mihomo 对 UDP 的域名目标先在本地解析：经 nameserver #CAGE 的 DoH，也就是经主代理 CONNECT 1.1.1.1:443。
# 开发上游不会 TLS，解析失败，所以没有回显。要断言的是：没有直连解析，开发上游只看到这类 CONNECT。
(($(ext_delta "${R0}" "${R1}") == 0)) || fail "c4. 域名 UDP 目标不应有绑物理网卡的向外拨号"
OTHER="$(tail -n +"$((UPN + 1))" "${UPLOG}" | grep -v '"cmd":"connect","atyp":1,"host":"1.1.1.1","port":443,' || true)"
[[ -z "${OTHER}" ]] || fail "c4. 开发上游新增的请求只能是 connect 1.1.1.1:443（经主代理的 DoH）"
ok "c4. 域名 UDP 目标经主代理的 DoH 解析（开发上游只收到 connect 1.1.1.1:443），没有直连"
KB="$(json "$(health)" 'd["boot_id"]')"
CODE="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 3 --unix-socket "${STATE}/ctl.sock" -X POST http://abroad-agent/v1/shutdown)"
[[ "${CODE}" == "202" ]] || fail "c5. 请求关机应回 202"
DOWN=0
for _ in $(seq 1 40); do if ! health >/dev/null; then DOWN=1; break; fi; sleep 0.25; done
[[ ${DOWN} -eq 1 ]] || fail "c5. 关机请求后 health 一直可达"
wait_health 90 || fail "c5. 关机后没有自动重启"
[[ "$(json "${HEALTH}" 'd["boot_id"]')" != "${KB}" ]] || fail "c5. boot_id 应改变"
[[ "$(json "${HEALTH}" 'd["main_proxy"]["configured"]')" == "False" ]] || fail "c5. 重启后主代理配置应已消失"
for _ in $(seq 1 40); do [[ "$(json "$(health)" 'd["engines"]["sing-box"]["state"]')" == "running" ]] && break; sleep 0.25; done
R0="$(netstats)"
"${BIN}/abroad-dev" apply-main-proxy --nodes "${NODES}" --selected exit --ctl-sock "${STATE}/ctl.sock" >/dev/null || fail "c5. 重新下发失败"
c1 c5
delta_log c5 "${R0}" "$(netstats)"
# c6（实测 b，只记录现象）：链式路线的第二跳是域名时，mihomo 是在虚拟机里本地解析（经网关 DNS，
# 没登记会得到 REFUSED），还是把域名交给前一跳（开发上游会收到带域名的 connect）。
CHAIN="${OUT}/dev-chain-${TS}.yaml"
printf 'proxies:\n - {name: hop1, type: socks5, server: 127.0.0.1, port: %s, udp: true}\n - {name: exit, type: socks5, server: second.example.test, port: 1080}\n' "${UPP}" >"${CHAIN}"
"${BIN}/abroad-dev" apply-main-proxy --nodes "${CHAIN}" --selected exit --chain hop1,exit --ctl-sock "${STATE}/ctl.sock" >/dev/null || fail "c6. 下发链式配置失败"
UPN="$(wc -l <"${UPLOG}" | tr -d ' ')"
R0="$(netstats)"
RES="$(probe --exit main --net tcp --target 198.51.100.7:80 --send-hex "${GETHEX}" --read-timeout 5s)" || fail "probe 失败"
sleep 0.5
R1="$(netstats)"
log "c6（只记录）. 链式 [开发上游 → second.example.test:1080]，TCP → 198.51.100.7:80 → ${RES}"
delta_log c6 "${R0}" "${R1}"
log "c6 网关 DNS 增量：dns_answered +$(st_delta "${R0}" "${R1}" dns_answered)，dns_refused +$(st_delta "${R0}" "${R1}" dns_refused)，dns_failed +$(st_delta "${R0}" "${R1}" dns_failed)"
# 开发上游读不出 HTTP 时要等到 10 秒超时才记这一行（result=closed），这里最多等 15 秒。
for _ in $(seq 1 60); do tail -n +"$((UPN + 1))" "${UPLOG}" | grep -q 'second.example.test' && break; sleep 0.25; done
log "c6 期间开发上游新增的请求："
tail -n +"$((UPN + 1))" "${UPLOG}" | tee -a "${EVID}"
if tail -n +"$((UPN + 1))" "${UPLOG}" | grep -q '"atyp":3,"host":"second.example.test"'; then
  log "c6 现象：第二跳的域名原样交给了前一跳（开发上游收到 ATYP=3 的 connect），虚拟机里没有解析"
else
  log "c6 现象：开发上游没有收到带域名的 connect（见上面的网关 DNS 增量）"
fi
C1="$(netstats)"; log "阶段 C 结束时 netstats：${C1}"
DOK=$(($(stat_of "${C1}" dial_ok) - $(stat_of "${C0}" dial_ok)))
DLO=$(($(stat_of "${C1}" dial_ok_loopback) - $(stat_of "${C0}" dial_ok_loopback)))
DPD=$(($(stat_of "${C1}" policy_denied) - $(stat_of "${C0}" policy_denied)))
DBF=$(($(stat_of "${C1}" bind_failed) - $(stat_of "${C0}" bind_failed)))
((DOK == DLO && DPD == 0 && DBF == 0)) || fail "阶段 C 不应有任何绑物理网卡的向外拨号（dial_ok +${DOK}，dial_ok_loopback +${DLO}，policy_denied +${DPD}，bind_failed +${DBF}）"
ok "阶段 C 没有一次绑物理网卡的向外拨号（dial_ok +${DOK} == dial_ok_loopback +${DLO}，policy_denied +0，bind_failed +0）"
section "7e. 阶段 E（tunsim：用户态模拟 本机应用 → utun → 网络栈 → data.sock → 虚拟机）"
# abroad-dev tunsim 在进程内用内存管道代替 utun（不需要 root、不碰宿主网络，只连 data.sock），
# 依次跑 DNS、TCP（假地址）、UDP、TCP（真实 IP）、无映射被拒、关闸门、关闭七步（internal/tunsim）。
# 同一台虚拟机，主代理仍是开发上游；c6 留下的是链式配置，先重新下发转 UDP 的配置。
APPLIED="$("${BIN}/abroad-dev" apply-main-proxy --nodes "${NODES}" --selected exit --loopback-udp --ctl-sock "${STATE}/ctl.sock")" || fail "E. 下发主代理失败"
[[ "${APPLIED}" == "applied udp=true" ]] || fail "E. 应为 applied udp=true，实际 ${APPLIED}"
# 下发主代理会重启虚拟机里的 mihomo：先循环探测到一次 204（最多 10 秒）再开始各步（M2-9c 第 0 步；
# 之前偶发过第一条连接拿到 0 字节 EOF）。
wait204() {
  local i res
  for i in $(seq 1 20); do
    res="$(probe --exit main --net tcp --target 198.51.100.7:80 --send-hex "${GETHEX}" --read-timeout 2s 2>/dev/null || true)"
    if [[ "$(json "${res:-{\}}" 'd.get("recv_hex","")' 2>/dev/null)" == "$(python3 -I -c 'print(b"HTTP/1.1 204".hex())')"* ]]; then
      log "$1. 主代理就绪（第 ${i} 次探测拿到 204）"
      return 0
    fi
    sleep 0.5
  done
  fail "$1. 下发主代理后 10 秒内没有拿到 204"
}
wait204 E
E0="$(netstats)"
T0="$(json "$(health)" 'd["data"]["total_streams"]')"
UPN="$(wc -l <"${UPLOG}" | tr -d ' ')"
TSIM_RC=0
# UDP 那一步发往真实 IP：主代理是开发上游时，mihomo 对 UDP 域名目标会先经主代理的 DoH 解析，开发上游答不了，
# 包会被丢（design 7.2、阶段 C 的 c4）。“UDP 域名目标真机端到端回显”留到真实远端节点验证。
TSIM="$("${BIN}/abroad-dev" tunsim --data-sock "${STATE}/data.sock" --udp-target 198.51.100.7:4500 --timeout 15s 2>>"${EVID}")" || TSIM_RC=$?
log "E. tunsim 报告：${TSIM}"
sleep 0.5
E1="$(netstats)"
T1="$(json "$(health)" 'd["data"]["total_streams"]')"
delta_log E "${E0}" "${E1}"
NEWUP="$(tail -n +"$((UPN + 1))" "${UPLOG}")"
log "E. 开发上游新增的请求："
printf '%s\n' "${NEWUP}" | tee -a "${EVID}"
tsim_diag() {
  log "E 诊断：mihomo 日志："
  ctl "http://abroad-agent/v1/logs?engine=mihomo&tail=40" | python3 -I -c 'import json,sys; print("\n".join(json.load(sys.stdin)["lines"]))' | tee -a "${EVID}"
  log "E 诊断：sing-box 日志："
  ctl "http://abroad-agent/v1/logs?engine=sing-box&tail=40" | python3 -I -c 'import json,sys; print("\n".join(json.load(sys.stdin)["lines"]))' | tee -a "${EVID}"
  log "E 诊断：data 段 $(json "$(health)" 'd["data"]')"
}
if [[ ${TSIM_RC} -ne 0 || "$(json "${TSIM}" 'd["ok"] and all(s["ok"] for s in d["steps"]) and len(d["steps"])==7 and "c_udp_realip" in [s["name"] for s in d["steps"]]')" != "True" ]]; then
  tsim_diag
  fail "E. tunsim 应退出 0 且七步全部 ok（退出码 ${TSIM_RC}）"
fi
ok "E. tunsim 七步全部 ok（DNS、TCP 假地址、UDP 真实 IP、TCP 真实 IP、无映射被拒、关闸门、关闭）"
for want in '"cmd":"connect","atyp":3,"host":"tunsim.test","port":80,"result":"204"' \
  '"cmd":"udp","atyp":1,"host":"198.51.100.7","port":4500,"result":"echo"' \
  '"cmd":"connect","atyp":1,"host":"203.0.113.9","port":80,"result":"204"'; do
  if ! printf '%s\n' "${NEWUP}" | grep -qF "${want}"; then
    tsim_diag
    fail "E. 开发上游应收到 ${want}"
  fi
done
if printf '%s\n' "${NEWUP}" | grep -qF '"host":"1.1.1.1","port":443'; then
  tsim_diag
  fail "E. 这次不应走域名 UDP 那条路（开发上游不应收到 connect 1.1.1.1:443）"
fi
ok "E. 开发上游收到：域名 tunsim.test 的 CONNECT（204）、198.51.100.7:4500 的 UDP（回显）、真实 IP 203.0.113.9 的 CONNECT（204）；没有 connect 1.1.1.1:443"
(($(ext_delta "${E0}" "${E1}") == 0 && $(st_delta "${E0}" "${E1}" policy_denied) == 0 && $(st_delta "${E0}" "${E1}" bind_failed) == 0)) ||
  fail "E. 不应有绑物理网卡的向外拨号（dial_ok−dial_ok_loopback 应 +0，policy_denied、bind_failed 应 +0）"
ok "E. 没有一次绑物理网卡的向外拨号（dial_ok +$(st_delta "${E0}" "${E1}" dial_ok) == dial_ok_loopback +$(st_delta "${E0}" "${E1}" dial_ok_loopback)）"
OB="$(json "${TSIM}" 'd["opens_before_gate"]')"
OA="$(json "${TSIM}" 'd["opens_after_gate"]')"
log "E. data.total_streams ${T0} → ${T1}（增量 $((T1 - T0))）；tunsim 开流 关闸门前 ${OB}、关闸门后 ${OA}"
[[ "${OA}" == "${OB}" && $((T1 - T0)) -eq ${OA} ]] || fail "E. 关闸门后不应再开流：total_streams 增量应等于 opens_after_gate，且等于 opens_before_gate"
ok "E. 关闸门之后没有再开流（total_streams 增量 $((T1 - T0)) == opens_after_gate == opens_before_gate）"
for _ in $(seq 1 40); do [[ "$(json "$(health)" 'd["data"]["active_streams"]')" == "0" ]] && break; sleep 0.25; done
[[ "$(json "$(health)" 'd["data"]["active_streams"]')" == "0" ]] || fail "E. tunsim 结束后 active_streams 应回到 0"
ok "E. tunsim 结束后虚拟机里的活动流回到 0"

section "7d. 阶段 D（主代理 = 本机独立的 mihomo → 开发上游；M2-3）"
# 链路：虚拟机 mihomo → 192.168.127.254:${HMP} → 本机独立 mihomo（scripts/dev/host-mihomo.sh，只听 127.0.0.1，
#       不开 TUN/DNS/external-controller，唯一出站是开发上游，规则只有 MATCH,up）→ 开发上游。不向外拨号。
HNODES="${OUT}/dev-nodes-hostmihomo-${TS}.yaml"
printf 'proxies:\n - {name: exit, type: socks5, server: 127.0.0.1, port: %s, udp: true}\n' "${HMP}" >"${HNODES}"
APPLIED="$("${BIN}/abroad-dev" apply-main-proxy --nodes "${HNODES}" --selected exit --loopback-udp --ctl-sock "${STATE}/ctl.sock")" || fail "D. 下发主代理失败"
[[ "${APPLIED}" == "applied udp=true" ]] || fail "D. 应为 applied udp=true，实际 ${APPLIED}"
# dprobe <名字>：TCP 198.51.100.7:80 和 UDP 198.51.100.7:53 各一次，结果放 DT、DU，netstats 放 D0、D1，开发上游新增放 DNEW。
dprobe() {
  local upn
  upn="$(wc -l <"${UPLOG}" | tr -d ' ')"
  D0="$(netstats)"
  DT="$(probe --exit main --net tcp --target 198.51.100.7:80 --send-hex "${GETHEX}" --read-timeout 5s)" || fail "probe 失败"
  DU="$(probe --exit main --net udp --target 198.51.100.7:53 --send-hex abcd --read-timeout 5s)" || fail "probe 失败"
  sleep 0.5
  D1="$(netstats)"
  DNEW="$(tail -n +"$((upn + 1))" "${UPLOG}")"
  log "$1. TCP → 198.51.100.7:80 → ${DT}"
  log "$1. UDP → 198.51.100.7:53 → ${DU}"
  delta_log "$1" "${D0}" "${D1}"
  log "$1. 开发上游新增的请求："
  printf '%s\n' "${DNEW}" | tee -a "${EVID}"
}
vm_logs() { # 反例阶段会刷出大量 connection refused，正例的现场要把它们滤掉才看得到
  log "$1：虚拟机 mihomo 日志（节选；connection refused 的行只计数）："
  ctl "http://abroad-agent/v1/logs?engine=mihomo&tail=200" | python3 -I -c 'import json,sys
ls=json.load(sys.stdin)["lines"]; r=[l for l in ls if "connection refused" in l]
print("\n".join([l for l in ls if "connection refused" not in l][-25:])); print("（connection refused 的行 %d 条）" % len(r))' | tee -a "${EVID}"
}
# d-neg（反例，验收 e）：本机 mihomo 还没起，端口上没有人听。TCP、UDP 都失败，开发上游收不到，也没有退回直连。
scripts/dev/host-mihomo.sh status >/dev/null 2>&1 && fail "d-neg. 本机 mihomo 不应在运行"
dprobe d-neg
[[ "$(json "${DT}" 'd["recv_hex"]')" == "" ]] || fail "d-neg. TCP 不应收到应答"
[[ "$(json "${DU}" 'd["recv_hex"]')" == "" ]] || fail "d-neg. UDP 不应收到应答"
[[ -z "${DNEW}" ]] || fail "d-neg. 开发上游不应收到请求"
(($(ext_delta "${D0}" "${D1}") == 0)) || fail "d-neg. 不应有绑物理网卡的向外拨号（没有退回直连）"
why() { json "$1" '("回应码 " + d["code"]) if d.get("code") else ("对端关闭（EOF）" if d["eof"] else ("读超时" if d["timeout"] else "无应答"))'; }
log "d-neg. 失败原因：TCP $(why "${DT}")；UDP $(why "${DU}")"
vm_logs d-neg
ok "d-neg. 本机 mihomo 没起时 TCP、UDP 都失败，开发上游没有收到请求，dial_ok−dial_ok_loopback +0（没有退回直连）"
# d：起本机 mihomo。
HSTART="$(scripts/dev/host-mihomo.sh start "${UPP}" 2>&1)" || { log "${HSTART}"; fail "D. 本机 mihomo 启动失败"; }
printf '%s\n' "${HSTART}" | tee -a "${EVID}"
HST="$(scripts/dev/host-mihomo.sh status)" || fail "D. status 应显示在运行"
log "D. host-mihomo status：$(printf '%s' "${HST}" | tr '\n' ' ')"
printf '%s\n' "${HST}" | tail -n +2 | grep -qv " 127\.0\.0\.1:" && fail "D. 本机 mihomo 只应监听 127.0.0.1"
ok "D. 本机 mihomo 在运行，只监听 127.0.0.1:${HMP}（TCP、UDP）；配置自查全部通过（见上）"
# BND 形态（验收 h）：本机直接向它做一次 UDP ASSOCIATE，看回给客户端的 BND 地址是什么形态。
BND="$(python3 -I - "${HMP}" <<'PY'
import socket, struct, sys
s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=5)
s.sendall(b"\x05\x01\x00"); assert s.recv(2) == b"\x05\x00"
s.sendall(b"\x05\x03\x00\x01\x00\x00\x00\x00\x00\x00")
r = s.recv(64)
if r[3] == 1:
    a = socket.inet_ntoa(r[4:8]); p = struct.unpack(">H", r[8:10])[0]
elif r[3] == 4:
    a = socket.inet_ntop(socket.AF_INET6, r[4:20]); p = struct.unpack(">H", r[20:22])[0]
else:
    a = "atyp%d" % r[3]; p = -1
print("rep=%d atyp=%d bnd=%s:%d" % (r[1], r[3], a, p))
PY
)" || BND="UDP ASSOCIATE 失败"
log "D. 本机 mihomo 对 UDP ASSOCIATE 回的 BND：${BND}"
dprobe d
HLOG="scripts/dev/out/host-mihomo/mihomo.log"
log "d. 本机 mihomo 日志（节选，地址只有回环和测试网段）："
grep -E "198\.51\.100\.7|UDP|udp|error|warn" "${HLOG}" | tail -n 20 | tee -a "${EVID}" || true
[[ "$(json "${DT}" 'd["recv_hex"]')" == "$(python3 -I -c 'print(b"HTTP/1.1 204".hex())')"* ]] || { vm_logs d; fail "d. TCP 应经这条链拿到 204"; }
printf '%s\n' "${DNEW}" | grep -qF '"cmd":"connect","atyp":1,"host":"198.51.100.7","port":80,"result":"204"' || fail "d. 开发上游应收到 connect 198.51.100.7:80"
grep -q "198.51.100.7:80" "${HLOG}" || fail "d. 本机 mihomo 的日志里应有这次 TCP（请求确实经它到达）"
(($(ext_delta "${D0}" "${D1}") == 0)) || fail "d. 不应有绑物理网卡的向外拨号"
ok "d. TCP 经 虚拟机 mihomo → 本机 mihomo → 开发上游 拿到 204（本机 mihomo 日志和开发上游各记一条），dial_ok−dial_ok_loopback +0"
if [[ "$(json "${DU}" 'd["recv_hex"]')" == "abcd" ]] && printf '%s\n' "${DNEW}" | grep -qF '"cmd":"udp","atyp":1,"host":"198.51.100.7","port":53,"result":"echo"'; then
  M23_UDP="回显"
  ok "d. UDP 经 本机 mihomo 的 UDP 中继到开发上游并回显"
else
  M23_UDP="失败"
  vm_logs d
  log "d. UDP 没有回显（只记录现场，由 architect 判定）：BND ${BND}"
fi
log "M2-3 结论素材：UDP ${M23_UDP}；BND ${BND}"
# 收尾：停本机 mihomo，端口释放、PID 文件删除，再 stop 一次不报错。
scripts/dev/host-mihomo.sh stop | tee -a "${EVID}"
[[ ! -e scripts/dev/out/host-mihomo/mihomo.pid ]] || fail "D. PID 文件应已删除"
for _ in $(seq 1 20); do lsof -nP -iTCP:"${HMP}" -sTCP:LISTEN >/dev/null 2>&1 || break; sleep 0.1; done
lsof -nP -iTCP:"${HMP}" -sTCP:LISTEN >/dev/null 2>&1 && fail "D. 端口 ${HMP} 应已释放"
scripts/dev/host-mihomo.sh stop | tee -a "${EVID}" || fail "D. 再 stop 一次不应报错"
ok "D. 本机 mihomo 已停止，端口已释放，PID 文件已删除；再 stop 一次不报错"

section "7f. 阶段 9b（abroadd 的数据面接真实虚拟机：abroad-dev daemon-sim；M2-9b-2）"
# daemon-sim 在进程内起 daemon：系统层假的（FakePF、sysnet.Fake + 命令模拟器），数据面真的（网络栈、fake-IP DNS、代理、
# 拨号器），utun 是内存管道，管道另一头接 tunsim 客户端；数据通道连这台虚拟机的 data.sock。不碰宿主机网络。
APPLIED="$("${BIN}/abroad-dev" apply-main-proxy --nodes "${NODES}" --selected exit --loopback-udp --ctl-sock "${STATE}/ctl.sock")" || fail "9b. 下发主代理失败"
[[ "${APPLIED}" == "applied udp=true" ]] || fail "9b. 应为 applied udp=true，实际 ${APPLIED}"
DSIM_ST="${OUT}/ds-${TS}"
DSIM_LOG="${OUT}/daemon-sim-${TS}.log"
G0="$(netstats)"
UPN="$(wc -l <"${UPLOG}" | tr -d ' ')"
DSIM_RC=0
DSIM="$("${BIN}/abroad-dev" daemon-sim --vm-dir "${STATE}" --state-dir "${DSIM_ST}" --exit-check-host generate.test \
  --udp-target 198.51.100.7:4500 --log "${DSIM_LOG}" 2>>"${EVID}")" || DSIM_RC=$?
G1="$(netstats)"
log "9b. daemon-sim 报告：${DSIM}"
log "9b. 各时间段的开流种类（种类/出口/协议，不含目的）："
json "${DSIM}" '"\n".join("  %s：%s" % (g["name"], ", ".join("%s×%d" % (k, v) for k, v in sorted(g["kinds"].items())) or "无") for g in d["segments"])' | tee -a "${EVID}"
delta_log 9b "${G0}" "${G1}"
log "9b. 开发上游新增的请求："
tail -n +"$((UPN + 1))" "${UPLOG}" | tee -a "${EVID}"
[[ ${DSIM_RC} -eq 0 && "$(json "${DSIM}" 'd["ok"]')" == "True" ]] || fail "9b. daemon-sim 应全部通过（退出码 ${DSIM_RC}）"
[[ "$(json "${DSIM}" '[p["name"] for p in d["phases"]]')" == "['enable', 'disable', 're-enable', 'restore']" ]] || fail "9b. 阶段不全"
OPENS="$(json "${DSIM}" '[p["opens"] for p in d["phases"]]')"
log "9b. 各阶段结束时的开流次数：${OPENS}；停用生效时 $(json "${DSIM}" 'd["phases"][1]["opens_before"]')（停用之后不应增长）"
[[ "$(json "${DSIM}" 'd["phases"][1]["opens"] == d["phases"][1]["opens_before"]')" == "True" ]] || fail "9b. 停用后不应再开流"
(($(ext_delta "${G0}" "${G1}") == 0)) || fail "9b. 不应有绑物理网卡的向外拨号"
ok "9b. 启用后 DNS、假地址 TCP、UDP、真实 IP TCP 都通过；停用后同样的探测全部失败、开流次数不增长；再启用恢复（同一个假地址）；最后回到 RESTORED、系统还原；dial_ok−dial_ok_loopback +0"
# k. 日志不泄漏：探测用的域名、目的地址不出现在 daemon 日志里（假地址由 daemon-sim 自查，报告里的 log_leak）。
[[ -s "${DSIM_LOG}" ]] || fail "9b. 应有 daemon 日志"
LEAK="$(grep -nE 'dsim\.test|198\.51\.100\.7|203\.0\.113\.9|generate\.test' "${DSIM_LOG}" || true)"
[[ -z "${LEAK}" && "$(json "${DSIM}" 'd["log_leak"]')" == "False" ]] || fail "9b. daemon 日志里出现了探测用的域名、地址或假地址：${LEAK}"
ok "9b. daemon 日志（$(wc -l <"${DSIM_LOG}" | tr -d ' ') 行）里搜不到探测用的域名、目的地址和假地址"
# 闸门不应抖动：启用后到停用前，监护不应因为任何核对不通过而关闸门（M2-9c 第 0 步查出的测试台问题的回归）。
if grep -q 'supervise/gate_closed' "${DSIM_LOG}"; then
  grep 'supervise/' "${DSIM_LOG}" | tee -a "${EVID}"
  fail "9b. 监护关过闸门（闸门抖动）"
fi
ok "9b. 整个过程中监护没有关过闸门（没有抖动）"
rm -rf "${DSIM_ST}"

term_wait "${SOCKS_PID}" "abroad-dev socks"
term_wait "${UP_PID}" "abroad-dev upstream"
term_wait "${VM_PID}" "abroad-vm"
VM_PID=""
stop_listeners
[[ ! -e "${STATE}/ctl.sock" && ! -e "${STATE}/data.sock" && ! -e "${STATE}/host.sock" ]] || fail "socket 没有清理"
ok "阶段 B、C 通过；socks、upstream、abroad-vm 都以 0 退出，socket 已清理"

section "7g. 阶段 9c（多出口多应用：开发出口 wg-a、clash-b，真实的直连拨号模块；M3-7）"
# 开发构建的 abroad-vm（-tags abroaddev 才有 --dev-agent：内核命令行加 abroad.dev=1，agent 注册 /v1/dev-exits）。
with_timeout 600 env CGO_ENABLED=1 go build -tags abroaddev -o "${BIN}/abroad-vm-dev" ./cmd/abroad-vm || fail "9c. 构建开发版 abroad-vm"
"${ROOT}/scripts/dev/sign.sh" "${BIN}/abroad-vm-dev" >>"${EVID}" 2>&1 || fail "9c. 签名开发版 abroad-vm"
# 三个开发上游：/whoami 回各自的名字（main 是主代理，wg-a、clash-b 是开发出口）。
# 本机的 bash 是 3.2，没有关联数组：三个端口各一个变量。
# 不用 $(...)：子 shell 里登记的 LISTENER_PIDS 带不回来，结束时停不掉这些上游。
up9c() { # up9c <名字> <变量名>：起一个带名字的开发上游，把端口写进变量
  "${BIN}/abroad-dev" upstream --name "$1" --log "${OUT}/up9c-$1-${TS}.jsonl" >"${OUT}/up9c-$1-${TS}.out" 2>&1 &
  LISTENER_PIDS+=("$!")
  local a
  a="$(bg_wait_line "${OUT}/up9c-$1-${TS}.out" listening)" || return 1
  printf -v "$2" '%s' "${a##*:}"
}
up9c main UP9_MAIN || fail "9c. 开发上游 main 没有起来"
up9c wg-a UP9_A || fail "9c. 开发上游 wg-a 没有起来"
up9c clash-b UP9_B || fail "9c. 开发上游 clash-b 没有起来"
NODES9="${OUT}/dev-nodes-9c-${TS}.yaml"
printf 'proxies:\n - {name: exit, type: socks5, server: 127.0.0.1, port: %s}\n' "${UP9_MAIN}" >"${NODES9}"
# 直连 socket 的上级目录必须是自己的、0700（abroad-vm 监听前检查，architect 判 W-2a）。
D9DIR="${OUT}/d9c"
mkdir -p "${D9DIR}" && chmod 700 "${D9DIR}"
DSOCK="${D9DIR}/d.sock"
rm -f "${DSOCK}"
VM_STATE="${STATE}"
VM_LOG="${OUT}/abroad-vm-${TS}-9c.log"
"${BIN}/abroad-vm-dev" run --image-dir guest/out --state-dir "${STATE}" --dev-agent --direct-sock "${DSOCK}" \
  --allow-outbound "127.0.0.1:${UP9_MAIN}" --allow-outbound "127.0.0.1:${UP9_A}" --allow-outbound "127.0.0.1:${UP9_B}" 2>"${VM_LOG}" &
VM_PID=$!
wait_health 90 || fail "9c. 90 秒内 health 不通"
for _ in $(seq 1 40); do [[ "$(json "$(health)" 'd["engines"]["sing-box"]["state"]')" == "running" ]] && break; sleep 0.25; done
"${BIN}/abroad-dev" apply-main-proxy --nodes "${NODES9}" --selected exit --ctl-sock "${STATE}/ctl.sock" >>"${EVID}" || fail "9c. 下发主代理失败"
DEVX="$(printf '{"exits":[{"Tag":"wg-a","Port":%s,"Server":"192.168.127.254"},{"Tag":"clash-b","Port":%s,"Server":"192.168.127.254"}]}' "${UP9_A}" "${UP9_B}")"
curl -fsS --max-time 30 --unix-socket "${STATE}/ctl.sock" -X PUT -d "${DEVX}" http://abroad-agent/v1/dev-exits >>"${EVID}" || fail "9c. 设置开发出口失败"
ok "9c. 开发版虚拟机已起（开发出口 wg-a、clash-b，直连拨号模块 ${DSOCK}）"
D9_ST="${OUT}/d9c-${TS}"
D9_LOG="${OUT}/daemon-sim-9c-${TS}.log"
D9_RC=0
D9N0="$(netstats)"
# 直连目标：能经物理网卡到公网时用默认的 1.1.1.1:443，否则用前置检查选出的局域网目标（端口不是 443，客户端先发字节）。
D9_TARGET=()
[[ ${PUBLIC_OK} -eq 1 ]] || D9_TARGET=(--direct-target "${LAN_TARGET}")
D9="$("${BIN}/abroad-dev" daemon-sim --multi --vm-dir "${STATE}" --state-dir "${D9_ST}" --exit-check-host generate.test \
  --direct-sock "${DSOCK}" ${D9_TARGET[@]+"${D9_TARGET[@]}"} --log "${D9_LOG}" 2>>"${EVID}")" || D9_RC=$?
D9N1="$(netstats)"
log "9c. 报告：$(printf '%s' "${D9}" | sed -E 's/[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/<地址>/g')"
[[ ${PUBLIC_OK} -eq 1 ]] && log "9c. 直连目标：公网 1.1.1.1:443" || log "9c. 直连目标：局域网（${LT#* }；本机不能经物理网卡到公网）"
D9DOK=$(($(stat_of "${D9N1}" direct_dial_ok) - $(stat_of "${D9N0}" direct_dial_ok)))
log "9c. 直连拨号：direct_dial_ok +${D9DOK}，绑定的网卡 $(stat_of "${D9N1}" interface)，bind_failed $(stat_of "${D9N1}" bind_failed)"
((D9DOK >= 2)) || fail "9c. 直连拨号计数应增长（改规则前后各一次），实际 +${D9DOK}"
[[ "$(stat_of "${D9N1}" interface)" == "${IFC}" ]] || fail "9c. 直连拨号绑的网卡应是物理网卡 ${IFC}"
json "${D9}" '"\n".join("  %s：期望 %s，实际 %s，%s" % (c["name"], c["want"], c["got"], "通过" if c["ok"] else "不通过") for c in d["checks"])' | tee -a "${EVID}"
[[ ${D9_RC} -eq 0 && "$(json "${D9}" 'd["ok"]')" == "True" ]] || fail "9c. 多出口多应用的检查没有全部通过（退出码 ${D9_RC}）"
# 日志不泄漏：测试用的域名、目的地址、应用 ID 不出现在 daemon 日志里（daemon-sim 自查，报告里的 log_leak；这里再搜一遍）。
LEAK9="$(grep -E 'whoami\.test|claude\.ai|com\.app|1\.1\.1\.1' "${D9_LOG}" || true)"
[[ -z "${LEAK9}" && "$(json "${D9}" 'd["log_leak"]')" == "False" ]] || fail "9c. daemon 日志里出现了测试用的域名、地址或应用 ID"
ok "9c. 三个应用经出口 wg-a、出口 clash-b、直连拨号模块；改成直连的应用访问置顶域名经 main、直连计数不变；日志干净"
rm -rf "${D9_ST}"

section "7h. 阶段 10a（M4-1 探查：命名空间、veth、nft 断网规则，同机的 WireGuard、OpenVPN 对端）"
# 同一台开发版虚拟机（内核命令行带 abroad.dev=1）。探查全部在虚拟机里，不出虚拟机；也可以单独跑 scripts/dev/netns-probe.sh。
NSP="$(curl -sS --max-time 200 --unix-socket "${STATE}/ctl.sock" -X POST http://abroad-agent/v1/dev/netns-probe)" || fail "10a. 探查接口调用失败"
printf '%s\n' "${NSP}" >>"${EVID}"
! printf '%s' "${NSP}" | grep -qE 'PRIVATE KEY|BEGIN CERTIFICATE|PrivateKey' || fail "10a. 探查报告里出现了密钥或证书"
[[ "$(json "${NSP}" 'd["ok"]')" == "True" ]] || fail "10a. 探查有失败的步骤：$(json "${NSP}" '"；".join(s["name"]+"："+s.get("detail","") for s in d["steps"] if not s["ok"])')"
ok "10a. 探查全部通过（$(json "${NSP}" 'len(d["steps"])') 步）：WireGuard、OpenVPN 隧道能通，断网规则生效，断开后只能到达对端端点"
term_wait "${VM_PID}" "abroad-vm（开发版）"
VM_PID=""
stop_listeners
[[ ! -e "${DSOCK}" ]] || fail "9c. 直连 socket 没有清理"
rmdir "${D9DIR}" 2>/dev/null || true

if [[ ${PANIC} -eq 1 ]]; then
  section "7. panic 探测（只记录现象，不判通过与否）"
  PIMG="${OUT}/panic-img"
  PSTATE="${OUT}/state-panic"
  rm -rf "${PIMG}" "${PSTATE}/console.log"
  mkdir -p "${PIMG}"
  for f in Image rootfs.img modloop; do ln -s "${ROOT}/guest/out/${f}" "${PIMG}/${f}"; done
  # 只有 TRAILER!!! 的 newc cpio：内核找不到 /init 也没有根盘参数，会 panic。
  python3 -I -c 'import gzip,sys
f=["00000000"]*13; f[4]="00000001"; f[11]="0000000B"
h=b"070701"+"".join(f).encode()+b"TRAILER!!!\0"; h+=b"\0"*((4-len(h)%4)%4)
sys.stdout.buffer.write(gzip.compress(h))' >"${PIMG}/initramfs.gz"
  start_vm "${PIMG}" "${PSTATE}"
  sleep 15
  section "panic 停住期间的 CPU（ps 只读查询）"
  ps -axo pid=,user=,%cpu=,comm= | grep -E "abroad-vm|com.apple.Virtualization.VirtualMachine" | grep -v grep | tee -a "${EVID}" || true
  sleep 25
  if kill -0 "${VM_PID}" 2>/dev/null; then
    log "40 秒后 abroad-vm 仍在运行"
  else
    log "abroad-vm 已退出"
  fi
  STOPS="$(grep -c "虚拟机意外停止" "${VM_LOG}" || true)"
  WD="$(grep -c "health 仍不通" "${VM_LOG}" || true)"
  PANICS="$(grep "Kernel panic - not syncing" "${PSTATE}/console.log" | grep -vc "end Kernel panic" || true)"
  log "40 秒内：内核 panic ${PANICS} 次，虚拟机意外停止 ${STOPS} 次，看门狗强停 ${WD} 次"
  grep -E "虚拟机" "${VM_LOG}" | head -n 20 | tee -a "${EVID}"
  section "panic 探测的控制台日志（节选）"
  grep -E "Kernel panic|Rebooting" "${PSTATE}/console.log" | head -n 4 | tee -a "${EVID}" || true
  T7="$(now_ms)"
  stop_vm
  log "停止 panic 探测轮用时 $(($(now_ms) - T7)) 毫秒"
  ps -p "$(cat "${PSTATE}/abroad-vm.pid" 2>/dev/null || echo 1)" -o comm= 2>/dev/null | grep -q abroad-vm && fail "panic 探测轮的 abroad-vm 仍在运行"
  ok "panic 探测轮已停止"
fi

section "用户环境（M2-3 验收 c）"
ENV1="$(env_snap)"
log "开始：$(printf '%s' "${ENV0}" | tr '\n' ' ')"
log "结束：$(printf '%s' "${ENV1}" | tr '\n' ' ')"
[[ "${ENV0}" == "${ENV1}" ]] || fail "用户环境变了（mihomo/clash 进程、1.1.1.1 的出口接口或系统代理设置）"
ok "用户环境没变：除本脚本启动的之外，mihomo/clash 进程 PID 集合、route -n get 1.1.1.1 的接口、scutil --proxy 的输出都与开始时相同"
# 模式拆成几段拼起来，免得这一行自己被匹配到（清单见 m2-plan M2-3 验收 c）。
BANPAT='p''kill|kil''lall|su''do|network''setup|pf''ctl|if''config|rou''te (-n )?(add|delete|change)'
BANNED="$(grep -vE '^[[:space:]]*#' scripts/dev/host-mihomo.sh scripts/dev/vm-up.sh | grep -nE "${BANPAT}" || true)"
[[ -z "${BANNED}" ]] || fail "脚本源码（非注释行）里出现了禁用的命令：${BANNED}"
ok "host-mihomo.sh、vm-up.sh 的非注释行里没有禁用的命令（结束进程按名字、提权、改网卡/路由/防火墙）"

section "结果"
pgrep -x abroad-vm >/dev/null && log "注意：系统里还有名为 abroad-vm 的进程（不是本脚本启动的不处理）" || ok "没有残留的 abroad-vm 进程"
ok "阶段 A 通过"
echo "证据文件：${EVID}"
