package stack

import (
	"context"
	"testing"

	"claude-abroad/internal/rules"
)

// M5-5 验收 c（网络栈一侧）：三个应用、分别走两个出口和直连的在途流，汇总的连接数和去向与实际一致；
// 流结束后条目消失；还在判定的流不算；上限生效。
func TestProxyApps(t *testing.T) {
	p := &Proxy{}
	add := func(app string, act rules.Action, reason rules.Reason) func() {
		_, e, done := p.track(context.Background())
		p.mu.Lock()
		e.decided, e.who, e.action, e.reason = true, rules.Who{App: rules.AppID(app)}, act, reason
		p.mu.Unlock()
		return done
	}
	var dones []func()
	for i := 0; i < 3; i++ {
		dones = append(dones, add("com.example.a", rules.ActMain, rules.RDefault))
	}
	dones = append(dones, add("com.example.a", rules.Action{Kind: rules.Proxy, Exit: "wg"}, rules.RRule))
	for i := 0; i < 2; i++ {
		dones = append(dones, add("com.example.b", rules.Action{Kind: rules.Proxy, Exit: "wg"}, rules.RRule))
	}
	endC := add("com.example.c", rules.ActDirect, rules.RRule)
	_, _, pending := p.track(context.Background()) // 还在判定
	defer pending()
	apps, trunc := p.Apps(10)
	if trunc || len(apps) != 3 {
		t.Fatalf("应有三个应用：%+v", apps)
	}
	if apps[0].App != "com.example.a" || apps[0].Conns != 4 || len(apps[0].Exits) != 2 || apps[0].Exits[0] != "main" || apps[0].Exits[1] != "wg" {
		t.Fatalf("a：%+v", apps[0])
	}
	if apps[1].App != "com.example.b" || apps[1].Conns != 2 || apps[1].Exits[0] != "wg" || apps[1].Reason != "rule" {
		t.Fatalf("b：%+v", apps[1])
	}
	if apps[2].App != "com.example.c" || apps[2].Exits[0] != "direct" {
		t.Fatalf("c：%+v", apps[2])
	}
	endC()
	if apps, _ := p.Apps(10); len(apps) != 2 {
		t.Fatal("流结束后条目应消失")
	}
	if apps, trunc := p.Apps(1); len(apps) != 1 || !trunc || apps[0].App != "com.example.a" {
		t.Fatal("上限生效，按连接数从多到少")
	}
	for _, d := range dones {
		d()
	}
	if apps, _ := p.Apps(10); len(apps) != 0 {
		t.Fatal("全部结束后为空")
	}
}
