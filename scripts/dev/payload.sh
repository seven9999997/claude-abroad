#!/usr/bin/env bash
# 组装安装用的 payload（v0.71，docs/app-install-plan.md 1.2）：在 integrity-build.sh 的产物上加安装脚本和逐文件摘要。
# 命令行（install.sh --from，默认就是这个目录）和应用包（阶段 B）用同一种布局：
#
#   payload/
#   ├── bin/{abroad,abroad-rescue,abroadd,abroad-guard,abroad-vm}
#   ├── image/{Image,initramfs.gz,modloop,rootfs.img}
#   ├── integrity.json   build.json
#   ├── scripts/m6/install/{install.sh,uninstall.sh,lib.sh}
#   ├── scripts/lib/common.sh            # install.sh 用到 ab_dev_flags_missing；放在和仓库相同的相对位置
#   └── payload.sha256                   # 上面每个文件一行（shasum -a 256 的格式，相对路径）
#
# 只复制和计算摘要：不安装、不注册服务、不运行产物。以自己的身份运行（不要 sudo）。
#
#   scripts/dev/integrity-build.sh --dev && scripts/dev/payload.sh
#   scripts/dev/payload.sh [--src <integrity 产物目录>] [--out <输出目录>]
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
[[ "$(id -u)" != "0" ]] || { echo "不要以 root 运行" >&2; exit 1; }

SRC="scripts/dev/out/integrity"
OUT="scripts/dev/out/payload"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --src) SRC="${2:?}"; shift 2 ;;
    --out) OUT="${2:?}"; shift 2 ;;
    *) echo "用法：$0 [--src <目录>] [--out <目录>]" >&2; exit 2 ;;
  esac
done

FILES=(bin/abroad bin/abroad-rescue bin/abroadd bin/abroad-guard bin/abroad-vm
  image/Image image/initramfs.gz image/modloop image/rootfs.img integrity.json build.json)
SCRIPTS=(scripts/m6/install/install.sh scripts/m6/install/uninstall.sh scripts/m6/install/lib.sh scripts/lib/common.sh)
miss=0
for f in "${FILES[@]}"; do
  [[ -f "${SRC}/${f}" ]] || { echo "缺少 ${SRC}/${f}：先执行 scripts/dev/integrity-build.sh --dev（要先有虚拟机镜像 guest/out）" >&2; miss=1; }
done
[[ "${miss}" = 0 ]] || exit 1

rm -rf "${OUT}"
mkdir -p "${OUT}/bin" "${OUT}/image" "${OUT}/scripts/m6/install" "${OUT}/scripts/lib"
for f in "${FILES[@]}"; do cp -p "${SRC}/${f}" "${OUT}/${f}"; done
for f in "${SCRIPTS[@]}"; do cp -p "${f}" "${OUT}/${f}"; done
(cd "${OUT}" && LC_ALL=C shasum -a 256 "${FILES[@]}" "${SCRIPTS[@]}" >payload.sha256)
(cd "${OUT}" && shasum -a 256 -s -c payload.sha256) || { echo "✗ 刚生成的 payload.sha256 核对不过" >&2; exit 1; }
echo "payload：${OUT}（$(wc -l <"${OUT}/payload.sha256" | tr -d ' ') 个文件，版本 $(cat "${OUT}/build.json")）"
