#!/bin/bash
# pf-user-check.sh：只验证一件事——本机的 pf 用 `user _abroad` 能不能认出 _abroad 发起的连接（R2 的精简版）。
#
# 启用时 pf 只放行 `user _abroad` 的出站；如果这条认不出来，虚拟机连不上主代理节点，出口检查超时、启用回滚。
# 这个脚本只拦“到 9.9.9.9:443 的 TCP”这一个目标，其余网络一概不动，不会断网；结束（含中断）时删掉测试规则、
# 释放 pf 的引用。需要 sudo：  sudo scripts/dev/pf-user-check.sh
set -u
[ "$(id -u)" = 0 ] || { echo "请用 sudo 运行：sudo $0"; exit 1; }
U=_abroad
T=9.9.9.9
A=com.apple/251.abroad-usercheck
ME=${SUDO_USER:-}
id "$U" >/dev/null 2>&1 || { echo "没有 ${U} 用户（还没装后台服务？）"; exit 1; }
[ -n "$ME" ] || { echo "请从普通用户用 sudo 运行（要拿普通用户做对照）"; exit 1; }
IF=$(route -n get default 2>/dev/null | awk '/interface:/{print $2}')
[ -n "$IF" ] || { echo "读不出默认网卡"; exit 1; }

TOKEN=""
cleanup() {
	pfctl -a "$A" -F rules >/dev/null 2>&1
	[ -n "$TOKEN" ] && pfctl -X "$TOKEN" >/dev/null 2>&1
}
trap cleanup EXIT INT TERM

try() { # 用户 是否绑网卡 → 打印 通/不通
	local who=$1 bind=$2 args=(-sk -m 5 -o /dev/null -w '%{http_code}')
	[ "$bind" = 1 ] && args+=(--interface "$IF")
	local code
	code=$(sudo -u "$who" /usr/bin/curl "${args[@]}" "https://${T}/" 2>/dev/null)
	if [ -n "$code" ] && [ "$code" != 000 ]; then echo 通; else echo 不通; fi
}

echo "默认网卡 ${IF}；测试目标 ${T}:443"
echo "1. 加规则之前：普通用户 $(try "$ME" 1)，${U} $(try "$U" 1)（两个都应该“通”，否则这台机器本来就连不上 ${T}）"

TOKEN=$(pfctl -E 2>&1 | awk '/Token/{print $NF}')
printf 'pass out quick inet proto tcp all user %s\nblock drop out quick inet proto tcp to %s port 443\n' "$U" "$T" | pfctl -a "$A" -f - 2>&1 | grep -v -e '^$' -e 'ALTQ' -e 'pf.conf' || true

echo "2. 加规则之后：普通用户 $(try "$ME" 1)（应该“不通”：说明测试规则生效了）"
echo "3. 加规则之后：${U} 绑网卡 $(try "$U" 1)、${U} 不绑网卡 $(try "$U" 0)（“通”= pf 认得出 ${U}；“不通”= 认不出）"
echo "4. 规则计数："
pfctl -a "$A" -vvsr 2>/dev/null | grep -E '^@|Packets' | sed 's/^/   /'
echo "完成，测试规则已删除。把上面整段输出贴回来就行。"
