#!/usr/bin/env bash
# 带完整性校验的构建（M6-7）：先编译清单里的程序，生成完整性清单，再把清单的摘要经 -ldflags 写进 abroadd 和
# abroad-guard 编译出来。只编译和写 scripts/dev/out/integrity/：不安装、不注册系统服务、不运行产物。
#
# 清单包含：abroad（命令行）、abroad-rescue（应急程序）、abroad-vm，以及 guest/out/ 下已经构建好的虚拟机镜像（有就加）。
# 不包含 abroadd、abroad-guard 自己（它们的内容取决于清单的摘要）。
#
# 用法：scripts/dev/integrity-build.sh          # 发布构建（不带开发参数）
#       scripts/dev/integrity-build.sh --dev    # 开发构建（-tags abroaddev）
# 运行时：abroadd/abroad-guard --integrity-manifest <out>/integrity.json --integrity-base <out>
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

TAGS=()
[[ "${1:-}" == "--dev" ]] && TAGS=(-tags abroaddev)

OUT="scripts/dev/out/integrity"
rm -rf "${OUT}"
mkdir -p "${OUT}/bin" "${OUT}/image"

go build ${TAGS[@]+"${TAGS[@]}"} -trimpath -o "${OUT}/bin/abroad" ./cmd/abroad
go build -trimpath -o "${OUT}/bin/abroad-rescue" ./cmd/abroad-rescue # 应急程序没有开发构建（M6-5b）
go build ${TAGS[@]+"${TAGS[@]}"} -trimpath -o "${OUT}/bin/abroad-vm" ./cmd/abroad-vm
scripts/dev/sign.sh "${OUT}/bin/abroad-vm" >/dev/null # 虚拟化授权（ad-hoc）：要在生成清单之前，签名会改文件内容
go build -o "${OUT}/abroad-dev" ./cmd/abroad-dev

FILES=(bin/abroad bin/abroad-rescue bin/abroad-vm)
for f in guest/out/Image guest/out/initramfs.gz guest/out/modloop guest/out/rootfs.img; do
  [[ -f "$f" ]] || continue
  cp "$f" "${OUT}/image/"
  FILES+=("image/$(basename "$f")")
done

DIGEST="$("${OUT}/abroad-dev" integrity-manifest -base "${OUT}" -out "${OUT}/integrity.json" "${FILES[@]}")"
LDF="-X claude-abroad/internal/integrity.ManifestDigest=${DIGEST}"
go build ${TAGS[@]+"${TAGS[@]}"} -trimpath -ldflags "${LDF}" -o "${OUT}/bin/abroadd" ./cmd/abroadd
go build ${TAGS[@]+"${TAGS[@]}"} -trimpath -ldflags "${LDF}" -o "${OUT}/bin/abroad-guard" ./cmd/abroad-guard
rm -f "${OUT}/abroad-dev"

echo "完整性清单：${OUT}/integrity.json（${#FILES[@]} 个文件）"
echo "清单摘要已写进 abroadd、abroad-guard：${DIGEST}"
