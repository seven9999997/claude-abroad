#!/usr/bin/env bash
# 一条命令构建并安装或升级开发版（architect 2026-11-27；v0.71 起已装过时原地升级）：
#   构建（开发构建 + 完整性清单）→ 组装 payload → 打包应用 →
#   已装过：原地升级（sudo install.sh --upgrade，保留主代理和设置）；
#   安装目录不在、只剩安装前的快照：卸载（只重做对照、删快照）→ 前提检查 → 安装；
#   没装过：前提检查 → 安装。
# 只编排已经审过的脚本，不新增任何改系统的命令；不传 --yes，不跳过各脚本自己的交互确认。
#
#   scripts/dev/reinstall.sh            # 不要带 sudo；需要管理员的步骤（升级，或卸载和安装）会自己问密码
#   DRY_RUN=1 scripts/dev/reinstall.sh  # 空跑：只按顺序打印会调用的脚本（RUN: 行）
#
# 自测用（只在 DRY_RUN=1 时生效）：REINSTALL_TEST_EUID 假装的用户号，REINSTALL_TEST_INSTALLED=0|1 假装安装目录在不在，
# REINSTALL_TEST_SNAP_ONLY=1 假装安装目录不在、只剩快照，
# REINSTALL_TEST_FAIL_AT=<1–6> 让第几步失败（REINSTALL_TEST_RC 指定失败时的退出码，默认 1）。
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}" || exit 1
DRY_RUN=${DRY_RUN:-0}
INST="/Library/Application Support/claude-abroad"
SNAP="/Library/Application Support/claude-abroad-install-snap"

say() { printf '[reinstall] %s\n' "$*" >&2; }

euid=$(id -u)
# 装过：安装目录在 → 原地升级；安装目录不在、只剩安装前的快照 → 先卸载（只重做对照、删快照；architect 2026-11-27）再安装
installed=0 snap_only=0
[ -e "${INST}" ] && installed=1
[ "${installed}" = 0 ] && [ -e "${SNAP}" ] && snap_only=1
if [ "${DRY_RUN}" = 1 ]; then
  euid=${REINSTALL_TEST_EUID:-$euid}
  installed=${REINSTALL_TEST_INSTALLED:-$installed}
  snap_only=0
  [ "${REINSTALL_TEST_SNAP_ONLY:-0}" = 1 ] && installed=0 && snap_only=1
else
  for v in REINSTALL_TEST_EUID REINSTALL_TEST_INSTALLED REINSTALL_TEST_FAIL_AT REINSTALL_TEST_SNAP_ONLY REINSTALL_TEST_RC; do
    [ -z "${!v:-}" ] || { say "${v} 只能在空跑（DRY_RUN=1）的自测里用"; exit 2; }
  done
fi
if [ "${euid}" = 0 ]; then
  say "请不带 sudo 运行：构建要以你自己的身份做；需要管理员的步骤（升级，或卸载和安装）会自己问密码。"
  exit 2
fi

# Go 在 /usr/local/go/bin 而 PATH 里没有时补上（只影响本脚本的构建）
command -v go >/dev/null 2>&1 || { [ -x /usr/local/go/bin/go ] && PATH="/usr/local/go/bin:${PATH}"; }

n=0
step() { # step <说明> <失败时的下一步> <命令…>
  local what=$1 next=$2 rc
  shift 2
  n=$((n + 1))
  say "第 ${n} 步：${what}"
  if [ "${DRY_RUN}" = 1 ]; then
    printf 'RUN: %s\n' "$*"
    rc=0
    [ "${REINSTALL_TEST_FAIL_AT:-}" = "${n}" ] && rc=${REINSTALL_TEST_RC:-1}
  else
    "$@"
    rc=$?
  fi
  if [ "${rc}" != 0 ]; then
    say "停在第 ${n} 步（${what}），退出码 ${rc}。${next}"
    [ -n "${UPGRADE_STEP:-}" ] && upgrade_hint "${rc}"
    say "网络有异常时看 docs/emergency.md。"
    exit "${rc}"
  fi
}

# 升级被拒或失败时的下一步（install.sh --upgrade 的退出码）
upgrade_hint() {
  case "$1" in
    10) say "状态不允许升级：照上面的原因处理（已启用时先 sudo abroad down 停用，升级后再启用），然后重新运行本脚本。" ;;
    11) say "后台服务不比这次构建旧：不降级。确实要装旧版本时先卸载（sudo scripts/m6/install/uninstall.sh）再运行本脚本。" ;;
    13) say "状态格式变了，不能原地升级：先卸载（sudo scripts/m6/install/uninstall.sh，主代理和设置会清空）再运行本脚本。" ;;
    14) say "后台服务的版本不明：在终端里运行本脚本，按提示确认；或执行 sudo scripts/m6/install/install.sh --upgrade --accept-unknown-version。" ;;
    20) say "升级失败，已经退回旧版本（主代理和设置都在）：看上面的原因，修好后重新运行本脚本。" ;;
    21) say "升级失败，退回旧版本也没有完成：先按 docs/emergency.md 处理；再运行本脚本会先接着退回。" ;;
  esac
}

step "构建后台程序（开发构建，带完整性清单和版本号）" "看上面的编译错误，修好后重新运行本脚本。" \
  scripts/dev/integrity-build.sh --dev
step "组装 payload（程序、镜像、清单、安装脚本）" "看上面的错误（缺虚拟机镜像时先 scripts/dev/vm-up.sh --no-panic），修好后重新运行本脚本。" \
  scripts/dev/payload.sh
step "打包应用（带 payload：之后可以在应用里升级后台服务）" "看上面的错误，修好后重新运行本脚本。" \
  scripts/dev/app-bundle.sh --payload scripts/dev/out/payload
if [ "${installed}" = 1 ]; then
  UPGRADE_STEP=1
  step "原地升级现在装着的开发版（主代理和设置保留）" "" \
    sudo scripts/m6/install/install.sh --upgrade
  UPGRADE_STEP=
  say "完成。后台服务已升级，主代理和设置都保留，不用重新配置。"
  exit 0
fi
if [ "${snap_only}" = 1 ]; then
  step "上一次卸载留下了安装前的快照：先重做卸载的对照（提示时照做）" "照卸载脚本的输出处理，可以重新运行本脚本重试。" \
    sudo scripts/m6/install/uninstall.sh
else
  n=$((n + 1))
  say "第 ${n} 步：没装过，跳过卸载"
fi
step "安装前的检查" "按上面列出的原因处理后重新运行本脚本。" \
  scripts/m6/install/install.sh --check-only
step "安装开发版" "安装失败时安装脚本会自动卸载，看它的输出。" \
  sudo scripts/m6/install/install.sh

say "完成。请添加主代理来源、选节点，然后执行 sudo abroad main apply"
