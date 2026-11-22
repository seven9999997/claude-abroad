# M4-9 的地区快照、还原、读回核对（m4-plan v2.13 决定 1）。用 source 引入（在 lib.sh 之后）。
# 不依赖 abroadd、它的状态文件或待还原文件：只按脚本自己在启用之前拍的快照还原。写法与 internal/region/mac.go 一致。
#
# 快照文件（key=值，每行一项；值为 none 表示原来没有这个键）：
#   tz=America/Los_Angeles  auto=0|1|none  user=<登录用户>  uid=<UID>
#   user_langs=zh-Hans-CN,en-US|none  user_locale=zh_CN|none  sys_langs=…|none
#
# 读写命令可以替换（自测用假命令，不碰本机）：M4_CMD_SYSTEMSETUP、M4_CMD_DEFAULTS、M4_CMD_LAUNCHCTL。

RG_SYSTEMSETUP="${M4_CMD_SYSTEMSETUP:-/usr/sbin/systemsetup}"
RG_DEFAULTS="${M4_CMD_DEFAULTS:-/usr/bin/defaults}"
RG_LAUNCHCTL="${M4_CMD_LAUNCHCTL:-/bin/launchctl}"
RG_AUTO_DOMAIN=/Library/Preferences/com.apple.timezone.auto
RG_GLOBAL=/Library/Preferences/.GlobalPreferences

# rg_parse_array：把 defaults read 的数组输出（"(\n  \"zh-Hans-CN\",\n  \"en-US\"\n)"）变成逗号分隔。
rg_parse_array() {
  tr -d '()"\n' | tr ',' '\n' | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' | grep . | paste -sd, -
}

# rg_read_* 输出当前值；键不存在时输出 none；读失败时输出 ERR。
rg_read_tz() {
  local out
  out=$("${RG_SYSTEMSETUP}" -gettimezone 2>/dev/null) || { echo ERR; return; }
  out=${out#*: }
  [ -n "$out" ] && echo "$out" || echo ERR
}
rg_read_key() { # rg_read_key <域> <键> [array]
  local out
  if out=$("${RG_DEFAULTS}" read "$1" "$2" 2>/dev/null); then
    if [ "${3:-}" = array ]; then printf '%s\n' "$out" | rg_parse_array; else printf '%s\n' "$out" | head -1; fi
  else
    echo none
  fi
}
rg_user_read() { # rg_user_read <用户> <UID> <键> [array]
  local out
  if out=$("${RG_LAUNCHCTL}" asuser "$2" /usr/bin/sudo -u "$1" "${RG_DEFAULTS}" read -g "$3" 2>/dev/null); then
    if [ "${4:-}" = array ]; then printf '%s\n' "$out" | rg_parse_array; else printf '%s\n' "$out" | head -1; fi
  else
    echo none
  fi
}

# rg_current <用户> <UID>：按快照格式输出当前值。
rg_current() {
  printf 'tz=%s\n' "$(rg_read_tz)"
  printf 'auto=%s\n' "$(rg_read_key "${RG_AUTO_DOMAIN}" Active)"
  printf 'user=%s\nuid=%s\n' "$1" "$2"
  printf 'user_langs=%s\n' "$(rg_user_read "$1" "$2" AppleLanguages array)"
  printf 'user_locale=%s\n' "$(rg_user_read "$1" "$2" AppleLocale)"
  printf 'sys_langs=%s\n' "$(rg_read_key "${RG_GLOBAL}" AppleLanguages array)"
}

rg_get() { sed -n "s/^$2=//p" "$1" | head -1; } # rg_get <快照文件> <键>

# rg_snapshot <文件> <用户> <UID>：拍快照（空跑时只打印）。读失败（ERR）的项让快照无效：没有原值就不能保证还原。
rg_snapshot() {
  if [ "$DRY_RUN" = 1 ]; then printf 'STATE: snap/region（时区、自动时区、用户和系统的语言、地区格式）\n'; return 0; fi
  rg_current "$2" "$3" >"$1"
  if grep -q '=ERR$' "$1"; then
    m4_log "地区快照里有读不出来的项：$(grep '=ERR$' "$1" | cut -d= -f1 | tr '\n' ' ')"
    return 1
  fi
  return 0
}

# rg_diff <快照文件> <当前值文件>：列出不一致的项（每行一个键名），一致时什么都不输出。
rg_diff() {
  local k
  for k in tz auto user_langs user_locale sys_langs; do
    [ "$(rg_get "$1" "$k")" = "$(rg_get "$2" "$k")" ] || echo "$k"
  done
}

# rg_restore <快照文件>：按快照逐项还原不同的项，再读回核对。返回 0 全部一致；1 有不一致（列在标准输出）；
# 3 快照缺失（什么都不改）。
rg_restore() {
  local snap=$1 user uid cur k v
  if [ "$DRY_RUN" = 1 ]; then
    printf 'CHANGE: %s -settimezone <快照里的时区>\n' "${RG_SYSTEMSETUP}"
    printf 'CHANGE: %s write %s Active -bool <快照里的值>（原来没有时 delete）\n' "${RG_DEFAULTS}" "${RG_AUTO_DOMAIN}"
    printf 'CHANGE: %s asuser <UID> /usr/bin/sudo -u <用户> %s write -g AppleLanguages -array <快照里的语言>（原来没有时 delete）\n' "${RG_LAUNCHCTL}" "${RG_DEFAULTS}"
    printf 'CHANGE: %s asuser <UID> /usr/bin/sudo -u <用户> %s write -g AppleLocale -string <快照里的地区格式>（原来没有时 delete）\n' "${RG_LAUNCHCTL}" "${RG_DEFAULTS}"
    printf 'CHANGE: %s write %s AppleLanguages -array <快照里的语言>（原来没有时 delete）\n' "${RG_DEFAULTS}" "${RG_GLOBAL}"
    return 0
  fi
  [ -f "$snap" ] || { m4_log "没有地区快照（${snap}）：run.sh 没走到拍快照这一步，地区不还原"; return 3; }
  user=$(rg_get "$snap" user); uid=$(rg_get "$snap" uid)
  cur=$(mktemp "${TMPDIR:-/tmp}/m4-region.XXXXXX")
  rg_current "$user" "$uid" >"$cur"
  for k in $(rg_diff "$snap" "$cur"); do
    v=$(rg_get "$snap" "$k")
    case "$k" in
      tz) m4_do "${RG_SYSTEMSETUP}" -settimezone "$v" >/dev/null ;;
      auto)
        case "$v" in
          none) m4_do "${RG_DEFAULTS}" delete "${RG_AUTO_DOMAIN}" Active ;;
          1) m4_do "${RG_DEFAULTS}" write "${RG_AUTO_DOMAIN}" Active -bool true ;;
          *) m4_do "${RG_DEFAULTS}" write "${RG_AUTO_DOMAIN}" Active -bool false ;;
        esac ;;
      user_langs)
        if [ "$v" = none ]; then m4_do "${RG_LAUNCHCTL}" asuser "$uid" /usr/bin/sudo -u "$user" "${RG_DEFAULTS}" delete -g AppleLanguages
        else
          # shellcheck disable=SC2046
          m4_do "${RG_LAUNCHCTL}" asuser "$uid" /usr/bin/sudo -u "$user" "${RG_DEFAULTS}" write -g AppleLanguages -array $(printf '%s' "$v" | tr ',' ' ')
        fi ;;
      user_locale)
        if [ "$v" = none ]; then m4_do "${RG_LAUNCHCTL}" asuser "$uid" /usr/bin/sudo -u "$user" "${RG_DEFAULTS}" delete -g AppleLocale
        else m4_do "${RG_LAUNCHCTL}" asuser "$uid" /usr/bin/sudo -u "$user" "${RG_DEFAULTS}" write -g AppleLocale -string "$v"; fi ;;
      sys_langs)
        if [ "$v" = none ]; then m4_do "${RG_DEFAULTS}" delete "${RG_GLOBAL}" AppleLanguages
        else
          # shellcheck disable=SC2046
          m4_do "${RG_DEFAULTS}" write "${RG_GLOBAL}" AppleLanguages -array $(printf '%s' "$v" | tr ',' ' ')
        fi ;;
    esac
  done
  rg_current "$user" "$uid" >"$cur"
  local bad
  bad=$(rg_diff "$snap" "$cur")
  rm -f "$cur"
  if [ -n "$bad" ]; then
    printf '%s\n' "$bad"
    return 1
  fi
  return 0
}
