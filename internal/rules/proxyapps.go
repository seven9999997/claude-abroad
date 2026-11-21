package rules

import (
	"path"
	"strings"
)

// 已知的代理软件（design 10.4）：不能设成“本机网络”，也不能进 offline_allow。
// 它们把别的应用（包括 Claude）的流量收进来再发出去，直连就等于把这些流量从本机 IP 送出。
// 清单可以追加；漏掉的代理软件由置顶域名和 Anthropic 地址段兜底（只防得住它转发 Claude 时的明文去向）。

var proxyBundleIDs = []string{
	"io.github.clash-verge-rev.clash-verge-rev", // Clash Verge Rev
	"io.github.clash-verge.clash-verge",         // Clash Verge（旧版）
	"com.west2online.clashx",                    // ClashX
	"com.west2online.clashxpro",                 // ClashX Pro
	"com.nssurge.surge-mac",                     // Surge
	"com.follow.clash",                          // FlClash
	"io.nekohasekai.sfavt",                      // sing-box（SFM）
	"io.nekohasekai.sfm",                        // sing-box（SFM，旧标识）
	"net.yanue.v2rayu",                          // V2rayU
	"party.mihomo.app",                          // Mihomo Party
	"ws.stash.app.mac",                          // Stash
	"com.crossutility.quantumult-x",             // Quantumult X
	"com.liguangming.shadowrocket",              // Shadowrocket
	"com.v2rayx.v2rayx",                         // V2RayX
	"com.qiuyuzhou.shadowsocksx-ng",             // ShadowsocksX-NG
}

var proxyBinaries = []string{
	"clash", "clash-meta", "mihomo", "verge-mihomo", "sing-box", "v2ray", "xray",
	"hysteria", "tuic-client", "ss-local", "sslocal", "trojan", "trojan-go", "naive",
}

// IsProxyApp 判断应用 ID 是否属于已知的代理软件（Bundle ID 不区分大小写）。
func IsProxyApp(id AppID) bool {
	s := string(id)
	if p, ok := strings.CutPrefix(s, "path:"); ok {
		b := strings.ToLower(path.Base(p))
		for _, n := range proxyBinaries {
			if b == n {
				return true
			}
		}
		return false
	}
	ls := strings.ToLower(s)
	for _, b := range proxyBundleIDs {
		if ls == b {
			return true
		}
	}
	return false
}
