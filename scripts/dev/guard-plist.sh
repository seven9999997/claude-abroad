#!/usr/bin/env bash
# M6-2b：生成开发用的 abroad-guard LaunchDaemon（开机最早阶段按状态文件加载断网规则，加载后退出）。
# 只生成文件、不安装：不调用 launchctl、不写 /Library/LaunchDaemons、不需要 sudo、不改本机网络。
# 安装和开机时序的验证放在 M6-9 的真机步骤里由用户执行。
#
#	scripts/dev/guard-plist.sh <abroad-guard 的绝对路径> <状态目录> [运行时目录]   # 输出到 scripts/dev/out/
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
if [[ $# -lt 2 || $# -gt 3 ]]; then
  echo "用法：$0 <abroad-guard 的绝对路径> <状态目录> [运行时目录（默认 /var/run/claude-abroad）]" >&2
  exit 2
fi
BIN=$1 STATE=$2 RUN=${3:-/var/run/claude-abroad}
for p in "${BIN}" "${STATE}" "${RUN}"; do
  [[ "${p}" == /* ]] || { echo "路径要写绝对路径：${p}" >&2; exit 2; }
  [[ "${p}" != *[\<\>\&\"\']* ]] || { echo "路径里不能有 XML 特殊字符：${p}" >&2; exit 2; }
done
LABEL="com.claude-abroad.dev.guard"
OUT="scripts/dev/out"
mkdir -p "${OUT}"
F="${OUT}/${LABEL}.plist"
cat >"${F}" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- 开发用（M6-2b）：开机时运行一次 abroad-guard，加载后退出（不常驻）。只由 scripts/dev/guard-plist.sh 生成，没有安装。 -->
<plist version="1.0">
<dict>
  <key>Label</key><string>${LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>${BIN}</string>
    <string>--state-dir</string><string>${STATE}</string>
    <string>--run-dir</string><string>${RUN}</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><false/>
  <key>LaunchOnlyOnce</key><true/>
  <key>StandardErrorPath</key><string>/var/log/claude-abroad-guard.log</string>
</dict>
</plist>
PLIST
/usr/bin/plutil -lint "${F}"
echo "已生成（没有安装）：${F}"
