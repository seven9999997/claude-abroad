package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/exitconf"
)

func udpReport(udp exitcheck.Result, cause exitcheck.Cause, https exitcheck.Result) exitcheck.Report {
	return exitcheck.Report{Exit: "main", At: time.Now(), Items: []exitcheck.Item{
		{Name: "https", Result: https}, {Name: "udp", Result: udp, Cause: cause},
	}}
}

// 补齐二验收 a、b：主代理的 UDP 结果记在当时下发的节点名下；状态给当前节点的，换节点之后跟着变；节点列表给每个节点最近
// 一次的（没检查过的是 unknown）；由“不是不可用”变成“不可用”时只记一条事件。
func TestMainUDPRecording(t *testing.T) {
	e := newEnv(t)
	d := e.start(e.opts())
	if d.mainUDPState() != "" {
		t.Fatal("没有下发的节点时状态里不应有")
	}
	d.setPushedMain("n1")
	if d.mainUDPState() != exitcheck.UDPUnknown {
		t.Fatal("没检查过：还不知道")
	}
	d.recordMainReport(udpReport(exitcheck.Unknown, exitcheck.CauseTimeout, exitcheck.Pass)) // 回应帧成功但没有应答
	if d.mainUDPState() != exitcheck.UDPUnavailable {
		t.Fatalf("出口通、UDP 没应答：不可用，实际 %s", d.mainUDPState())
	}
	d.recordMainReport(udpReport(exitcheck.Unknown, exitcheck.CauseTimeout, exitcheck.Pass))
	n := 0
	for _, ev := range d.events.List() {
		if ev.Source == "main_proxy" && ev.Kind == "udp_unavailable" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("一直不可用只记一条事件：%d", n)
	}
	d.recordMainReport(exitcheck.Report{Exit: "hk"}) // 分流出口的报告不算
	d.setPushedMain("n2")
	if d.mainUDPState() != exitcheck.UDPUnknown {
		t.Fatal("换节点后状态跟着变")
	}
	d.recordMainReport(udpReport(exitcheck.Pass, exitcheck.CauseOK, exitcheck.Pass))
	if st := status(t, d); st.MainUDP != exitcheck.UDPAvailable {
		t.Fatalf("状态里的结果：%q", st.MainUDP)
	}
	if d.nodeUDP("n1") != exitcheck.UDPUnavailable || d.nodeUDP("n2") != exitcheck.UDPAvailable || d.nodeUDP("n3") != exitcheck.UDPUnknown {
		t.Fatal("每个节点最近一次的结果")
	}
	if d.mainUDPState() != exitcheck.UDPAvailable {
		t.Fatal("当前节点可用")
	}
	d.recordMainReport(udpReport(exitcheck.Unknown, exitcheck.CauseConnect, exitcheck.Unknown))
	if d.mainUDPState() != exitcheck.UDPUnknown {
		t.Fatal("出口本身不通：还不知道")
	}
}

// 补齐二验收 b：来源列表里每个节点带最近一次的 UDP 结果；节点地址不出现。
func TestMainUDPInSourceList(t *testing.T) {
	m := newMainEnv(t, false)
	m.addYAML(t, "本地", mainNodesYAML)
	id := m.nodeID(t, "relay")
	m.d.setPushedMain(id)
	m.d.recordMainReport(udpReport(exitcheck.Fail, exitcheck.CauseUnexpectedReply, exitcheck.Pass))
	_, b := call(t, m.d, "GET", "/v1/main-proxy/sources", nil)
	var vs []MainProxySourceView
	if json.Unmarshal(b, &vs) != nil || len(vs) == 0 {
		t.Fatalf("%s", b)
	}
	seen := 0
	for _, n := range vs[0].Nodes {
		want := exitcheck.UDPUnknown
		if n.ID == id {
			want = exitcheck.UDPUnavailable
		}
		if n.UDP != want {
			t.Fatalf("%s 的 UDP：%q，应为 %q", n.Name, n.UDP, want)
		}
		seen++
	}
	if seen == 0 || strings.Contains(string(b), "relay.example.com") {
		t.Fatalf("节点列表：%s", b)
	}
}

// 补齐二验收 c：回退标记——规则指向的出口不可用时标出、带原因，出口恢复后消失；指向主代理、本机网络、阻止的规则永远
// 没有；出口的受影响规则。回退时流量仍走主代理（TestExitsAvailabilityAndFallback 原样守着）。
func TestRuleFallbackMarks(t *testing.T) {
	e := newExitEnv(t, nil)
	e.ch.set(func() { e.ch.split["hk"] = exitcheck.Fail })
	if code, msg := putExit(t, e.d, "hk", exitconf.WireGuard, tWG); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	r := "default: proxy:main\nrules:\n  - {app: com.example.a, action: \"proxy:hk\"}\n  - {app: com.example.m, action: \"proxy:main\"}\n  - {app: com.example.b, action: block}\n  - {app: com.example.d, action: direct}\n"
	if code, msg := putRules(t, e.d, r, false); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	waitExit(t, "检查过一次", func() bool { return e.ch.n("hk") > 0 && !e.up("hk") })
	apps := func() AppsView {
		_, b := call(t, e.d, "GET", "/v1/apps", nil)
		var v AppsView
		if json.Unmarshal(b, &v) != nil {
			t.Fatalf("%s", b)
		}
		return v
	}
	v := apps()
	if len(v.Fallbacks) != 1 || v.Fallbacks[0].App != "com.example.a" || v.Fallbacks[0].Exit != "hk" || v.Fallbacks[0].Reason == "" {
		t.Fatalf("应只标出指向不可用出口的那一条：%+v", v.Fallbacks)
	}
	st := status(t, e.d)
	cnt := -1
	for _, x := range st.Exits {
		if x.Tag == "hk" {
			cnt = x.AffectedCount
		}
	}
	if cnt != 1 {
		t.Fatalf("状态里出口的受影响规则条数：%d", cnt)
	}
	if b, _ := json.Marshal(st); strings.Contains(string(b), "com.example.a") {
		t.Fatal("状态里不应出现应用 ID")
	}
	_, xb := call(t, e.d, "GET", "/v1/exits", nil)
	var xs struct {
		Exits []ExitView `json:"exits"`
	}
	if json.Unmarshal(xb, &xs) != nil || len(xs.Exits) != 1 || len(xs.Exits[0].Affected) != 1 || xs.Exits[0].Affected[0] != "com.example.a" {
		t.Fatalf("出口列表的受影响规则：%s", xb)
	}
	e.ch.set(func() { e.ch.split["hk"] = exitcheck.Pass })
	e.d.exitLoop.wake()
	waitExit(t, "恢复", func() bool { return e.up("hk") })
	if v := apps(); len(v.Fallbacks) != 0 {
		t.Fatalf("恢复后标记消失：%+v", v.Fallbacks)
	}
}
