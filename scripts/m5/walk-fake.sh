#!/usr/bin/env bash
# M5-11 第一级走查（假系统）：不需要管理员权限、不改本机网络、不会断网，随时可以做。
# 启动开发模式的假系统 abroadd（--fake-system --fake-main-proxy，整套系统是内存里的假实现），再打开打包好的应用
# （app/.build/bundle/ClaudeAbroad.app，经 ABROAD_SOCKET 连这个 abroadd）。应用开着时脚本留在前台读键：
#   d  只停假后台（应用不动：看“后台服务没有运行”）
#   u  用同一个状态目录、同一个 socket 路径把假后台再起来（应用自动重连）
#   q  全部停掉并清理（应用退出时也一样）
# 按 Ctrl+C 或关掉终端：全部停掉（应用和 abroadd），删掉临时目录。只按自己记下的 PID 结束进程。
#
#	scripts/m5/walk-fake.sh               # 正式走查：先起假后台再开应用，按 docs/m5-walkthrough.md 的第一级逐项看
#	scripts/m5/walk-fake.sh --app-first   # 先开应用（后台还没起来），按回车后再起假后台（走查 L1-03 用）
#	scripts/m5/walk-fake.sh --check       # 空跑：起假后台、读状态、只停后台、同一路径再起来、再读状态，不打开应用
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
umask 077
[[ "$(id -u)" != "0" ]] || { echo "不要以 root 运行（第一级走查不需要管理员权限）" >&2; exit 1; }
MODE=normal
case "${1:-}" in
  --check) MODE=check ;;
  --app-first) MODE=appfirst ;;
  "") ;;
  *) echo "用法：$0 [--app-first | --check]" >&2; exit 2 ;;
esac

APP="app/.build/bundle/ClaudeAbroad.app"
TMP="$(mktemp -d /tmp/abroad-walk.XXXXXX)" # 短路径：unix socket 的路径有长度上限
ST="${TMP}/st"
SOCK="${ST}/api.sock" # 整个走查期间不变：假后台停了再起来还是这个路径
DPID=""
APID=""

stop_pid() { # stop_pid <PID> <命令行里必须有的字符串>：核对命令行再结束
  local pid=$1 want=$2
  [[ -n "${pid}" ]] && kill -0 "${pid}" 2>/dev/null || return 0
  ps -p "${pid}" -o command= 2>/dev/null | grep -qF -- "${want}" || return 0
  kill -TERM "${pid}" 2>/dev/null || true
  for _ in $(seq 1 50); do kill -0 "${pid}" 2>/dev/null || return 0; sleep 0.1; done
  kill -KILL "${pid}" 2>/dev/null || true
}
daemon_up() { [[ -n "${DPID}" ]] && kill -0 "${DPID}" 2>/dev/null; }
start_daemon() {
  daemon_up && { echo "假后台已经在运行"; return 0; }
  "${TMP}/abroadd" --fake-system --fake-main-proxy --state-dir "${ST}" 2>>"${TMP}/abroadd.log" &
  DPID=$!
  for _ in $(seq 1 100); do [[ -S "${SOCK}" ]] && break; sleep 0.1; done
  [[ -S "${SOCK}" ]] || { echo "abroadd 没有起来：" >&2; tail -5 "${TMP}/abroadd.log" >&2; return 1; }
  echo "假后台已启动（pid ${DPID}，socket ${SOCK}，状态 $(status_state)）"
}
stop_daemon() {
  daemon_up || { echo "假后台没有在运行"; return 0; }
  stop_pid "${DPID}" "${TMP}/abroadd"
  DPID=""
  echo "假后台已停（应用不动；socket 路径保持不变，按 u 再起来）"
}
status_state() { curl -sS -m 5 --unix-socket "${SOCK}" http://abroadd/v1/status 2>/dev/null | sed -n 's/.*"state":"\([A-Z]*\)".*/\1/p'; }
cleanup() {
  stop_pid "${APID}" "${APP}/Contents/MacOS/ClaudeAbroad"
  stop_pid "${DPID}" "${TMP}/abroadd"
  [[ -n "${TMP}" && "${TMP}" == /tmp/abroad-walk.* ]] && rm -rf -- "${TMP}"
  if [[ -n "${DPID}" ]] && kill -0 "${DPID}" 2>/dev/null; then echo "✗ abroadd（pid ${DPID}）没有停下" >&2; else echo "已全部停掉，临时目录已删"; fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM HUP

go build -tags abroaddev -o "${TMP}/abroadd" ./cmd/abroadd # 开发构建才有假系统模式（M6-1）

if [[ "${MODE}" = check ]]; then
  # 空跑：不打开应用。起来 → 应用主代理（属主，直接成功）→ 只停后台 → 同一路径再起来 → 读状态和引导进度。
  start_daemon
  first="${SOCK}"
  code=$(curl -sS -m 10 -o /dev/null -w '%{http_code}' -X POST --unix-socket "${SOCK}" http://abroadd/v1/main-proxy/apply)
  echo "应用主代理：HTTP ${code}"
  stop_daemon
  [[ ! -S "${SOCK}" ]] && echo "停后台后 socket 已删除" || echo "停后台后 socket 还在"
  start_daemon
  [[ "${first}" = "${SOCK}" ]] && echo "两次的 socket 路径相同：${SOCK}"
  setup="$(curl -sS -m 5 --unix-socket "${SOCK}" http://abroadd/v1/setup)"
  case "${setup}" in
    *'{"id":"main_proxy","done":true}'*) echo "重启后假主代理的出口基准还在（同一个状态目录）" ;;
    *) echo "重启后假主代理的出口基准不在：${setup}" ;;
  esac
  [[ -d "${APP}" ]] && echo "应用包在：${APP}" || echo "（应用包还没打：scripts/dev/app-bundle.sh）"
  echo "空跑：不打开应用"
  exit 0
fi

[[ -x "${APP}/Contents/MacOS/ClaudeAbroad" ]] || { echo "没有应用包：先执行 scripts/dev/app-bundle.sh" >&2; exit 1; }
start_app() {
  ABROAD_SOCKET="${SOCK}" "${APP}/Contents/MacOS/ClaudeAbroad" >"${TMP}/app.log" 2>&1 &
  APID=$!
  echo "应用已打开（连 ${SOCK}）"
}

if [[ "${MODE}" = appfirst ]]; then
  start_app
  echo "后台还没有起来：应用里点“下一步”，应停在“后台服务”一步（显示说明和命令）。看完按回车启动假后台。"
  read -r _ </dev/tty || true
  start_daemon
else
  start_daemon
  start_app
fi

echo "按 d 只停假后台，u 用同一路径再起来，q 全部停掉（应用退出时也会全部停掉）。按 docs/m5-walkthrough.md 的“第一级”逐项看。"
while kill -0 "${APID}" 2>/dev/null; do
  key=""
  read -r -s -n 1 -t 1 key </dev/tty || true
  case "${key}" in
    d) stop_daemon ;;
    u) start_daemon || true ;;
    q) break ;;
  esac
done
echo "结束走查"
