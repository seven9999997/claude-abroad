#!/usr/bin/env bash
# 核对 DRY_RUN=1 空跑的输出（m4-plan v2.13 验收 c、d、f，v2.14 验收 n、o）：
#   - 每一条 CHANGE: 都落在 scripts/m4/commands.txt 的清单里；
#   - 禁止项沿用 M3（pfctl -d/-e、不带锚点的 pfctl -f 和 -F all、pkill、killall、networksetup -setdnsservers、对默认路由的
#     route delete、tcpdump 写文件或执行命令），另加：
#       1. 改语言的命令（AppleLanguages、AppleLocale）只能是 restore 按快照写回（带“快照里的”）；脚本自己改语言只经 abroadd，
#          默认的空跑里看不到“语言 xx”的目标，打开 M4_CHANGE_LANGUAGE=1 的空跑里才有；
#       2. 向进程发信号只能是本脚本记下的 PID 文件（abroad-vm、抓包、假 Claude 进程）和死人开关；脚本自己不结束任何 Claude 进程，
#          不启动真实的 claude（出现 claude 的命令只能是 m4fake 临时目录下的那个副本）；
#       3. 改系统代理、改 IPv6 必须成对出现（run 里制造之后有撤销），restore 里有兜底；
#   - run.sh：死人开关之前只有建状态目录；快照（网络、地区、持久副本）在安装程序、建用户之前；
#   - restore.sh：停服务和进程 → 撤销人为泄漏 → 清网络 → 还原地区 → 清 pf → 备份 abroadd 状态 → 删临时文件 → 删用户
#     → 取消死人开关；
#   - 默认时区取自“应用主代理”的预览（STEP 行）。
# 用法：scripts/m4/check-commands.sh <run 空跑输出> <restore 空跑输出> [lang|kill|tz]（第三个参数：这份空跑打开了哪个开关）
set -uo pipefail
cd "$(dirname "$0")/../.."
[ $# -ge 2 ] || { echo "用法：$0 <run 空跑输出> <restore 空跑输出> [lang|kill|tz]" >&2; exit 2; }
MODE=${3:-default}
fails=0
bad() { echo "✗ $*"; fails=$((fails + 1)); }
pats=$(grep -v '^#' scripts/m4/commands.txt | grep .)
BAN='pfctl (-d|-e)( |$)|pfctl( -o none)? -f |pfctl -F all|pkill|killall|networksetup -setdnsservers|route .*delete.*default|route .*delete -net 0\.0\.0\.0/0'
TCPDUMP_BAN='tcpdump.*( -[a-zA-Z]*[wWCGzrZ]| --(write|relinquish|compress))'
FAKE='/var/run/claude-abroad-m4/m4fake/\.local/share/claude/versions/0\.0\.0-m4fake/claude'
PATF=$(mktemp "${TMPDIR:-/tmp}/m4cc.XXXXXX"); CMDF=$(mktemp "${TMPDIR:-/tmp}/m4cc.XXXXXX")
trap 'rm -f "$PATF" "$CMDF"' EXIT
printf '%s\n' "$pats" >"$PATF"
for f in "$1" "$2"; do
  grep '^CHANGE: ' "$f" | sed 's/^CHANGE: //' >"$CMDF"
  n=$(grep -c . "$CMDF")
  # 整份一次匹配（逐条调用 grep 太慢）：不落在任何一条模式里的，就是清单外的命令。
  while IFS= read -r cmd; do bad "不在清单里：$cmd"; done < <(grep -vE -f "$PATF" "$CMDF")
  while IFS= read -r cmd; do bad "禁止的命令：$cmd"; done < <(grep -E -- "$BAN" "$CMDF")
  while IFS= read -r cmd; do bad "tcpdump 只能只读抓包：$cmd"; done < <(grep -E -- "$TCPDUMP_BAN" "$CMDF")
  # 规则 1：改语言只能是按快照写回。
  while IFS= read -r cmd; do bad "脚本不能自己改语言（只能 restore 按快照写回）：$cmd"; done < <(grep -E -- '(write|delete).*(AppleLanguages|AppleLocale)' "$CMDF" | grep -vF -- '快照里的')
  # 规则 2：信号只发给记下的 PID 文件和死人开关；不结束 Claude。
  while IFS= read -r cmd; do bad "只能向本脚本记下的 PID 文件发信号：$cmd"; done < <(grep -E -- '(^| )kill( |$)' "$CMDF" |
    grep -vE -- '^kill( -TERM| -KILL)? <(abroad-vm|tcpdump-[a-z0-9-]+|fake)\.pid 里的 PID>$|^kill <死人开关的 PID>$')
  # 规则 2（续）：任何出现 claude 可执行文件的命令（CHANGE、USER）只能是 m4fake 下的副本。
  while IFS= read -r line; do
    printf '%s\n' "$line" | grep -qE -- "${FAKE}" || bad "启动了真实的 claude 或别的 Claude 进程：${line}"
  done < <(grep -E '^(CHANGE|USER): ' "$f" | grep -E '(^|[ /])claude( |$)' | grep -vE 'claude-(abroad|facts)')
  grep -qE '^(CHANGE|USER): .*(pkill|killall|kill ).*[Cc]laude' "$f" && bad "脚本在结束 Claude 进程"
  echo "$(basename "$f")：核对了 $n 条改动命令"
done
line_of() { grep -n -m1 -E "$1" "$2" | cut -d: -f1; }
# 规则 3：成对出现。
on=$(line_of '^CHANGE: networksetup -setsecurewebproxystate .* on$' "$1"); off=$(line_of '^CHANGE: networksetup -setsecurewebproxystate [^<].* off$' "$1")
if [ -n "$on" ]; then
  [ -n "$off" ] && [ "$off" -gt "$on" ] && echo "✓ run 里打开系统代理（${on}）之后有关闭（${off}）" || bad "run 里打开了系统代理却没有成对的关闭"
fi
grep -qE '^CHANGE: networksetup -setsecurewebproxystate .* off$' "$2" || bad "restore 里没有关闭系统代理的兜底"
v6a=$(line_of '^CHANGE: networksetup -setv6automatic [^<]' "$1"); v6o=$(line_of '^CHANGE: networksetup -setv6off [^<]' "$1")
if [ -n "$v6a" ]; then
  [ -n "$v6o" ] && [ "$v6o" -gt "$v6a" ] && echo "✓ run 里打开 IPv6（${v6a}）之后有关闭（${v6o}）" || bad "run 里打开了 IPv6 却没有成对的关闭"
fi
grep -qE '^CHANGE: networksetup -setv6' "$2" || bad "restore 里没有按快照还原 IPv6 的兜底"
# 死人开关之前只允许建状态目录。
pre=$(awk '/^CHANGE: 死人开关/{exit} /^CHANGE: /{print}' "$1")
if ! grep -q '^CHANGE: 死人开关' "$1"; then
  bad "run.sh 的空跑里没有死人开关"
elif printf '%s\n' "$pre" | grep -vE '^CHANGE: (mkdir -p|chmod 755) /var/run/claude-abroad-m4$' | grep -q .; then
  bad "死人开关之前有建状态目录以外的改动：$(printf '%s' "$pre" | tr '\n' ' ')"
else
  echo "✓ 死人开关之前只有建状态目录（$(printf '%s\n' "$pre" | grep -c .) 条）"
fi
# run.sh：死人开关 → 快照（网络、地区、持久副本）→ 安装程序 → 建用户。
dm=$(line_of 'CHANGE: 死人开关' "$1"); s1=$(line_of 'CHANGE: mkdir -p /var/run/claude-abroad-m4/snap$' "$1")
sr=$(line_of '^STATE: snap/region' "$1"); s2=$(line_of 'CHANGE: cp -R /var/run/claude-abroad-m4/snap/' "$1")
i1=$(line_of 'CHANGE: mkdir -p /Library/Application Support/claude-abroad-m4/bin' "$1"); u1=$(line_of 'CHANGE: dscl . -create' "$1")
if [ -n "$dm" ] && [ -n "$s1" ] && [ -n "$sr" ] && [ -n "$s2" ] && [ -n "$i1" ] && [ -n "$u1" ] &&
  [ "$dm" -lt "$s1" ] && [ "$s1" -lt "$sr" ] && [ "$sr" -lt "$s2" ] && [ "$s2" -lt "$i1" ] && [ "$i1" -lt "$u1" ]; then
  echo "✓ run.sh 的顺序：死人开关（${dm}）→ 网络快照（${s1}）→ 地区快照（${sr}）→ 持久副本（${s2}）→ 安装程序（${i1}）→ 建用户（${u1}）"
else
  bad "run.sh 的顺序不对：死人开关 ${dm} 快照 ${s1}/${sr}/${s2} 安装 ${i1} 建用户 ${u1}"
fi
# restore.sh 的顺序。
a=$(line_of 'CHANGE: launchctl bootout' "$2"); b=$(line_of 'CHANGE: kill -TERM <abroad-vm' "$2")
p=$(line_of 'CHANGE: kill -TERM <tcpdump-' "$2"); fk=$(line_of 'CHANGE: kill -TERM <fake\.pid' "$2")
lk=$(line_of 'CHANGE: networksetup -setsecurewebproxystate .* off$' "$2"); fr=$(line_of 'CHANGE: rm -rf /var/run/claude-abroad-m4/m4fake$' "$2")
c=$(line_of 'CHANGE: route -n delete' "$2"); d=$(line_of 'CHANGE: scutil <<remove' "$2"); e=$(line_of 'CHANGE: networksetup -setv6' "$2")
r=$(line_of 'CHANGE: /usr/sbin/systemsetup -settimezone' "$2"); r2=$(line_of 'CHANGE: /bin/launchctl asuser .*AppleLocale' "$2")
g=$(line_of 'CHANGE: /sbin/pfctl -a .* -F all' "$2"); m=$(line_of 'CHANGE: mv /var/run/claude-abroad-m4/abroadd ' "$2")
sx=$(line_of 'CHANGE: rm -rf /var/run/claude-abroad-m4/secret$' "$2")
h=$(line_of 'CHANGE: dscl . -delete' "$2"); k=$(line_of 'CHANGE: kill <死人开关' "$2")
miss=0
for v in a b p fk lk fr c d e r r2 g m sx h k; do [ -n "${!v}" ] || { bad "restore.sh 的空跑里缺了一步（${v}）"; miss=1; }; done
if [ $miss = 0 ] && [ "$a" -lt "$lk" ] && [ "$b" -lt "$lk" ] && [ "$p" -lt "$lk" ] && [ "$fk" -lt "$lk" ] && [ "$fk" -lt "$fr" ] &&
  [ "$lk" -lt "$c" ] && [ "$fr" -lt "$c" ] && [ "$c" -lt "$r" ] && [ "$d" -lt "$r" ] && [ "$e" -lt "$r" ] && [ "$r" -lt "$g" ] && [ "$r2" -lt "$g" ] &&
  [ "$g" -lt "$m" ] && [ "$m" -lt "$sx" ] && [ "$sx" -lt "$h" ] && [ "$h" -lt "$k" ]; then
  echo "✓ restore.sh 的顺序：停服务（${a}）、虚拟机（${b}）、抓包（${p}）、假 Claude（${fk}）→ 撤销泄漏（${lk}、${fr}）→ 清网络（${c}、${d}、${e}）→ 还原地区（${r}、${r2}）→ 清 pf（${g}）→ 备份 abroadd 状态（${m}）→ 删临时文件（${sx}）→ 删用户（${h}）→ 取消死人开关（${k}）"
elif [ $miss = 0 ]; then
  bad "restore.sh 的顺序不对"
fi
# 默认时区来自出口（验收 o）；语言开关；结束 Claude 的开关（验收 f）。
grep -q '^STEP: 取“应用主代理”预览里的出口时区' "$1" && echo "✓ 有“取应用主代理预览里的出口时区”这一步" || bad "缺少“取应用主代理预览里的出口时区”这一步"
case "$MODE" in
  lang) grep -qE '^STEP: 锁定手动目标：时区 .*、语言 [a-zA-Z]' "$1" && echo "✓ 打开改语言的开关：目标里有语言" || bad "打开了改语言的开关，目标里却没有语言" ;;
  *) grep -q '^STEP: 锁定手动目标：时区 .*，语言和地区格式保持现在的$' "$1" && echo "✓ 默认不改语言（语言和地区格式保持现在的）" || bad "默认的空跑里出现了改语言的目标" ;;
esac
case "$MODE" in
  tz) grep -q '^STEP: 目标时区用显式覆盖 M4_TZ=' "$1" && echo "✓ 显式给了 M4_TZ 时用覆盖值" || bad "给了 M4_TZ 却没用覆盖值" ;;
  *) grep -q '^STEP: 目标时区用显式覆盖' "$1" && bad "没给 M4_TZ 却出现了覆盖" ;;
esac
ab=$(grep -m1 '^ABROADD: ' "$1")
printf '%s' "$ab" | grep -q -- '--dev-skip-region' && bad "abroadd 的参数里不能有 --dev-skip-region"
case "$MODE" in
  kill) printf '%s' "$ab" | grep -q -- '--dev-skip-claude-kill' && bad "打开了 M4_REAL_KILL，abroadd 却仍带跳过参数" || echo "✓ 打开 M4_REAL_KILL：abroadd 不带跳过结束 Claude 的参数" ;;
  *) printf '%s' "$ab" | grep -q -- '--dev-skip-claude-kill' && echo "✓ 默认：abroadd 带跳过结束 Claude 的参数、不带跳过地区的参数" || bad "默认时 abroadd 应带 --dev-skip-claude-kill" ;;
esac
[ $fails = 0 ] && echo "✓ 命令核对通过" || echo "✗ 命令核对没通过（$fails 处）"
exit $((fails > 0))
