# M3 真机脚本的判定函数（m3-plan v2.10 验收 g、v2.11 验收 m）。纯函数：只读给定的文件和数字，不碰系统。
# scripts/m3/selftest.sh 喂样本测它们。用 source 引入。
#
# 约定：判定函数返回 0 表示通过、1 表示不通过、2 表示跳过；把一句说明（不含地址）打到标准输出。

# jget <JSON> <路径>：取 JSON 里的数字（路径用点分隔，例如 stack.reasons.pinned）；取不到时输出 0。
# 用 perl 的 JSON::PP（系统自带，不依赖 python）。
jget() {
  printf '%s' "$1" | /usr/bin/perl -MJSON::PP -e '
    my $d = eval { JSON::PP->new->decode(do { local $/; <STDIN> }) };
    for my $k (split /\./, $ARGV[0]) { $d = (ref $d eq "HASH") ? $d->{$k} : undef; }
    print((defined $d && !ref $d) ? $d : 0);
  ' "$2" 2>/dev/null || printf 0
}

# join_cn <项…>：用中文分号连接（bash 3.2 的 IFS 不能可靠地用多字节字符）。
join_cn() {
  local out="" x
  for x in "$@"; do out="${out:+${out}；}${x}"; done
  echo "${out}"
}

# delta <前> <后>：差值（取不到数字时按 0）。
delta() {
  local a=$1 b=$2
  case "$a" in '' | *[!0-9]*) a=0 ;; esac
  case "$b" in '' | *[!0-9]*) b=0 ;; esac
  echo $((b - a))
}

# judge_marker <出口的上游日志> <其他出口的上游日志> <标记>：本次的标记出现在这个出口的日志里（作为 CONNECT 的 host
# 的开头），而不在其他出口的日志里。只看带标记的那一条，不看日志总行数（Chrome 新配置启动时有一批后台连接）。
judge_marker() {
  local mine=$1 other=$2 mark=$3 inm=0 ino=0
  grep -qF "\"host\":\"${mark}" "$mine" 2>/dev/null && inm=1
  grep -qF "\"host\":\"${mark}" "$other" 2>/dev/null && ino=1
  if [ $inm = 1 ] && [ $ino = 0 ]; then echo "本次标记只出现在这个出口的上游日志里"; return 0; fi
  if [ $inm = 0 ] && [ $ino = 1 ]; then echo "本次标记出现在另一个出口的上游日志里（走错了出口）"; return 1; fi
  if [ $inm = 1 ]; then echo "本次标记两个出口的日志里都有"; return 1; fi
  echo "两个出口的上游日志里都没有本次标记（没经开发出口，可能走了主代理或直连）"
  return 1
}

# count_packets <tcpdump 文本输出>：数包行（tcpdump -n -l 每个包一行，以时间戳开头）。
count_packets() {
  [ -f "$1" ] || { echo 0; return; }
  grep -cE '^[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]+ ' "$1" || true
}

# count_dns_queries <tcpdump 53 端口的文本输出>：数对 claude.ai、anthropic.com（含子域、大小写不敏感）的查询行。
count_dns_queries() {
  [ -f "$1" ] || { echo 0; return; }
  grep -E '^[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]+ ' "$1" | grep -ciE '[ ?](([a-z0-9-]+\.)*)(claude\.ai|anthropic\.com)\.?([ ,]|$)' || true
}

# judge_delta <差值> <下限> <名字>：差值不小于下限。
judge_delta() {
  if [ "$1" -ge "$2" ]; then echo "$3 增加了 $1"; return 0; fi
  echo "$3 增加了 $1（应至少 $2）"
  return 1
}

# judge_pinned_case <pinned 差> <主代理流数差> <发往 160.79.104.0/21 的包数> <对 claude/anthropic 的 DNS 查询数>：
# Chrome 设成本机网络后访问 claude.ai（m3-plan v2.10 决定 4），四条同时成立才通过。
judge_pinned_case() {
  local pin=$1 main=$2 pk=$3 q=$4 why=()
  [ "$pin" -gt 0 ] || why+=("置顶判定计数没有增长")
  [ "$main" -gt 0 ] || why+=("主代理一侧没有新连接")
  [ "$pk" -eq 0 ] || why+=("物理网卡上抓到 ${pk} 个发往 Anthropic 地址段的包")
  [ "$q" -eq 0 ] || why+=("物理网卡上有 ${q} 条对 claude.ai/anthropic.com 的 DNS 查询")
  if [ ${#why[@]} -eq 0 ]; then
    echo "置顶判定 +${pin}、主代理新连接 +${main}、发往 Anthropic 段的包 0、相关 DNS 查询 0"
    return 0
  fi
  join_cn "${why[@]}"
  return 1
}

# judge_direct_domain <直连成功数差>：S3，直连的应用按域名访问，经直连拨号模块连上。
judge_direct_domain() {
  if [ "$1" -gt 0 ]; then echo "直连成功计数 +$1"; return 0; fi
  echo "直连成功计数没有增长（看 direct_fail 的应答码）"
  return 1
}

# judge_rawip443 <直连成功数差> <port443_timeout 差>：nc 连裸 IP 的 443 不发数据，等不到首包，改走主代理。
judge_rawip443() {
  local d=$1 t=$2
  if [ "$d" -eq 0 ] && [ "$t" -gt 0 ]; then echo "443 首包超时改走主代理（port443_timeout +${t}），直连计数不变"; return 0; fi
  echo "直连计数 +${d}、port443_timeout +${t}（预期直连不变、port443_timeout 增长）"
  return 1
}

# judge_offline <普通域名的直连差> <claude.ai 的直连差> <发往 Anthropic 段的包数>：S8，停用后 offline_allow 的应用。
judge_offline() {
  local plain=$1 cl=$2 pk=$3 why=()
  [ "$plain" -gt 0 ] || why+=("访问普通域名没有经直连连上")
  [ "$cl" -eq 0 ] || why+=("访问 claude.ai 竟然经直连发出了 ${cl} 条")
  [ "$pk" -eq 0 ] || why+=("物理网卡上抓到 ${pk} 个发往 Anthropic 地址段的包")
  if [ ${#why[@]} -eq 0 ]; then echo "普通域名经直连 +${plain}；claude.ai 不通、没有发往 Anthropic 段的包"; return 0; fi
  join_cn "${why[@]}"
  return 1
}

# judge_batch <direct-probe 批量模式的 JSON>：S6，在途 fd。全部成功才通过；握手全部失败（拨号都成功）时
# 跳过（目标不可达，换 M3_DIRECT_TARGET）；其余不通过。
judge_batch() {
  local j=$1 n ok df hf
  n=$(jget "$j" count); ok=$(jget "$j" ok); df=$(jget "$j" dial_fail); hf=$(jget "$j" handshake_fail)
  if [ "$n" -gt 0 ] && [ "$ok" -eq "$n" ]; then echo "${n} 次全部成功（拨号、收 fd、TLS 握手）"; return 0; fi
  if [ "$n" -gt 0 ] && [ "$ok" -eq 0 ] && [ "$df" -eq 0 ] && [ "$hf" -eq "$n" ]; then
    echo "拨号都成功但 TLS 握手全部失败：目标可能不可达或证书名不对，请换 M3_DIRECT_TARGET、M3_DIRECT_TLS_NAME 重跑"
    return 2
  fi
  echo "${n} 次里成功 ${ok}、拨号失败 ${df}、握手失败 ${hf}"
  return 1
}

# judge_selfheal <模块一侧 stale_config 的差> <直连成功数差>：探针带自己的配置之后，abroadd 发起的直连收到
# stale_config、自动补发，直连照常成功（m3-plan v2.9 的自愈路径）。
judge_selfheal() {
  if [ "$1" -gt 0 ] && [ "$2" -gt 0 ]; then echo "模块回了 stale_config ${1} 次，abroadd 补发后直连成功 +${2}"; return 0; fi
  echo "模块一侧 stale_config +${1}、直连成功 +${2}（预期都增长）"
  return 1
}
