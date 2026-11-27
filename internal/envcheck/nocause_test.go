package envcheck

import (
	"context"
	"strings"
	"testing"

	"claude-abroad/internal/exitcheck"
)

// 借用虚拟机修复（v0.70，验收 e）：02 出口 IP 的说明里没有原因码，原因用中文。
func TestExitItemsNoRawCause(t *testing.T) {
	for _, c := range exitcheck.Causes() {
		for _, res := range []exitcheck.Result{exitcheck.Fail, exitcheck.Unknown} {
			r := rep(nil, "203.0.113.7", "JP", "Example ISP")
			for i := range r.Items {
				if r.Items[i].Name == "rule" {
					r.Items[i] = exitcheck.Item{Name: "rule", Result: res, Cause: c}
				}
			}
			it := ExitIP(probeOf(r, &jp))
			o := it.Run(context.Background())
			if c != exitcheck.CauseOK && strings.Contains(o.Detail+o.Fix, string(c)) {
				t.Errorf("原因 %s（%s）：说明里有原因码：%q", c, res, o.Detail)
			}
		}
	}
}
