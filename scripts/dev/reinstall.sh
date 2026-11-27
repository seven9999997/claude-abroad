#!/usr/bin/env bash
# 一条命令重装开发版（过渡用，architect 2026-11-27）：构建 → 已装过就卸载 → 前提检查 → 安装。
# 只编排已经审过的脚本，不新增任何改系统的命令；不传 --yes，不跳过卸载脚本自己的交互确认。
# 等“原地升级”（install.sh --upgrade）做好后，已装过时改为升级、不再清空主代理配置。
#
#   scripts/dev/reinstall.sh            # 不要带 sudo；卸载和安装两步会自己问管理员密码
#   DRY_RUN=1 scripts/dev/reinstall.sh  # 空跑：只按顺序打印会调用的脚本（RUN: 行）
#
# 自测用（只在 DRY_RUN=1 时生效）：REINSTALL_TEST_EUID 假装的用户号，REINSTALL_TEST_INSTALLED=0|1 假装装没装过，
# REINSTALL_TEST_FAIL_AT=<1–5> 让第几步失败。
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}" || exit 1
DRY_RUN=${DRY_RUN:-0}
INST="/Library/Application Support/claude-abroad"

say() { printf '[reinstall] %s\n' "$*" >&2; }

euid=$(id -u)
installed=0
[ -e "${INST}" ] && installed=1
if [ "${DRY_RUN}" = 1 ]; then
  euid=${REINSTALL_TEST_EUID:-$euid}
  installed=${REINSTALL_TEST_INSTALLED:-$installed}
else
  for v in REINSTALL_TEST_EUID REINSTALL_TEST_INSTALLED REINSTALL_TEST_FAIL_AT; do
    [ -z "${!v:-}" ] || { say "${v} 只能在空跑（DRY_RUN=1）的自测里用"; exit 2; }
  done
fi
if [ "${euid}" = 0 ]; then
  say "请不带 sudo 运行：构建要以你自己的身份做；需要管理员的两步（卸载、安装）会自己问密码。"
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
    [ "${REINSTALL_TEST_FAIL_AT:-}" = "${n}" ] && rc=1
  else
    "$@"
    rc=$?
  fi
  if [ "${rc}" != 0 ]; then
    say "停在第 ${n} 步（${what}），退出码 ${rc}。${next}"
    say "网络有异常时看 docs/emergency.md。"
    exit "${rc}"
  fi
}

step "构建后台程序（开发构建，带完整性清单）" "看上面的编译错误，修好后重新运行本脚本。" \
  scripts/dev/integrity-build.sh --dev
step "打包应用" "看上面的错误，修好后重新运行本脚本。" \
  scripts/dev/app-bundle.sh
if [ "${installed}" = 1 ]; then
  step "卸载现在装着的开发版（提示时输入 uninstall）" "照卸载脚本的输出处理，可以重新运行本脚本重试。" \
    sudo scripts/m6/install/uninstall.sh
else
  n=$((n + 1))
  say "第 ${n} 步：没装过，跳过卸载"
fi
step "安装前的检查" "按上面列出的原因处理后重新运行本脚本。" \
  scripts/m6/install/install.sh --check-only
step "安装开发版" "安装失败时安装脚本会自动卸载，看它的输出。" \
  sudo scripts/m6/install/install.sh

say "完成。主代理配置已随卸载清空：请重新添加来源、选节点，然后执行 sudo abroad main apply"
