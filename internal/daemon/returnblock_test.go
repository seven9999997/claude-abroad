package daemon

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"claude-abroad/internal/pf"
)

// M6-11a：打开了首次准备且做完了 → 回国后锚点恰好是回国拦截那一组、留一个 token，状态里报告生效和地址段；
// 卸载标记在、没打开首次准备时 → 回国行为和以前一样（锚点清空、token 全部释放）。
func TestReturnBlockSwitch(t *testing.T) {
	want, _ := pf.Render(pf.ReturnBlockConfig())
	t.Run("打开且做完首次准备", func(t *testing.T) {
		e := newPrepEnv(t)
		d := e.start(false)
		st := status(t, d)
		// 全新安装时状态就是“已回国”：启动时就加载这一组（和解析锁一样，从安装起就生效）
		if st.ReturnBlock == nil || !st.ReturnBlock.On || !st.ReturnBlock.Active || !slices.Equal(st.ReturnBlock.Nets, []string{"160.79.104.0/21", "2607:6bc0::/32"}) || st.ReturnBlock.Note == "" {
			t.Fatalf("启用前：%+v", st.ReturnBlock)
		}
		if string(e.sys.PF.Raw(pf.DefaultAnchor)) != string(want) || len(e.sys.PF.Tokens()) != 1 {
			t.Fatalf("启用前锚点：%s", e.sys.PF.Raw(pf.DefaultAnchor))
		}
		post(t, d, "/v1/enable", 200)
		restoreWith(t, d, false)
		st = status(t, d)
		if string(e.sys.PF.Raw(pf.DefaultAnchor)) != string(want) || !e.sys.PF.Active(pf.DefaultAnchor) || len(e.sys.PF.Tokens()) != 1 {
			t.Fatalf("回国后锚点：%s tokens=%v", e.sys.PF.Raw(pf.DefaultAnchor), e.sys.PF.Tokens())
		}
		if st.State != "RESTORED" || !st.ReturnBlock.Active || st.Protected {
			t.Fatalf("回国后状态：%s %+v 保护=%v", st.State, st.ReturnBlock, st.Protected)
		}
		// 重启 abroadd：照样生效
		d = e.restart(t, d, false)
		if st = status(t, d); !st.ReturnBlock.Active || string(e.sys.PF.Raw(pf.DefaultAnchor)) != string(want) {
			t.Fatalf("重启后：%+v", st.ReturnBlock)
		}
	})
	t.Run("卸载过", func(t *testing.T) {
		e := newPrepEnv(t)
		d := e.start(false)
		post(t, d, "/v1/enable", 200)
		_ = os.WriteFile(filepath.Join(e.dir, UninstalledFile), []byte("x\n"), 0o600)
		if d.returnBlockOn() {
			t.Fatal("卸载过时开关应关")
		}
		if st := status(t, d); st.ReturnBlock == nil || st.ReturnBlock.On {
			t.Fatalf("%+v", st.ReturnBlock)
		}
	})
	t.Run("没打开首次准备", func(t *testing.T) {
		e := newEnv(t)
		d := e.start(e.opts())
		post(t, d, "/v1/enable", 200)
		restoreWith(t, d, false)
		st := status(t, d)
		if st.ReturnBlock != nil || e.sys.PF.Active(pf.DefaultAnchor) || len(e.sys.PF.Tokens()) != 0 {
			t.Fatalf("应和以前一样：%+v %v", st.ReturnBlock, e.sys.PF.Tokens())
		}
	})
	t.Run("首次准备没做完", func(t *testing.T) {
		e := newPrepEnv(t)
		d := e.start(false)
		_ = os.Remove(filepath.Join(e.dir, PreparedFile))
		if d.returnBlockOn() {
			t.Fatal("没有完成标记时开关应关")
		}
	})
}
