#!/usr/bin/env bash
# 列出本项目和用户目录下定义的 Claude Code 子 agent：名称、模型、工具、说明。
# 用法：scripts/agents.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"

show() {
  local dir="$1" label="$2"
  [ -d "$dir" ] || return 0
  local found=0
  for f in "$dir"/*.md; do
    [ -e "$f" ] || continue
    found=1
    awk -v file="$f" -v label="$label" '
      NR == 1 && $0 != "---" { exit }
      NR > 1 && $0 == "---" { exit }
      NR > 1 {
        k = $0; sub(/:.*/, "", k)
        v = $0; sub(/^[^:]*:[ ]*/, "", v)
        m[k] = v
      }
      END {
        printf "[%s] %s\n", label, (m["name"] != "" ? m["name"] : file)
        printf "  模型：%s\n", (m["model"] != "" ? m["model"] : "inherit（跟随主会话）")
        printf "  工具：%s\n", (m["tools"] != "" ? m["tools"] : "全部")
        printf "  文件：%s\n", file
        printf "  说明：%s\n\n", m["description"]
      }' "$f"
  done
  [ "$found" = 1 ] || echo "[$label] （没有 agent）"
}

show "$root/.claude/agents" "本项目"
show "$HOME/.claude/agents" "用户全局"
