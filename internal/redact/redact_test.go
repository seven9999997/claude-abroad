package redact

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestWriterRedactsLines(t *testing.T) {
	var out bytes.Buffer
	w := &Writer{W: &out}
	// gvisor-tap-vsock 的 udp_proxy 会这样打印拨号错误。
	fmt.Fprint(w, `time="x" level=error msg="dial udp 203.0.113.9:443: i/o timeout"`)
	if out.Len() != 0 {
		t.Fatal("不完整的行应先缓存")
	}
	fmt.Fprint(w, "\nnext 2001:db8::5 line\n")
	got := out.String()
	if strings.Contains(got, "203.0.113.9") || strings.Contains(got, "2001:db8::5") || strings.Count(got, "\n") != 2 {
		t.Fatalf("应按行脱敏：%q", got)
	}
}
