#!/usr/bin/env bash
# M4-8 的真实虚拟机阶段（m4-plan v2.11 验收 j）：分流出口经 daemon 的正式接口（PUT /v1/exits/<标签>）增加，
# daemon 把端点集合交给 abroad-vm（host.sock）、把出口交给 agent（ctl.sock），三个应用各走一个出口；
# 删除被规则引用的出口被要求先改规则；隧道断开后回退主代理，恢复后走回去。
# 对端是同一虚拟机里的命名空间（agent 的 /v1/dev/exit-peer）；主代理是本机回环上的开发上游。
# daemon 跑在 daemon-sim 里（假系统层：不碰 pf、路由、DNS）。以当前用户运行，不需要 sudo，不改宿主机网络。
#
#   scripts/dev/exits-daemon-e2e.sh            # 构建后跑
#   scripts/dev/exits-daemon-e2e.sh --no-build # 用已有的构建
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
[ "$(id -u)" != 0 ] || { echo "不要用 root 运行" >&2; exit 2; }
BUILD=1
[ "${1:-}" = "--no-build" ] && BUILD=0
OUT="${ROOT}/scripts/dev/out"
BIN="${OUT}/bin"
TS="$(date +%Y%m%d-%H%M%S)"
EVID="${OUT}/exits-daemon-e2e-${TS}.txt"
STATE="${OUT}/exitsd"
DST="${OUT}/dsx-${TS}"
VM_LOG="${OUT}/abroad-vm-${TS}-exitsd.log"
D_LOG="${OUT}/daemon-sim-exits-${TS}.log"
SECRETS="${STATE}/exits-${TS}.json"
mkdir -p "${BIN}" "${STATE}"
VM_PID=""
UP_PID=""
FAILS=0
log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*" | tee -a "${EVID}"; }
ok() { log "✓ $*"; }
bad() { log "✗ $*"; FAILS=$((FAILS + 1)); }
fail() { log "✗ 失败：$*"; exit 1; }
json() { python3 -I -c 'import json,sys; d=json.loads(sys.argv[1]); print(eval(sys.argv[2]))' "$1" "$2"; }
ctl() { curl -sS -m 120 --unix-socket "${STATE}/ctl.sock" "$@"; }
netstats() { curl -fsS --max-time 3 --unix-socket "${STATE}/host.sock" http://abroad-vm/v1/netstats; }
cleanup() {
  rm -f "${SECRETS}"
  rm -rf "${DST}"
  if [ -n "${VM_PID}" ]; then
    kill -TERM "${VM_PID}" 2>/dev/null || true
    for _ in $(seq 1 15); do kill -0 "${VM_PID}" 2>/dev/null || break; sleep 1; done
    kill -KILL "${VM_PID}" 2>/dev/null || true
    wait "${VM_PID}" 2>/dev/null || true
  fi
  if [ -n "${UP_PID}" ]; then kill "${UP_PID}" 2>/dev/null || true; wait "${UP_PID}" 2>/dev/null || true; fi
}
trap cleanup EXIT

log "# M4-8 分流出口经 daemon 的真实虚拟机阶段，macOS $(sw_vers -productVersion)，Go $(go version | awk '{print $3}')"
if [ "${BUILD}" = 1 ]; then
  log "构建 abroad-agent、镜像、开发版 abroad-vm、abroad-dev"
  env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -tags abroaddev -trimpath -ldflags "-s -w" -o guest/out/abroad-agent ./cmd/abroad-agent || fail "构建 abroad-agent"
  go run ./cmd/abroad-imagebuild build -agent guest/out/abroad-agent >>"${EVID}" 2>&1 || fail "构建镜像"
  go run ./cmd/abroad-imagebuild verify >>"${EVID}" 2>&1 || fail "镜像检查"
  env CGO_ENABLED=1 go build -tags abroaddev -o "${BIN}/abroad-vm-dev" ./cmd/abroad-vm || fail "构建 abroad-vm-dev"
  scripts/dev/sign.sh "${BIN}/abroad-vm-dev" >>"${EVID}" 2>&1 || fail "签名 abroad-vm-dev"
  go build -o "${BIN}/abroad-dev" ./cmd/abroad-dev || fail "构建 abroad-dev"
fi

log "起主代理用的开发上游"
"${BIN}/abroad-dev" upstream --name main --log "${OUT}/upx-main-${TS}.jsonl" >"${OUT}/upx-main-${TS}.out" 2>&1 &
UP_PID=$!
UP=""
for _ in $(seq 1 50); do UP="$(grep -o 'listening 127.0.0.1:[0-9]*' "${OUT}/upx-main-${TS}.out" | head -1 | sed 's/.*://')"; [ -n "${UP}" ] && break; sleep 0.1; done
[ -n "${UP}" ] || fail "开发上游没有起来"
NODES="${OUT}/upx-nodes-${TS}.yaml"
printf 'proxies:\n - {name: exit, type: socks5, server: 127.0.0.1, port: %s}\n' "${UP}" >"${NODES}"

log "起虚拟机（开发标记；出站收紧，只放行开发上游）"
rm -f "${STATE}/console.log"
"${BIN}/abroad-vm-dev" run --image-dir guest/out --state-dir "${STATE}" --dev-agent --allow-outbound "127.0.0.1:${UP}" 2>"${VM_LOG}" &
VM_PID=$!
up=0
for _ in $(seq 1 180); do
  kill -0 "${VM_PID}" 2>/dev/null || { tail -n 30 "${VM_LOG}" | tee -a "${EVID}"; fail "abroad-vm 提前退出"; }
  ctl http://abroad-agent/v1/health >/dev/null 2>&1 && { up=1; break; }
  sleep 0.5
done
[ $up = 1 ] || fail "90 秒内 health 不通"
for _ in $(seq 1 40); do [ "$(json "$(ctl http://abroad-agent/v1/health)" 'd["engines"]["sing-box"]["state"]')" = running ] && break; sleep 0.25; done
"${BIN}/abroad-dev" apply-main-proxy --nodes "${NODES}" --selected exit --ctl-sock "${STATE}/ctl.sock" >>"${EVID}" 2>&1 || fail "下发主代理失败"
S0="$(netstats)" || fail "读不到 netstats"
D0="$(json "${S0}" 'd["dial_ok"]-d["dial_ok_loopback"]')"
log "宿主绑物理网卡的拨号起点 ${D0}；出站模式 $(json "${S0}" 'd["outbound_mode"]')"

log "建同机对端（WireGuard、OpenVPN 服务端，socks 节点）"
( umask 077; ctl -X POST "http://abroad-agent/v1/dev/exit-peer?action=up" >"${SECRETS}" )
grep -q '"exits"' "${SECRETS}" || fail "建对端失败"
KEY="$(python3 -I -c 'import json,sys,re; d=json.load(open(sys.argv[1])); print(re.search(r"PrivateKey = (\S+)", d["exits"][0]["conf"]).group(1))' "${SECRETS}")"

log "daemon-sim --exits（正式的出口接口、下发、按出口检查、回退）"
REP="$("${BIN}/abroad-dev" daemon-sim --vm-dir "${STATE}" --state-dir "${DST}" --exit-check-host generate.test \
  --exits "${SECRETS}" --log "${D_LOG}" --timeout 20s 2>>"${EVID}")"
RC=$?
json "${REP}" '"\n".join("  %s：期望 %s，实际 %s，%s" % (c["name"], c["want"], c["got"], "通过" if c["ok"] else "不通过") for c in d["checks"])' | tee -a "${EVID}"
[ -z "$(json "${REP}" 'd.get("err","")')" ] || log "错误：$(json "${REP}" 'd.get("err")')"
[ ${RC} -eq 0 ] && [ "$(json "${REP}" 'd["ok"]')" = True ] && ok "daemon-sim 的分流出口阶段全部通过" || bad "daemon-sim 的分流出口阶段有不通过的项（退出码 ${RC}）"
[ "$(json "${REP}" 'd["log_leak"]')" = False ] && ok "daemon 日志里没有测试密钥、端点地址、应用 ID" || bad "daemon 日志里有不该出现的内容"
S1="$(netstats)"
log "宿主出站白名单里分流出口的端点：$(json "${S1}" 'd.get("exit_endpoint_count")') 个（wg 的 UDP 51820、ovpn 的 UDP 1194、sub 的节点 TCP 1081）"
[ "$(json "${S1}" 'd.get("exit_endpoint_count")')" = 3 ] && ok "daemon 把三个出口的端点（精确到协议和端口）交给了 abroad-vm" || bad "端点集合不对"
D1="$(json "${S1}" 'd["dial_ok"]-d["dial_ok_loopback"]')"
[ "${D1}" = "${D0}" ] && ok "全过程宿主绑物理网卡的拨号数不变（${D0} → ${D1}）" || bad "宿主绑物理网卡的拨号数变了（${D0} → ${D1}）"
LEAK=0
for f in "${D_LOG}" "${VM_LOG}" "${STATE}/console.log"; do grep -qF -- "${KEY}" "$f" 2>/dev/null && LEAK=1; done
[ ${LEAK} = 0 ] && ok "测试密钥不在 daemon 日志、abroad-vm 日志、控制台日志里" || bad "测试密钥出现在日志里"
ctl -X POST "http://abroad-agent/v1/dev/exit-peer?action=down" >/dev/null

log "停虚拟机（SIGTERM）"
kill -TERM "${VM_PID}" 2>/dev/null
for _ in $(seq 1 15); do kill -0 "${VM_PID}" 2>/dev/null || break; sleep 1; done
wait "${VM_PID}" 2>/dev/null
VRC=$?
VM_PID=""
[ ${VRC} = 0 ] && ok "abroad-vm 以 0 退出" || bad "abroad-vm 的退出码是 ${VRC}"
if [ ${FAILS} = 0 ]; then log "✓ 全部通过；证据 ${EVID}"; else log "✗ ${FAILS} 项不通过；证据 ${EVID}"; exit 1; fi
