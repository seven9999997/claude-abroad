//go:build abroaddev

package guard

import (
	"bytes"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"claude-abroad/internal/pf"
	"claude-abroad/internal/state"
)

// M6-11b（开发构建）：guard 按选择文件选地址段——测试 → 测试段；真实 → 真实段；不在、损坏、权限过宽 → 不加载。
func TestGuardReturnBlockSetDev(t *testing.T) {
	setup := func(t *testing.T, write func(dir string)) *rig {
		r := newRig(t)
		r.setState(t, state.Restored)
		r.mark(t, state.PreparedFile)
		write(r.stateDir)
		return r
	}
	for name, c := range map[string]struct {
		write func(dir string)
		nets  []any
	}{
		"测试": {func(d string) { _ = state.WriteReturnBlockSet(d, pf.SetTest) }, []any{pf.TestNets}},
		"真实": {func(d string) { _ = state.WriteReturnBlockSet(d, pf.SetReal) }, []any{pf.AnthropicNets}},
		"不在": {func(string) {}, nil},
		"损坏": {func(d string) { _ = os.WriteFile(filepath.Join(d, state.ReturnBlockSetFile), []byte("all\n"), 0o600) }, nil},
		"权限过宽": {func(d string) { _ = os.WriteFile(filepath.Join(d, state.ReturnBlockSetFile), []byte("test\n"), 0o644) }, nil},
	} {
		t.Run(name, func(t *testing.T) {
			r := setup(t, c.write)
			o := r.run(t)
			if c.nets == nil {
				if o != NotNeeded || len(r.fake.Calls()) != 0 {
					t.Fatalf("开发构建读不出选择时不应加载：%v %v", o, r.fake.Calls())
				}
				return
			}
			want, _ := pf.Render(pf.Config{ReturnBlock: c.nets[0].([]netip.Prefix)})
			if o != Loaded || !bytes.Equal(r.anchor(), want) {
				t.Fatalf("%v\n%s", o, r.anchor())
			}
		})
	}
}
