package agent

import (
	"fmt"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"dial 203.0.113.7:443 failed":                       "dial <已脱敏>:443 failed",
		"listen 127.0.0.1:1090 ok":                          "listen 127.0.0.1:1090 ok",
		"bind 0.0.0.0:80":                                   "bind 0.0.0.0:80",
		"to [2001:db8::1]:443":                              "to [<已脱敏>]:443",
		"lo ::1 fine":                                       "lo ::1 fine",
		"at 12:34:56 started":                               "at 12:34:56 started",
		"fetch https://user:pw@sub.example.com/a?token=xyz": "fetch https://<已脱敏>@sub.example.com/<已脱敏>",
		"password=hunter2 next":                             "password=<已脱敏> next",
		`"password": "hunter2"`:                             `"password": <已脱敏>`,
		"  uuid: 1b2c3d4e-0000-1111-2222-333344445555":      "  uuid: <已脱敏>",
		"secret:abc, other":                                 "secret:<已脱敏>, other",
		"get https://sub.example.com/link/AbCdEf123 ok":     "get https://sub.example.com/<已脱敏> ok",
		"server:2001:db8::1 down":                           "server:<已脱敏> down",
		"12:34:56.2001:db8::1 x":                            "12:34:56.<已脱敏> x",
		"via fe80::1%eth0 ok":                               "via <已脱敏>%eth0 ok",
		"version 1.14.4 ready":                              "version 1.14.4 ready",
	} {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q)\n  得到 %q\n  应为 %q", in, got, want)
		}
	}
}

func TestRingLogLinesAndCap(t *testing.T) {
	r := NewRingLog(100)
	fmt.Fprint(r, "a\nb\npartial")
	if lines, _ := r.Tail(0); strings.Join(lines, "|") != "a|b" {
		t.Fatalf("不完整的行应先缓存：%v", lines)
	}
	fmt.Fprint(r, " done\n")
	if lines, _ := r.Tail(1); lines[0] != "partial done" {
		t.Fatalf("拼行：%v", lines)
	}
	for i := 0; i < 50; i++ {
		fmt.Fprintf(r, "line-%02d\n", i)
	}
	lines, dropped := r.Tail(0)
	total := 0
	for _, l := range lines {
		total += len(l) + 1
	}
	if total > 100 || dropped == 0 || lines[len(lines)-1] != "line-49" {
		t.Fatalf("应封顶并计丢弃数：%d 字节，丢 %d，末行 %q", total, dropped, lines[len(lines)-1])
	}
	if got, _ := r.Tail(3); len(got) != 3 || got[2] != "line-49" {
		t.Fatalf("tail 3：%v", got)
	}
}

func TestRingLogLongLine(t *testing.T) {
	r := NewRingLog(RingLogBytes)
	fmt.Fprintln(r, strings.Repeat("x", MaxLogLine*2))
	lines, _ := r.Tail(0)
	if len(lines) != 1 || !strings.HasSuffix(lines[0], "[截断]") || len(lines[0]) > MaxLogLine+20 {
		t.Fatalf("超长行应截断：%d 字节", len(lines[0]))
	}
	// 没有换行的超长输出也不会无限积累。
	r2 := NewRingLog(RingLogBytes)
	fmt.Fprint(r2, strings.Repeat("y", MaxLogLine*3))
	if lines, _ := r2.Tail(0); len(lines) == 0 {
		t.Fatal("无换行的超长输出应被收下")
	}
}
