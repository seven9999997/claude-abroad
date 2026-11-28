#!/usr/bin/env bash
# 带完整性校验的构建（M6-7）：先编译清单里的程序，生成完整性清单，再把清单的摘要经 -ldflags 写进 abroadd 和
# abroad-guard 编译出来。只编译和写 scripts/dev/out/integrity/：不安装、不注册系统服务、不运行产物。
#
# 清单包含：abroad（命令行）、abroad-rescue（应急程序）、abroad-vm、build.json（版本号），以及 guest/out/ 下已经构建好的
# 虚拟机镜像（有就加）。
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

# 版本号（v0.71 原地升级）：提交号、构建时间（UTC）、工作区有没有未提交的改动，写进全部程序和 build.json。
# 构建时间可以用 ABROAD_BUILD_TIME 指定（只给自测用：格式同下）。
COMMIT="$(git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)"
BUILD_TIME="${ABROAD_BUILD_TIME:-$(date -u +%Y%m%dT%H%M%SZ)}"
[[ "${BUILD_TIME}" =~ ^[0-9]{8}T[0-9]{6}Z$ ]] || { echo "构建时间的格式不对：${BUILD_TIME}" >&2; exit 2; }
DIRTY=false
[[ -z "$(git status --porcelain 2>/dev/null)" ]] || DIRTY=true
BI="-X claude-abroad/internal/buildinfo.Commit=${COMMIT} -X claude-abroad/internal/buildinfo.BuildTime=${BUILD_TIME} -X claude-abroad/internal/buildinfo.Dirty=${DIRTY}"

go build ${TAGS[@]+"${TAGS[@]}"} -trimpath -ldflags "${BI}" -o "${OUT}/bin/abroad" ./cmd/abroad
# 本机签名证书（v0.73）：命令行用它签名（要在生成清单之前），指纹写进 abroadd，调用方签名校验只认这张证书。
# ABROAD_LOCAL_SIGN=0 时不签、不写指纹（校验不启用，和以前一样）。
LEAF=""
if [[ "${ABROAD_LOCAL_SIGN:-1}" != 0 ]]; then
  LEAF="$(scripts/dev/local-sign.sh ensure)"
  scripts/dev/local-sign.sh sign "${OUT}/bin/abroad" --identifier com.claude-abroad.cli
fi
go build -trimpath -ldflags "${BI}" -o "${OUT}/bin/abroad-rescue" ./cmd/abroad-rescue # 应急程序没有开发构建（M6-5b）
# abroad-vm 要用 cgo 调 Virtualization.framework：强制打开 cgo、用系统自带的编译器（2026-11-27 真机：用户终端里的 conda
# 环境让 Go 找不到 C 编译器，悄悄以 CGO_ENABLED=0 编出了一个起不了虚拟机的 abroad-vm），编完核对，没带上 cgo 就失败。
env CGO_ENABLED=1 CC=/usr/bin/clang CXX=/usr/bin/clang++ go build ${TAGS[@]+"${TAGS[@]}"} -trimpath -ldflags "${BI}" -o "${OUT}/bin/abroad-vm" ./cmd/abroad-vm
go version -m "${OUT}/bin/abroad-vm" | grep -q 'CGO_ENABLED=1' ||
  { echo "abroad-vm 没有用 cgo 编出来，起不了虚拟机：请确认装了 Xcode 命令行工具（xcode-select --install）" >&2; exit 1; }
scripts/dev/sign.sh "${OUT}/bin/abroad-vm" >/dev/null # 虚拟化授权（ad-hoc）：要在生成清单之前，签名会改文件内容
go build -o "${OUT}/abroad-dev" ./cmd/abroad-dev

SF="$(sed -n 's/^const StateFormat = \([0-9][0-9]*\)$/\1/p' internal/buildinfo/buildinfo.go)"
[[ -n "${SF}" ]] || { echo "读不出 internal/buildinfo 的 StateFormat" >&2; exit 1; }
printf '{"commit":"%s","build_time":"%s","dirty":%s,"state_format":%s}\n' "${COMMIT}" "${BUILD_TIME}" "${DIRTY}" "${SF}" >"${OUT}/build.json"

FILES=(bin/abroad bin/abroad-rescue bin/abroad-vm build.json)
for f in guest/out/Image guest/out/initramfs.gz guest/out/modloop guest/out/rootfs.img; do
  [[ -f "$f" ]] || continue
  cp "$f" "${OUT}/image/"
  FILES+=("image/$(basename "$f")")
done

DIGEST="$("${OUT}/abroad-dev" integrity-manifest -base "${OUT}" -out "${OUT}/integrity.json" "${FILES[@]}")"
LDF="-X claude-abroad/internal/integrity.ManifestDigest=${DIGEST} ${BI}"
[[ -z "${LEAF}" ]] || LDF="${LDF} -X claude-abroad/internal/callersig.LeafSHA1=${LEAF}"
go build ${TAGS[@]+"${TAGS[@]}"} -trimpath -ldflags "${LDF}" -o "${OUT}/bin/abroadd" ./cmd/abroadd
go build ${TAGS[@]+"${TAGS[@]}"} -trimpath -ldflags "${LDF}" -o "${OUT}/bin/abroad-guard" ./cmd/abroad-guard
# 自检（M6-7 补审返工）：用和 abroadd 同一份校验逻辑检查产物；签名和生成清单的顺序弄反、或清单之后又改了文件，这里就失败。
"${OUT}/abroad-dev" integrity-check -manifest "${OUT}/integrity.json" -base "${OUT}" -digest "${DIGEST}" ||
  { echo "✗ 自检不通过：产物和完整性清单对不上（清单生成之后又改了文件？）" >&2; exit 1; }
rm -f "${OUT}/abroad-dev"
# abroadd --version 要和 build.json 一致（安装脚本也会再核对一次）。
[[ "$("${OUT}/bin/abroadd" --version)" == "$(cat "${OUT}/build.json")" ]] ||
  { echo "✗ abroadd --version 和 build.json 不一致" >&2; exit 1; }

echo "完整性清单：${OUT}/integrity.json（${#FILES[@]} 个文件）"
echo "清单摘要已写进 abroadd、abroad-guard：${DIGEST}"
if [[ -n "${LEAF}" ]]; then
  echo "调用方签名校验：已启用（命令行已用本机证书签名，指纹已写进 abroadd）"
else
  echo "调用方签名校验：未启用（ABROAD_LOCAL_SIGN=0）"
fi
