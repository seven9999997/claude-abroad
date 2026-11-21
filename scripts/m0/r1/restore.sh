#!/bin/bash
# R1 还原：卸载临时服务、结束测试进程、删除安装目录和临时用户、停死人开关。
# 幂等，可重复执行；电脑重启过（/var/run 里的状态没了）也能删掉临时用户。
# 不碰 pf、路由、网卡、DNS（R1 没改过它们）。
# 用法：sudo scripts/m0/r1/restore.sh
set -u
here="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../lib.sh
. "$here/../lib.sh"

[ "$(id -u)" = 0 ] || m0_die "需要用 sudo 运行"

# R2 的残留（pf 规则可能还在，Mac 可能断着网，R2 的死人开关正等着还原）：什么都不动。
# 否则停掉死人开关、删掉测试用户会让 R2 的自动恢复失效。
# 只看“活的” pf 状态；pf_status_before 是 R2 跑完后留下的记录，不算。
if [ -e "$M0_STATE/pf_anchors" ] || [ -e "$M0_STATE/pf_tokens" ]; then
  m0_die "这是 R2 的残留状态（pf 规则可能还在），R1 的还原不处理。请执行 sudo scripts/m0/r2/restore.sh"
fi

m0_log "R1 还原开始${M0_FROM_DEADMAN:+（由死人开关触发）}"

# 1. 卸载服务（不存在就忽略）。
for label in com.claude-abroad.m0.r1-user com.claude-abroad.m0.r1-root com.claude-abroad.m0.r1-userhome \
  $(m0_state_get labels); do
  if launchctl print "system/$label" >/dev/null 2>&1; then
    launchctl bootout "system/$label" 2>/dev/null || true
    m0_log "已卸载服务 $label"
  fi
done
rm -f "$M0_STATE/labels"

# 2. 结束记下的 abroad-vm（只结束程序名确实是 abroad-vm 的）。
for pid in $(m0_state_get pids); do
  for _ in $(seq 1 30); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 1
  done
  if kill -0 "$pid" 2>/dev/null; then
    case "$(ps -p "$pid" -o comm= 2>/dev/null)" in
      *abroad-vm)
        kill -KILL "$pid" 2>/dev/null || true
        m0_log "强制结束 abroad-vm（pid $pid）"
        ;;
    esac
  fi
done
rm -f "$M0_STATE/pids"

# 3. 删除临时用户：先按状态删，再不依赖状态补一遍（电脑重启后 /var/run 被清空）。
#    只删 RealName 是本测试设置的那个，避免误删同名的其他用户。
m0_users_delete
if dscl . -read "/Users/$M0_TEST_USER" >/dev/null 2>&1; then
  rn=$(dscl . -read "/Users/$M0_TEST_USER" RealName 2>/dev/null | sed '1s/^RealName:[[:space:]]*//' | tr -d '\n' | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
  if [ "$rn" = "claude-abroad M0 test" ]; then
    pkill -u "$M0_TEST_USER" 2>/dev/null || true
    dscl . -delete "/Users/$M0_TEST_USER" 2>/dev/null || true
    dscl . -delete "/Groups/$M0_TEST_USER" 2>/dev/null || true
    dscacheutil -flushcache
    m0_log "已删除临时用户 $M0_TEST_USER（按 RealName 识别）"
  else
    m0_log "用户 $M0_TEST_USER 存在但 RealName 是“${rn}”，不是本测试建的，不删"
  fi
fi

# 4. 删除安装目录（只删 r1 子目录；上级目录为空则一并删）。
R1DIR=$(m0_state_get r1_dir)
[ -n "$R1DIR" ] || R1DIR="$M0_INSTALL/r1"
case "$R1DIR" in
  "$M0_INSTALL/r1")
    if [ -e "$R1DIR" ]; then rm -rf "$R1DIR" && m0_log "已删除 $R1DIR"; fi
    ;;
  *) m0_log "状态里的目录 $R1DIR 不是预期路径，跳过删除" ;;
esac
rmdir "$M0_INSTALL" 2>/dev/null || true
rm -f "$M0_STATE/r1_dir" "$M0_STATE/r1"

# 5. 停死人开关，清空状态目录。
m0_deadman_stop
rm -f "$M0_STATE/deadman.log" "$M0_STATE/pf_status_before"
rmdir "$M0_STATE/sock" 2>/dev/null || true
rmdir "$M0_STATE" 2>/dev/null || true
m0_log "R1 还原完成"
