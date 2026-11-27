#!/usr/bin/env bash
# M5-11：把 SwiftPM 编出的 ClaudeAbroad 组装成应用包（.app），临时签名（ad-hoc），输出到被忽略的构建目录
# app/.build/bundle/ClaudeAbroad.app。只编译和拷贝：不安装到“应用程序”、不注册任何系统服务、不启动产物。
# 应用包带 Bundle ID（com.claude-abroad.app），这样应用里系统通知的实现才会被选中（m5-plan v2.9 决定 3）。
#
#	scripts/dev/app-bundle.sh            # 编译（release）并组装、签名、校验
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
[[ "$(id -u)" != "0" ]] || { echo "不要以 root 运行" >&2; exit 1; }
[[ $# -eq 0 ]] || { echo "用法：$0" >&2; exit 2; }

BUNDLE_ID="com.claude-abroad.app"
NAME="ClaudeAbroad"
OUTDIR="app/.build/bundle"
APP="${OUTDIR}/${NAME}.app"

(cd app && swift build -c release --product "${NAME}")
BIN="$(cd app && swift build -c release --product "${NAME}" --show-bin-path)/${NAME}"
[[ -x "${BIN}" ]] || { echo "没有编出 ${BIN}" >&2; exit 1; }

rm -rf "${APP}"
mkdir -p "${APP}/Contents/MacOS" "${APP}/Contents/Resources"
cp "${BIN}" "${APP}/Contents/MacOS/${NAME}"
VERSION="$(git describe --tags --always 2>/dev/null || echo dev)"
# 应用图标（照原型的 APPICON；源文件 app/Resources/AppIcon.svg，改了用 scripts/dev/make-icon.sh 重新生成）
cp app/Resources/AppIcon.icns "${APP}/Contents/Resources/AppIcon.icns"
cat >"${APP}/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleIdentifier</key><string>${BUNDLE_ID}</string>
  <key>CFBundleName</key><string>Claude 出国</string>
  <key>CFBundleDisplayName</key><string>Claude 出国</string>
  <key>CFBundleExecutable</key><string>${NAME}</string>
  <key>CFBundleIconFile</key><string>AppIcon</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>0.5</string>
  <key>CFBundleVersion</key><string>${VERSION}</string>
  <key>LSMinimumSystemVersion</key><string>14.0</string>
  <key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
EOF
# 临时签名（没有付费开发者账号：签名、公证推迟到 M6）。
codesign --force --sign - --identifier "${BUNDLE_ID}" "${APP}"
codesign --verify --strict "${APP}"
echo "应用包：${APP}（Bundle ID ${BUNDLE_ID}，临时签名，codesign --verify 通过）"
