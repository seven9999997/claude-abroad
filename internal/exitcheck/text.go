package exitcheck

// 面向用户的文字（docs/vm-borrow-plan.md v2 第 6 节）：原因码和检查项名是给程序看的稳定代码（JSON 里照旧），
// 给人看的文字一律经这里翻译，不把原值拼进去（“channel_down”这类内部说法不出现在面向用户的输出里）。

var causeText = map[Cause]string{
	CauseOK:              "通过",
	CauseNotNeeded:       "这条规则不需要",
	CauseMainland:        "出口在中国大陆",
	CauseRuleViolation:   "出口变了，不符合出口变化规则",
	CauseUnexpectedReply: "收到的应答不对（可能被劫持，或证书不对）",
	CauseTimeout:         "超时",
	CauseConnect:         "连不上",
	CauseRateLimited:     "查询服务暂时限流或出错",
	CauseBadFormat:       "查询服务的应答格式不对",
	CauseChannelDown:     "到虚拟机的数据通道打不开（虚拟机没有运行或刚刚退出）",
	CauseNoBaseline:      "没有出口基准",
}

// CauseText 是原因的中文说法；不认识的原因回“原因不明”（不回显原值）。
func CauseText(c Cause) string {
	if s, ok := causeText[c]; ok {
		return s
	}
	return "原因不明"
}

var itemLabel = map[string]string{
	"https":    "HTTPS 访问",
	"ip":       "出口 IP",
	"mainland": "是否在大陆",
	"rule":     "出口变化规则",
	"doh":      "加密 DNS",
	"geo":      "位置",
	"udp":      "UDP",
}

// ItemLabel 是检查项的中文名；不认识的项回“其他检查项”。
func ItemLabel(name string) string {
	if s, ok := itemLabel[name]; ok {
		return s
	}
	return "其他检查项"
}

// ItemNames 是所有检查项（测试用来核对每项都有中文名）。
func ItemNames() []string {
	return []string{"https", "ip", "mainland", "rule", "geo", "udp", "doh"}
}

// Causes 是判定表里的所有原因（测试用来核对每个原因都有中文）。
func Causes() []Cause {
	out := make([]Cause, 0, len(causeResult))
	for c := range causeResult {
		out = append(out, c)
	}
	return out
}

// NetworkCause 报告这个原因是不是网络类的“没测成”（超时、连不上、限流、格式、通道），而不是明确不通过。
func NetworkCause(c Cause) bool {
	switch c {
	case CauseTimeout, CauseConnect, CauseRateLimited, CauseBadFormat, CauseChannelDown:
		return true
	}
	return false
}
