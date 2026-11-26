#!/usr/bin/env bash
# M6-11b 端到端里只有这里碰 pf，而且只有两种固定的形式（selftest 核对本文件里的 pfctl 只有这三行）：
#   pfcheck.sh rules   只读：打印本项目锚点里的规则（核对回国拦截是测试段）
#   pfcheck.sh kill    只清掉目的地在测试段里的连接状态（验证定向清理的写法和效果；测试段是文档保留地址，没有真实连接）
# 不接受别的参数；空跑（DRY_RUN=1）只打印。
set -uo pipefail
[ $# = 1 ] || { echo "用法：$0 rules|kill" >&2; exit 2; }
run() { if [ "${DRY_RUN:-0}" = 1 ]; then printf '%s: %s\n' "$1" "${*:2}"; else "${@:2}"; fi; }
case "$1" in
  rules) run QUERY pfctl -a com.apple/250.abroad -s rules ;;
  kill)
    run CHANGE pfctl -k 0.0.0.0/0 -k 192.0.2.0/24 || exit 1
    run CHANGE pfctl -k ::/0 -k 2001:db8:abd::/48 ;;
  *) echo "用法：$0 rules|kill" >&2; exit 2 ;;
esac
