#!/usr/bin/env bash
# 发版物料（M6-8c，m6-plan M6-8）：只构建、比对、复制，不安装、不签名以外改任何东西。输出到 scripts/dev/out/release/：
#   1. 补丁核对（scripts/dev/patch-check.sh）；
#   2. 发布构建（scripts/dev/integrity-build.sh，不带开发参数），并核对 abroadd 里没有任何 --dev- 参数；
#   3. 软件包和上游文件存档 sources/：guest/manifest.json 里每个 Alpine 软件包和上游文件（内核、minirootfs、引擎）
#      从 guest/out/cache 复制过来并逐个核对 SHA-256（Alpine 稳定分支更新后会删旧版本，R19）；
#   4. 构建清单 BUILD-MANIFEST.txt：源码版本、Go 版本、镜像清单和完整性清单的摘要、每个程序和镜像文件的 SHA-256、
#      存档的文件、补丁核对结果。
# 前提：guest/out 里已经有构建好的镜像和缓存（scripts/dev/vm-up.sh 或 abroad-imagebuild 跑过一次）。
#
#   scripts/dev/release-materials.sh
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
OUT="scripts/dev/out/release"
rm -rf "${OUT:?}"
mkdir -p "${OUT}/sources"

echo "== 1. 补丁核对"
PATCH=$(scripts/dev/patch-check.sh)
echo "${PATCH}"

echo "== 2. 发布构建"
scripts/dev/integrity-build.sh >"${OUT}/integrity-build.txt"
B="scripts/dev/out/integrity"
. scripts/lib/common.sh # ab_dev_flags_present（自测 scripts/lib/buildcheck-selftest.sh）
# 用公共函数（先存下输出再判断）：abroadd -h 的退出码是 2，直接接管道在 pipefail 下这条检查永远不会触发
if ab_dev_flags_present "${B}/bin/abroadd" >/dev/null; then
  echo "✗ 发布构建的 abroadd 里还有开发参数" >&2
  exit 1
fi
echo "✓ 发布构建的 abroadd 不认任何 --dev- 参数"
cp -R "${B}" "${OUT}/build"

echo "== 3. 软件包和上游文件存档"
python3 -I - "${OUT}/sources" <<'PY'
import hashlib, json, os, shutil, sys
dst = sys.argv[1]
m = json.load(open("guest/manifest.json", encoding="utf-8"))
items = []
for a in m["artifacts"]:
    items.append(("cache", a["url"].rsplit("/", 1)[1], a["sha256"]))
for p in m["packages"]["list"]:
    items.append(("cache/apk", p["name"] + "-" + p["version"] + ".apk", p["sha256"]))
bad = 0
with open(os.path.join(dst, "SHA256SUMS"), "w", encoding="utf-8") as sums:
    for sub, name, want in items:
        src = os.path.join("guest/out", sub, name)
        try:
            got = hashlib.sha256(open(src, "rb").read()).hexdigest()
        except OSError:
            print("✗ 缓存里没有 %s（先跑一次镜像构建）" % name); bad += 1; continue
        if got != want:
            print("✗ %s 的 SHA-256 和 guest/manifest.json 不一致" % name); bad += 1; continue
        os.makedirs(os.path.join(dst, sub.replace("cache", "").strip("/") or "."), exist_ok=True)
        shutil.copyfile(src, os.path.join(dst, sub.replace("cache", "").strip("/"), name))
        sums.write("%s  %s\n" % (want, os.path.join(sub.replace("cache", "").strip("/"), name).lstrip("/")))
if bad:
    sys.exit(1)
print("✓ 存档 %d 个文件（%d 个上游文件、%d 个 Alpine 软件包），SHA-256 和清单一致" % (len(items), len(m["artifacts"]), len(m["packages"]["list"])))
PY

echo "== 4. 构建清单"
{
  echo "# Claude 出国 发版物料的构建清单（scripts/dev/release-materials.sh 生成）"
  echo "source.commit: $(git rev-parse HEAD)$(git diff --quiet HEAD -- . ':!scripts/dev/out' 2>/dev/null || echo ' (有未提交的改动)')"
  echo "go: $(go version | awk '{print $3, $4}')"
  echo "guest/manifest.json: $(shasum -a 256 guest/manifest.json | cut -d' ' -f1)"
  echo "integrity.json: $(shasum -a 256 "${B}/integrity.json" | cut -d' ' -f1)（已写进 abroadd、abroad-guard）"
  echo "patches: ${PATCH#✓ }"
  echo
  echo "## 程序和镜像（SHA-256）"
  (cd "${B}" && find bin image integrity.json -type f | LC_ALL=C sort | xargs shasum -a 256)
  echo
  echo "## 存档（sources/SHA256SUMS）"
  cat "${OUT}/sources/SHA256SUMS"
} >"${OUT}/BUILD-MANIFEST.txt"
echo "✓ 构建清单：${OUT}/BUILD-MANIFEST.txt"
