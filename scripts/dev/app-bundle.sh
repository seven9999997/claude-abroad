#!/usr/bin/env bash
# M5-11：把 SwiftPM 编出的 ClaudeAbroad 组装成应用包（.app），临时签名（ad-hoc），输出到被忽略的构建目录
# app/.build/bundle/ClaudeAbroad.app。只编译和拷贝：不安装到“应用程序”、不注册任何系统服务、不启动产物。
# 应用包带 Bundle ID（com.claude-abroad.app），这样应用里系统通知的实现才会被选中（m5-plan v2.9 决定 3）。
#
#	scripts/dev/app-bundle.sh                    # 编译（release）并组装、签名、校验
#	scripts/dev/app-bundle.sh --payload <目录>   # 同上，并把 payload（scripts/dev/payload.sh 组装的）放进
#	                                             # Contents/Resources/payload/，版本号写进 Info.plist（v0.71c，应用里安装、升级后台服务用）
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
[[ "$(id -u)" != "0" ]] || { echo "不要以 root 运行" >&2; exit 1; }
PAYLOAD=""
case "${1:-}" in
  "") ;;
  --payload) PAYLOAD="${2:?用法：$0 [--payload <目录>]}"; [[ $# -eq 2 ]] || { echo "用法：$0 [--payload <目录>]" >&2; exit 2; } ;;
  *) echo "用法：$0 [--payload <目录>]" >&2; exit 2 ;;
esac
if [[ -n "${PAYLOAD}" ]]; then
  [[ -f "${PAYLOAD}/payload.sha256" && -f "${PAYLOAD}/build.json" ]] || { echo "${PAYLOAD} 不是 payload 目录（先执行 scripts/dev/payload.sh）" >&2; exit 1; }
  (cd "${PAYLOAD}" && shasum -a 256 -s -c payload.sha256) || { echo "${PAYLOAD}/payload.sha256 和里面的文件对不上：重新执行 scripts/dev/payload.sh" >&2; exit 1; }
fi

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
# payload（v0.71c）：放在 Contents/Resources/payload/（会话里实测：签名后 codesign --verify --strict 不报错）；
# 版本号写进 Info.plist，应用用 Bundle 的信息读，不读文件。
PLIST_PAYLOAD=""
if [[ -n "${PAYLOAD}" ]]; then
  cp -R "${PAYLOAD}" "${APP}/Contents/Resources/payload"
  bj() { /usr/bin/python3 -I -c 'import json,sys; v=json.load(open(sys.argv[1]))[sys.argv[2]]; print(str(v).lower() if isinstance(v,bool) else v)' "${PAYLOAD}/build.json" "$1"; }
  PLIST_PAYLOAD="  <key>ClaudeAbroadPayloadCommit</key><string>$(bj commit)</string>
  <key>ClaudeAbroadPayloadBuildTime</key><string>$(bj build_time)</string>
  <key>ClaudeAbroadPayloadDirty</key><string>$(bj dirty)</string>
  <key>ClaudeAbroadPayloadStateFormat</key><string>$(bj state_format)</string>"
fi
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
${PLIST_PAYLOAD}
</dict>
</plist>
EOF
# 签名（v0.73）：用本机签名证书（scripts/dev/local-sign.sh；没有就自动生成），abroadd 只认这张证书签的应用。
# ABROAD_LOCAL_SIGN=0 时退回临时签名（ad-hoc；调用方签名校验启用的后台会把它当成没签名、只能查看）。
SIGN_KIND="临时签名"
if [[ "${ABROAD_LOCAL_SIGN:-1}" != 0 ]]; then
  "$(dirname "$0")/local-sign.sh" sign "${APP}" --identifier "${BUNDLE_ID}"
  SIGN_KIND="本机证书签名"
else
  codesign --force --sign - --identifier "${BUNDLE_ID}" "${APP}"
fi
codesign --verify --strict "${APP}"
if [[ -n "${PAYLOAD}" ]]; then
  # 签名不能改动 payload 里的文件（abroad-vm 自带的虚拟化授权签名尤其不能动）
  (cd "${APP}/Contents/Resources/payload" && shasum -a 256 -s -c payload.sha256) || { echo "✗ 签名之后 payload 的内容变了" >&2; exit 1; }
fi
echo "应用包：${APP}（Bundle ID ${BUNDLE_ID}，${SIGN_KIND}，codesign --verify --strict 通过${PAYLOAD:+，带 payload}）"
