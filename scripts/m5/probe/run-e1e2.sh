#!/usr/bin/env bash
# M5-1 探查 E1、E2（m5-plan v2 第 5 节）：本人授权的核验方式、未签名程序申请自定义权限项、规则不共享、授权框显示什么。
# 由用户在这台 Mac 上以自己的账号执行（脚本里需要 root 的几步用 sudo，会要你输入一次管理员密码）；开发会话只做语法检查和空跑。
# 不改网络，不碰授权数据库里别的规则：只登记、最后删除我们自己的那一个权限项 com.claude-abroad.sensitive。
#
#   scripts/m5/probe/run-e1e2.sh            # 正式执行（会弹两次系统授权框，见 README）
#   DRY_RUN=1 scripts/m5/probe/run-e1e2.sh  # 空跑：只打印每一步会做什么
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "${ROOT}"
DRY_RUN=${DRY_RUN:-0}
[ "$(id -u)" != 0 ] || { echo "请用你自己的账号运行（不要 sudo）：授权框要出现在你的桌面上" >&2; exit 2; }
RIGHT=com.claude-abroad.sensitive
OUT="${ROOT}/scripts/m5/probe/out"
TS=$(date +%Y%m%d-%H%M%S)
RES="${OUT}/e1e2-${TS}.txt"
W=$(mktemp -d "${TMPDIR:-/tmp}/m5e1e2.XXXXXX")
chmod 700 "$W"
mkdir -p "$OUT"
PROBE="${ROOT}/scripts/m5/probe/swift/.build/debug/AuthzProbe"
GOV="$W/goverify"
REGISTERED=0
REQ_PID=""

say() { printf '%s\n' "$*" | sed "s/$(id -un)/<用户>/g" | tee -a "$RES"; }
run() { # run <说明> <命令…>：空跑时只打印
  if [ "$DRY_RUN" = 1 ]; then printf 'STEP: %s：%s\n' "$1" "${*:2}"; return 0; fi
  local out rc
  out=$("${@:2}" 2>&1); rc=$?
  say "## $1（退出码 ${rc}）"; say "$out"
  return $rc
}
cleanup() {
  [ -n "${REQ_PID}" ] && kill "${REQ_PID}" 2>/dev/null
  if [ "${REGISTERED}" = 1 ] && [ "$DRY_RUN" != 1 ]; then
    sudo "${PROBE}" remove >/dev/null 2>&1 && echo "（已删除权限项 ${RIGHT}）" || echo "（删除权限项失败：请手动执行 sudo ${PROBE} remove）"
  fi
  rm -rf "$W"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

: >"$RES"
say "# M5-1 探查 E1、E2，$(date '+%F %T')，macOS $(sw_vers -productVersion)"

# ---- 0. 编出两个小程序（普通用户）----
if [ "$DRY_RUN" = 1 ]; then
  echo "STEP: 编译：swift build（scripts/m5/probe/swift 的 AuthzProbe）、CGO_ENABLED=1 go build（scripts/m5/probe/goverify）"
else
  (cd scripts/m5/probe/swift && swift build --product AuthzProbe >/dev/null 2>&1) || { echo "编译 AuthzProbe 失败" >&2; exit 1; }
  CGO_ENABLED=1 go build -o "$GOV" ./scripts/m5/probe/goverify || { echo "编译 goverify 失败" >&2; exit 1; }
  codesign -s - -f "$PROBE" "$GOV" >/dev/null 2>&1 || true # ad-hoc 签名（和以后的开发版一样，没有开发者账号）
fi

# ---- 1. 登记之前：这个权限项应该不存在 ----
run "登记之前读权限项（应读不到）" "${PROBE}" show

# ---- 2. 登记（root）----
echo "下一步要用 sudo 登记权限项，终端里可能会要你输入管理员密码。"
if run "登记权限项（规则：本人认证、不共享、不缓存）" sudo "${PROBE}" register; then REGISTERED=1; fi
[ "$DRY_RUN" = 1 ] && REGISTERED=0
run "登记之后读权限项（E2：shared 应为 0/false）" "${PROBE}" show

# ---- 3. 第一次申请（普通用户，弹授权框）----
if [ "$DRY_RUN" = 1 ]; then
  echo "STEP: 第一次申请：AuthzProbe request（弹系统授权框，输入你的登录密码或用指纹），外部形式写进 0600 的临时文件，授权会话保持"
else
  echo
  echo ">>> 马上会弹出系统的授权框。请先看清楚框里写的申请者名字和提示文字，然后用你的登录密码（或指纹）授权。"
  mkfifo "$W/hold"
  "${PROBE}" request "$W/ext1" <"$W/hold" >"$W/req1.log" 2>&1 &
  REQ_PID=$!
  exec 7>"$W/hold" # 保持写端打开，授权会话就一直在
  for _ in $(seq 1 120); do [ -s "$W/ext1" ] && break; kill -0 "$REQ_PID" 2>/dev/null || break; sleep 1; done
  say "## 第一次申请：$( [ -s "$W/ext1" ] && echo 授权成功 || echo "没有拿到授权：$(cat "$W/req1.log")")"
  echo
  read -r -p "授权框上显示的“申请者”名字是什么？（照原样输入，看不清就写“看不清”）：" WHO
  read -r -p "授权框上的提示文字是不是“Claude 出国要执行一项敏感操作（M5-1 探查用…）”？(y/n/其他说明)：" TXT
  say "## E2 授权框：申请者显示为【${WHO}】；提示文字是我们登记的那句：【${TXT}】"
fi

# ---- 4. 核验（root，模拟 abroadd）----
EXT=$( [ "$DRY_RUN" = 1 ] && echo '<外部形式>' || cat "$W/ext1" 2>/dev/null)
run "E1：以 root 用 Swift 核验（应通过）" sudo "${PROBE}" verify "${EXT}"
run "E1：以 root 用 Go（cgo，只链接系统框架）核验（应通过）" sudo "${GOV}" "${EXT}"
run "E1：以 root 核验一个没登记的权限项（应不通过）" sudo "${PROBE}" verify "${EXT}" com.claude-abroad.not-registered
run "E1：以普通用户核验（记录结果：abroadd 以 root 运行，这一项只是对照）" "${PROBE}" verify "${EXT}"

# ---- 5. 第二个进程用自己的授权会话申请同一个权限项（E2：不共享时应再次弹框）----
if [ "$DRY_RUN" = 1 ]; then
  echo "STEP: 第二次申请：另一个 AuthzProbe 进程、自己的授权会话申请同一个权限项（规则不共享时应再次弹框）"
else
  echo
  echo ">>> 接下来另一个程序会用它自己的授权会话申请同一个权限项。"
  echo ">>> 如果又弹出了授权框，说明“不共享”生效：这次请点“取消”即可。如果没有弹框、直接成功了，也照实回答。"
  "${PROBE}" request "$W/ext2" </dev/null >"$W/req2.log" 2>&1
  r2=$?
  read -r -p "刚才是否又弹出了授权框？(y/n)：" AGAIN
  say "## E2 不共享：第二个进程申请时又弹框：【${AGAIN}】；它的结果：$( [ -s "$W/ext2" ] && echo "拿到了授权" || echo "没拿到授权（退出码 ${r2}）")"
fi

# ---- 6. 结束第一次的授权会话后再核验（记录：会话结束后外部形式是否失效）----
if [ "$DRY_RUN" = 1 ]; then echo "STEP: 结束第一次的授权会话，再以 root 核验同一个外部形式（记录是否失效）"; else
  echo >&7; exec 7>&-
  wait "$REQ_PID" 2>/dev/null; REQ_PID=""
  run "会话结束后以 root 再核验同一个外部形式（记录）" sudo "${PROBE}" verify "${EXT}"
fi

# ---- 7. 删除权限项（root）----
if run "删除权限项" sudo "${PROBE}" remove; then REGISTERED=0; fi
run "删除之后读权限项（应读不到）" "${PROBE}" show
say ""
say "完成。请把这个文件的全部内容贴回来：${RES}"
echo "结果：${RES}"
