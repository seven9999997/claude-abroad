#!/usr/bin/env bash
# 由 app/Resources/AppIcon.svg 生成 app/Resources/AppIcon.icns（改了 SVG 后手动执行一次，生成物入库）。
# 只在仓库里读写：临时的 .iconset 放在 mktemp 目录，用完删掉。不改系统。
set -euo pipefail
cd "$(dirname "$0")/../.."
T=$(mktemp -d "${TMPDIR:-/tmp}/appicon.XXXXXX"); trap 'rm -rf "${T:?}"' EXIT
mkdir "$T/AppIcon.iconset"
swift scripts/dev/make-icon.swift app/Resources/AppIcon.svg "$T/AppIcon.iconset"
iconutil -c icns "$T/AppIcon.iconset" -o app/Resources/AppIcon.icns
echo "已生成 app/Resources/AppIcon.icns"
