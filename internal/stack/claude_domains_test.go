package stack

import (
	"sync"
	"testing"
)

// M6-12：只对识别为 Claude 的连接，把不在内置置顶后缀之下的域名交给观察者（只观察，判定不变）；
// 内置后缀（含新追加的 claudeusercontent.com）、别的应用、没有观察者时都不记；没有观察者时也不为它去识别应用。
func TestClaudeDomainObserver(t *testing.T) {
	e := newM3Env(t, m3Rules)
	var mu sync.Mutex
	var got []string
	e.p.SetClaudeDomainObserver(func(n string) { mu.Lock(); got = append(got, n); mu.Unlock() })
	dial := func(port uint16, name string) {
		t.Helper()
		conn, err := e.cl.dialFrom(port, ap(e.lookupA(t, name), 443), ioWait)
		if err != nil {
			t.Fatalf("%s：%v", name, err)
		}
		if err := echoOnce(conn, []byte("x")); err != nil {
			t.Fatalf("%s：%v", name, err)
		}
		conn.Close()
	}
	dial(portClaude+1, "plain.example.org")             // Claude、普通域名：记
	dial(portClaude+2, "claude.ai")                     // Claude、内置后缀：不记
	dial(portClaude+3, "x.frame.claudeusercontent.com") // 新追加的内置后缀：不记
	dial(portA+1, "other.example.net")                  // 别的应用：不记
	mu.Lock()
	if len(got) != 1 || got[0] != "plain.example.org" {
		t.Fatalf("只应记 Claude 访问的普通域名：%v", got)
	}
	mu.Unlock()
	// 去掉观察者：不再记
	e.p.SetClaudeDomainObserver(nil)
	dial(portClaude+4, "plain2.example.org")
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("去掉观察者后不应再记：%v", got)
	}
}

// M6-12 建议改：配置用不到应用、只为记域名才识别时，已知是内置后缀下的域名不识别（不会记，省掉热路径上的识别）；
// 普通域名照常识别。
func TestClaudeDomainObserverSkipsIdentForPinned(t *testing.T) {
	e := newM3Env(t, "")
	e.p.SetClaudeDomainObserver(func(string) {})
	dial := func(port uint16, name string) {
		t.Helper()
		conn, err := e.cl.dialFrom(port, ap(e.lookupA(t, name), 443), ioWait)
		if err != nil {
			t.Fatalf("%s：%v", name, err)
		}
		_ = echoOnce(conn, []byte("x"))
		conn.Close()
	}
	dial(portClaude+1, "claude.ai")
	dial(portClaude+2, "api.anthropic.com")
	if n := e.ident.calls.Load(); n != 0 {
		t.Fatalf("内置域名不应为记录去识别：%d", n)
	}
	dial(portClaude+3, "plain.example.org")
	if e.ident.calls.Load() == 0 {
		t.Fatal("普通域名应识别")
	}
}
