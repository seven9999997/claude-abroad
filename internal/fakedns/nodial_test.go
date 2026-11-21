package fakedns

import (
	"go/ast"
	"testing"

	"claude-abroad/internal/srccheck"
)

// 不准直接拨号（m3-plan M3-4 验收 i）：直连只能经直连拨号模块，代理只能经数据通道。
// 白名单：doh.go 的 http.Transport、http.Client（拨号经 DialTLSContext 走数据通道的 Opener，design 4.3）。
func TestNoDial(t *testing.T) {
	srccheck.NoDial(t, ".", map[string][]string{"doh.go": {"http.Transport", "http.Client"}})
}

// doh.go 里的 http.Transport 必须设了 DialTLSContext（否则它会自己直接拨号）。
func TestDoHTransportDialsViaOpener(t *testing.T) {
	af := srccheck.Parse(t, "doh.go")
	found := false
	ast.Inspect(af, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if sel, ok := cl.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Transport" {
			for _, e := range cl.Elts {
				if kv, ok := e.(*ast.KeyValueExpr); ok {
					if k, ok := kv.Key.(*ast.Ident); ok {
						switch k.Name {
						case "DialTLSContext":
							found = true
						case "DialContext", "Dial", "DialTLS":
							t.Errorf("http.Transport 不应设 %s", k.Name)
						case "Proxy": // 只允许显式写 nil（不读环境变量里的代理）
							if v, ok := kv.Value.(*ast.Ident); !ok || v.Name != "nil" {
								t.Error("http.Transport 的 Proxy 只能是 nil")
							}
						}
					}
				}
			}
		}
		return true
	})
	if !found {
		t.Fatal("doh.go 的 http.Transport 应设 DialTLSContext（经数据通道）")
	}
}
