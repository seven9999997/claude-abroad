package buildinfo

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestBuildInfoDefaults(t *testing.T) {
	i := Get()
	if i.Commit != "unknown" || i.BuildTime != "unknown" || i.Dirty {
		t.Fatalf("没写入时应是 unknown/false：%+v", i)
	}
	if i.Known() {
		t.Fatal("没写入时不应算“版本已知”")
	}
	for _, c := range []struct {
		i    Info
		want bool
	}{
		{Info{Commit: "5e00dc9", BuildTime: "20261127T101500Z"}, true},
		{Info{Commit: "5e00dc9abcde", BuildTime: "20261127T101500Z", Dirty: true}, true},
		{Info{Commit: "5E00DC9", BuildTime: "20261127T101500Z"}, false},
		{Info{Commit: "5e00dc9", BuildTime: "2026-11-27"}, false},
		{Info{Commit: "", BuildTime: "20261127T101500Z"}, false},
	} {
		if got := c.i.Known(); got != c.want {
			t.Errorf("%+v Known=%v，应为 %v", c.i, got, c.want)
		}
	}
}

func TestVersionJSON(t *testing.T) {
	b, _ := json.Marshal(Version{Info: Info{Commit: "abc1234", BuildTime: "20261127T101500Z", Dirty: true}, StateFormat: StateFormat})
	want := fmt.Sprintf(`{"commit":"abc1234","build_time":"20261127T101500Z","dirty":true,"state_format":%d}`, StateFormat)
	if string(b) != want {
		t.Fatalf("字段名要和 build.json、/v1/status 一致：\n得到 %s\n应为 %s", b, want)
	}
}
