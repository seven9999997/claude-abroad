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

go build -tags abroaddev -o "${TMP}/abroadd" ./cmd/abroadd # 开发构建才有假系统模式（M6-1）
"${TMP}/abroadd" --fake-system --fake-main-proxy --state-dir "${ST}" 2>"${TMP}/abroadd.log" &
PID=$!
SOCK="${ST}/api.sock"
for _ in $(seq 1 100); do [[ -S "${SOCK}" ]] && break; sleep 0.1; done
[[ -S "${SOCK}" ]] || { echo "abroadd 没有起来：" >&2; cat "${TMP}/abroadd.log" >&2; exit 1; }
echo "abroadd 已启动（pid ${PID}，--fake-system --fake-main-proxy）"

cd app
# swift-testing 的宏插件偶发“plugin for module 'TestingMacros' not found”（2026-11-27 排查）：swift-build 默认用显式
# 模块构建，依赖扫描给编译任务算出要加载的宏插件（-load-resolved-plugin）；偶发时测试目标的所有编译任务都缺了
# TestingMacros 这一项（它只在工具链的 host/plugins/testing 子目录里，靠 -plugin-path 找），整个测试目标就编不过。
# 把这个插件经 -load-plugin-library 显式交给编译器后，扫描结果里每次都有它（改为进程内加载）。调低并行数不管用（-j 2 更糟）。
# build 和 test 用同一组参数，免得两步之间来回整体重编。插件不在这个位置（换了工具链布局）时不加，照旧靠下面的重跑兜底。
SWIFT_FLAGS=() # 空数组展开写成 ${SWIFT_FLAGS[@]+...}：macOS 自带的 bash 3.2 在 set -u 下展开空数组会报错
TESTING_MACROS="$(dirname "$(xcrun --find swift)")/../lib/swift/host/plugins/testing/libTestingMacros.dylib"
if [[ -f "${TESTING_MACROS}" ]]; then
  SWIFT_FLAGS=(-Xswiftc -load-plugin-library -Xswiftc "${TESTING_MACROS}")
else
  echo "（没找到 swift-testing 的宏插件 ${TESTING_MACROS}，不显式加载）"
fi
swift build ${SWIFT_FLAGS[@]+"${SWIFT_FLAGS[@]}"}
# 兜底：仍出现上面那种错误时只对这一种错误重跑，最多 5 次，并在最后打印一共重跑了几次（看它是不是在变严重）；
# 其他失败照常失败。
MAX=5
retries=0
for try in $(seq 1 ${MAX}); do
  set +e
  ABROAD_SOCKET="${SOCK}" swift test ${SWIFT_FLAGS[@]+"${SWIFT_FLAGS[@]}"} >"${TMP}/swift-test.txt" 2>&1
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
