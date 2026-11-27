package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"claude-abroad/internal/api"
	"claude-abroad/internal/buildinfo"
	"claude-abroad/internal/pf"
	"claude-abroad/internal/state"
)

// 原地升级（v0.71）：/v1/status 总是带版本号和状态格式版本号（没写入版本号时是 unknown）。
func TestStatusBuildInfo(t *testing.T) {
	e := newEnv(t)
	d := e.start(e.opts())
	st := status(t, d)
	if st.Build == nil || *st.Build != buildinfo.Get() || st.Build.Known() {
		t.Fatalf("build：%+v", st.Build)
	}
	if st.StateFormat != buildinfo.StateFormat {
		t.Fatalf("state_format：%d", st.StateFormat)
	}
	if st.Uninstall == nil || st.Uninstall.Running || st.Uninstall.Done || st.Busy {
		t.Fatalf("刚启动时不忙、没卸载：busy=%v %+v", st.Busy, st.Uninstall)
	}
}

// 原地升级只在没有正在进行的操作时进行：忙标记、卸载中、已卸载都在状态里如实给出。
func TestStatusBusyAndUninstallFlags(t *testing.T) {
	e := newEnv(t)
	d := e.start(e.opts())
	if err := d.m.Exclusive(func() error {
		if st := status(t, d); !st.Busy {
			t.Error("占着忙标记时 busy 应为真")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if st := status(t, d); st.Busy {
		t.Fatal("放开后 busy 应为假")
	}
	d.uninstalling.Store(true)
	if st := status(t, d); !st.Uninstall.Running || st.Uninstall.Done {
		t.Fatalf("卸载中：%+v", st.Uninstall)
	}
	d.uninstalling.Store(false)
	if err := os.WriteFile(filepath.Join(e.dir, UninstalledFile), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := status(t, d); st.Uninstall.Running || !st.Uninstall.Done {
		t.Fatalf("已卸载：%+v", st.Uninstall)
	}
}

// 原地升级的“停服务、重启”走现有路径（设计 11.4，v0.71；顾问 2026-11-27 风险 3 的守护）：abroadd 正常退出时
// 不恢复网络、不清 pf，状态文件保持原样；同一状态目录起新实例后，状态文件、pf 锚点、pf 启用引用都和停之前一样。
// 已回国（回国拦截生效、留一个启用引用）和已停用（断网规则生效）两种。
func TestStopStartKeepsState(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, d *Daemon)
		state state.State
	}{
		{"已回国", func(t *testing.T, d *Daemon) {}, state.Restored},
		{"已停用", func(t *testing.T, d *Daemon) {
			post(t, d, "/v1/enable", 200)
			post(t, d, "/v1/disable", 200)
		}, state.Disabled},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newPrepEnv(t)
			d := e.start(false)
			c.setup(t, d)
			waitStartupSettled(t, d)
			snap := func() ([]byte, string, []string, bool) {
				b, err := os.ReadFile(filepath.Join(e.dir, state.FileName))
				if err != nil {
					t.Fatal(err)
				}
				return b, string(e.sys.PF.Raw(pf.DefaultAnchor)), e.sys.PF.Tokens(), e.sys.PF.Active(pf.DefaultAnchor)
			}
			f0, a0, t0, act0 := snap()
			if !act0 || len(t0) == 0 {
				t.Fatalf("前提：锚点应生效、有启用引用：active=%v tokens=%v", act0, t0)
			}
			if st := status(t, d); st.State != string(c.state) {
				t.Fatalf("前提：状态应为 %s：%s", c.state, st.State)
			}
			if err := d.Shutdown(); err != nil {
				t.Fatal(err)
			}
			e.d = nil
			e.cur.Store(nil)
			f1, a1, t1, act1 := snap()
			if !bytes.Equal(f0, f1) || a0 != a1 || !slices.Equal(t0, t1) || !act1 {
				t.Fatalf("停服务后应一样：\n状态文件 %s\n→ %s\n锚点相同=%v 引用 %v → %v 生效=%v", f0, f1, a0 == a1, t0, t1, act1)
			}
			d = e.start(false)
			f2, a2, t2, act2 := snap()
			// 已停用：utun 是进程持有的设备，abroadd 退出时随之消失，重启时按现有的恢复路径重建（设计 10“DISABLED：重建 utun、
			// pf……”），名字可能变；状态文件和锚点里只有这个名字跟着变，其余逐字相同。
			if c.state == state.Disabled {
				var o, n state.File
				_ = json.Unmarshal(f0, &o)
				_ = json.Unmarshal(f2, &n)
				if o.Utun == "" || n.Utun == "" {
					t.Fatalf("已停用时应有 utun：%q → %q", o.Utun, n.Utun)
				}
				f2 = bytes.ReplaceAll(f2, []byte(`"utun": "`+n.Utun+`"`), []byte(`"utun": "`+o.Utun+`"`))
				a2 = strings.ReplaceAll(a2, n.Utun, o.Utun)
			}
			if !bytes.Equal(f0, f2) || a0 != a2 || !slices.Equal(t0, t2) || !act2 {
				t.Fatalf("重启后应一样：\n状态文件 %s\n→ %s\n锚点相同=%v 引用 %v → %v 生效=%v", f0, f2, a0 == a2, t0, t2, act2)
			}
			if st := status(t, d); st.State != string(c.state) || st.Corrupt || st.Prepare == nil || !st.Prepare.Done {
				t.Fatalf("重启后状态：%s corrupt=%v %+v", st.State, st.Corrupt, st.Prepare)
			}
		})
	}
}

// 2026-11-27 真机：原地升级后马上操作被回“忙”，文字却是“已经有一个启用、停用或恢复在进行”。启动检查没结束时，
// /v1/status 的 starting 为真；被拒的改动类请求仍是 409 busy，文字换成 StartingBusyText；不忙的请求照常进行。
// 启动检查结束后 starting 为假，忙时回原来的文字。
func TestStartingBusyText(t *testing.T) {
	e := newEnv(t)
	d := e.start(e.opts())
	if st := status(t, d); st.Starting {
		t.Fatal("启动检查结束后 starting 应为假")
	}
	d.startupHold.Store(true)
	hold := func(fn func()) {
		t.Helper()
		if err := d.m.Exclusive(func() error { fn(); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	hold(func() {
		if st := status(t, d); !st.Starting || !st.Busy {
			t.Errorf("启动中且占着忙标记：starting=%v busy=%v", st.Starting, st.Busy)
		}
		code, b := call(t, d, "POST", "/v1/enable", nil)
		eb := codeOf(t, b)
		if code != http.StatusConflict || eb.Code != api.CodeBusy || eb.Error != StartingBusyText {
			t.Errorf("启动中被拒：%d %s", code, b)
		}
	})
	if code, b := call(t, d, "POST", "/v1/enable", nil); code != http.StatusOK {
		t.Fatalf("启动中、不忙时启用照常进行：%d %s", code, b)
	}
	d.startupHold.Store(false)
	if st := status(t, d); st.Starting {
		t.Fatal("启动检查结束后 starting 应为假")
	}
	hold(func() {
		code, b := call(t, d, "POST", "/v1/enable", nil)
		eb := codeOf(t, b)
		if code != http.StatusConflict || eb.Code != api.CodeBusy || eb.Error == StartingBusyText || !strings.Contains(eb.Error, "已经有一个") {
			t.Errorf("启动后忙：%d %s", code, b)
		}
	})
}
