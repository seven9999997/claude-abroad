//go:build windows

package wfp

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"claude-abroad/internal/wfprules"
)

// 结构体大小与 64 位 Windows 的 C 定义一致（fwpmtypes.h、fwptypes.h）。
func TestStructSizes(t *testing.T) {
	for name, c := range map[string][2]uintptr{
		"FWPM_FILTER0":           {unsafe.Sizeof(fwpmFilter0{}), 200},
		"FWPM_FILTER_CONDITION0": {unsafe.Sizeof(fwpmFilterCondition0{}), 40},
		"FWP_VALUE0":             {unsafe.Sizeof(fwpValue0{}), 16},
		"FWP_BYTE_BLOB":          {unsafe.Sizeof(fwpByteBlob{}), 16},
		"FWPM_SESSION0":          {unsafe.Sizeof(fwpmSession0{}), 72},
		"FWPM_PROVIDER0":         {unsafe.Sizeof(fwpmProvider0{}), 64},
		"FWPM_SUBLAYER0":         {unsafe.Sizeof(fwpmSublayer0{}), 72},
		"FWPM_ACTION0":           {unsafe.Sizeof(fwpmAction0{}), 20},
	} {
		if c[0] != c[1] {
			t.Errorf("%s 大小 %d，应为 %d", name, c[0], c[1])
		}
	}
	if o := unsafe.Offsetof(fwpmFilter0{}.ProviderContextKey); o != 152 {
		t.Errorf("providerContextKey 偏移 %d，应为 152", o)
	}
	if o := unsafe.Offsetof(fwpmFilter0{}.FilterID); o != 176 {
		t.Errorf("filterId 偏移 %d，应为 176", o)
	}
}

func TestGUIDConversion(t *testing.T) {
	g := wfprules.ProviderKey
	w := toWin(g)
	if fromWin(w) != g {
		t.Fatal("往返不一致")
	}
	if w.String() != "{"+g.String()+"}" {
		t.Fatalf("%s ≠ %s", w.String(), g.String())
	}
}

// smokeFilter 只拦“回环上发往 1 号端口的 TCP 连接”：没有服务用这个端口，不影响任何实际流量。
func smokeFilter(t *testing.T) FilterSpec {
	t.Helper()
	f := wfprules.Filter{Key: wfprules.SublayerKey, Name: "smoke/loopback-port-1", Layer: wfprules.LayerConnectV4,
		Sublayer: wfprules.SublayerKey, Provider: wfprules.ProviderKey, Weight: 1, Action: wfprules.Block,
		Conds: []wfprules.Cond{{Field: wfprules.FieldLoopback, Value: "1"}, {Field: wfprules.FieldProtocol, Value: "tcp"}, {Field: wfprules.FieldRemotePort, Value: "1"}}}
	f.Key, _ = wfprules.ParseGUID("9D1E9A5C-3C1B-4E0A-8F57-5A1F00000001")
	s, err := Encode(f)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func isAdmin() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// k. 冒烟测试（design-windows 0.5 第 9 条）：只碰我们自己的提供者，以及回环上一条不影响任何实际流量的过滤器。
// 动态会话里添加 → 另一个会话能枚举到 → 摘要与期望一致 → 关闭动态会话后自动消失；
// 再在动态会话里添加一次 → DeleteOurs 整批删除 → 枚举为空。无论成败都清理。
func TestSmoke(t *testing.T) {
	if !isAdmin() {
		t.Skip("需要管理员权限（托管构建环境和用户的管理员 PowerShell 有）")
	}
	watcher, err := Open(false)
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	before, err := watcher.Enumerate()
	if err != nil {
		t.Fatal(err)
	}
	if !NothingOurs(before) {
		t.Fatalf("开始前已经有我们的对象，先用还原工具清理：%v", CleanPlan(before))
	}
	t.Logf("开始前别家在出站连接层的过滤器：\n%s", FormatOthers(SummarizeOthers(before.Others, before.Providers, before.Sublayers)))
	defer func() { // 兜底：无论成败都删掉我们的对象
		if n, err := watcher.DeleteOurs(); err != nil || n > 0 {
			t.Logf("兜底清理：删除 %d 条（%v）", n, err)
		}
	}()

	spec := smokeFilter(t)
	dyn, err := Open(true)
	if err != nil {
		t.Fatal(err)
	}
	if err := dyn.Install([]FilterSpec{spec}, true); err == nil {
		t.Fatal("动态会话不应能添加持久对象")
	}
	if err := dyn.Install([]FilterSpec{spec}, false); err != nil {
		dyn.Close()
		t.Fatal(err)
	}
	mid, err := watcher.Enumerate()
	if err != nil {
		dyn.Close()
		t.Fatal(err)
	}
	if len(mid.Ours) != 1 || !mid.OurProvider || !mid.OurSublayer {
		dyn.Close()
		t.Fatalf("枚举不到刚加的对象：%d 条，提供者 %v，子层 %v", len(mid.Ours), mid.OurProvider, mid.OurSublayer)
	}
	got, err := Decode(mid.Ours[0])
	want, _ := Decode(spec)
	if err != nil || string(wfprules.Summary([]wfprules.Filter{got})) != string(wfprules.Summary([]wfprules.Filter{want})) {
		dyn.Close()
		t.Fatalf("枚举到的过滤器与期望不一致（%v）：\n%s\n%s", err, wfprules.Summary([]wfprules.Filter{got}), wfprules.Summary([]wfprules.Filter{want}))
	}
	if err := dyn.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := watcher.Enumerate()
	if err != nil {
		t.Fatal(err)
	}
	if !NothingOurs(after) {
		t.Fatalf("动态会话关闭后仍有我们的对象：%v", CleanPlan(after))
	}

	// 整批删除：再加一次，由别的会话删。
	dyn2, err := Open(true)
	if err != nil {
		t.Fatal(err)
	}
	defer dyn2.Close()
	if err := dyn2.Install([]FilterSpec{spec}, false); err != nil {
		t.Fatal(err)
	}
	snap, _ := watcher.Enumerate()
	if plan := CleanPlan(snap); len(plan) != 3 {
		t.Fatalf("还原工具的计划应是 1 条过滤器、子层、提供者：%v", plan)
	}
	n, err := watcher.DeleteOurs()
	if err != nil || n != 1 {
		t.Fatalf("整批删除：%d 条，%v", n, err)
	}
	final, _ := watcher.Enumerate()
	if !NothingOurs(final) {
		t.Fatalf("整批删除后仍有我们的对象：%v", CleanPlan(final))
	}
	// 可重复执行：再删一次不报错、删 0 条。
	if n, err := watcher.DeleteOurs(); err != nil || n != 0 {
		t.Fatalf("重复删除：%d 条，%v", n, err)
	}
}
