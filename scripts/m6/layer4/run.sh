#!/usr/bin/env bash
# 第四层的真机实测（R21、R22、R23；fallback-plan 第 6 节实测 1–4）。前提：开发安装已经装好（scripts/m6/install），
# 并且在应用里**已启用**（所有流量都在接管下：即使 Claude Code 绕开本机代理端口，也走主代理，不暴露本机 IP）。
#
# 脚本写入 Claude Code 的托管设置分片（和 abroadd 首次准备写的逐字相同），以你自己的身份启动 Claude Code（claude -p）
# 各发一句话，经 abroadd 状态里第四层的计数和最近连接核对：
#   T4 分片可读、Claude Code 的版本（实测 4）；
#   T0 不带代理变量：连接经本机代理端口（第四层生效的前提）；
#   T1a/T1b/T1c 启动环境带大写 HTTPS_PROXY、小写 https_proxy（都指向本脚本的诱饵端口）、no_proxy=*：
#     仍经本机代理端口、诱饵端口没有连接（实测 1，R21）；
#   T2a/T2b 托管设置改指向“没人监听”和“一律 403”的端口：Claude Code 应当失败；成功了就是绕开了代理（实测 2，R22）；
#   T3 （可选，你来操作）Claude 桌面版里开一个 Code 会话：连接经本机代理端口（实测 3）；
#   T5 从普通终端用 curl 经端口访问：状态里识别出的程序和 Claude Code 的不同，各按规则走（R23）。
# 结束时删掉分片（内容一致才删）、结束诱饵监听；abroadd、网络、地区都不动（之后在应用里自己回国或停用）。
#
# **写入分片的那一刻起，这台 Mac 上所有 Claude Code（含正在运行的会话）都经本机代理端口联网**；已启用时照常能用，
# 脚本结束删掉分片后恢复。会用你的账号发几句很短的话。会话里只做空跑、--check-only 和自测。
#
#   sudo -E scripts/m6/layer4/run.sh
#   scripts/m6/layer4/run.sh --check-only
#   DRY_RUN=1 scripts/m6/layer4/run.sh
# 环境变量：M6L_CLAUDE（claude 的路径；默认在你的登录 shell 里找）、M6L_DEADMAN_SECS（默认 1800）、M6L_YES=1。
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "${ROOT}" || exit 1
. scripts/m6/layer4/lib.sh

exec 3>&1
CHECK_ONLY=0
case "${1:-}" in --check-only) CHECK_ONLY=1 ;; "") ;; *) echo "用法：sudo $0 [--check-only]" >&2; exit 2 ;; esac
OUT="${ROOT}/scripts/m6/layer4/out"
TS="$(date +%Y%m%d-%H%M%S)"
mkdir -p "${OUT}"
M6L_LOG="${OUT}/m6l-${TS}.log"
RESULTS="${OUT}/m6l-results-${TS}.txt"
: >"${M6L_LOG}"; : >"${RESULTS}"
API="${M6L_SOCK}/api.sock"
RESTORE="${ROOT}/scripts/m6/layer4/restore.sh"
PROMPT="只回复 ok 两个字母"

res() { printf '%-4s %-6s %s\n' "$1" "$2" "$3" | tee -a "${RESULTS}" >&2; }
step() { printf 'STEP: %s\n' "$*" >&3; m6l_log "$*"; }
get() {
  if [ "$DRY_RUN" = 1 ]; then printf 'API: GET %s\n' "$1" >&3; echo '{}'; return; fi
  curl -sS -m 10 --unix-socket "${API}" "http://abroadd$1" 2>/dev/null || echo '{}'
}
# cc <字段>：第四层状态里的一项（accepted、refused、recent.0.app……）。
cc() { get /v1/status | /usr/bin/python3 -I -c 'import json,sys
d=(json.load(sys.stdin).get("claude_code") or {})
for k in sys.argv[1].split("."):
    d = d[int(k)] if isinstance(d, list) and k.isdigit() and int(k) < len(d) else (d.get(k) if isinstance(d, dict) else None)
print("" if d is None else (str(d).lower() if isinstance(d, bool) else d))' "$1" 2>/dev/null; }
state() { get /v1/status | /usr/bin/python3 -I -c 'import json,sys; print(json.load(sys.stdin).get("state",""))' 2>/dev/null; }
# 回国拦截用的是不是测试段（M6-11b）：没有这一项（没打开管控）或是测试段时为 ok；真实段时为 real。
rbset() { get /v1/status | /usr/bin/python3 -I -c 'import json,sys
r=json.load(sys.stdin).get("return_block")
print("ok" if not r or r.get("test") else "real")' 2>/dev/null; }

# 以你自己的身份在登录 shell 里运行 claude -p（最多 120 秒）；返回 claude 的退出码，输出存到文件。
run_claude() { # run_claude <名字> [环境变量…]
  local name=$1; shift
  if [ "$DRY_RUN" = 1 ]; then printf 'CLAUDE: %s env %s claude -p\n' "$name" "$*"; return 0; fi
  sudo -u "${SUDO_USER}" -i env "$@" /usr/bin/perl -e 'alarm 120; exec @ARGV' "${CLAUDE}" -p "${PROMPT}" >"${OUT}/m6l-${name}-${TS}.txt" 2>&1
}
# 诱饵监听：只计数（decoy、dead 不起），或一律回 403（deny）。PID 文件由它自己写，命令行里带 m6l-listener。
listener() { # listener <名字> <端口> <count|deny>
  if [ "$DRY_RUN" = 1 ]; then printf 'CHANGE: 本脚本的监听 %s 127.0.0.1:%s（%s）\n' "$1" "$2" "$3"; return 0; fi
  /usr/bin/python3 -I -c '
import os, socket, sys, threading
name, port, mode, st = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4]
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.bind(("127.0.0.1", port)); s.listen(16)
open(os.path.join(st, name + ".pid"), "w").write(str(os.getpid()))
n = 0
while True:
    c, _ = s.accept(); n += 1
    open(os.path.join(st, name + ".count"), "w").write(str(n))
    if mode == "deny":
        try:
            c.settimeout(5); c.recv(4096)
            c.sendall(b"HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
        except Exception: pass
    c.close()
' "$1" "$2" "$3" "${M6L_STATE}" m6l-listener >>"${OUT}/m6l-listener-${TS}.log" 2>&1 &
  for _ in $(seq 1 20); do [ -f "${M6L_STATE}/$1.pid" ] && return 0; sleep 0.2; done
  m6l_die "本脚本的监听 $1 没有起来（端口 $2 被占用？）"
}
count() { [ "$DRY_RUN" = 1 ] && { echo 0; return; }; cat "${M6L_STATE}/$1.count" 2>/dev/null || echo 0; }
write_frag() { # write_frag <端口>
  if [ "$DRY_RUN" = 1 ]; then printf 'CHANGE: install -m 644 -o root -g wheel <内容：abroad-dev claude-settings content --port %s> %s\n' "$1" "${M6L_FRAG}"; return; fi
  "${AD}" claude-settings content --port "$1" >"${M6L_STATE}/frag.content" || m6l_die "生成托管设置内容失败"
  m6l_do install -m 644 -o root -g wheel "${M6L_STATE}/frag.content" "${M6L_FRAG}" || m6l_die "写托管设置失败"
}

precheck() {
  local reasons=()
  [ -S "${API}" ] || reasons+=("连不上 abroadd（${API}）：先用 scripts/m6/install 装好开发安装")
  [ "$(state)" = ENABLED ] || reasons+=("abroadd 不在已启用状态：先在应用里启用（实测要在接管下做，绕开代理也不暴露本机 IP）")
  [ "$(rbset)" = real ] && reasons+=("回国拦截用的是真实的 Anthropic 地址段（发布构建，或没有只管测试目标）：回国后这台 Mac 直连 Anthropic 会被拒，这里的 Claude Code 会话会断开。实测用开发安装（测试段）")
  [ "$(cc enabled)" = true ] || reasons+=("第四道的开关没打开（v0.68 起默认关）：先执行 sudo abroad claude-proxy on（开发安装只管测试目标：只监听，不写托管设置），做完 sudo abroad claude-proxy off")
  [ "$(cc listening)" = true ] || reasons+=("本机代理端口 ${M6L_PORT} 没在听：$(cc listen_error)")
  [ -e "${M6L_STATE}" ] && reasons+=("上一次运行没有结束（${M6L_STATE}）：先执行 sudo scripts/m6/layer4/restore.sh")
  if [ -e "${M6L_FRAG}" ]; then reasons+=("${M6L_FRAG} 已经存在（开发安装默认不写它；可能是别的运行留下的）：确认后删除"); fi
  [ -n "${CLAUDE}" ] || reasons+=("找不到 claude：设 M6L_CLAUDE=<路径>")
  if [ ${#reasons[@]} -gt 0 ]; then for r in "${reasons[@]}"; do m6l_log "前提不满足：${r}"; done; return 1; fi
  m6l_log "前提检查通过（claude：${CLAUDE}）"
}

[ "$DRY_RUN" = 1 ] && SUDO_USER=${SUDO_USER:-$(id -un)}
CLAUDE="${M6L_CLAUDE:-}"
if [ -z "${CLAUDE}" ] && [ "$DRY_RUN" != 1 ]; then
  CLAUDE=$(sudo -u "${SUDO_USER:-$(id -un)}" -i sh -c 'command -v claude' 2>/dev/null | tail -1)
fi
[ "$DRY_RUN" = 1 ] && CLAUDE=${CLAUDE:-claude}
if [ "${CHECK_ONLY}" = 1 ]; then m6l_test_guard check; precheck; exit $?; fi
m6l_test_guard
m6l_require_root
if ! precheck; then [ "$DRY_RUN" = 1 ] && m6l_log "（空跑：前提不满足，继续打印）" || exit 3; fi
if [ "$DRY_RUN" != 1 ] && [ "${M6L_YES:-0}" != 1 ]; then
  m6l_log "写入托管设置的那一刻起，这台 Mac 上所有 Claude Code（含正在运行的会话）都经本机代理端口联网，脚本结束时恢复。会用你的账号发几句很短的话。输入 yes 继续："
  read -r ans </dev/tty; [ "${ans}" = yes ] || { m6l_log "没有输入 yes，什么都没改"; exit 3; }
fi

m6l_state_init
m6l_deadman_start "${M6L_DEADMAN_SECS:-1800}" "${RESTORE}"
m6l_arm_restore_trap "${RESTORE}"
# abroad-dev（生成托管设置内容，唯一来源是 internal/claudecfg）：以你自己的身份编译。
AD="${M6L_STATE}/abroad-dev"
if [ "$DRY_RUN" != 1 ]; then
  GO=$(sudo -u "${SUDO_USER}" -i sh -c 'command -v go' | tail -1)
  sudo -u "${SUDO_USER}" "${GO:-go}" build -o "${OUT}/abroad-dev" ./cmd/abroad-dev || m6l_die "go build ./cmd/abroad-dev 失败"
  cp "${OUT}/abroad-dev" "${AD}"
fi
[ -d "$(dirname "${M6L_CC_DIR}")" ] || m6l_state_set created_parent 1
[ -d "${M6L_CC_DIR}" ] || { m6l_do mkdir -p "${M6L_CC_DIR}"; m6l_state_set created_dir 1; }

step "T4 写入托管设置分片，核对可读和版本"
write_frag "${M6L_PORT}"
if [ "$DRY_RUN" != 1 ]; then
  ver=$(sudo -u "${SUDO_USER}" -i "${CLAUDE}" --version 2>&1 | head -1)
  if sudo -u "${SUDO_USER}" cat "${M6L_FRAG}" >/dev/null 2>&1; then res T4 通过 "分片普通用户可读；Claude Code 版本：${ver}（是否已加载以 T0 为准）"; else res T4 不通过 "普通用户读不到分片"; fi
fi

# T0、T1：都应经本机代理端口；T1 的诱饵端口没有连接。
listener decoy "${M6L_DECOY}" count
one() { # one <编号> <说明> [环境变量…]
  local id=$1 what=$2 a0 d0 rc a1 d1 app; shift 2
  step "${id} ${what}"
  a0=$(cc accepted); d0=$(count decoy)
  run_claude "${id}" "$@"; rc=$?
  [ "$DRY_RUN" = 1 ] && { res "${id}" 跳过 "空跑"; return; }
  a1=$(cc accepted); d1=$(count decoy); app=$(cc recent.0.app)
  if [ "${a1:-0}" -gt "${a0:-0}" ] && [ "${d1}" = "${d0}" ] && [ "$rc" = 0 ]; then
    res "${id}" 通过 "${what}：经本机代理端口 $((a1 - a0)) 条连接，诱饵端口 0 条，识别为 ${app}（claude=$(cc recent.0.claude)，去向 $(cc recent.0.action) $(cc recent.0.exit)）"
  else
    res "${id}" 不通过 "${what}：本机代理端口 +$((${a1:-0} - ${a0:-0}))，诱饵端口 +$((d1 - d0))，claude 退出码 ${rc}（输出见 m6l-${id}-${TS}.txt）"
  fi
}
one T0 "不带代理变量"
one T1a "启动环境带 HTTPS_PROXY 指向别的端口" "HTTPS_PROXY=http://127.0.0.1:${M6L_DECOY}"
one T1b "启动环境带 https_proxy 指向别的端口" "https_proxy=http://127.0.0.1:${M6L_DECOY}"
one T1c "启动环境带 no_proxy=*" "no_proxy=*" "NO_PROXY=*"

# T2：托管设置改指向没人监听、一律 403 的端口：Claude Code 应当失败（成功了说明绕开了代理，在接管下经主代理出去了）。
fail_case() { # fail_case <编号> <端口> <说明>
  local id=$1 port=$2 what=$3 rc
  step "${id} ${what}"
  write_frag "${port}"
  run_claude "${id}"; rc=$?
  [ "$DRY_RUN" = 1 ] && { res "${id}" 跳过 "空跑"; return; }
  if [ "$rc" != 0 ] && ! grep -qi '^ok' "${OUT}/m6l-${id}-${TS}.txt"; then
    res "${id}" 通过 "${what}：Claude Code 连不上（退出码 ${rc}），没有回退直连"
  else
    res "${id}" 不通过 "${what}：Claude Code 成功了，说明代理不可用时它绕开了代理（输出见 m6l-${id}-${TS}.txt）"
  fi
}
fail_case T2a "${M6L_DEAD}" "代理端口没人监听"
listener deny "${M6L_DENY}" deny
fail_case T2b "${M6L_DENY}" "代理端口一律回 403"
[ "$DRY_RUN" = 1 ] || { [ "$(count deny)" -gt 0 ] || m6l_log "注意：403 端口没有收到连接，T2b 可能没有经过它"; }
write_frag "${M6L_PORT}"

# T3：桌面版里的 Code 会话（可选，你来操作）。
step "T3 Claude 桌面版里的 Code 会话（可选）"
if [ "$DRY_RUN" = 1 ]; then printf 'WAIT: 在 Claude 桌面版里开一个 Code 会话发一句话（没装桌面版输入 s 跳过）\n' >&3; res T3 跳过 "空跑"; else
  a0=$(cc accepted)
  m6l_log "在 Claude 桌面版里开一个 Code 会话发一句话，做完按回车（没装桌面版输入 s 跳过）"
  read -r ans </dev/tty || ans=s
  if [ "${ans}" = s ]; then res T3 跳过 "你跳过了"; else
    a1=$(cc accepted)
    if [ "${a1:-0}" -gt "${a0:-0}" ]; then res T3 通过 "桌面版的 Code 会话经本机代理端口（识别为 $(cc recent.0.app)）"; else res T3 不通过 "本机代理端口没有新连接：桌面版里的 Code 会话不读这份托管设置"; fi
  fi
fi

# T5：普通终端经端口访问，识别出的程序和 Claude Code 不同（R23）。
step "T5 普通终端用 curl 经本机代理端口访问"
if [ "$DRY_RUN" = 1 ]; then printf 'CURL: curl -x http://127.0.0.1:%s https://www.gstatic.com/generate_204\n' "${M6L_PORT}"; res T5 跳过 "空跑"; else
  code=$(sudo -u "${SUDO_USER}" /usr/bin/curl -sS -m 20 -x "http://127.0.0.1:${M6L_PORT}" -o /dev/null -w '%{http_code}' https://www.gstatic.com/generate_204 2>/dev/null)
  app=$(cc recent.0.app); cl=$(cc recent.0.claude)
  if [ "${app}" != "" ] && [ "${app}" != unknown ] && [ "${cl}" = false ]; then
    res T5 通过 "curl 识别为 ${app}（不按 Claude 处理），去向 $(cc recent.0.action) $(cc recent.0.exit)，应答 ${code}"
  else
    res T5 不通过 "curl 识别为 ${app:-无}（claude=${cl}）：识别不出时一律按 Claude 处理（设计 R23 的备选），应答 ${code}"
  fi
fi

step "收尾：删掉分片、结束本脚本的监听"
M6L_RESTORED=1
trap - EXIT
if [ "$DRY_RUN" = 1 ]; then DRY_RUN=1 bash "${RESTORE}"; else bash "${RESTORE}"; fi
m6l_log "结果（${RESULTS}）："
[ "$DRY_RUN" = 1 ] || cat "${RESULTS}" >&2
m6l_log "abroadd 还在已启用状态：之后在应用里自己回国或停用。请贴回 ${RESULTS} 和 scripts/m6/layer4/out/ 下的输出。"
