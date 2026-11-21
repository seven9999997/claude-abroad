package sniff

import (
	"go/ast"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"claude-abroad/internal/rules"
	"claude-abroad/internal/srccheck"
)

const perInput = 50 * time.Millisecond

// within 判断 fn 能否在 perInput 内完成。并行模糊测试时偶尔有调度或 GC 停顿，
// 所以超时时最多重测两次，三次里最短的仍超过才算慢（不是靠运气：真慢的输入每次都慢）。
func within(fn func()) (time.Duration, bool) {
	best := time.Duration(1<<63 - 1)
	for i := 0; i < 3; i++ {
		start := time.Now()
		fn()
		best = min(best, time.Since(start))
		if best <= perInput {
			return best, true
		}
	}
	return best, false
}

// checkName：有名字时一定是带名字的种类，并且已经归一。
func checkName(t *testing.T, r Result) {
	t.Helper()
	switch r.Kind {
	case rules.TLSName, rules.QUICName, rules.HTTPHost:
		if n, err := rules.NormalizeName(r.Name); err != nil || n != r.Name {
			t.Fatalf("名字没有归一：%q", r.Name)
		}
	default:
		if r.Name != "" {
			t.Fatalf("%v 不应带名字", r.Kind)
		}
	}
}

func FuzzTLS(f *testing.F) {
	f.Add(hello(sniExt("a.example")))
	f.Add(records(hsMsg(hsClientHello, helloBody(sniExt("b.example"), ext{extECH, []byte{1}})), 7, 20))
	f.Add([]byte{0x16, 0x03, 0x01, 0x00, 0x05, 0x01, 0x00, 0x00, 0x01, 0x00})
	f.Add(readHex(f, "curl-clienthello.hex"))
	f.Fuzz(func(t *testing.T, b []byte) {
		var r, fin Result
		var more bool
		if d, ok := within(func() { r, more = TLS(b); fin = Final(b) }); !ok {
			t.Fatalf("处理用了 %v", d)
		}
		// e. 偏严的不变量：判成 NotTLS 的前提是它不是任何 TLS 记录头的前缀。
		if !more && r.Kind == rules.NotTLS && MayBeTLS(b) {
			t.Fatal("TLS 记录头的前缀被判成了 NotTLS")
		}
		if fin.Kind == rules.NotTLS && MayBeTLS(b) {
			t.Fatal("Final 把 TLS 记录头的前缀判成了 NotTLS")
		}
		checkName(t, r)
		checkName(t, fin)
		// needMore 收敛：不断补字节，最终一定能下结论。
		for i := 0; more && i < 10; i++ {
			b = append(b, make([]byte, 4096)...)
			r, more = TLS(b)
		}
		if more {
			t.Fatalf("补到 %d 字节仍要更多", len(b))
		}
	})
}

func FuzzQUIC(f *testing.F) {
	f.Add(readHex(f, "rfc9001-a2-client-initial.hex"), []byte{})
	f.Add(readHex(f, "rfc9369-a2-client-initial.hex"), []byte{})
	f.Add([]byte{0xc0}, cryptoFrame(0, hsMsg(1, helloBody(sniExt("q.example")))))
	f.Add([]byte{}, append([]byte{0, 1}, cryptoFrame(5, []byte("abc"))...))
	f.Fuzz(func(t *testing.T, d []byte, frames []byte) {
		var r Result
		if dt, ok := within(func() { r = QUIC(d) }); !ok {
			t.Fatalf("处理用了 %v", dt)
		}
		if r.Kind == rules.NotTLS && (MayBeQUIC(d) || MayBeTLS(d)) {
			t.Fatal("符合 QUIC 形状或像 TLS 的数据报被判成了 NotTLS")
		}
		checkName(t, r)
		// 用模糊的帧内容生成一个能解开的 Initial，考验帧解析和 ClientHello 解析。
		if len(frames) <= 1100 {
			for _, v := range []uint32{quicV1, quicV2} {
				r := QUIC(sealInitial(t, v, []byte{9, 9, 9, 9}, 0, frames))
				if r.Kind == rules.NotTLS {
					t.Fatal("能解开的 Initial 被判成了 NotTLS")
				}
				checkName(t, r)
			}
		}
	})
}

func FuzzHTTP(f *testing.F) {
	f.Add([]byte("GET / HTTP/1.1\r\nHost: a.example\r\n\r\n"))
	f.Add([]byte("CONNECT b.example:443 HTTP/1.1\r\n\r\n"))
	f.Add([]byte("POST"))
	f.Fuzz(func(t *testing.T, b []byte) {
		var r, c Result
		var more, cmore bool
		if d, ok := within(func() { r, more = HTTP(b); c, cmore = Classify(b) }); !ok {
			t.Fatalf("处理用了 %v", d)
		}
		checkName(t, r)
		checkName(t, c)
		if !cmore && c.Kind == rules.NotTLS && MayBeTLS(b) {
			t.Fatal("Classify 把 TLS 记录头的前缀判成了 NotTLS")
		}
		for i := 0; more && i < 3; i++ {
			b = append(b, "\r\n\r\n"...)
			r, more = HTTP(b)
			if more && i == 2 {
				b = append(b, make([]byte, MaxHTTPHeader)...)
				r, more = HTTP(b)
			}
		}
		if more {
			t.Fatal("补上头部结尾后仍要更多")
		}
	})
}

// g. 只依赖标准库和 internal/rules；不执行命令；除了传入的连接，不做网络操作；不打日志。
func TestSourceRules(t *testing.T) {
	srccheck.NoExec(t, ".")
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af := srccheck.Parse(t, f)
		for _, im := range af.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			if strings.Contains(strings.SplitN(p, "/", 2)[0], ".") || strings.HasPrefix(p, "claude-abroad/") && p != "claude-abroad/internal/rules" {
				t.Errorf("%s 导入了 %s（只允许标准库和 internal/rules）", f, p)
			}
			switch p {
			case "log", "log/slog", "os/exec", "syscall", "net/http":
				t.Errorf("%s 导入了 %s", f, p)
			}
		}
		ast.Inspect(af, func(n ast.Node) bool {
			if x, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := x.X.(*ast.Ident); ok {
					name := id.Name + "." + x.Sel.Name
					if id.Name == "net" && (strings.HasPrefix(x.Sel.Name, "Dial") || strings.HasPrefix(x.Sel.Name, "Listen") ||
						strings.HasPrefix(x.Sel.Name, "Lookup") || strings.HasPrefix(x.Sel.Name, "Resolve")) {
						t.Errorf("%s 调用了 %s", f, name)
					}
					if id.Name == "fmt" && strings.HasPrefix(x.Sel.Name, "Print") {
						t.Errorf("%s 调用了 %s", f, name)
					}
				}
			}
			if c, ok := n.(*ast.CallExpr); ok {
				if id, ok := c.Fun.(*ast.Ident); ok && (id.Name == "println" || id.Name == "print") {
					t.Errorf("%s 调用了 %s", f, id.Name)
				}
			}
			return true
		})
	}
}
