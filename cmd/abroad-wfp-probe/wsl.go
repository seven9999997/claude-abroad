package main

import (
	"strings"
	"unicode/utf16"
)

// WSL 观察项（WR9）：过滤器生效期间从 WSL 里访问一次外网，只记录通还是不通。只调用系统自带的入口
// %SystemRoot%\System32\wsl.exe，不安装、不修改任何东西；没装 WSL、没有发行版时记“未执行”。

// WSLMarkerOK、WSLMarkerFail 是探测命令在 WSL 里打印的标记。
const (
	WSLMarkerOK   = "ABROAD_WSL_OK"
	WSLMarkerFail = "ABROAD_WSL_FAIL"
)

// WSLProbeScript 是在 WSL 发行版里执行的探测：用 bash 的 /dev/tcp 连一次目标，最多等 5 秒。
func WSLProbeScript(host string, port string) string {
	return "if timeout 5 bash -c 'exec 3<>/dev/tcp/" + host + "/" + port + "' 2>/dev/null; then echo " + WSLMarkerOK +
		"; else echo " + WSLMarkerFail + "; fi"
}

// WSLObservation 是 Windows 一侧采到的原始结果（纯数据，便于测试）。
type WSLObservation struct {
	EntryMissing bool   // 系统里没有 wsl.exe
	ListOut      []byte // wsl.exe --list --quiet 的输出（UTF-16LE，可能带 BOM）
	ListFailed   bool   // wsl.exe --list 退出码非 0
	ProbeOut     string // 探测命令的输出
	ProbeErr     string // 探测命令没能执行或超时（空表示正常退出）
	// Baseline 是装过滤器之前同样探测一次的结果（只看 ProbeOut、ProbeErr）；为 nil 表示没有做基线。
	Baseline *WSLObservation
}

// DecodeWSLList 解析 wsl.exe --list --quiet 的输出：UTF-16LE（带或不带 BOM），每行一个发行版名。
// 输出不是 UTF-16 时按普通文本处理。
func DecodeWSLList(b []byte) []string {
	text := string(b)
	if len(b) >= 2 && len(b)%2 == 0 && (b[0] == 0xFF && b[1] == 0xFE || looksUTF16LE(b)) {
		u := make([]uint16, 0, len(b)/2)
		for i := 0; i+1 < len(b); i += 2 {
			u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
		}
		if len(u) > 0 && u[0] == 0xFEFF {
			u = u[1:]
		}
		text = string(utf16.Decode(u))
	}
	var out []string
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(strings.Trim(l, "\r\x00"))
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// looksUTF16LE：奇数位置的字节大多是 0（ASCII 字符的 UTF-16LE 形式）。
func looksUTF16LE(b []byte) bool {
	zero, n := 0, 0
	for i := 1; i < len(b); i += 2 {
		n++
		if b[i] == 0 {
			zero++
		}
	}
	return n > 0 && zero*2 > n
}

// ClassifyWSL 把原始结果分类：
//   - 没有 wsl.exe → 未执行（没装 WSL）；
//   - 列不出发行版 → 未执行（没有发行版；只装了组件、或这个账户下没有注册发行版时 wsl.exe 也以非 0 退出）；
//   - 探测命令没能执行、超时，或输出里没有标记 → 无法判断（调用失败）；
//   - 输出 OK 标记 → 放行（通）；输出 FAIL 标记 → 拦截（不通，分不清是谁挡的，详情里写明）；
//   - 但装过滤器之前的基线就不通（或基线调用失败）时，过滤器期间的结果说明不了任何事 → 无法判断。
func ClassifyWSL(o WSLObservation) (Outcome, string) {
	out, why := classifyWSLOnce(o)
	if o.Baseline == nil || (out != Allowed && out != Blocked) {
		return out, why
	}
	b := *o.Baseline
	b.EntryMissing, b.ListOut, b.ListFailed, b.Baseline = false, o.ListOut, false, nil
	if bo, bwhy := classifyWSLOnce(b); bo != Allowed {
		return Unknown, "装过滤器之前 WSL 里就连不上外网（" + bwhy + "），过滤器期间：" + why
	}
	return out, why + "；装过滤器之前是通的"
}

func classifyWSLOnce(o WSLObservation) (Outcome, string) {
	if o.EntryMissing {
		return NotRun, "没有装 WSL（系统里没有 wsl.exe）"
	}
	distros := DecodeWSLList(o.ListOut)
	if len(distros) == 0 {
		if o.ListFailed {
			return NotRun, "WSL 没有可用的发行版（wsl.exe --list 以非 0 退出）"
		}
		return NotRun, "WSL 没有可用的发行版"
	}
	if o.ProbeErr != "" {
		return Unknown, "调用失败：" + o.ProbeErr
	}
	switch {
	case strings.Contains(o.ProbeOut, WSLMarkerOK):
		return Allowed, "通（WSL 里连上了）"
	case strings.Contains(o.ProbeOut, WSLMarkerFail):
		return Blocked, "不通（WSL 里 5 秒内没连上；分不清是宿主的过滤器还是别的原因挡的）"
	}
	return Unknown, "调用失败：输出里没有探测标记（发行版里可能没有 bash 或 timeout）"
}
