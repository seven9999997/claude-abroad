#!/usr/bin/env bash
# 核对 DRY_RUN=1 空跑的输出（m3-plan v2.10 验收 d、e，v2.11 验收 l）：
#   - 每一条 CHANGE: 都落在 scripts/m3/commands.txt 的清单里；
#   - 没有禁止的命令：同 M2（pfctl -d/-e、不带锚点的 pfctl -f 和 -F all、pkill、killall、networksetup -setdnsservers、
#     对默认路由的 route delete），另加：tcpdump 只能只读抓包（不能带 -w、-W、-C、-G、-z、-r 等写文件或执行的选项），
#     Chrome 只能以用户身份（sudo -u）启动；
#   - run.sh：死人开关之前只有建状态目录；快照在安装程序、建用户之前；
#   - restore.sh：停服务 → 停虚拟机、上游、抓包、Chrome → 删抓包文本和 Chrome 临时目录 → 清网络 → 清 pf
#     → 备份 abroadd 状态 → 删用户 → 取消死人开关；
#   - 直连的测试目标是域名（S3、S6b、S8），另有“裸 IP 的 443”和“claude.ai”各一条；Chrome 临时目录的建立、改属主、删除都在。
# 用法：scripts/m3/check-commands.sh <run 空跑输出> <restore 空跑输出>
set -uo pipefail
cd "$(dirname "$0")/../.."
[ $# -eq 2 ] || { echo "用法：$0 <run 空跑输出> <restore 空跑输出>" >&2; exit 2; }
fails=0
bad() { echo "✗ $*"; fails=$((fails + 1)); }
pats=$(grep -v '^#' scripts/m3/commands.txt | grep .)
BAN='pfctl (-d|-e)( |$)|pfctl( -o none)? -f |pfctl -F all|pkill|killall|networksetup -setdnsservers|route .*delete.*default|route .*delete -net 0\.0\.0\.0/0'
# tcpdump：出现写文件、轮转、执行命令、读文件的选项就是违规（只允许 -i 网卡 -n -l [-q] 过滤式）。
TCPDUMP_BAN='tcpdump.*( -[a-zA-Z]*[wWCGzrZ]| --(write|relinquish|compress))'
for f in "$1" "$2"; do
  n=0
  while IFS= read -r line; do
    cmd=${line#CHANGE: }
    n=$((n + 1))
    ok=0
    while IFS= read -r p; do
      if printf '%s\n' "$cmd" | grep -qE -- "$p"; then ok=1; break; fi
    done <<<"$pats"
    [ $ok = 1 ] || bad "不在清单里：$cmd"
    if printf '%s\n' "$cmd" | grep -qE -- "$BAN"; then bad "禁止的命令：$cmd"; fi
    if printf '%s\n' "$cmd" | grep -qE -- "$TCPDUMP_BAN"; then bad "tcpdump 只能只读抓包：$cmd"; fi
    if printf '%s\n' "$cmd" | grep -qE -- '(^| )open .*Chrome' && ! printf '%s\n' "$cmd" | grep -qE -- '^sudo -u [^ ]+ open '; then
      bad "Chrome 必须以用户身份（sudo -u）启动：$cmd"
    fi
  done < <(grep '^CHANGE: ' "$f")
  echo "$(basename "$f")：核对了 $n 条改动命令"
done
# 死人开关之前只允许建状态目录。
pre=$(awk '/^CHANGE: 死人开关/{exit} /^CHANGE: /{print}' "$1")
if ! grep -q '^CHANGE: 死人开关' "$1"; then
  bad "run.sh 的空跑里没有死人开关"
elif printf '%s\n' "$pre" | grep -vE '^CHANGE: (mkdir -p|chmod 755) /var/run/claude-abroad-m3$' | grep -q .; then
  bad "死人开关之前有建状态目录以外的改动：$(printf '%s' "$pre" | tr '\n' ' ')"
else
  echo "✓ 死人开关之前只有建状态目录（$(printf '%s\n' "$pre" | grep -c .) 条），其余改动都在它之后"
fi
line_of() { grep -n -m1 -E "$1" "$2" | cut -d: -f1; }
# run.sh：快照（含持久副本）在安装程序、建用户之前。
s1=$(line_of 'CHANGE: mkdir -p /var/run/claude-abroad-m3/snap$' "$1"); s2=$(line_of 'CHANGE: cp -R /var/run/claude-abroad-m3/snap/' "$1")
i1=$(line_of 'CHANGE: mkdir -p /Library/Application Support/claude-abroad-m3/bin' "$1"); u1=$(line_of 'CHANGE: dscl . -create' "$1")
dm=$(line_of 'CHANGE: 死人开关' "$1")
if [ -n "$s1$s2$i1$u1$dm" ] && [ "$dm" -lt "$s1" ] && [ "$s1" -lt "$s2" ] && [ "$s2" -lt "$i1" ] && [ "$i1" -lt "$u1" ]; then
  echo "✓ run.sh 的顺序：死人开关（${dm}）→ 快照（${s1}、持久副本 ${s2}）→ 安装程序（${i1}）→ 建用户（${u1}）"
else
  bad "run.sh 的顺序不对：死人开关 ${dm} 快照 ${s1}/${s2} 安装 ${i1} 建用户 ${u1}"
fi
# restore.sh 的顺序。
a=$(line_of 'CHANGE: launchctl bootout' "$2"); b=$(line_of 'CHANGE: kill -TERM <abroad-vm' "$2")
u=$(line_of 'CHANGE: kill -TERM <(wg-a|clash-b)\.pid' "$2"); p=$(line_of 'CHANGE: kill -TERM <tcpdump-' "$2")
ch=$(line_of 'CHANGE: kill -TERM <chrome\.pid' "$2"); cr=$(line_of 'CHANGE: rm -f /var/run/claude-abroad-m3/cap-' "$2")
cd_=$(line_of 'CHANGE: rm -rf /var/run/claude-abroad-m3/chrome$' "$2")
c=$(line_of 'CHANGE: route -n delete' "$2"); d=$(line_of 'CHANGE: scutil <<remove' "$2"); e=$(line_of 'CHANGE: networksetup -setv6' "$2")
g=$(line_of 'CHANGE: /sbin/pfctl -a .* -F all' "$2"); m=$(line_of 'CHANGE: mv /var/run/claude-abroad-m3/abroadd ' "$2")
h=$(line_of 'CHANGE: dscl . -delete' "$2"); k=$(line_of 'CHANGE: kill <死人开关' "$2")
for v in a b u p ch cr cd_ c d e g m h k; do [ -n "${!v}" ] || bad "restore.sh 的空跑里缺了一步（${v}）"; done
if [ -n "$a" ] && [ -n "$b" ] && [ -n "$u" ] && [ -n "$p" ] && [ -n "$ch" ] && [ -n "$cr" ] && [ -n "$cd_" ] && [ -n "$c" ] &&
  [ -n "$d" ] && [ -n "$e" ] && [ -n "$g" ] && [ -n "$m" ] && [ -n "$h" ] && [ -n "$k" ] &&
  [ "$a" -lt "$c" ] && [ "$b" -lt "$c" ] && [ "$u" -lt "$c" ] && [ "$p" -lt "$c" ] && [ "$ch" -lt "$c" ] &&
  [ "$ch" -lt "$cd_" ] && [ "$p" -lt "$cr" ] && [ "$cr" -lt "$c" ] && [ "$cd_" -lt "$c" ] &&
  [ "$c" -lt "$g" ] && [ "$d" -lt "$g" ] && [ "$e" -lt "$g" ] && [ "$g" -lt "$m" ] && [ "$m" -lt "$h" ] && [ "$h" -lt "$k" ]; then
  echo "✓ restore.sh 的顺序：停服务（${a}）、停虚拟机（${b}）、上游（${u}）、抓包（${p}）、Chrome（${ch}）→ 删抓包文本（${cr}）、Chrome 临时目录（${cd_}）→ 清网络（${c}、${d}、${e}）→ 清 pf（${g}）→ 备份 abroadd 状态（${m}）→ 删用户（${h}）→ 取消死人开关（${k}）"
else
  bad "restore.sh 的顺序不对"
fi
# 验收 l：直连目标是域名；裸 IP 的 443、claude.ai 各一条；Chrome 以用户身份启动、临时目录的三步都在。
nc_dom=$(grep -cE "^USER: .*/usr/bin/nc -w [0-9]+ '[a-z0-9.-]*[a-z][a-z0-9.-]*' 443" "$1" || true)
nc_raw=$(grep -cE '^USER: .*/usr/bin/nc -w [0-9]+ [0-9.]+ 443' "$1" || true)
nc_cl=$(grep -cE '^USER: .*/usr/bin/nc -w [0-9]+ claude\.ai 443' "$1" || true)
[ "$nc_dom" -ge 3 ] && echo "✓ 直连的测试目标是域名（S3、S6b、S8 共 ${nc_dom} 条）" || bad "直连的测试目标应是域名（找到 ${nc_dom} 条）"
[ "$nc_raw" -ge 1 ] && echo "✓ 有“nc 连裸 IP 的 443（应走主代理）”这一小步" || bad "缺少“裸 IP 的 443”这一小步"
[ "$nc_cl" -ge 1 ] && echo "✓ 停用后有“nc 连 claude.ai”这一步" || bad "缺少停用后访问 claude.ai 的一步"
for pat in 'mkdir -p /var/run/claude-abroad-m3/chrome$' 'chown [^ ]+ /var/run/claude-abroad-m3/chrome$' 'chmod 700 /var/run/claude-abroad-m3/chrome$' 'sudo -u [^ ]+ open -na Google Chrome'; do
  grep -qE "^CHANGE: ${pat}" "$1" || bad "run.sh 的空跑里缺少：${pat}"
done
grep -qE '^CHANGE: rm -rf /var/run/claude-abroad-m3/chrome$' "$2" || bad "restore.sh 的空跑里缺少删除 Chrome 临时目录"
[ $fails = 0 ] && echo "✓ 命令核对通过" || echo "✗ 命令核对没通过（$fails 处）"
exit $((fails > 0))
