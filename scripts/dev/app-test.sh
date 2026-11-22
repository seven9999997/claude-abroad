#!/usr/bin/env bash
# M5-7 验收 f：Mac 应用的测试。编出 abroadd，以 --fake-system 启动（整套系统是内存里的假实现，不碰本机），
# 把 socket 位置经 ABROAD_SOCKET 交给 swift test（集成测试据此启用；没有时跳过），跑完停掉 abroadd、删掉临时目录。
# 只按自己记下的 PID 结束 abroadd；以当前用户运行；不启动应用、不创建窗口。
#
#	scripts/dev/app-test.sh            # swift build + swift test（含集成测试）
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
umask 077
[[ "$(id -u)" != "0" ]] || { echo "不要以 root 运行" >&2; exit 1; }
[[ $# -eq 0 ]] || { echo "用法：$0" >&2; exit 2; }

TMP="$(mktemp -d /tmp/abroad-app.XXXXXX)" # 短路径：unix socket 的路径有长度上限
ST="${TMP}/st"
PID=""
cleanup() {
  if [[ -n "${PID}" ]] && kill -0 "${PID}" 2>/dev/null && [[ "$(ps -p "${PID}" -o comm= 2>/dev/null)" == *abroadd ]]; then
    kill -TERM "${PID}" 2>/dev/null || true
    wait "${PID}" 2>/dev/null || true
  fi
  if [[ -n "${PID}" ]] && kill -0 "${PID}" 2>/dev/null; then
    echo "✗ abroadd（pid ${PID}）没有停下" >&2
  fi
  [[ -n "${TMP}" && "${TMP}" == /tmp/abroad-app.* ]] && rm -rf -- "${TMP}"
  [[ ! -e "${TMP}" ]] && echo "abroadd 已停，临时目录已删"
}
trap cleanup EXIT

go build -o "${TMP}/abroadd" ./cmd/abroadd
"${TMP}/abroadd" --fake-system --fake-main-proxy --state-dir "${ST}" 2>"${TMP}/abroadd.log" &
PID=$!
SOCK="${ST}/api.sock"
for _ in $(seq 1 100); do [[ -S "${SOCK}" ]] && break; sleep 0.1; done
[[ -S "${SOCK}" ]] || { echo "abroadd 没有起来：" >&2; cat "${TMP}/abroadd.log" >&2; exit 1; }
echo "abroadd 已启动（pid ${PID}，--fake-system --fake-main-proxy）"

cd app
swift build
# Command Line Tools 里的 swift-build 偶尔编测试时找不到 swift-testing 的宏插件（“plugin for module 'TestingMacros'
# not found”，同样的源码重跑就过）。只对这一种错误重跑，最多 5 次，并在最后打印一共重跑了几次（看它是不是在变严重）；
# 其他失败照常失败。
MAX=5
retries=0
for try in $(seq 1 ${MAX}); do
  set +e
  ABROAD_SOCKET="${SOCK}" swift test >"${TMP}/swift-test.txt" 2>&1
  rc=$?
  set -e
  if [[ ${rc} -ne 0 ]] && grep -q "plugin for module 'TestingMacros' not found" "${TMP}/swift-test.txt" && [[ ${try} -lt ${MAX} ]]; then
    retries=$((retries + 1))
    echo "（第 ${try} 次：编测试时没找到 swift-testing 的宏插件，重跑）"
    continue
  fi
  cat "${TMP}/swift-test.txt"
  echo "测试宏插件的偶发错误：重跑了 ${retries} 次（上限 $((MAX - 1)) 次）"
  exit ${rc}
done
