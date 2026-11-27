#!/usr/bin/env bash
# 一条命令构建应用并放进“应用程序”（v0.71c，docs/app-install-plan.md 3.2）：以自己的身份运行、不用 sudo。
#   开发构建 + 完整性清单（integrity-build.sh --dev）→ 组装 payload（payload.sh）→ 打包应用、把 payload 放进应用包
#   （app-bundle.sh --payload）→ 复制到 /Applications/ClaudeAbroad.app。
# 之后打开应用，按提示点“安装后台服务”或“升级后台服务”，输一次管理员密码，由应用自己装好后台服务。
# 不安装后台服务、不注册系统服务、不改网络、不运行产物。
#
# 放进“应用程序”的规则（放置逻辑只有这一份：scripts/dev/reinstall.sh 升级后台成功后也调用 --place-only，不另写一份）：
#   - 没有同名的：复制进去；
#   - 有同名的、包标识是我们的（com.claude-abroad.app）、你自己能改：先复制到同目录的临时名，再改名替换（不会只剩半个）；
#   - 同名的是符号链接、或包标识不是我们的：不碰，退出码 3；
#   - 是我们的但你改不了（属主是别人或不可写，例如以前的安装脚本以管理员身份放的）：不碰，退出码 4；
#   - 应用还开着：照样替换，结尾提示退出并重新打开。
#
#   scripts/dev/build-app.sh
#   scripts/dev/build-app.sh --place-only    # 不构建，只把 app/.build/bundle 里现成的应用放进“应用程序”（reinstall.sh 用）
#   DRY_RUN=1 scripts/dev/build-app.sh       # 只打印会执行的步骤
# 自测用（scripts/dev/build-app-selftest.sh）：BUILD_APP_TEST_DEST=<临时目录> 代替 /Applications，
# BUILD_APP_TEST_BUNDLE=<现成的应用包> 跳过构建（两个都要给，目标不能是 /Applications）；
# BUILD_APP_TEST_FOREIGN_OWNER=1 假装目标的属主是别人（只能和前两个一起用）。
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}" || exit 1
[ "$(id -u)" != 0 ] || { echo "请不带 sudo 运行：应用包属于你自己的账户" >&2; exit 2; }
PLACE_ONLY=0
case "$*" in
  "") ;;
  --place-only) PLACE_ONLY=1 ;;
  *) echo "用法：$0 [--place-only]" >&2; exit 2 ;;
esac
DRY_RUN=${DRY_RUN:-0}
BUNDLE_ID="com.claude-abroad.app"
say() { printf '[build-app] %s\n' "$*" >&2; }

DEST=/Applications
BUNDLE="${ROOT}/app/.build/bundle/ClaudeAbroad.app"
if [ -n "${BUILD_APP_TEST_DEST:-}${BUILD_APP_TEST_BUNDLE:-}" ]; then
  [ -n "${BUILD_APP_TEST_DEST:-}" ] && [ -n "${BUILD_APP_TEST_BUNDLE:-}" ] || { say "BUILD_APP_TEST_DEST 和 BUILD_APP_TEST_BUNDLE 要一起给"; exit 2; }
  case "${BUILD_APP_TEST_DEST}" in /Applications | /Applications/) say "自测的目标不能是 /Applications"; exit 2 ;; esac
  DEST=${BUILD_APP_TEST_DEST}
  BUNDLE=${BUILD_APP_TEST_BUNDLE}
elif [ -n "${BUILD_APP_TEST_FOREIGN_OWNER:-}" ]; then
  say "BUILD_APP_TEST_FOREIGN_OWNER 只能和 BUILD_APP_TEST_DEST、BUILD_APP_TEST_BUNDLE 一起用"; exit 2
fi
TARGET="${DEST}/ClaudeAbroad.app"

command -v go >/dev/null 2>&1 || { [ -x /usr/local/go/bin/go ] && PATH="/usr/local/go/bin:${PATH}"; }
run() { if [ "${DRY_RUN}" = 1 ]; then printf 'RUN: %s\n' "$*"; else "$@"; fi; }

if [ -z "${BUILD_APP_TEST_BUNDLE:-}" ] && [ "${PLACE_ONLY}" = 0 ]; then
  say "第 1 步：开发构建和完整性清单"
  run scripts/dev/integrity-build.sh --dev || { say "构建失败，看上面的错误"; exit 1; }
  say "第 2 步：组装 payload"
  run scripts/dev/payload.sh || { say "组装 payload 失败（缺虚拟机镜像时先 scripts/dev/vm-up.sh --no-panic）"; exit 1; }
  say "第 3 步：打包应用（带 payload）"
  run scripts/dev/app-bundle.sh --payload scripts/dev/out/payload || { say "打包失败，看上面的错误"; exit 1; }
fi
[ "${DRY_RUN}" = 1 ] || [ -f "${BUNDLE}/Contents/Info.plist" ] || { say "没有应用包 ${BUNDLE}"; exit 1; }

bundle_id() { /usr/bin/plutil -extract CFBundleIdentifier raw -o - "$1/Contents/Info.plist" 2>/dev/null; }
owned() { [ -O "$1" ] && [ "${BUILD_APP_TEST_FOREIGN_OWNER:-0}" != 1 ]; }

if [ "${PLACE_ONLY}" = 1 ]; then say "把应用放进 ${DEST}"; else say "第 4 步：放进 ${DEST}"; fi
if [ "${DRY_RUN}" = 1 ]; then
  printf 'RUN: cp -R %s %s（已有我们自己的同名应用时先复制到临时名再替换；别人的不碰）\n' "${BUNDLE}" "${TARGET}"
  exit 0
fi
if [ -e "${TARGET}" ] || [ -L "${TARGET}" ]; then
  if [ -L "${TARGET}" ] || [ "$(bundle_id "${TARGET}")" != "${BUNDLE_ID}" ]; then
    say "${TARGET} 不是 Claude 出国的应用（包标识不是 ${BUNDLE_ID}），不碰它。请自己处理后再运行。"
    exit 3
  fi
  if [ ! -w "${TARGET}" ] || [ ! -w "${TARGET}/Contents" ] || [ ! -w "${DEST}" ] || ! owned "${TARGET}"; then
    if [ "${PLACE_ONLY}" = 1 ]; then
      say "${TARGET} 的属主是别的账户（或你没有写权限），你自己改不了，没有替换它。请在访达里把它移到废纸篓（会问管理员密码），再运行 scripts/dev/build-app.sh。"
    else
      say "${TARGET} 是以前的安装脚本以管理员身份放的，你自己改不了：请先运行一次 scripts/dev/reinstall.sh（原地升级时会把它换成新的并交给你），之后再用本脚本。"
    fi
    exit 4
  fi
fi
# 应用还开着时照样替换（运行中的进程用的是已经打开的文件），结尾提示退出并重新打开
running=0
/usr/bin/pgrep -f "^${TARGET}/Contents/MacOS/" >/dev/null 2>&1 && running=1
NEW="${DEST}/.ClaudeAbroad.app.new-$$"
OLD="${DEST}/.ClaudeAbroad.app.old-$$"
rm -rf "${NEW:?}" "${OLD:?}"
cp -R "${BUNDLE}" "${NEW}" || { rm -rf "${NEW:?}"; say "复制应用失败"; exit 1; }
if [ -e "${TARGET}" ]; then
  mv "${TARGET}" "${OLD}" || { rm -rf "${NEW:?}"; say "替换失败：旧的应用没有动"; exit 1; }
fi
if ! mv "${NEW}" "${TARGET}"; then
  [ -e "${OLD}" ] && mv "${OLD}" "${TARGET}"
  rm -rf "${NEW:?}"
  say "替换失败，已放回旧的应用"
  exit 1
fi
rm -rf "${OLD:?}"
if [ "${PLACE_ONLY}" = 1 ]; then
  say "应用已放进 ${TARGET}。"
else
  say "完成：${TARGET}。打开它，按提示安装或升级后台服务（会弹一次管理员密码框）。"
fi
[ "${running}" = 0 ] || say "应用还开着：已经换成新的，请退出并重新打开应用。"
exit 0
