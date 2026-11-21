#!/usr/bin/env bash
# M4-1 的探查（m4-plan M4-1，风险 R19、R5）：在真实虚拟机里建命名空间、veth、nft 断网规则，
# 起同机的 WireGuard 和 OpenVPN 对端，各通一条隧道，再断开验证断网规则。
# 以当前用户运行，不需要 sudo，不改宿主机网络：探查全部在虚拟机里，不出虚拟机（本机拒绝绑物理网卡时也能跑）。
#
#   scripts/dev/netns-probe.sh            # 构建镜像（带 M4-1 的软件包）、开发版 abroad-vm，起虚拟机，跑探查
#   scripts/dev/netns-probe.sh --no-build # 用已有的 guest/out 和 scripts/dev/out/bin/abroad-vm-dev
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
[ "$(id -u)" != 0 ] || { echo "不要用 root 运行" >&2; exit 2; }
BUILD=1
[ "${1:-}" = "--no-build" ] && BUILD=0
OUT="${ROOT}/scripts/dev/out"
BIN="${OUT}/bin"
TS="$(date +%Y%m%d-%H%M%S)"
EVID="${OUT}/netns-probe-${TS}.txt"
STATE="${OUT}/nsprobe"
VM_LOG="${OUT}/abroad-vm-${TS}-nsprobe.log"
REPORT="${OUT}/netns-probe-${TS}.json"
mkdir -p "${BIN}" "${STATE}"
VM_PID=""
log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*" | tee -a "${EVID}"; }
fail() { log "✗ 失败：$*"; exit 1; }
# 停虚拟机用 SIGTERM（和 vm-up 一样）：abroad-vm 收到后经 /v1/shutdown 优雅关机并退出。
# 只经 ctl.sock 发 /v1/shutdown 会被监护当成意外退出，1 秒后重启虚拟机（M1-4a 的设计）。
stop_vm() {
  [ -n "${VM_PID}" ] || return 0
  kill -TERM "${VM_PID}" 2>/dev/null || true
  for _ in $(seq 1 15); do kill -0 "${VM_PID}" 2>/dev/null || break; sleep 1; done
  kill -0 "${VM_PID}" 2>/dev/null && { log "abroad-vm 15 秒内没有退出，发 SIGKILL"; kill -KILL "${VM_PID}" 2>/dev/null || true; }
  wait "${VM_PID}" 2>/dev/null || true
  VM_PID=""
}
trap stop_vm EXIT

log "# M4-1 探查，macOS $(sw_vers -productVersion)，Go $(go version | awk '{print $3}')"
if [ "${BUILD}" = 1 ]; then
  log "构建 abroad-agent、镜像（带 M4-1 的软件包）"
  env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o guest/out/abroad-agent ./cmd/abroad-agent || fail "构建 abroad-agent"
  go run ./cmd/abroad-imagebuild build -agent guest/out/abroad-agent 2>&1 | tee -a "${EVID}"
  [ "${PIPESTATUS[0]}" -eq 0 ] || fail "构建镜像"
  go run ./cmd/abroad-imagebuild verify 2>&1 | tee -a "${EVID}"
  [ "${PIPESTATUS[0]}" -eq 0 ] || fail "镜像检查"
  log "构建开发版 abroad-vm（-tags abroaddev，带 --dev-agent）"
  env CGO_ENABLED=1 go build -tags abroaddev -o "${BIN}/abroad-vm-dev" ./cmd/abroad-vm || fail "构建 abroad-vm-dev"
  scripts/dev/sign.sh "${BIN}/abroad-vm-dev" >>"${EVID}" 2>&1 || fail "签名 abroad-vm-dev"
fi
python3 -I -c 'import json,sys; d=json.load(open(sys.argv[1])); p=d.get("packages") or {}; print("镜像里的软件包：%d 个（openvpn %s、wireguard-tools-wg %s、nftables %s）" % (len(p), p.get("openvpn"), p.get("wireguard-tools-wg"), p.get("nftables")))' guest/out/build.json | tee -a "${EVID}"

log "起虚拟机（开发标记：内核命令行带 abroad.dev=1）"
rm -f "${STATE}/console.log"
"${BIN}/abroad-vm-dev" run --image-dir guest/out --state-dir "${STATE}" --dev-agent 2>"${VM_LOG}" &
VM_PID=$!
ok=0
for _ in $(seq 1 180); do
  kill -0 "${VM_PID}" 2>/dev/null || { tail -n 30 "${VM_LOG}" | tee -a "${EVID}"; fail "abroad-vm 提前退出（日志 ${VM_LOG}）"; }
  curl -fsS -m 3 --unix-socket "${STATE}/ctl.sock" http://abroad-agent/v1/health >/dev/null 2>&1 && { ok=1; break; }
  sleep 0.5
done
[ $ok = 1 ] || { tail -n 30 "${VM_LOG}" | tee -a "${EVID}"; fail "90 秒内 health 不通"; }
log "health 可达"

log "跑探查（POST /v1/dev/netns-probe，最多 3 分钟）"
code=$(curl -sS -m 200 --unix-socket "${STATE}/ctl.sock" -X POST -o "${REPORT}" -w '%{http_code}' http://abroad-agent/v1/dev/netns-probe)
[ "${code}" = 200 ] || { cat "${REPORT}" 2>/dev/null | tee -a "${EVID}"; fail "探查接口返回 HTTP ${code}（没有开发标记时是 404）"; }
python3 -I - "${REPORT}" <<'EOF' | tee -a "${EVID}"
import json, sys
d = json.load(open(sys.argv[1]))
for s in d["steps"]:
    print("%s %-52s %6d ms  %s" % ("✓" if s["ok"] else "✗", s["name"], s["ms"], s.get("detail", "")))
if d.get("modules_failed"):
    print("加载失败的模块：", "；".join(d["modules_failed"]))
print("OpenVPN 配置的改写：", "；".join(d.get("ovpn_rewrites") or []))
print("结论：", "全部通过" if d["ok"] else "有失败的步骤")
EOF
# 报告里不应有密钥和证书（探查自己保证，这里再查一次）。
if grep -qE 'PRIVATE KEY|BEGIN CERTIFICATE|PrivateKey' "${REPORT}"; then fail "报告里出现了密钥或证书"; fi
log "日志尾部（虚拟机控制台，最后 15 行，供出错时诊断）："
tail -n 15 "${STATE}/console.log" 2>/dev/null >>"${EVID}" || true
stop_vm
if python3 -I -c 'import json,sys; sys.exit(0 if json.load(open(sys.argv[1]))["ok"] else 1)' "${REPORT}"; then
  log "✓ 探查全部通过；报告 ${REPORT}，证据 ${EVID}"
else
  fail "探查有失败的步骤；报告 ${REPORT}，证据 ${EVID}"
fi
