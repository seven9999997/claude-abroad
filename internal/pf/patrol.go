package pf

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"sync"
	"time"
)

// NetSnapshot 是当前网络里和规则有关的部分。
type NetSnapshot struct {
	Direct []netip.Prefix // 物理网卡的直连网段（经 LANPrefixes 过滤后才进表）
	VMIfs  []string       // 本机虚拟机网桥接口
}

// NetSource 读取当前网络（M2-7 的 sysnet 来适配；本包不读系统网络配置）。
type NetSource interface {
	Snapshot(ctx context.Context) (NetSnapshot, error)
}

// EventKind 是巡检事件的种类。
type EventKind string

const (
	EvReloaded          EventKind = "reloaded"           // 重新加载了规则（WasAbsent、Foreign 说明原因）
	EvEnabled           EventKind = "enabled"            // pf 原本没启用，重新启用了
	EvNetError          EventKind = "net_error"          // 读不到当前网络或规则生成失败，沿用上一次的期望
	EvVMIfDropped       EventKind = "vmif_dropped"       // 名字不合规的网桥被丢掉
	EvNotReferenced     EventKind = "not_referenced"     // 主规则集不再引用 com.apple/*
	EvVerifyFailed      EventKind = "verify_failed"      // 加载后复核不一致
	EvEnsureError       EventKind = "ensure_error"       // 其他错误（pfctl 执行失败等）
	EvRepeatedlyCleared EventKind = "repeatedly_cleared" // 连续 3 轮都发生了修复
)

// Event 是一条巡检事件。只有种类和不敏感的细节（接口名、错误类别），不带地址。
type Event struct {
	Kind      EventKind
	WasAbsent bool
	Foreign   bool
	Detail    string
}

// DefaultPatrolInterval 是巡检的默认间隔（设计 6.1）。
const DefaultPatrolInterval = 5 * time.Second

// RepeatedThreshold 是“连续几轮发生修复”就回调一次 RepeatedlyCleared。
const RepeatedThreshold = 3

// Patrol 是 pf 巡检（设计 6.1、M2-6c）：每轮按当前网络重算期望规则，调用 Ensure。
//   - 期望规则算不出来时不放松：沿用上一次成功的期望；从未成功过时用最小配置（只有 User 和当前 utun）；
//   - 保护是否有效由 Protected 给出；巡检自己不碰闸门，也不改主规则集（闸门联动在 M2-7）；
//   - 新 token 经 OnToken 交出去（不能为 nil，否则 Step 直接返回错误，不调用 Enable）。
type Patrol struct {
	C        *Controller
	Net      NetSource
	User     string
	Utun     func() string // 当前 utun 名（为空表示还没有 utun）
	AllowLAN func() bool   // “允许局域网”是否打开（nil 按打开）
	Interval time.Duration // 为零时用 DefaultPatrolInterval
	// OnToken、OnEvent 在 Patrol 持锁时被调用：回调里不能再调用 Patrol 的方法（会死锁）。
	OnToken func(string)
	OnEvent func(Event) // 可为 nil

	mu           sync.Mutex
	lastExpected []byte // 上一次成功算出的期望规则
	lastApplied  []byte // 上一次成功应用（Ensure 无错）的规则
	protected    bool
	softFails    int // 连续的“其他错误”轮数
	repairStreak int // 连续发生修复的轮数
	// 上一轮复核不一致时的期望规则和锚点原始打印：下一轮两者都没变就不再重载、清状态（设计 6.1）。
	verifyExp, verifyShown []byte
	// paused 由 Flush 置上：暂停期间 Step 什么都不做，Protected 为 false；Apply 解除（M2-7c）。
	paused bool
}

// ErrNoTokenSink 是 OnToken 为 nil（新 token 没地方交，不允许丢）。
var ErrNoTokenSink = errors.New("巡检没有配置 OnToken，不能取得启用引用")

// Protected 报告 pf 保护当前是否有效。
func (p *Patrol) Protected() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.protected
}

func (p *Patrol) emit(e Event) {
	if p.OnEvent != nil {
		p.OnEvent(e)
	}
}

func (p *Patrol) utun() string {
	if p.Utun == nil {
		return ""
	}
	return p.Utun()
}

// expected 算本轮的期望规则（调用方持锁）。
func (p *Patrol) expected(ctx context.Context) ([]byte, error) {
	snap, err := p.Net.Snapshot(ctx)
	if err == nil {
		var vmifs []string
		for _, v := range snap.VMIfs {
			if ValidVMIf(v) {
				vmifs = append(vmifs, v)
			} else {
				p.emit(Event{Kind: EvVMIfDropped, Detail: "网桥名不合规，已忽略"})
			}
		}
		c := Config{UtunName: p.utun(), VMIfs: vmifs, User: p.User}
		if p.AllowLAN == nil || p.AllowLAN() {
			c.LAN = LANPrefixes(snap.Direct)
		}
		rules, rerr := Render(c)
		if rerr == nil {
			p.lastExpected = rules
			return rules, nil
		}
		err = rerr
	}
	p.emit(Event{Kind: EvNetError, Detail: "读不到当前网络或生成规则失败，沿用上一次的期望"})
	if p.lastExpected != nil {
		return p.lastExpected, nil
	}
	// 从来没算出过：最小配置（不放行局域网和网桥）。
	return Render(Config{UtunName: p.utun(), User: p.User})
}

// Apply 解除暂停并立即跑一轮（状态机设置好 utun 之后调用；pf 的应用都经同一个 Patrol，M2-7c）。
func (p *Patrol) Apply(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused = false
	return p.step(ctx)
}

// Flush 清空锚点，忘掉上一次应用的规则，并把巡检置为暂停：之后的 Step 不再把规则加载回去，直到下一次 Apply。
// 清空失败时同样保持暂停（锚点可能还在，由调用方重试）。
func (p *Patrol) Flush(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused, p.protected = true, false
	p.lastExpected, p.lastApplied = nil, nil
	p.verifyExp, p.verifyShown = nil, nil
	p.softFails, p.repairStreak = 0, 0
	return p.C.Flush(ctx)
}

// Paused 报告巡检是否处于暂停（Flush 之后、Apply 之前）。
func (p *Patrol) Paused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paused
}

// Step 跑一轮巡检。与 Run、其他 Step 互斥。返回本轮 Ensure 的错误（已经按种类计入 Protected 和事件）。
// 暂停期间什么都不做，返回 nil。
func (p *Patrol) Step(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.paused {
		return nil
	}
	return p.step(ctx)
}

func (p *Patrol) step(ctx context.Context) error {
	if p.OnToken == nil {
		return ErrNoTokenSink
	}
	if p.C == nil || p.Net == nil {
		return errors.New("巡检没有配置 Controller 或 NetSource")
	}
	exp, err := p.expected(ctx)
	if err != nil {
		p.protected = false
		p.emit(Event{Kind: EvEnsureError, Detail: "连最小配置也生成不了"})
		return err
	}
	if p.verifyShown != nil && bytes.Equal(exp, p.verifyExp) {
		if st, serr := p.C.Status(ctx); serr == nil && st.Enabled && st.Referenced && bytes.Equal(st.AnchorRules, p.verifyShown) {
			// 和上一轮复核不一致时一模一样：不 Load、不 Kill、不计修复，仍判保护失效。
			p.protected, p.softFails, p.repairStreak = false, 0, 0
			p.emit(Event{Kind: EvVerifyFailed, Detail: "与上一轮相同，不再重载"})
			return ErrVerifyFailed
		}
	}
	p.verifyExp, p.verifyShown = nil, nil
	r, err := p.C.Ensure(ctx, exp, p.lastApplied)
	if r.NewToken != "" {
		p.OnToken(r.NewToken)
		p.emit(Event{Kind: EvEnabled})
	}
	if r.Loaded {
		p.emit(Event{Kind: EvReloaded, WasAbsent: r.WasAbsent, Foreign: r.Foreign})
	}
	if r.WasAbsent || r.Foreign || r.NewToken != "" {
		p.repairStreak++
		if p.repairStreak >= RepeatedThreshold {
			p.emit(Event{Kind: EvRepeatedlyCleared, Detail: "规则连续 3 轮被清掉、被改或 pf 被关，可能有别的软件在改"})
			p.repairStreak = 0
		}
	} else {
		p.repairStreak = 0
	}
	switch {
	case err == nil:
		p.protected, p.softFails, p.lastApplied = true, 0, exp
	case errors.Is(err, ErrAnchorNotReferenced):
		p.protected, p.softFails = false, 0
		// Ensure 只在复核通过（或不需要加载）之后才返回这个错误，所以锚点里已经是期望的规则。
		p.lastApplied = exp
		p.emit(Event{Kind: EvNotReferenced, Detail: "别的软件替换了系统防火墙规则"})
	case errors.Is(err, ErrVerifyFailed):
		p.protected, p.softFails = false, 0
		p.verifyExp, p.verifyShown = exp, r.ShownAfterLoad
		p.emit(Event{Kind: EvVerifyFailed})
	case errors.Is(err, ErrStatusUnavailable):
		// 看不到状态，可能是偶发：连续 2 轮才判失效。
		p.softFails++
		if p.softFails >= 2 {
			p.protected = false
		}
		p.emit(Event{Kind: EvEnsureError, Detail: "读不到 pf 的状态"})
	default:
		// 已经确认坏状态（锚点不在、被改、pf 没启用），修复却失败：当轮判失效。
		p.protected, p.softFails = false, 0
		p.emit(Event{Kind: EvEnsureError, Detail: "修复失败"})
	}
	return err
}

// Run 立即跑一轮，之后每 Interval 跑一轮，直到 ctx 结束。
func (p *Patrol) Run(ctx context.Context) {
	iv := p.Interval
	if iv <= 0 {
		iv = DefaultPatrolInterval
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		_ = p.Step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
