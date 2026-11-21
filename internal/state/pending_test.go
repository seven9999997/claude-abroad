package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"claude-abroad/internal/sysnet"
)

func (b *bed) pending() (RegionPending, bool) {
	p, ok, err := b.store.LoadPending()
	if err != nil {
		b.t.Fatalf("待还原文件：%v", err)
	}
	return p, ok
}

// assertNetClean：除地区外和初始值相等（地区单独核对）。
func (b *bed) assertNetClean() {
	b.t.Helper()
	got := b.net.Snapshot()
	got.Locale = b.init.Locale
	if !got.Equal(b.init) {
		b.t.Fatalf("网络没有还原干净：%+v", got)
	}
	if f := b.state(); f.State != Restored || b.fpf.Active("claude-abroad") {
		b.t.Fatalf("恢复应走到 RESTORED：%s", f.State)
	}
}

var errBoom = errors.New("命令失败")

// a. 恢复时对读、时区、自动时区、用户级、系统级各注入一次失败：都走到 RESTORED、网络干净，
// 待还原文件里正好是失败的项；之后一次重试成功，文件删除，设置等于初始值。
func TestRestoreRegionBestEffort(t *testing.T) {
	cases := []struct {
		name  string
		inj   func(b *bed)
		items []string
	}{
		{"读", func(b *bed) { b.net.FailNext(sysnet.OpLocaleRead, errBoom) }, nil},
		{"时区", func(b *bed) { b.net.FailNext(sysnet.OpLocaleTZ, errBoom) }, []string{ItemTimezone}},
		{"自动时区", func(b *bed) { b.net.FailNext(sysnet.OpLocaleAuto, errBoom) }, []string{ItemAutoTZ}},
		{"用户级", func(b *bed) { b.net.FailNext(sysnet.OpLocaleUser, errBoom) }, []string{ItemUser}},
		{"系统级", func(b *bed) { b.net.FailNext(sysnet.OpLocaleSys, errBoom) }, []string{ItemSystem}},
		{"登录的用户换了", func(b *bed) { b.net.SetConsoleUser("bob") }, []string{ItemUser}},
		{"读和时区", func(b *bed) {
			b.net.FailNext(sysnet.OpLocaleRead, errBoom)
			b.net.FailNext(sysnet.OpLocaleTZ, errBoom)
		}, []string{ItemTimezone}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newBed(t)
			mustOK(t, b.m.Enable(bg))
			c.inj(b)
			mustOK(t, b.m.Restore(bg))
			b.assertNetClean()
			p, ok := b.pending()
			if !slices.Equal(p.Items, c.items) || ok != (len(c.items) > 0) {
				t.Fatalf("待还原的项：%v（应为 %v）", p.Items, c.items)
			}
			if len(c.items) == 0 {
				if !b.locale().Equal(b.init.Locale) {
					t.Fatal("读失败时按全部项写回，应等于初始值")
				}
				return
			}
			if !hasEvent(b, EvRegionPending) || slices.Equal(b.m.PendingItems(), nil) {
				t.Fatal("应有事件和待还原的项")
			}
			// 只有失败的项和所属用户。
			raw, _ := os.ReadFile(filepath.Join(b.dir, PendingFileName))
			for _, it := range []string{ItemTimezone, ItemAutoTZ, ItemUser, ItemSystem} {
				if slices.Contains(c.items, it) != strings.Contains(string(raw), `"`+it+`"`) {
					t.Fatalf("文件里的项不对：%s", raw)
				}
			}
			if fi, _ := os.Stat(filepath.Join(b.dir, PendingFileName)); fi.Mode().Perm() != 0o600 {
				t.Fatalf("权限：%v", fi.Mode())
			}
			b.net.SetConsoleUser("alice")
			mustOK(t, b.m.RetryRegionPending(bg))
			if _, ok := b.pending(); ok || !hasEvent(b, EvRegionPendingCleared) {
				t.Fatal("重试成功后文件应删除并有事件")
			}
			if !b.locale().Equal(b.init.Locale) {
				t.Fatalf("重试后应等于初始值：%+v", b.locale())
			}
		})
	}
}

// 持续失败：文件保留；部分成功时只留下剩下的项；不是 RESTORED 时不重试。
func TestRetryRegionPendingPartial(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	b.net.FailNext(sysnet.OpLocaleTZ, errBoom)
	b.net.FailNext(sysnet.OpLocaleSys, errBoom)
	mustOK(t, b.m.Restore(bg))
	if p, _ := b.pending(); !slices.Equal(p.Items, []string{ItemSystem, ItemTimezone}) {
		t.Fatalf("%v", p.Items)
	}
	b.net.FailNext(sysnet.OpLocaleTZ, errBoom)
	mustOK(t, b.m.RetryRegionPending(bg))
	if p, _ := b.pending(); !slices.Equal(p.Items, []string{ItemTimezone}) {
		t.Fatalf("部分成功后只留下剩下的：%v", p.Items)
	}
	mustOK(t, b.m.Enable(bg)) // 文件里的时区盖到原值上，启用成功后删掉文件
	if _, ok := b.pending(); ok {
		t.Fatal("启用成功后应删除待还原文件")
	}
	if o := b.state().Region.Original; o == nil || o.Timezone != b.init.Locale.Timezone {
		t.Fatalf("原值应取自待还原文件：%+v", o)
	}
	mustOK(t, b.m.RetryRegionPending(bg)) // 不是 RESTORED：不做
	mustOK(t, b.m.Restore(bg))
	b.assertClean(b.init) // 回到最初的初始值，而不是中间状态
}

// 待还原文件损坏：忽略并发事件，不影响启用和恢复。
func TestRegionPendingCorrupt(t *testing.T) {
	b := newBed(t)
	if err := os.MkdirAll(b.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i, content := range []string{"{not json", `{"v":1,"items":["timezone"],"timezone":"Mars/Base"}`} {
		os.WriteFile(filepath.Join(b.dir, PendingFileName), []byte(content), 0o600)
		mustOK(t, b.m.Enable(bg))
		mustOK(t, b.m.Restore(bg))
		b.assertClean(b.init)
		if !hasEvent(b, EvRegionPendingCorrupt) {
			t.Fatalf("第 %d 种：应有事件", i)
		}
		mustOK(t, b.m.RetryRegionPending(bg))
	}
	os.Chmod(filepath.Join(b.dir, PendingFileName), 0o644) // 权限不对也按损坏
	if _, _, err := b.store.LoadPending(); !errors.Is(err, ErrPendingCorrupt) {
		t.Fatal("别人可读的文件应按损坏处理")
	}
}

// 崩溃穷举加“写待还原文件”这个注入点：恢复时时区写入失败，在恢复的每一个改动事件之后崩溃，新进程 Recover；
// 最终要么等于初始值，要么不相等的项都在待还原文件里，且重试后相等。
func TestCrashWithPending(t *testing.T) {
	probe := newBed(t)
	mustOK(t, probe.m.Enable(bg))
	probe.net.FailNext(sysnet.OpLocaleTZ, errBoom)
	_, names := probe.countChanges(func() { _ = probe.m.Restore(bg) })
	if !slices.ContainsFunc(names, func(s string) bool { return strings.HasPrefix(s, "store pending save") }) {
		t.Fatalf("改动事件里没有写待还原文件：%v", names)
	}
	for k := 1; k <= len(names); k++ {
		for _, reboot := range []bool{false, true} {
			t.Run(fmt.Sprintf("%02d %s/重启=%v", k, names[k-1], reboot), func(t *testing.T) {
				b := newBed(t)
				mustOK(t, b.m.Enable(bg))
				b.net.FailNext(sysnet.OpLocaleTZ, errBoom)
				if !b.runToCrash(k, func() { _ = b.m.Restore(bg) }) {
					t.Fatal("没崩溃")
				}
				if reboot {
					if _, err := b.reboot(); err != nil {
						t.Fatal(err)
					}
				} else {
					b.crash()
				}
				mustOK(t, b.m.Recover(bg))
				if b.state().State != Restored { // 崩溃在写 RESTORING 之前：Recover 回到启用，再恢复一次（不再注入失败）
					mustOK(t, b.m.Restore(bg))
				}
				b.assertNetClean()
				if !b.locale().Equal(b.init.Locale) {
					if _, ok := b.pending(); !ok {
						t.Fatalf("地区和初始值不等，但没有待还原文件：%+v", b.locale())
					}
					mustOK(t, b.m.RetryRegionPending(bg))
				}
				if _, ok := b.pending(); ok || !b.locale().Equal(b.init.Locale) {
					t.Fatalf("重试后应等于初始值：%+v", b.locale())
				}
				b.assertClean(b.init)
			})
		}
	}
}
