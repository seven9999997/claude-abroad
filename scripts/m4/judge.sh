# M4 真机脚本的判定函数（m4-plan v2.13 验收 g，v2.14 验收 m）。纯函数：只读给定的文件、JSON 和数字，不碰系统。
# scripts/m4/selftest.sh 喂样本测它们。用 source 引入。
#
# 约定：返回 0 通过、1 不通过、2 跳过；把一句说明打到标准输出。说明里不出现任何 IP 地址：
# 比较 IP 的地方调用方只传“eq/ne/none”（相等、不等、没测到）。

# jget <JSON> <路径>：取 JSON 里的标量（路径用点分隔，数组下标写数字，例如 items.0.result）；取不到时输出空串。
jget() {
  printf '%s' "$1" | /usr/bin/perl -MJSON::PP -e '
    my $d = eval { JSON::PP->new->decode(do { local $/; <STDIN> }) };
    for my $k (split /\./, $ARGV[0]) {
      if (ref $d eq "HASH") { $d = $d->{$k} } elsif (ref $d eq "ARRAY" && $k =~ /^\d+$/) { $d = $d->[$k] } else { $d = undef }
    }
    if (!defined $d) { print "" } elsif (JSON::PP::is_bool($d)) { print($d ? "true" : "false") } elsif (!ref $d) { print $d }
  ' "$2" 2>/dev/null
}

# env_item <envcheck 报告 JSON> <项目编号>：输出这一项的 result（pass/fail/unknown），没有时输出空串。
env_item() {
  printf '%s' "$1" | /usr/bin/perl -MJSON::PP -e '
    my $d = eval { JSON::PP->new->decode(do { local $/; <STDIN> }) } || {};
    $d = $d->{envcheck} if ref $d->{envcheck} eq "HASH";
    for my $it (@{ $d->{items} || [] }) { if ($it->{id} eq $ARGV[0]) { print $it->{result}; last } }
  ' "$2" 2>/dev/null
}

# env_summary <报告 JSON>：输出“硬性项 通过/不通过/无法判断 的编号”，例如 "fail=03,06 unknown=10"。
env_summary() {
  printf '%s' "$1" | /usr/bin/perl -MJSON::PP -e '
    my $d = eval { JSON::PP->new->decode(do { local $/; <STDIN> }) } || {};
    $d = $d->{envcheck} if ref $d->{envcheck} eq "HASH";
    my (%b, $n);
    for my $it (@{ $d->{items} || [] }) { $n++; next unless $it->{level} eq "hard"; push @{ $b{$it->{result}} }, $it->{id} }
    print "items=", ($n || 0), " fail=", join(",", @{ $b{fail} || [] }), " unknown=", join(",", @{ $b{unknown} || [] });
  ' 2>/dev/null
}

join_cn() {
  local out="" x
  for x in "$@"; do out="${out:+${out}；}${x}"; done
  echo "${out}"
}
delta() {
  local a=$1 b=$2
  case "$a" in '' | *[!0-9]*) a=0 ;; esac
  case "$b" in '' | *[!0-9]*) b=0 ;; esac
  echo $((b - a))
}
# count_packets <抓包文本>：tcpdump 的输出行数（文件不在时 0）。
count_packets() { [ -f "$1" ] && grep -c . "$1" 2>/dev/null || echo 0; }

# judge_region <快照文件> <读回文件> <期望时区> <是否改语言 0|1> <期望语言> <期望地区格式>（T1，R4）：
#   时区等于期望、自动时区关；不改语言时用户的语言列表、地区格式与快照相同；改语言时首选语言、地区格式等于期望。
judge_region() {
  local snap=$1 now=$2 tz=$3 chl=$4 lang=$5 loc=$6 bad=()
  [ -f "$snap" ] && [ -f "$now" ] || { echo "缺快照或读回值"; return 1; }
  [ "$(sed -n 's/^tz=//p' "$now")" = "$tz" ] || bad+=("时区不是目标")
  [ "$(sed -n 's/^auto=//p' "$now")" = 0 ] || bad+=("自动时区没关")
  if [ "$chl" = 1 ]; then
    [ "$(sed -n 's/^user_langs=//p' "$now" | cut -d, -f1)" = "$lang" ] || bad+=("首选语言不是目标")
    [ "$(sed -n 's/^user_locale=//p' "$now")" = "$loc" ] || bad+=("地区格式不是目标")
  else
    [ "$(sed -n 's/^user_langs=//p' "$now")" = "$(sed -n 's/^user_langs=//p' "$snap")" ] || bad+=("没打开改语言的开关，语言列表却变了")
    [ "$(sed -n 's/^user_locale=//p' "$now")" = "$(sed -n 's/^user_locale=//p' "$snap")" ] || bad+=("没打开改语言的开关，地区格式却变了")
  fi
  if [ ${#bad[@]} -eq 0 ]; then echo "时区、自动时区、语言与地区格式都符合预期"; return 0; fi
  join_cn "${bad[@]}"; return 1
}

# judge_envcheck <报告 JSON>（T2）：17 项都有结果；硬性项没有明确不通过（无法判断的列出来，不算不通过）。
judge_envcheck() {
  local s n f u
  s=$(env_summary "$1")
  n=$(printf '%s' "$s" | sed -n 's/^items=\([0-9]*\).*/\1/p'); f=$(printf '%s' "$s" | sed -n 's/.* fail=\([^ ]*\).*/\1/p')
  u=$(printf '%s' "$s" | sed -n 's/.* unknown=\(.*\)$/\1/p')
  [ "${n:-0}" -ge 17 ] || { echo "只有 ${n:-0} 项结果（应有 17 项）"; return 1; }
  [ -z "$f" ] || { echo "硬性项明确不通过：${f}${u:+；无法判断：${u}}"; return 1; }
  echo "17 项都有结果，硬性项没有不通过${u:+（无法判断：${u}）}"
  return 0
}

# judge_leak <名字> <项目编号> <制造后的报告> <制造后闸门 true|false> <撤销后的报告> <撤销后闸门>（T3）：
#   制造后这一项不通过、闸门关；撤销后这一项通过、闸门开。
judge_leak() {
  local name=$1 id=$2 r1 r2
  r1=$(env_item "$3" "$id"); r2=$(env_item "$5" "$id")
  if [ "$r1" = fail ] && [ "$4" = false ] && [ "$r2" = pass ] && [ "$6" = true ]; then
    echo "${name}：第 ${id} 项检出、闸门关闭；撤销后通过、闸门重新打开"; return 0
  fi
  echo "${name}：制造后第 ${id} 项 ${r1:-无结果}、闸门 $([ "$4" = true ] && echo 开 || echo 关)；撤销后第 ${id} 项 ${r2:-无结果}、闸门 $([ "$6" = true ] && echo 开 || echo 关)"
  return 1
}

# judge_exit_route <出口=主代理 eq|ne|none> <出口=本机 eq|ne|none> <主代理=本机>（T4）：经分流出口测得的出口 IP
#   与本机真实 IP 不同；与主代理的出口不同（相同时如实记录，可能是同一家同一个出口，不判不通过——v2.14 建议改 5）。
judge_exit_route() {
  [ "$1" = none ] || [ "$2" = none ] && { echo "没测到出口 IP（经分流出口的请求失败）"; return 1; }
  [ "$2" = eq ] && { echo "经分流出口测得的出口 IP 等于本机真实 IP（泄漏）"; return 1; }
  [ "$3" = eq ] && { echo "主代理的出口 IP 等于本机真实 IP（主代理没生效）"; return 1; }
  if [ "$1" = eq ]; then echo "出口 IP 与本机不同；与主代理的出口相同（可能是同一家同一个出口，如实记录）"; return 0; fi
  echo "出口 IP 与本机、与主代理的出口都不同"; return 0
}

# judge_exit_down <请求成功 1|0> <=主代理 eq|ne|none> <=本机 eq|ne|none> <=原分流出口 eq|ne|none> <物理网卡上发往目标的包数>
#   （T4 断开后，v2.14 必须改 1）：三条同时成立——请求失败，或出口 IP 等于主代理的；绝不等于本机、也不等于原分流出口；
#   物理网卡上抓不到发往目标的包。
judge_exit_down() {
  local ok=$1 m=$2 r=$3 o=$4 pk=$5 bad=()
  [ "$r" = eq ] && bad+=("出口 IP 等于本机真实 IP")
  [ "$ok" = 1 ] && [ "$o" = eq ] && bad+=("出口 IP 仍是原分流出口的（配置没生效）")
  [ "$ok" = 1 ] && [ "$m" != eq ] && [ "$r" != eq ] && bad+=("请求成功了，但出口 IP 不是主代理的")
  case "$pk" in '' | *[!0-9]*) pk=0 ;; esac
  [ "$pk" -gt 0 ] && bad+=("物理网卡上抓到 ${pk} 个发往目标的包")
  if [ ${#bad[@]} -gt 0 ]; then join_cn "${bad[@]}"; return 1; fi
  if [ "$ok" = 1 ]; then echo "出口不可用时回退主代理（出口 IP 等于主代理的），物理网卡上没有发往目标的包"; else
    echo "出口不可用时请求失败，物理网卡上没有发往目标的包"; fi
  return 0
}

# judge_udp_domain <dig 得到应答 1|0> <物理网卡上发往 53 的包数>（T4b）。
judge_udp_domain() {
  local pk=$2
  case "$pk" in '' | *[!0-9]*) pk=0 ;; esac
  [ "$pk" -gt 0 ] && { echo "物理网卡上抓到 ${pk} 个发往 53 的包（DNS 没经出口）"; return 1; }
  [ "$1" = 1 ] && { echo "UDP 的域名目标经订阅出口得到应答，物理网卡上没有发往 53 的包"; return 0; }
  echo "没得到应答"; return 1
}

# judge_realip <测到 1|0> <状态目录、日志、status 里出现的次数>（T6）：真实 IP 一次都不能出现；没测到时跳过。
judge_realip() {
  [ "$1" = 1 ] || { echo "没测到本机真实公网 IP，跳过这项搜索"; return 2; }
  case "$2" in '' | *[!0-9]*) echo "搜索出错"; return 1 ;; esac
  [ "$2" = 0 ] && { echo "状态目录、日志、status 里都没有本机真实 IP"; return 0; }
  echo "本机真实 IP 出现了 $2 次"; return 1
}

# judge_team <假定的 Team ID> <读到的 Team ID>（T8，R17）：没装桌面版时跳过。
judge_team() {
  [ -n "$2" ] || { echo "没装 Claude 桌面版（或读不到签名），跳过"; return 2; }
  [ "$1" = "$2" ] && { echo "桌面版的签名 Team ID 与假定值相同"; return 0; }
  echo "桌面版的签名 Team ID 与假定值不同"; return 1
}

# judge_fake_claude <识别结果 JSON（claude-facts --path 的输出）>（T3 第二种的前提）：假 Claude 进程按路径特征算 Claude Code。
judge_fake_claude() {
  local m r
  m=$(jget "$1" 0.match); r=$(jget "$1" 0.reason)
  [ "$m" = true ] && [ "$r" = "安装位置" ] && { echo "假 Claude 进程按安装位置特征被认作 Claude Code"; return 0; }
  echo "假 Claude 进程没有被认作 Claude Code（match=${m:-?}，依据 ${r:-无}）"; return 1
}

# judge_preview <409 的应答 JSON>（T9）：有确认码；预览里的命令行没有主目录的绝对路径（已换成 ~）。
judge_preview() {
  local c n
  c=$(jget "$1" confirm)
  [ ${#c} = 32 ] || { echo "没有拿到确认码"; return 1; }
  n=$(printf '%s' "$1" | grep -c '/Users/' || true)
  [ "$n" = 0 ] || { echo "预览里有主目录的绝对路径"; return 1; }
  echo "预览：要结束 $(printf '%s' "$1" | /usr/bin/perl -MJSON::PP -e 'my $d=eval{JSON::PP->new->decode(do{local $/;<STDIN>})}||{}; print scalar @{$d->{preview}{processes}||[]}') 个进程"
  return 0
}

# judge_proxy_restored <快照文件（service=、Enabled=、Server=、Port=）> <networksetup -getsecurewebproxy 的读回>
#   （T3 撤销系统代理后，v2.14 验收后建议改 1）：开关、服务器、端口三项都与快照一致。原来服务器为空时，读回为空或端口 0 都算一致。
judge_proxy_restored() {
  local snap=$1 now=$2 k a b bad=()
  for k in Enabled Server Port; do
    a=$(sed -n "s/^${k}=//p" "$snap" 2>/dev/null); b=$(printf '%s\n' "$now" | sed -n "s/^${k}: //p")
    if [ "$k" = Port ]; then [ "${a:-0}" = "${b:-0}" ] && continue; fi
    [ "$a" = "$b" ] || bad+=("${k} 与启用前不同")
  done
  if [ ${#bad[@]} -eq 0 ]; then echo "开关、服务器、端口与启用前一致"; return 0; fi
  join_cn "${bad[@]}"; return 1
}
