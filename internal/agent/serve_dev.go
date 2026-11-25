//go:build abroaddev

package agent

import (
	"net/http"
	"os"
	"time"

	"claude-abroad/internal/kmod"
)

// 开发构建（go build -tags abroaddev）才有的接口：内核命令行带 abroad.dev=1 时注册开发用的多出口（m3-plan v2.8 决定 7）、
// M4-1 的探查、M4-7 端到端用的同机对端。发布构建没有这个文件，相关代码整段不编译（M6-1，m6-plan v2.1）。
func init() {
	devSetup = func(e *Env, sbset *SingBoxSet, main MainExits) (ExitTable, devHooks) {
		b, err := os.ReadFile(e.path("/proc/cmdline"))
		if err != nil || !ParseDevCmdline(string(b)) {
			return main, nil
		}
		dev := &DevExits{Set: sbset}
		e.logf("serve：开发标记已打开，注册 /v1/dev-exits")
		return devExitTable{MainExits: main, dev: dev}, devAPI{dev: dev}
	}
}

type devAPI struct{ dev *DevExits }

func (d devAPI) register(api *API, e *Env, loader kmod.Loader, exits *Exits) func() {
	if api.Dev == nil {
		api.Dev = map[string]http.Handler{}
	}
	api.Dev["/v1/dev-exits"] = d.dev
	// M4-1 的探查（m4-plan M4-1）：同样只在开发标记打开时注册。
	pops := newProbeOps(func(names []string) []kmod.Result {
		var out []kmod.Result
		for _, n := range names {
			rs, err := loader.LoadNames([]string{n})
			if err != nil {
				rs = []kmod.Result{{Name: n, Error: err.Error()}}
			}
			out = append(out, rs...)
		}
		return out
	})
	api.Dev["/v1/dev/netns-probe"] = &NetnsProbe{Ops: pops}
	e.logf("serve：开发标记已打开，注册 /v1/dev/netns-probe")
	dops, ok := pops.(devPeerOps)
	if !ok {
		return nil
	}
	peer := &DevPeer{Ops: dops, Exits: exits}
	api.Dev["/v1/dev/exit-peer"] = peer
	e.logf("serve：开发标记已打开，注册 /v1/dev/exit-peer")
	return func() {
		t0 := time.Now()
		peer.teardown()
		e.logf("serve：拆开发用对端用时 %s", time.Since(t0).Round(time.Millisecond))
	}
}
