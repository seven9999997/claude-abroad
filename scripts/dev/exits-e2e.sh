#!/usr/bin/env bash
# M4-7 的虚拟机端到端（m4-plan v2.8 验收 c–i）：对端是同一台虚拟机里的另一个命名空间（决定 8），
# 三个分流出口 wg（WireGuard）、ovpn（OpenVPN）、sub（订阅 mihomo，节点是对端里的 socks）经 PUT /v1/exits 下发，
# 宿主一侧经 data.sock 指定出口开流。
# 以当前用户运行，不需要 sudo，不改宿主机网络：流量全在虚拟机里，不出虚拟机。
#
#   scripts/dev/exits-e2e.sh            # 构建 abroad-agent、镜像、开发版 abroad-vm、abroad-dev，再跑
#   scripts/dev/exits-e2e.sh --no-build # 用已有的构建
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
[ "$(id -u)" != 0 ] || { echo "不要用 root 运行" >&2; exit 2; }
BUILD=1
[ "${1:-}" = "--no-build" ] && BUILD=0
OUT="${ROOT}/scripts/dev/out"
BIN="${OUT}/bin"
TS="$(date +%Y%m%d-%H%M%S)"
EVID="${OUT}/exits-e2e-${TS}.txt"
STATE="${OUT}/exitse2e"
VM_LOG="${OUT}/abroad-vm-${TS}-exitse2e.log"
SECRETS="${STATE}/exits-${TS}.json" # 测试用的配置（含测试密钥），0600，结束时删除
mkdir -p "${BIN}" "${STATE}"
VM_PID=""
FAILS=0
log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*" | tee -a "${EVID}"; }
ok() { log "✓ $*"; }
bad() { log "✗ $*"; FAILS=$((FAILS + 1)); }
fail() { log "✗ 失败：$*"; exit 1; }
json() { python3 -I -c 'import json,sys; d=json.loads(sys.argv[1]); print(eval(sys.argv[2]))' "$1" "$2"; }
ctl() { curl -sS -m 120 --unix-socket "${STATE}/ctl.sock" "$@"; }
peer() { ctl -X POST "http://abroad-agent/v1/dev/exit-peer?$1"; }
pstat() { ctl http://abroad-agent/v1/dev/exit-peer; }
health() { ctl http://abroad-agent/v1/health; }
netstats() { curl -fsS --max-time 3 --unix-socket "${STATE}/host.sock" http://abroad-vm/v1/netstats; }
ext_dials() { local s; s="$(netstats)" && json "$s" 'd["dial_ok"]-d["dial_ok_loopback"]'; }
flows() { "${BIN}/abroad-dev" exit-flows --data-sock "${STATE}/data.sock" "$@"; }
# 停虚拟机用 SIGTERM（和 vm-up 一样）：abroad-vm 收到后经 /v1/shutdown 优雅关机并以 0 退出。
# 只经 ctl.sock 发 /v1/shutdown 会被监护当成意外退出，1 秒后重启虚拟机（M1-4a 的设计）。
VM_RC=""
stop_vm() {
  rm -f "${SECRETS}"
  [ -n "${VM_PID}" ] || return 0
  kill -TERM "${VM_PID}" 2>/dev/null || true
  for _ in $(seq 1 15); do kill -0 "${VM_PID}" 2>/dev/null || break; sleep 1; done
  kill -0 "${VM_PID}" 2>/dev/null && { log "abroad-vm 15 秒内没有退出，发 SIGKILL"; kill -KILL "${VM_PID}" 2>/dev/null || true; }
  wait "${VM_PID}" 2>/dev/null
  VM_RC=$?
  VM_PID=""
}
trap stop_vm EXIT
# exit_up <出口> <true|false> <秒>：等 health 里该出口的可用状态变成期望值。
exit_up() {
  local i h
  for i in $(seq 1 $(($3 * 2))); do
    h="$(health)"
    [ "$(json "$h" "str(any(e['tag']=='$1' and e['up'] for e in d.get('exits') or [])).lower()")" = "$2" ] && return 0
    sleep 0.5
  done
  return 1
}

log "# M4-7 分流出口的虚拟机端到端，macOS $(sw_vers -productVersion)，Go $(go version | awk '{print $3}')"
if [ "${BUILD}" = 1 ]; then
  log "构建 abroad-agent、镜像、开发版 abroad-vm、abroad-dev"
  env GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o guest/out/abroad-agent ./cmd/abroad-agent || fail "构建 abroad-agent"
  go run ./cmd/abroad-imagebuild build -agent guest/out/abroad-agent >>"${EVID}" 2>&1 || fail "构建镜像"
  go run ./cmd/abroad-imagebuild verify >>"${EVID}" 2>&1 || fail "镜像检查"
  env CGO_ENABLED=1 go build -tags abroaddev -o "${BIN}/abroad-vm-dev" ./cmd/abroad-vm || fail "构建 abroad-vm-dev"
  scripts/dev/sign.sh "${BIN}/abroad-vm-dev" >>"${EVID}" 2>&1 || fail "签名 abroad-vm-dev"
  go build -o "${BIN}/abroad-dev" ./cmd/abroad-dev || fail "构建 abroad-dev"
fi

log "起虚拟机（开发标记）"
rm -f "${STATE}/console.log"
"${BIN}/abroad-vm-dev" run --image-dir guest/out --state-dir "${STATE}" --dev-agent 2>"${VM_LOG}" &
VM_PID=$!
up=0
for _ in $(seq 1 180); do
  kill -0 "${VM_PID}" 2>/dev/null || { tail -n 30 "${VM_LOG}" | tee -a "${EVID}"; fail "abroad-vm 提前退出"; }
  health >/dev/null 2>&1 && { up=1; break; }
  sleep 0.5
done
[ $up = 1 ] || fail "90 秒内 health 不通"
for _ in $(seq 1 40); do [ "$(json "$(health)" 'd["engines"]["sing-box"]["state"]')" = running ] && break; sleep 0.25; done
D0="$(ext_dials)" || fail "读不到 netstats"
log "health 可达；宿主绑物理网卡的拨号起点 ${D0}"

log "建对端命名空间（WireGuard、OpenVPN 服务端，socks 节点，回显，DNS，计数）"
( umask 077; peer action=up >"${SECRETS}" )
grep -q '"exits"' "${SECRETS}" || { head -c 300 "${SECRETS}" | tee -a "${EVID}"; fail "建对端失败"; }
WGKEY="$(python3 -I -c 'import json,sys,re; d=json.load(open(sys.argv[1])); print(re.search(r"PrivateKey = (\S+)", d["exits"][0]["conf"]).group(1))' "${SECRETS}")"
OVKEY="$(python3 -I -c 'import json,sys; d=json.load(open(sys.argv[1])); c=d["exits"][1]["conf"]; k=c.split("<key>")[1].split("</key>")[0].strip().splitlines(); print(k[1])' "${SECRETS}")"

log "PUT /v1/exits（wg、ovpn、sub）"
code=$(ctl -o /dev/null -w '%{http_code}' -X PUT --data-binary "@${SECRETS}" http://abroad-agent/v1/exits)
[ "${code}" = 200 ] || fail "PUT /v1/exits 回 ${code}"
for e in wg ovpn sub; do exit_up $e true 60 || fail "出口 $e 60 秒内没有变为可用：$(json "$(health)" 'd.get("exits")')"; done
ok "三个出口都可用：$(json "$(health)" 'd["exits"]')"

log "## c. 每个出口一次 TCP、一次 UDP 回显，回显来自对应的对端"
for spec in "wg 10.77.0.1:7 wg" "ovpn 10.78.0.1:7 ovpn" "sub 10.199.0.2:7 node"; do
  set -- ${spec}
  for n in tcp udp; do
    r="$(flows --exit "$1" --net $n --target "$2" --want-tag "$3" --n 1)" && ok "$1 $n：${r}" || bad "$1 $n：${r}"
  done
done
log "三个出口同时运行，并发各 50 条流"
flows --exit wg --target 10.77.0.1:7 --want-tag wg --n 50 >"${STATE}/f-wg" &
P1=$!
flows --exit ovpn --target 10.78.0.1:7 --want-tag ovpn --n 50 >"${STATE}/f-ovpn" &
P2=$!
flows --exit sub --target 10.199.0.2:7 --want-tag node --n 50 >"${STATE}/f-sub" &
P3=$!
for p in "wg:$P1" "ovpn:$P2" "sub:$P3"; do
  if wait "${p#*:}"; then ok "${p%%:*} 并发 50：$(cat "${STATE}/f-${p%%:*}")"; else bad "${p%%:*} 并发 50：$(cat "${STATE}/f-${p%%:*}")"; fi
done

log "## e. 解析只经隧道"
S1="$(pstat)"
for spec in "wg wg" "ovpn ovpn"; do
  set -- ${spec}
  r="$(flows --exit "$1" --target echo.exit.test:7 --want-tag "$2" --n 1)" && ok "$1 经域名：${r}" || bad "$1 经域名：${r}"
done
S2="$(pstat)"
log "DNS 查询：起点 $(json "$S1" 'd["dns"]')，终点 $(json "$S2" 'd["dns"]')；根命名空间转发的 53：$(json "$S2" 'd["root_dns53"]')；对端收到出口命名空间的其他包：$(json "$S2" 'd["peer"].get("exitns_other")')"
[ "$(json "$S2" 'd["dns"]["wg"]>'"$(json "$S1" 'd["dns"]["wg"]')")" = True ] && ok "wg 的查询出现在 wg 隧道里" || bad "wg 隧道里没有看到查询"
[ "$(json "$S2" 'd["dns"]["ovpn"]>'"$(json "$S1" 'd["dns"]["ovpn"]')")" = True ] && ok "ovpn 的查询出现在 ovpn 隧道里（推送的 DNS 经固定脚本拿到）" || bad "ovpn 隧道里没有看到查询"
[ "$(json "$S2" 'd["root_dns53"]')" = 0 ] && ok "根命名空间没有转发任何 53 端口的包" || bad "根命名空间转发了 53 端口的包"
[ "$(json "$S2" 'd["dns"]["node"]')" = 0 ] && ok "对端 veth 地址上没有收到查询" || bad "对端 veth 地址上收到了查询"

log "## l. OpenVPN 推送项过滤（服务端额外推送 setenv-safe、route、redirect-gateway）"
S="$(pstat)"
log "ovpn 命名空间的路由表：$(json "$S" 'd["routes"].get("ovpn")')"
log "ovpn 客户端日志里和推送有关的行：$(json "$S" 'chr(10).join(d["push_log"][-25:])')"
[ "$(json "$S" "(lambda d: (lambda R: (lambda D: not any('10.99.' in r or r.startswith(('0.0.0.0/1','128.0.0.0/1')) for r in R) and len(D)==1 and ' dev tun' in D[0])([r for r in R if r.startswith('default')]))(d['routes']['ovpn']))(d)")" = True ] \
  && ok "推送的 route、redirect-gateway 没有生效：路由表里只有 agent 设的默认路由、veth、端点和隧道网段" || bad "ovpn 的路由表里有推送来的路由"
for o in "setenv-safe" "route " "redirect-gateway"; do # 日志里的地址已脱敏，按选项名匹配
  [ "$(json "$S" "any(\"removed by filter: '$o\" in l for l in d['push_log'])")" = True ] && ok "日志显示推送项“$o”被过滤" || bad "日志里没看到“$o”被过滤"
done
[ "$(json "$S" 'any(("ifconfig" in l or "OPTIONS IMPORT" in l) for l in d["push_log"])')" = True ] && ok "白名单里的项（隧道地址等）生效了，隧道正常" || log "（日志里没找到 ifconfig 的导入记录；隧道可用见上面的回显）"

log "## m. 推送的 DNS"
log "normal 模式（旧式 dhcp-option DNS）：dns 文件 $(json "$(pstat)" 'd["ovpn_dns"]')"
[ "$(json "$(pstat)" 'd["ovpn_dns"]')" = "['10.78.0.1']" ] && ok "旧式推送的 DNS 经固定脚本写进了出口目录" || bad "旧式推送的 DNS 没有拿到"
# wait_dns <期望的 dns 文件内容（python 表达式）> <秒>：等客户端重连、固定脚本重写 dns 文件
# 比较时去重：2.6 推送 dns 时 OpenVPN 同时给新旧两种环境变量，同一个地址会写两遍（读文件的 socks 去重）。
wait_dns() { local i; for i in $(seq 1 $(($2 * 2))); do [ "$(json "$(pstat)" "sorted(set(d['ovpn_dns']))==sorted(set($1))")" = True ] && return 0; sleep 0.5; done; return 1; }
peer "action=ovpn-mode&mode=bad" >/dev/null
if wait_dns "['127.0.0.1','198.18.0.5','10.200.2.1']" 90 && exit_up ovpn true 60; then
  P1="$(pstat)"
  r="$(flows --exit ovpn --target echo.exit.test:7 --want-tag pub --n 1)" && ok "推送的全是不可用地址（回环、fake 段、veth 网段）：退到固定解析器，回显来自对端冒充的 1.1.1.1：${r}" || bad "推送非法地址时没有退到固定解析器：${r}"
  P2="$(pstat)"
  [ "$(json "$P2" 'd["dns"]["pub"]>'"$(json "$P1" 'd["dns"]["pub"]')")" = True ] && [ "$(json "$P2" 'd["root_dns53"]')" = 0 ] && ok "退到固定解析器的查询经隧道到达（对端 1.1.1.1 计数增长，根命名空间没有转发 53）" || bad "固定解析器的查询没有经隧道到达"
else bad "换成 bad 模式后客户端没有重连（dns 文件 $(json "$(pstat)" 'd["ovpn_dns"]')）"; fi
peer "action=ovpn-mode&mode=dns26" >/dev/null
if wait_dns "['10.78.0.1']" 90 && exit_up ovpn true 60; then
  ok "2.6 的 dns 推送项也能拿到：dns 文件 $(json "$(pstat)" 'd["ovpn_dns"]')（两条说明新旧两种环境变量都给了）"
  r="$(flows --exit ovpn --target echo.exit.test:7 --want-tag ovpn --n 1)" && ok "dns26 模式经域名：${r}" || bad "dns26 模式经域名：${r}"
else bad "2.6 的 dns 推送项没有拿到（dns 文件 $(json "$(pstat)" 'd["ovpn_dns"]')）"; fi
peer "action=ovpn-mode&mode=normal" >/dev/null
wait_dns "['10.78.0.1']" 90 && exit_up ovpn true 60 || bad "换回 normal 模式后 ovpn 没有恢复"

log "## n. 用户配置带 pull-filter"
BODY="$(python3 -I -c 'import json,sys; d=json.load(open(sys.argv[1])); c=d["exits"][1]["conf"].replace("nobind\n","nobind\npull-filter accept route\n"); print(json.dumps({"exits":[d["exits"][0],dict(d["exits"][1],conf=c),d["exits"][2]]}))' "${SECRETS}")"
RESP="$(ctl -w ' %{http_code}' -X PUT -d "${BODY}" http://abroad-agent/v1/exits)"
case "${RESP}" in *"pull-filter"*" 422") ok "带 pull-filter 被拒：${RESP%% *}…（422，指出行号）" ;; *) bad "带 pull-filter 应被拒：${RESP}" ;; esac
log "（回应：$(printf '%s' "${RESP}" | head -c 200)）"

log "## v2.10 b. 订阅出口经域名：解析在节点一侧，mihomo 不在本地解析"
Q1="$(pstat)"
r="$(flows --exit sub --target echo.exit.test:7 --want-tag pub --n 1)" && ok "sub 经域名到达对端；回显来自节点一侧解析出的地址（节点退到 1.1.1.1，对端冒充）：${r}" || bad "sub 经域名：${r}"
QU="$(pstat)"
if r="$(flows --exit sub --net udp --target echo.exit.test:7 --want-tag pub --n 1)"; then ok "sub 经域名的 UDP：${r}"; Q2="$(pstat)"; else
  Q2="$(pstat)"
  log "sub 经域名的 UDP 没通：${r}；对端冒充 DoH 的 1.1.1.1:443 收到的连接 $(json "$QU" 'd["echo"].get("doh443",0)') → $(json "$Q2" 'd["echo"].get("doh443",0)')"
  if [ "$(json "$Q2" 'd["echo"].get("doh443",0)>'"$(json "$QU" 'd["echo"].get("doh443",0)')")" = True ] && [ "$(json "$Q2" 'd["root_out53"]')" = "$(json "$QU" 'd["root_out53"]')" ]; then
    ok "UDP 的域名目标由 mihomo 自己解析，按配置经节点走 DoH（连接经节点到达对端的 1.1.1.1:443；对端没有真的 DoH 服务，所以这一步在测试环境里解析失败），没有从本地发出 DNS"
  else bad "sub 经域名的 UDP 既没通，也没看到解析经节点"; fi
fi
log "对端 DNS 计数 $(json "$Q1" 'd["dns"]') → $(json "$Q2" 'd["dns"]')；根命名空间自己发往 eth0 的 53：$(json "$Q1" 'd["root_out53"]') → $(json "$Q2" 'd["root_out53"]')，发往 eth0 的全部：$(json "$Q1" 'd["root_out"]') → $(json "$Q2" 'd["root_out"]')；转发的 53：$(json "$Q2" 'd["root_dns53"]')"
[ "$(json "$Q2" 'd["dns"]["pub"]>'"$(json "$Q1" 'd["dns"]["pub"]')")" = True ] && ok "查询出现在节点一侧（对端 1.1.1.1 的计数增长）" || bad "节点一侧没有看到查询"
[ "$(json "$Q2" 'd["root_out53"]')" = "$(json "$Q1" 'd["root_out53"]')" ] && [ "$(json "$Q2" 'd["root_dns53"]')" = 0 ] && ok "mihomo 没有发出 DNS 查询：根命名空间发往 eth0 的 53 不变，转发的 53 为 0" || bad "根命名空间发出了 DNS 查询"
[ "$(json "$Q2" 'd["dns"]["node"]')" = 0 ] && ok "对端 veth 地址上没有收到查询" || bad "对端 veth 地址上收到了查询"

log "## v2.10 c. R20：订阅出口的节点不可用时开流失败，不改走直连"
peer action=node-down >/dev/null
sleep 1
N1="$(pstat)"; ND1="$(ext_dials)"
r="$(flows --exit sub --target 10.199.0.2:7 --want-tag node --n 3)"
log "节点停掉后 sub 开流（TCP）：${r}"
[ "$(json "$r" 'd["good"]')" = 0 ] && ok "节点不可用时 TCP 开流失败" || bad "节点不可用时 TCP 居然通了"
r="$(flows --exit sub --net udp --target 10.199.0.2:7 --want-tag node --n 3)"
log "节点停掉后 sub 开流（UDP）：${r}"
[ "$(json "$r" 'd["good"]')" = 0 ] && ok "节点不可用时 UDP 开流失败" || bad "节点不可用时 UDP 居然通了"
r="$(flows --exit sub --target echo.exit.test:7 --want-tag pub --n 2)"
[ "$(json "$r" 'd["good"]')" = 0 ] && ok "节点不可用时经域名开流失败：${r}" || bad "节点不可用时经域名居然通了：${r}"
N2="$(pstat)"; ND2="$(ext_dials)"
[ "$(json "$N2" 'd["root_out"]')" = "$(json "$N1" 'd["root_out"]')" ] && ok "根命名空间没有往 eth0 发任何包（没有改走直连，也没有本地解析）" || bad "根命名空间往 eth0 发了包：$(json "$N1" 'd["root_out"]') → $(json "$N2" 'd["root_out"]')"
[ "$(json "$N2" 'json.dumps(d["echo"],sort_keys=True)')" = "$(json "$N1" 'json.dumps(d["echo"],sort_keys=True)')" ] && ok "对端所有回显的计数不变（流量没有经别的路到达）" || bad "回显计数变了"
[ "${ND2}" = "${ND1}" ] && ok "宿主绑物理网卡的拨号数不变（${ND1} → ${ND2}）" || bad "宿主绑物理网卡的拨号数变了（${ND1} → ${ND2}）"
peer action=node-up >/dev/null
ok_up=0
for _ in $(seq 1 20); do r="$(flows --exit sub --target 10.199.0.2:7 --want-tag node --n 1)" && { ok_up=1; break; }; sleep 0.5; done
[ ${ok_up} = 1 ] && ok "节点恢复后 sub 恢复：${r}" || bad "节点恢复后 sub 没有恢复：${r}"

log "## d、o. 隧道断开时不泄漏（wg、ovpn 各一次；断开时有一条经隧道的长连接正在发数据）"
for e in wg ovpn; do
  T=$( [ $e = wg ] && echo 10.77.0.1:7 || echo 10.78.0.1:7 )
  flows --exit $e --target $T --want-tag $e --n 2 --hold 8s >"${STATE}/hold-$e" 2>&1 &
  HP=$!
  sleep 2
  B="$(pstat)"
  R="$(peer "action=break&exit=$e")"
  log "$e 断开后从命名空间里主动发包：$(json "$R" 'd["attempts"]')"
  [ "$(json "$R" 'all(v!="" for v in d["attempts"].values())')" = True ] && ok "$e：主动发往对端普通端口、53、公网的包全部被拦" || bad "$e：有发包成功"
  exit_up $e false 15 && ok "$e 在 health 里变为不可用：$(json "$(health)" "[x for x in d['exits'] if x['tag']=='$e']")" || bad "$e 没有变为不可用"
  r="$(flows --exit $e --target 10.77.0.1:7 --n 3)"
  [ "$(json "$r" 'd["codes"].get("exit_down",0)')" = 3 ] && ok "$e 开流得到 exit_down：${r}" || bad "$e 开流应得到 exit_down：${r}"
  for o in wg ovpn sub; do
    [ $o = $e ] && continue
    t=$( [ $o = wg ] && echo 10.77.0.1:7 || { [ $o = ovpn ] && echo 10.78.0.1:7 || echo 10.199.0.2:7; } )
    g=$( [ $o = sub ] && echo node || echo $o )
    r="$(flows --exit $o --target $t --want-tag $g --n 5)" && ok "$e 断开时 $o 不受影响：${r}" || bad "$e 断开时 $o 受影响：${r}"
  done
  wait ${HP}
  log "$e 长连接：$(cat "${STATE}/hold-$e")"
  A="$(pstat)"
  [ "$(json "$A" 'd["root_eth0"]')" = "$(json "$B" 'd["root_eth0"]')" ] && ok "$e：根命名空间没有把出口命名空间的包转发到 eth0（$(json "$A" 'd["root_eth0"]')）" || bad "$e：根命名空间转发了包到 eth0"
  log "对端计数：断开前 $(json "$B" 'd["peer"]')，之后 $(json "$A" 'd["peer"]')；$e 命名空间被拦下的包 $(json "$B" "d['dropped'].get('$e')") → $(json "$A" "d['dropped'].get('$e')")"
  [ "$(json "$A" 'd["peer"].get("exitns_other",0)')" = "$(json "$B" 'd["peer"].get("exitns_other",0)')" ] && ok "$e：对端没收到出口命名空间发往端点以外的包" || bad "$e：对端收到了发往端点以外的包"
  peer "action=restore&exit=$e" >/dev/null
  exit_up $e true 90 && ok "$e 恢复后变回可用" || bad "$e 恢复后 90 秒内没有变回可用"
done

log "## h. 状态：引擎被杀死、WireGuard 握手超时"
peer "action=kill&exit=wg" >/dev/null
exit_up wg false 10 && ok "wg 的 socks 被杀后 health 显示不可用" || bad "wg 的 socks 被杀后没有显示不可用"
exit_up wg true 30 && ok "引擎重启后 wg 变回可用" || bad "wg 没有变回可用"
peer "action=skew&d=4m" >/dev/null
exit_up wg false 10 && ok "时钟后拨 4 分钟：wg 因握手超时不可用：$(json "$(health)" "[x for x in d['exits'] if x['tag']=='wg']")" || bad "握手超时没有显示不可用"
peer "action=skew&d=0s" >/dev/null
exit_up wg true 10 && ok "时钟复原后 wg 可用" || bad "时钟复原后 wg 不可用"
H="$(health)"
[ "$(json "$H" 'any(s in json.dumps(d["exits"]) for s in ["10.199.","10.77.","10.78.","10.200."])')" = False ] && ok "health 的出口状态里没有端点和隧道地址" || bad "health 里出现了地址"

log "## f. 凭据"
S="$(pstat)"
log "出口目录：$(json "$S" 'd["files"]')"
OURS='("nft.conf","wg.conf","dns","client.conf","config.yaml","root-nat.conf")'
[ "$(json "$S" 'all(v=="d0700" for v in d["files"].values() if v[0]=="d")')" = True ] && ok "出口目录都是 0700（只给 root）" || bad "有目录不是 0700"
[ "$(json "$S" 'all(v=="f0600" for k,v in d["files"].items() if k.split("/")[-1] in '"${OURS}"')')" = True ] && ok "agent 写的配置和凭据文件都是 0600" || bad "有文件不是 0600"
log "引擎自己建的文件（在 0700 目录里）：$(json "$S" '{k:v for k,v in d["files"].items() if v[0]=="f" and k.split("/")[-1] not in '"${OURS}"'}')"
code=$(ctl -o /dev/null -w '%{http_code}' -X PUT -d '{"exits":[{"tag":"bad","kind":"wireguard","conf":"[Interface]\nPostUp = id\n"}]}' http://abroad-agent/v1/exits)
S3="$(pstat)"
[ "${code}" = 422 ] && [ "$(json "$S3" 'any("/bad" in k for k in d["files"])')" = False ] && [ "$(json "$S3" 'len(d["status"])')" = 3 ] && ok "不合法的配置回 422，不留目录，原有出口不变" || bad "不合法的配置：${code}"
LEAK=0
for k in "${WGKEY}" "${OVKEY}"; do
  for src in "$(health)" "$(ctl http://abroad-agent/v1/exits)" "$(ctl 'http://abroad-agent/v1/logs?engine=sing-box&tail=5000')" "$(ctl 'http://abroad-agent/v1/logs?engine=mihomo&tail=5000')"; do
    case "${src}" in *"${k}"*) LEAK=1 ;; esac
  done
  grep -qF -- "${k}" "${STATE}/console.log" "${VM_LOG}" 2>/dev/null && LEAK=1
done
[ ${LEAK} = 0 ] && ok "测试密钥不在控制台日志、abroad-vm 日志、health、/v1/exits、引擎日志里" || bad "测试密钥出现在日志或接口里"

log "## i. 幂等"
E1="$(pstat)"
code=$(ctl -o /dev/null -w '%{http_code}' -X PUT --data-binary "@${SECRETS}" http://abroad-agent/v1/exits)
sleep 1
E2="$(pstat)"
[ "${code}" = 200 ] && [ "$(json "$E1" 'json.dumps(d["engines"],sort_keys=True)')" = "$(json "$E2" 'json.dumps(d["engines"],sort_keys=True)')" ] && ok "同样的集合重复下发：没有引擎重启" || bad "重复下发改变了引擎：${code}"
python3 -I -c 'import json,sys; d=json.load(open(sys.argv[1])); d["exits"][2]["conf"]+="# 改一下\n"; json.dump(d,open(sys.argv[1],"w"))' "${SECRETS}"
code=$(ctl -o /dev/null -w '%{http_code}' -X PUT --data-binary "@${SECRETS}" http://abroad-agent/v1/exits)
E3="$(pstat)"
[ "${code}" = 200 ] && [ "$(json "$E2" 'json.dumps({k:v for k,v in d["engines"].items() if "sub" not in k},sort_keys=True)')" = "$(json "$E3" 'json.dumps({k:v for k,v in d["engines"].items() if "sub" not in k},sort_keys=True)')" ] \
  && [ "$(json "$E2" 'd["engines"]["mihomo-sub"]["since"]')" != "$(json "$E3" 'd["engines"]["mihomo-sub"]["since"]')" ] && ok "只改 sub：只重启了 sub 的引擎" || bad "改 sub 影响了别的出口：${code}"
code=$(ctl -o /dev/null -w '%{http_code}' -X PUT -d '{"exits":[]}' http://abroad-agent/v1/exits)
E4="$(pstat)"
log "清空后：命名空间 $(json "$E4" 'd["netns"]')，网卡 $(json "$E4" 'd["links"]')，文件 $(json "$E4" 'list(d["files"])')"
[ "${code}" = 200 ] && [ "$(json "$E4" '[n for n in d["netns"] if n.startswith("ex-")]')" = "[]" ] && [ "$(json "$E4" '[l for l in d["links"] if l.startswith("vx")]')" = "[]" ] \
  && [ "$(json "$E4" '[k for k in d["files"] if k.startswith("/run/claude-abroad/exits/") and k!="/run/claude-abroad/exits/root-nat.conf"]')" = "[]" ] && ok "清空后命名空间、veth、出口目录都不残留" || bad "清空后有残留"

log "## g. 没有隐式直连"
D1="$(ext_dials)"
[ "${D1}" = "${D0}" ] && ok "全过程宿主绑物理网卡的拨号数不变（${D0} → ${D1}）" || bad "宿主绑物理网卡的拨号数变了（${D0} → ${D1}）"

peer action=down >/dev/null
log "停虚拟机（SIGTERM）"
stop_vm
[ "${VM_RC}" = 0 ] && ok "abroad-vm 以 0 退出" || bad "abroad-vm 的退出码是 ${VM_RC}"
grep -q "serve：收到关机请求" "${STATE}/console.log" && ok "虚拟机里的 serve 收到了关机请求：$(grep -o '拆分流出口用时 [^ ]*\|停引擎用时 [^ ]*' "${STATE}/console.log" | tr '\n' ' ')" || bad "控制台里没有关机记录"
log "日志尾部（控制台最后 15 行）："
tail -n 15 "${STATE}/console.log" 2>/dev/null | tee -a "${EVID}" >/dev/null
if [ ${FAILS} = 0 ]; then log "✓ 全部通过；证据 ${EVID}"; else log "✗ ${FAILS} 项不通过；证据 ${EVID}"; exit 1; fi
