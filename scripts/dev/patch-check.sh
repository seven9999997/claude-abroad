#!/usr/bin/env bash
# 补丁核对（M6-8c，发版物料）：从干净的上游版本依次打 patches/gvisor-tap-vsock/0001–0007，结果和 third_party/gvisor-tap-vsock
# 做 diff -r，必须完全一致（上游有、我们删掉的说明类文件除外，见 TRIMMED）。
# 上游源码经 Go 模块代理下载（go mod download，校验和由 sum.golang.org 核对），不 clone、不改仓库。只写 scripts/dev/out/patch-check/。
#
#   scripts/dev/patch-check.sh
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

MODULE=github.com/containers/gvisor-tap-vsock
UPSTREAM=v0.8.9 # 和 Claude-Cage 的 go.mod 相同（0001–0004 来自 Cage；0005–0007 是本项目的）
# 上游有、third_party 里删掉的文件（只有说明、仓库元数据，不参与编译）。
TRIMMED=(.gitattributes .github .gitignore CODE-OF-CONDUCT.md DEVELOPMENT.md README.md SECURITY.md cmd/qemu-wrapper/README.md doc/MAINTAINERS.md)

OUT="scripts/dev/out/patch-check"
rm -rf "${OUT:?}"
mkdir -p "${OUT}"
DIR=$(cd "${OUT}" && GOFLAGS=-mod=mod go mod download -json "${MODULE}@${UPSTREAM}" | python3 -I -c 'import json,sys; d=json.load(sys.stdin); print(d.get("Dir","")) if not d.get("Error") else sys.exit(d["Error"])')
[ -d "${DIR}" ] || { echo "下载上游 ${MODULE}@${UPSTREAM} 失败" >&2; exit 1; }
cp -R "${DIR}" "${OUT}/upstream"
chmod -R u+w "${OUT}/upstream"
n=0
for p in patches/gvisor-tap-vsock/[0-9][0-9][0-9][0-9]-*.patch; do
  (cd "${OUT}/upstream" && patch -p1 -s --no-backup-if-mismatch --forward <"${ROOT}/${p}") || { echo "✗ 补丁打不上：${p}" >&2; exit 1; }
  n=$((n + 1))
done
for t in "${TRIMMED[@]}"; do rm -rf "${OUT:?}/upstream/${t}"; done
if diff -r "${OUT}/upstream" third_party/gvisor-tap-vsock >"${OUT}/diff.txt"; then
  echo "✓ 补丁核对通过：${MODULE}@${UPSTREAM} 依次打 ${n} 个补丁后和 third_party/gvisor-tap-vsock 完全一致（删掉的说明文件 ${#TRIMMED[@]} 个）"
else
  echo "✗ 补丁核对不通过：打完补丁的上游和 third_party 不一致，差异见 ${OUT}/diff.txt" >&2
  head -20 "${OUT}/diff.txt" >&2
  exit 1
fi
