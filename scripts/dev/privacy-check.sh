#!/usr/bin/env bash
# 公开仓库的隐私检查：扫工作区里会进版本库的文件（以及最近一次提交的内容），不许出现本机路径、私钥、个人邮箱，
# 以及 docs/LOCAL.patterns（不进版本库，每行一个扩展正则）里列的本机信息。CI 里没有本地模式文件，只扫通用模式。
#
#   scripts/dev/privacy-check.sh           # 工作区
#   scripts/dev/privacy-check.sh --commit  # 再加上 HEAD 提交的内容和提交说明
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
GENERIC=(
  '/Users/[A-Za-z]'
  'C:\\Users\\[A-Za-z]'
  '/home/[a-z][a-z0-9_-]*/'
  'BEGIN (OPENSSH|RSA|EC|DSA)? ?PRIVATE KEY'
  '[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+\.(com|cn|net|org|io)\b'
)
# 通用模式的合理例外：上游源码里的测试私钥和作者邮箱、许可证文本、Alpine 签名公钥的文件名、无回复地址。
ALLOW='^(third_party/|licenses/|guest/keys/|internal/imagebuild/testdata/apk/)|noreply@|@example\.(com|org|net)|@lists\.alpinelinux\.org|git@github\.com|/Users/(alice|bob|u|secret-person)[/"]'
LOCALPAT="${ROOT}/docs/LOCAL.patterns"
files() { { git ls-files; git ls-files --others --exclude-standard; } | sort -u | while read -r f; do [ -f "$f" ] && printf '%s\n' "$f"; done; }
HITS=0
scan() { # $1 标签，$2 是若干行 “文件:行号:内容”
  local tag="$1" out="$2"
  [ -z "${out}" ] && return
  printf '%s\n' "${out}" | sed "s/^/[${tag}] /" | cut -c1-200
  HITS=$((HITS + $(printf '%s\n' "${out}" | wc -l)))
}
mapfile_files="$(files | grep -v -E '^docs/LOCAL\.(md|patterns)$')"
for p in "${GENERIC[@]}"; do
  scan "通用" "$(printf '%s\n' "${mapfile_files}" | tr '\n' '\0' | xargs -0 grep -I -n -E -- "$p" 2>/dev/null | grep -v -E "${ALLOW}")"
done
if [ -f "${LOCALPAT}" ]; then
  scan "本地" "$(printf '%s\n' "${mapfile_files}" | tr '\n' '\0' | xargs -0 grep -I -n -i -E -f "${LOCALPAT}" 2>/dev/null)"
fi
if [ "${1:-}" = "--commit" ]; then
  for p in "${GENERIC[@]}"; do scan "提交" "$(git grep -I -n -E -- "$p" HEAD 2>/dev/null | sed 's/^HEAD://' | grep -v -E "${ALLOW}")"; done
  if [ -f "${LOCALPAT}" ]; then
    scan "提交" "$(git grep -I -n -i -E -f "${LOCALPAT}" HEAD 2>/dev/null | sed 's/^HEAD://')"
    scan "提交说明" "$(git log -1 --format='%B' | grep -n -i -E -f "${LOCALPAT}")"
  fi
fi
if [ "${HITS}" = 0 ]; then echo "隐私检查：零命中"; else echo "隐私检查：${HITS} 处命中"; exit 1; fi
