#!/usr/bin/env bash
# 本机签名证书（设计 11.1 v0.73）：没有付费开发者账号时，用本机生成的自签名代码签名证书给应用和命令行签名，
# abroadd 只认这张证书的指纹（构建时写进去），别的程序就不能直接启用、停用。
#
# 证书不放进登录钥匙串、不需要设成信任、不需要 sudo：私钥和证书存成 PKCS#12 文件（只有你自己能读），
# 每次签名时临时建一个钥匙串导入、签完就删。证书只在这台电脑上有意义；别人用你构建的包时同样只认这张证书的指纹，
# 不需要对方信任它（从网上下载的包第一次打开仍会被系统拦一次，见设计 11.1）。
#
#   scripts/dev/local-sign.sh ensure                 # 没有就生成（一次性），打印指纹
#   scripts/dev/local-sign.sh leaf                   # 打印指纹（没有证书时退出码 3）
#   scripts/dev/local-sign.sh sign <路径> [--identifier <标识>] [--entitlements <plist>]
#
# 证书所在目录：$ABROAD_SIGN_DIR，默认 ~/Library/Application Support/claude-abroad-dev/signing（不进版本库）。
# 不带 sudo 运行（证书属于你自己的账户）。
set -euo pipefail

say() { printf '[local-sign] %s\n' "$*" >&2; }
[ "$(id -u)" != 0 ] || { say "请不带 sudo 运行：签名证书属于你自己的账户"; exit 2; }

DIR="${ABROAD_SIGN_DIR:-${HOME}/Library/Application Support/claude-abroad-dev/signing}"
P12="${DIR}/identity.p12"
PASSF="${DIR}/p12.pass"
LEAF="${DIR}/leaf.sha1"
NAME="Claude Abroad Local Signing"

have() { [ -s "${P12}" ] && [ -s "${PASSF}" ] && [ -s "${LEAF}" ]; }

ensure() {
  if have; then
    cat "${LEAF}"
    return 0
  fi
  umask 077
  mkdir -p "${DIR}"
  chmod 700 "${DIR}"
  local t
  t="$(mktemp -d)"
  trap 'rm -rf "${t}"' RETURN
  cat >"${t}/req.cnf" <<EOF
[req]
distinguished_name=dn
prompt=no
[dn]
CN=${NAME}
[ext]
basicConstraints=critical,CA:false
keyUsage=critical,digitalSignature
extendedKeyUsage=critical,codeSigning
subjectKeyIdentifier=hash
EOF
  # 有效期 20 年：过期后签名照样能校验指纹，但新的签名做不出来，那时重新生成并重装即可。
  openssl req -x509 -newkey rsa:3072 -nodes -keyout "${t}/key.pem" -out "${t}/cert.pem" -days 7300 \
    -config "${t}/req.cnf" -extensions ext >/dev/null 2>&1 || { say "生成证书失败（openssl req）"; exit 1; }
  openssl rand -hex 24 >"${PASSF}"
  # -legacy 不一定有：macOS 自带的 LibreSSL 不认识它，Homebrew 的 OpenSSL 3 不加它 security 导入会失败。
  local legacy=()
  openssl pkcs12 -help 2>&1 | grep -q -- '-legacy' && legacy=(-legacy)
  openssl pkcs12 -export ${legacy[@]+"${legacy[@]}"} -inkey "${t}/key.pem" -in "${t}/cert.pem" -name "${NAME}" \
    -out "${P12}" -passout "file:${PASSF}" >/dev/null 2>&1 || { say "导出证书失败（openssl pkcs12）"; exit 1; }
  openssl x509 -in "${t}/cert.pem" -outform der | shasum -a 1 | cut -d' ' -f1 >"${LEAF}"
  chmod 600 "${P12}" "${PASSF}" "${LEAF}"
  say "已生成本机签名证书（${DIR}）"
  cat "${LEAF}"
}

leaf() {
  have || { say "还没有本机签名证书：先运行 scripts/dev/local-sign.sh ensure"; exit 3; }
  cat "${LEAF}"
}

sign() {
  local target="" ident="" ent=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --identifier) ident="${2:?--identifier 要跟标识}"; shift 2 ;;
      --entitlements) ent="${2:?--entitlements 要跟 plist}"; shift 2 ;;
      -*) say "不认识的参数：$1"; exit 2 ;;
      *) [ -z "${target}" ] || { say "只能给一个路径"; exit 2; }; target="$1"; shift ;;
    esac
  done
  [ -n "${target}" ] && [ -e "${target}" ] || { say "要签名的文件不存在：${target}"; exit 2; }
  have || ensure >/dev/null
  local sha pass kc t
  sha="$(cat "${LEAF}")"
  pass="$(cat "${PASSF}")"
  t="$(mktemp -d)"
  kc="${t}/sign.keychain-db"
  # 临时钥匙串：不加进搜索列表、不设默认，签完连目录一起删（失败也删）。
  trap 'security delete-keychain "${kc}" >/dev/null 2>&1 || true; rm -rf "${t}"' RETURN
  security create-keychain -p "${pass}" "${kc}" >/dev/null
  security set-keychain-settings "${kc}" # 不自动锁
  security unlock-keychain -p "${pass}" "${kc}"
  security import "${P12}" -k "${kc}" -P "${pass}" -T /usr/bin/codesign >/dev/null ||
    { say "导入签名证书失败（证书文件坏了？删掉 ${DIR} 后重新运行 ensure，再重装）"; exit 1; }
  security set-key-partition-list -S apple-tool:,apple: -s -k "${pass}" "${kc}" >/dev/null 2>&1 || true
  local args=(--force --keychain "${kc}" --sign "${sha}" --timestamp=none)
  [ -n "${ident}" ] && args+=(--identifier "${ident}")
  [ -n "${ent}" ] && args+=(--entitlements "${ent}")
  codesign "${args[@]}" "${target}" 2>"${t}/err" || { say "签名失败：$(head -1 "${t}/err")"; exit 1; }
  codesign --verify --strict -R="certificate leaf = H\"${sha}\"" "${target}" ||
    { say "签名之后校验不通过：${target}"; exit 1; }
}

case "${1:-}" in
  ensure) ensure ;;
  leaf) leaf ;;
  sign) shift; sign "$@" ;;
  *) say "用法：$0 ensure | leaf | sign <路径> [--identifier <标识>] [--entitlements <plist>]"; exit 2 ;;
esac
