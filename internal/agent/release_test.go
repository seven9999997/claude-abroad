//go:build !abroaddev

package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// M6-1：默认（发布）构建里 agent 没有开发接口：devSetup 为 nil（开发出口、探查、同机对端的代码整段不编译），
// 路由里也没有这些路径。
func TestNoDevInReleaseBuild(t *testing.T) {
	if devSetup != nil {
		t.Fatal("默认构建不应有开发接口")
	}
	h := (&API{State: &State{}}).Handler()
	for _, p := range []string{"/v1/dev-exits", "/v1/dev/netns-probe", "/v1/dev/exit-peer"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s：%d，应为 404", p, rec.Code)
		}
	}
}
