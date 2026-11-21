package procinfo

import (
	"testing"

	"claude-abroad/internal/srccheck"
)

// 不准直接拨号（m3-plan M3-4 验收 i）：直连只能经直连拨号模块，代理只能经数据通道。
func TestNoDial(t *testing.T) {
	srccheck.NoDial(t, ".", nil)
}
