package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"claude-abroad/internal/apiclient"
)

var reIPv4CLI = regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}\b`)
var reIPv6CLI = regexp.MustCompile(`(?i)\b[0-9a-f]{1,4}(:[0-9a-f]{0,4}){2,7}\b`)

// 借用虚拟机修复（v0.70）：main apply、main check 期间的进度写到标准错误；--json 时标准输出只有 JSON；
// 进度和最后一行结果里没有 IP（只有国家和时区）。
func TestCLIMainApplyProgress(t *testing.T) {
	r := newRig(t, rigOpt{main: true})
	for _, args := range [][]string{{"--yes", "main", "apply"}, {"main", "check"}} {
		x := mustOK(t, r.run(args...), strings.Join(args, " "))
		if !strings.Contains(x.err, "正在检查出口…") {
			t.Fatalf("%v：标准错误应有进度：%+v", args, x)
		}
		if !strings.Contains(x.out, "US（America/Los_Angeles）") {
			t.Fatalf("%v：结果行：%q", args, x.out)
		}
		if reIPv4CLI.MatchString(x.all()) || reIPv6CLI.MatchString(x.all()) {
			t.Fatalf("%v：输出里有地址：%+v", args, x)
		}
		js := r.run(append([]string{"--json"}, args...)...)
		mustJSON(t, js, strings.Join(args, " "))
		if strings.Contains(js.out, "正在") {
			t.Fatalf("--json 时进度不应进标准输出：%q", js.out)
		}
	}
}

// 借用虚拟机时的几个明确原因：打印原文、退出码 ExitFail。
func TestCLIBorrowErrorCodes(t *testing.T) {
	for _, c := range []string{"vm_unavailable", "main_push_failed", "main_unreachable", "no_main_selected", "state_incomplete"} {
		var errb bytes.Buffer
		a := &app{stderr: &errb}
		body := []byte(`{"code":"` + c + `","error":"启动虚拟机失败：拉起虚拟机失败。请在“引擎与虚拟机”页看最近的错误。"}`)
		if got := a.codeOf(apiclient.Response{Code: 503, Body: body}); got != ExitFail {
			t.Fatalf("%s：退出码 %d", c, got)
		}
		if !strings.Contains(errb.String(), "拉起虚拟机失败") {
			t.Fatalf("%s：应打印原文：%q", c, errb.String())
		}
	}
}
