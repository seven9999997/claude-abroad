package pf

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// FakePF 是内存里的假 pf（实现 Runner），解析 Controller 拼出的参数（M2-6b）。
//   - 规则按 pfctl 关闭优化器时的形式“打印”（补修饰、user 打成 UID）；Load 的参数里没有 -o none 时
//     改用优化后的形式（DHCP 合并成 <__automatic_0>），这样漏掉 -o none 会让 Ensure 复核失败；
//   - 钩子：ClearAnchor、Disable、DropMainRef、FailNext(操作)；
//   - 记录调用序列；
//   - Eval(pkt)：pf 启用且主规则集引用了锚点时，用 pf.Eval 对当前锚点求值，否则放行。给 6c、M2-7 断言泄漏用。
//
// 操作名：load、flush、enable、release、kill、info、mainrules、anchorrules、table。
type FakePF struct {
	UIDs map[string]string // 用户名 → UID（打印 user 时用；没有就打印用户名）

	mu         sync.Mutex
	enabled    bool
	referenced bool
	anchors    map[string]*fakeAnchor
	tokens     map[string]bool
	nextToken  int
	failNext   map[string]error
	calls      []string
	kills      int
	afterLoad  func() // 只给测试：下一次加载完成后调用一次（不持锁），模拟别人刚好在加载和复核之间写了别的内容
}

// AfterNextLoad 设置下一次加载完成后要做的事（只调用一次）。
func (f *FakePF) AfterNextLoad(fn func()) { f.mu.Lock(); f.afterLoad = fn; f.mu.Unlock() }

type fakeAnchor struct {
	raw       []byte // 加载的原文（求值用）
	optimized bool   // 加载时没带 -o none
}

// NewFakePF 返回一个“系统刚开机”的假 pf：未启用，主规则集引用 com.apple/*，锚点都空。
func NewFakePF() *FakePF {
	return &FakePF{referenced: true, anchors: map[string]*fakeAnchor{}, tokens: map[string]bool{},
		failNext: map[string]error{}, nextToken: 1000}
}

// Reset 模拟重启：锚点空、未启用、token 清空、主规则集引用 com.apple/*（不记调用）。
func (f *FakePF) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enabled, f.referenced = false, true
	f.anchors = map[string]*fakeAnchor{}
	f.tokens = map[string]bool{}
}

// ClearAnchor 模拟别的软件清空了所有锚点。
func (f *FakePF) ClearAnchor() { f.mu.Lock(); f.anchors = map[string]*fakeAnchor{}; f.mu.Unlock() }

// Disable 模拟别的软件执行了 pfctl -d（所有启用引用作废）。
func (f *FakePF) Disable() {
	f.mu.Lock()
	f.enabled = false
	f.tokens = map[string]bool{}
	f.mu.Unlock()
}

// DropMainRef 模拟别的软件 pfctl -f 换掉主规则集，不再引用 com.apple/*。
func (f *FakePF) DropMainRef() { f.mu.Lock(); f.referenced = false; f.mu.Unlock() }

// RestoreMainRef 模拟主规则集恢复引用 com.apple/*。
func (f *FakePF) RestoreMainRef() { f.mu.Lock(); f.referenced = true; f.mu.Unlock() }

// FailNext 让下一次 op 操作失败。
func (f *FakePF) FailNext(op string, err error) { f.mu.Lock(); f.failNext[op] = err; f.mu.Unlock() }

// Calls 返回调用过的操作名序列。
func (f *FakePF) Calls() []string { f.mu.Lock(); defer f.mu.Unlock(); return slices.Clone(f.calls) }

// ResetCalls 清空调用记录。
func (f *FakePF) ResetCalls() { f.mu.Lock(); f.calls = nil; f.mu.Unlock() }

// Tokens 返回当前有效的启用引用（已排序）。
func (f *FakePF) Tokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.tokens))
	for t := range f.tokens {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

// Active 报告锚点里的规则是否在生效：pf 启用、主规则集引用 com.apple/*、锚点里有规则。
func (f *FakePF) Active(anchor string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enabled && f.referenced && f.anchors[anchor] != nil
}

// Kills 返回 KillStates 被调用的次数。
func (f *FakePF) Kills() int { f.mu.Lock(); defer f.mu.Unlock(); return f.kills }

// Eval 用当前生效的规则对一个包求值：pf 没启用、主规则集不引用锚点、锚点为空、没有规则命中时都等于“不拦”。
func (f *FakePF) Eval(anchor string, pkt Packet) (bool, error) {
	f.mu.Lock()
	a := f.anchors[anchor]
	active := f.enabled && f.referenced && a != nil
	var raw []byte
	if a != nil {
		raw = a.raw
	}
	f.mu.Unlock()
	if !active {
		return true, nil
	}
	// 锚点里没有规则命中：由系统的主规则集决定，默认放行（M6-11a 回国拦截那一组只有拦截规则；接管的规则以
	// “全部拦截”结尾，总有一条命中）。
	matched, pass, err := EvalMatch(raw, pkt)
	return !matched || pass, err
}

func (f *FakePF) op(name string) error {
	f.calls = append(f.calls, name)
	if err := f.failNext[name]; err != nil {
		delete(f.failNext, name)
		return err
	}
	return nil
}

// Run 实现 Runner。
func (f *FakePF) Run(ctx context.Context, args []string, stdin []byte) ([]byte, []byte, error) {
	so, se, err := f.run(ctx, args, stdin)
	if err == nil && slices.Contains(args, "-f") {
		f.mu.Lock()
		h := f.afterLoad
		f.afterLoad = nil
		f.mu.Unlock()
		if h != nil {
			h()
		}
	}
	return so, se, err
}

func (f *FakePF) run(_ context.Context, args []string, stdin []byte) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := strings.Join(args, " ")
	switch {
	case j == "-s info":
		if err := f.op("info"); err != nil {
			return nil, nil, err
		}
		if f.enabled {
			return []byte("Status: Enabled for 0 days 00:00:01           Debug: Urgent\n"), nil, nil
		}
		return []byte("Status: Disabled for 0 days 00:00:01          Debug: Urgent\n"), nil, nil
	case j == "-s rules":
		if err := f.op("mainrules"); err != nil {
			return nil, nil, err
		}
		if f.referenced {
			return []byte("scrub-anchor \"com.apple/*\" all fragment reassemble\nanchor \"com.apple/*\" all\n"), nil, nil
		}
		// 别的软件换掉了主规则集：过滤锚点没了，但 scrub-anchor 还在（最容易骗过子串判断的情形）。
		return []byte("scrub-anchor \"com.apple/*\" all fragment reassemble\npass all flags S/SA keep state\n"), nil, nil
	case j == "-E":
		if err := f.op("enable"); err != nil {
			return nil, nil, err
		}
		f.enabled = true
		f.nextToken++
		tok := fmt.Sprint(f.nextToken)
		f.tokens[tok] = true
		return nil, []byte("pf enabled\nToken : " + tok + "\n"), nil
	case len(args) == 2 && args[0] == "-X":
		if err := f.op("release"); err != nil {
			return nil, nil, err
		}
		if !f.tokens[args[1]] {
			return nil, []byte("pfctl: invalid token\n"), errors.New("exit status 1")
		}
		delete(f.tokens, args[1])
		if len(f.tokens) == 0 {
			f.enabled = false
		}
		return nil, []byte("pf disabled\n"), nil
	case j == "-F states":
		if err := f.op("kill"); err != nil {
			return nil, nil, err
		}
		f.kills++
		return nil, nil, nil
	}
	// 带锚点的操作。
	var anchor string
	rest := args
	optimizerOff := false
	if len(rest) >= 2 && rest[0] == "-o" && rest[1] == "none" {
		optimizerOff, rest = true, rest[2:]
	}
	if len(rest) >= 2 && rest[0] == "-a" {
		anchor, rest = rest[1], rest[2:]
	} else {
		return nil, nil, fmt.Errorf("假 pf 不认识的参数：%v", args)
	}
	switch strings.Join(rest, " ") {
	case "-f -":
		if err := f.op("load"); err != nil {
			return nil, nil, err
		}
		if _, err := Eval(stdin, Packet{}); err != nil {
			return nil, []byte("syntax error\n"), errors.New("exit status 1")
		}
		f.anchors[anchor] = &fakeAnchor{raw: slices.Clone(stdin), optimized: !optimizerOff}
		return nil, nil, nil
	case "-F all":
		if err := f.op("flush"); err != nil {
			return nil, nil, err
		}
		delete(f.anchors, anchor)
		return nil, nil, nil
	case "-s rules":
		if err := f.op("anchorrules"); err != nil {
			return nil, nil, err
		}
		a := f.anchors[anchor]
		if a == nil {
			return nil, nil, nil
		}
		return f.print(a), nil, nil
	case "-t abroad_lan -T show":
		if err := f.op("table"); err != nil {
			return nil, nil, err
		}
		a := f.anchors[anchor]
		if a == nil || expectedTable(a.raw) == nil {
			return nil, []byte("pfctl: Table does not exist.\n"), errors.New("exit status 1")
		}
		var b strings.Builder
		for _, p := range expectedTable(a.raw) {
			if p.Bits() == 32 {
				fmt.Fprintf(&b, "   %s\n", p.Addr())
			} else {
				fmt.Fprintf(&b, "   %s\n", p)
			}
		}
		return []byte(b.String()), nil, nil
	}
	return nil, nil, fmt.Errorf("假 pf 不认识的参数：%v", args)
}

// print 按 pfctl 的打印形式输出锚点里的规则（不打印 table 行，表另查）。
func (f *FakePF) print(a *fakeAnchor) []byte {
	var out []string
	dhcpDone := false
	for _, l := range ruleLines(a.raw) {
		if a.optimized && strings.Contains(l, " port 68 to ") {
			// 优化器把 DHCP 合并成自动表。
			if !dhcpDone {
				out = append(out, "pass out quick inet proto udp from any port = 68 to <__automatic_0> port = 67 keep state")
				dhcpDone = true
			}
			continue
		}
		p := strings.ReplaceAll(l, " port ", " port = ")
		p = strings.Replace(p, " inet to ", " inet from any to ", 1)
		p = strings.Replace(p, " inet6 to ", " inet6 from any to ", 1)
		if i := strings.Index(p, " user "); i >= 0 {
			u := p[i+len(" user "):]
			if uid := f.UIDs[u]; uid != "" {
				u = uid
			}
			p = p[:i] + " user = " + u
		}
		if strings.HasPrefix(p, "pass ") && !strings.HasSuffix(p, " no state") {
			if strings.Contains(p, " proto udp ") {
				p += " keep state"
			} else {
				p += " flags S/SA keep state"
			}
		}
		out = append(out, p)
	}
	return []byte(strings.Join(out, "\n") + "\n")
}

// Raw 是锚点里加载的原始规则文本（没有时为 nil）。
func (f *FakePF) Raw(anchor string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.anchors[anchor]; a != nil {
		return slices.Clone(a.raw)
	}
	return nil
}
