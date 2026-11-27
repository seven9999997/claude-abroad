#!/usr/bin/env bash
# “是不是开发构建”两处判断的自测（2026-11-27：abroadd -h 的退出码是 2，直接接管道在 pipefail 下判断永远为假，
# 开发安装的前提检查一直误报“不是开发构建”，发版物料的“没有开发参数”检查形同虚设）。
#   1. 用会打印帮助、退出码为 2 的假程序（和 Go 的 flag 包行为相同）测公共函数：
#      ab_dev_flags_missing（开发安装用）：开发构建返回 0；正式构建返回 1 并报缺；程序不存在返回 2；
#      ab_dev_flags_present（发版物料用）：开发构建返回 0 并列出参数；正式构建返回 1；
#   2. 敏感性：旧写法（“-h | grep -q”）在同样的假程序上判断错误——证明上面的测试能抓到这类问题；
#   3. 两个脚本确实调用这两个函数，并且不再出现“abroadd -h … | grep”的写法；
#   4. 真编出两种构建再测一遍（和假程序的结论一致）。
# 不用 root，不改系统。用法：scripts/lib/buildcheck-selftest.sh
set -uo pipefail
cd "$(dirname "$0")/../.."
TMP=$(mktemp -d "${TMPDIR:-/tmp}/buildcheck.XXXXXX")
trap 'rm -rf "${TMP:?}"' EXIT
fails=0
ok() { echo "✓ $*"; }
bad() { echo "✗ $*"; fails=$((fails + 1)); }
. scripts/lib/common.sh

# ---- 1. 假程序 ----
cat >"${TMP}/fake-dev" <<'EOF'
#!/bin/sh
cat >&2 <<'H'
Usage of abroadd:
  -dev-fake-claude-dir string
    	开发构建：只针对这个目录
  -dev-resolver-test-domain
    	开发构建：只锁测试域名
  -launch-control
    	打开启动管控
H
exit 2
EOF
cat >"${TMP}/fake-release" <<'EOF'
#!/bin/sh
cat >&2 <<'H'
Usage of abroadd:
  -launch-control
    	打开启动管控
  -state-dir string
    	状态目录
H
exit 2
EOF
chmod +x "${TMP}/fake-dev" "${TMP}/fake-release"

check_pair() { # check_pair <开发构建> <正式构建> <说明>
  local dev=$1 rel=$2 what=$3 out rc
  out=$(ab_dev_flags_missing "$dev" --dev-fake-claude-dir --dev-resolver-test-domain); rc=$?
  [ $rc = 0 ] && [ -z "$out" ] && ok "${what}：开发构建通过开发安装的检查" || bad "${what}：开发构建被误判（rc=$rc，报缺 $out）"
  out=$(ab_dev_flags_missing "$rel" --dev-fake-claude-dir --dev-resolver-test-domain); rc=$?
  [ $rc = 1 ] && printf '%s' "$out" | grep -q -- '--dev-fake-claude-dir' && ok "${what}：正式构建被开发安装的检查拦下" || bad "${what}：正式构建没被拦下（rc=$rc）"
  out=$(ab_dev_flags_present "$dev"); rc=$?
  [ $rc = 0 ] && printf '%s' "$out" | grep -q -- 'dev-fake-claude-dir' && ok "${what}：发版物料的检查认出开发构建里的开发参数" || bad "${what}：发版物料的检查没认出开发参数（rc=$rc）"
  out=$(ab_dev_flags_present "$rel"); rc=$?
  [ $rc = 1 ] && [ -z "$out" ] && ok "${what}：正式构建通过发版物料的检查" || bad "${what}：正式构建被发版物料的检查误拦（rc=$rc，$out）"
}
check_pair "${TMP}/fake-dev" "${TMP}/fake-release" "假程序（退出码 2）"
ab_dev_flags_missing "${TMP}/nope" --dev-x >/dev/null; [ $? = 2 ] && ok "程序不存在：开发安装的检查返回 2" || bad "程序不存在时返回值不对"
ab_dev_flags_present "${TMP}/nope" >/dev/null; [ $? = 2 ] && ok "程序不存在：发版物料的检查返回 2" || bad "程序不存在时返回值不对"

# ---- 2. 敏感性：旧写法在同样的假程序上判断错 ----
if "${TMP}/fake-dev" -h 2>&1 | grep -q -- '-dev-fake-claude-dir'; then
  bad "旧写法在假程序上居然判断对了：假程序没有模拟出退出码 2，上面的测试抓不到这类问题"
else
  ok "旧写法（-h | grep -q，pipefail）在同样的假程序上判断为假：上面的测试能抓到这类问题"
fi

# ---- 3. 两个脚本用的是这两个函数 ----
grep -q 'ab_dev_flags_missing "${SRC}/bin/abroadd"' scripts/m6/install/install.sh && ok "开发安装的前提检查调用 ab_dev_flags_missing" || bad "开发安装的前提检查没有调用 ab_dev_flags_missing"
grep -q 'ab_dev_flags_present "${B}/bin/abroadd"' scripts/dev/release-materials.sh && ok "发版物料调用 ab_dev_flags_present" || bad "发版物料没有调用 ab_dev_flags_present"
if grep -rnE --include='*.sh' -e '-h 2>&1 *\| *grep' scripts | grep -v '/out/' | grep -v 'buildcheck-selftest.sh' >"${TMP}/pipes"; then
  bad "还有“-h 2>&1 | grep”的写法：$(cat "${TMP}/pipes")"
else
  ok "脚本里没有“-h 2>&1 | grep”的写法了"
fi

# ---- 4. 真的两种构建 ----
if command -v go >/dev/null 2>&1; then
  go build -o "${TMP}/abroadd-release" ./cmd/abroadd 2>"${TMP}/b1" && go build -tags abroaddev -o "${TMP}/abroadd-dev" ./cmd/abroadd 2>"${TMP}/b2" &&
    check_pair "${TMP}/abroadd-dev" "${TMP}/abroadd-release" "真编出的 abroadd" || bad "编不出 abroadd：$(cat "${TMP}/b1" "${TMP}/b2")"
else
  bad "没有 go：真编出的两种构建没测"
fi

echo
[ $fails = 0 ] && echo "自测全部通过" || echo "自测：${fails} 处不通过"
exit $((fails > 0))
