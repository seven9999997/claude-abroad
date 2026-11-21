#!/usr/bin/env bash
# 开发用（M2-9a 验收 j）：编出 abroadd，以 --fake-system 启动（整套系统是内存里的假实现，不碰本机），
# 用 curl 经 unix socket 走一遍 启用 → 状态 → 停用 → 再启用 → 恢复 → 状态，证据写进 scripts/dev/out/。
# 只按自己记下的 PID 结束 abroadd；以当前用户运行。
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
umask 077
[[ "$(id -u)" != "0" ]] || { echo "不要以 root 运行" >&2; exit 1; }
[[ $# -eq 0 ]] || { echo "用法：$0" >&2; exit 2; }

OUT="scripts/dev/out"
TS="$(date +%Y%m%d-%H%M%S)"
EVID="${OUT}/abroadd-fake-${TS}.txt"
ST="${OUT}/abroadd-state-${TS}"
mkdir -p "${OUT}/bin"
: >"${EVID}"
log() { echo "[$(date +%H:%M:%S)] $*" | tee -a "${EVID}"; }
fail() { log "✗ 失败：$*"; exit 1; }

go build -o "${OUT}/bin/abroadd" ./cmd/abroadd
PID=""
cleanup() {
  if [[ -n "${PID}" ]] && kill -0 "${PID}" 2>/dev/null && [[ "$(ps -p "${PID}" -o comm= 2>/dev/null)" == *abroadd ]]; then
    kill -TERM "${PID}" 2>/dev/null || true
    wait "${PID}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

"${OUT}/bin/abroadd" --fake-system --state-dir "${ST}" 2>"${OUT}/abroadd-fake-${TS}.log" &
PID=$!
SOCK="${ST}/api.sock"
for _ in $(seq 1 50); do [[ -S "${SOCK}" ]] && break; sleep 0.1; done
[[ -S "${SOCK}" ]] || fail "abroadd 没有起来"
log "abroadd 已启动（pid ${PID}，--fake-system，状态目录 ${ST}）"
log "目录权限 $(stat -f %Lp "${ST}")，socket 权限 $(stat -f %Lp "${SOCK}")"

# step <方法> <路径> <期望状态码> [请求体]
step() {
  local code body data=()
  [[ -n "${4:-}" ]] && data=(--data-binary "$4")
  body="$(curl -sS --max-time 20 --unix-socket "${SOCK}" -X "$1" ${data[@]+"${data[@]}"} -o - -w '\n%{http_code}' "http://abroadd$2")"
  code="${body##*$'\n'}"
  body="${body%$'\n'*}"
  log "$1 $2 → ${code}"
  printf '%s\n' "${body}" | python3 -I -c 'import json,sys
t=sys.stdin.read()
try: d=json.loads(t)
except ValueError: print("（非 JSON）", t.strip()[:200]); sys.exit(0)
print(json.dumps({k:v for k,v in d.items() if k not in ("events","confirm")}, ensure_ascii=False), "events:", len(d.get("events",[])))' | tee -a "${EVID}"
  [[ "${code}" == "$3" ]] || fail "$1 $2 应返回 $3"
  LAST="${body}"
}
st_is() { python3 -I -c 'import json,sys; d=json.loads(sys.argv[1]); sys.exit(0 if eval(sys.argv[2]) else 1)' "${LAST}" "$1" || fail "status 不符：$1"; }

step GET /v1/status 200;   st_is 'd["state"]=="RESTORED" and not d["gate_open"]'
step POST /v1/enable 200;  st_is 'd["state"]=="ENABLED" and d["gate_open"] and d["protected"] and d["utun"].startswith("utun")'
step GET /v1/status 200;   st_is 'd["state"]=="ENABLED" and d["gate_open"]'
step POST /v1/disable 200; st_is 'd["state"]=="DISABLED" and not d["gate_open"] and d["protected"]'
step POST /v1/enable 200;  st_is 'd["state"]=="ENABLED" and d["gate_open"]'
# 恢复要两步（m4-plan v2.3 决定 5）：第一次 409 带预览和确认码（证据里不记确认码），带确认码再调一次。
step POST /v1/restore 409; st_is 'len(d["confirm"])==32 and "processes" in d["preview"]'
CONFIRM="$(python3 -I -c 'import json,sys; print(json.loads(sys.argv[1])["confirm"])' "${LAST}")"
step POST /v1/restore 200 "{\"confirm\":\"${CONFIRM}\"}"; st_is 'd["state"]=="RESTORED" and not d["gate_open"] and not d.get("utun")'
step GET /v1/status 200;   st_is 'd["state"]=="RESTORED"'
log "最近的事件（种类）：$(python3 -I -c 'import json,sys; print(" ".join(e["source"]+"/"+e["kind"] for e in json.loads(sys.argv[1])["events"]))' "${LAST}")"

step GET /v1/nope 404

kill -TERM "${PID}"
RC=0
wait "${PID}" || RC=$?
PID=""
[[ ${RC} -eq 0 ]] || fail "abroadd 退出码 ${RC}"
[[ ! -e "${SOCK}" ]] || fail "socket 应已删除"
log "✓ 收到 SIGTERM 后以 0 退出，socket 已删除"
log "abroadd 日志（节选）："
tail -n 20 "${OUT}/abroadd-fake-${TS}.log" | tee -a "${EVID}"
log "✓ 通过。证据：${EVID}"
