#!/usr/bin/env bash
# R3：查连接表识别进程的准确率（root 部分，m3-plan M3-3 验收 g）。
# 普通用户在连接表里只能看到本进程自己的套接字（macOS 27.2.1 实测，netstat 也一样），
# 所以“识别别的进程（curl）”要 root。脚本只在本机回环上起服务、跑 curl，不改任何网络设置，不碰 pf、路由、DNS。
#
#   sudo scripts/m3/r3-accuracy.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
[ "$(id -u)" = 0 ] || { echo "请用 sudo 运行：sudo $0" >&2; exit 2; }
[ -n "${SUDO_USER:-}" ] || { echo "请用 sudo 从自己的账号运行（需要 SUDO_USER 来编译和归还文件）" >&2; exit 2; }

OUT="${ROOT}/scripts/m3/out"
TS="$(date +%Y%m%d-%H%M%S)"
LOG="${OUT}/r3-curl-${TS}.txt"
BIN_DIR="$(sudo -u "${SUDO_USER}" mktemp -d)"
trap 'rm -rf "${BIN_DIR}"' EXIT
mkdir -p "${OUT}"

echo "编译 abroad-dev（以 ${SUDO_USER} 身份）…"
sudo -u "${SUDO_USER}" go build -o "${BIN_DIR}/abroad-dev" ./cmd/abroad-dev

ulimit -n 8192 2>/dev/null || true
{
  echo "# R3 curl 模式（root），macOS $(sw_vers -productVersion)（$(sw_vers -buildVersion)），$(date '+%F %T')"
  echo "# 只连本机回环；识别目标是 /usr/bin/curl。"
  # 测法和目标（architect 审 M3-3 时定）：认错两档都必须为 0。
  echo "## 总数 1000，分 10 批，每批并发 100（目标：识别率 ≥95%，99 分位 ≤20ms）"
  "${BIN_DIR}/abroad-dev" r3 --mode curl -n 1000 --batch 100 --min-rate 95 --max-p99 20ms || echo "（rc=$?：未达标或出错，见上面的数字）"
  sleep 30 # 等上一轮的临时端口释放
  echo "## 一次并发 300（目标：识别率 ≥90%）"
  "${BIN_DIR}/abroad-dev" r3 --mode curl -n 300 --min-rate 90 || echo "（rc=$?：未达标或出错，见上面的数字）"
  echo "## 只读实跑：识别子进程（nc）"
} >"${LOG}" 2>&1
# 子进程识别的测试（只读 sysctl，只连本机回环）。测试编译缓存放在临时目录，不污染 root 的主目录。
GOCACHE="${BIN_DIR}/gocache" GOFLAGS=-count=1 go test -tags integration -run ReadOnly -v ./internal/procinfo/ >>"${LOG}" 2>&1 || true
chown "${SUDO_USER}" "${LOG}"
echo "完成。请把 ${LOG} 贴回给主会话。"
