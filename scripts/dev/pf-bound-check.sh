#!/bin/bash
# pf-bound-check.sh：用和虚拟机完全相同的拨号方式（IP_BOUND_IF 绑物理网卡，m0-r2-probe）验证
# pf 的 `pass out quick inet proto tcp all user _abroad` 能不能放行 _abroad 的连接，并打出被拦时系统给的错误原文。
#
# 只拦“到 223.5.5.5:443 的 TCP”这一个目标，其余网络不动，不断网；结束（含中断）时删测试规则、释放 pf 引用。
#   sudo scripts/dev/pf-bound-check.sh
set -u
[ "$(id -u)" = 0 ] || { echo "请用 sudo 运行：sudo $0"; exit 1; }
U=_abroad
T=223.5.5.5:443
A=com.apple/251.abroad-boundcheck
ME=${SUDO_USER:-}
[ -n "$ME" ] || { echo "请从普通用户用 sudo 运行"; exit 1; }
id "$U" >/dev/null 2>&1 || { echo "没有 ${U} 用户"; exit 1; }
PHY=$(route -n get default 2>/dev/null | awk '/interface:/{print $2}')
[ -n "$PHY" ] || { echo "读不出默认网卡"; exit 1; }
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
GO=$(sudo -u "$ME" bash -lc 'command -v go' 2>/dev/null); GO=${GO:-/usr/local/go/bin/go}

DIR=$(mktemp -d /tmp/abroad-boundcheck.XXXXXX)
chmod 755 "$DIR"
TOKEN=""
cleanup() {
	pfctl -a "$A" -F rules >/dev/null 2>&1
	[ -n "$TOKEN" ] && pfctl -X "$TOKEN" >/dev/null 2>&1
	rm -rf "$DIR"
}
trap cleanup EXIT INT TERM

echo "编译探测程序……"
B=$(sudo -u "$ME" mktemp -d)
sudo -u "$ME" bash -c "cd '$ROOT' && '$GO' build -o '$B/probe' ./cmd/m0-r2-probe" || { echo "编译失败"; exit 1; }
cp "$B/probe" "$DIR/probe" && chmod 755 "$DIR/probe" && rm -rf "$B"

p() { # 用户 是否绑网卡
	local a=(dial -net tcp -to "$T")
	[ "$2" = 1 ] && a+=(-iface "$PHY")
	sudo -u "$1" "$DIR/probe" "${a[@]}" 2>&1 | sed 's/^RESULT //'
}

echo "物理网卡 ${PHY}；测试目标 ${T}"
echo "1. 加规则前：${ME} 绑网卡：$(p "$ME" 1)"
echo "   加规则前：${U} 绑网卡：$(p "$U" 1)"

TOKEN=$(pfctl -E 2>&1 | awk '/Token/{print $NF}')
printf 'pass out quick inet proto tcp all user %s\nblock drop out quick inet proto tcp to %s port 443\n' "$U" "${T%:*}" |
	pfctl -a "$A" -f - 2>&1 | grep -v -e '^$' -e 'ALTQ' -e 'flushing of rules' -e 'present in the main' || true

echo "2. 加规则后：${ME} 绑网卡（应被拦）：$(p "$ME" 1)"
echo "3. 加规则后：${U} 绑网卡（关键）：$(p "$U" 1)"
echo "   加规则后：${U} 不绑网卡：$(p "$U" 0)"
echo "4. 规则计数："
pfctl -a "$A" -vvsr 2>/dev/null | grep -E '^@|Packets' | sed 's/^/   /'
echo "完成，测试规则已删除。把整段输出贴回来就行。"
