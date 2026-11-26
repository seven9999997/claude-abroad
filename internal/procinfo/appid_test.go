package procinfo

import (
	"testing"

	"claude-abroad/internal/rules"
)

// 补齐四验收 f、g：四种种类；共用进程的清单集中在一处、每个都有说明、都是 system: 形式。
func TestAppKindAndShared(t *testing.T) {
	for id, want := range map[rules.AppID]string{
		"com.example.a": KindApp, "system:apsd": KindSystem, "path:/usr/local/bin/x": KindPath, rules.Unknown: KindUnknown, "": KindUnknown,
	} {
		if got := AppKind(id); got != want {
			t.Errorf("%q：%s，应为 %s", id, got, want)
		}
	}
	if len(SharedSystem) == 0 {
		t.Fatal("清单不能为空")
	}
	for id, note := range SharedSystem {
		if AppKind(rules.AppID(id)) != KindSystem || note == "" {
			t.Errorf("%s：应是系统进程且有说明", id)
		}
	}
	// 清单里的进程按路径识别出来就是这个 ID（这台 Mac 上看到的路径）
	for p, id := range map[string]string{
		"/usr/libexec/nsurlsessiond":                                                "system:nsurlsessiond",
		"/System/Library/PrivateFrameworks/ApplePushService.framework/apsd":         "system:apsd",
		"/System/Library/PrivateFrameworks/CloudKitDaemon.framework/Support/cloudd": "system:cloudd",
		"/usr/sbin/mDNSResponder":                                                   "system:mDNSResponder",
		"/usr/libexec/trustd":                                                       "system:trustd",
		"/usr/libexec/timed":                                                        "system:timed",
	} {
		if got := AppIDForPath(p, nil); string(got) != id || SharedSystem[id] == "" {
			t.Errorf("%s → %s（应为 %s，且在清单里）", p, got, id)
		}
	}
}
