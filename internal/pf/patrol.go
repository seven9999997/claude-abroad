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
	// KillReturnBlockStates 打开时，回国拦截那一组被重新加载（原来不在或被别人改过）之后，只清掉目的地在这几段里的状态
	// （M6-11b）：被清掉到重新加载之间连上这几段的连接靠已有的状态会一直通着。命令在 macOS 上的效果待真机验证，默认关。
	KillReturnBlockStates bool
	// ReturnBlockNets 给出回国拦截用的地址段（M6-11b：真实段或开发构建的测试段，abroadd 按选择文件给）；nil 时用真实段。
	ReturnBlockNets func() []netip.Prefix
	// OnApplied 在一轮成功应用（Ensure 无错）之后调用，参数是这轮的规则配置（M6-2b：abroadd 另存给开机时的
	// guard、写“本次开机已加载”的标记）。同样在 Patrol 持锁时调用。可为 nil。
	OnApplied func(Config)

	mu           sync.Mutex
	lastExpected []byte // 上一次成功算出的期望规则
	lastCfg      Config // 算出 lastExpected 的配置
	lastApplied  []byte // 上一次成功应用（Ensure 无错）的规则
	protected    bool
	softFails    int // 连续的“其他错误”轮数
	repairStreak int // 连续发生修复的轮数
	// 上一轮复核不一致时的期望规则和锚点原始打印：下一轮两者都没变就不再重载、清状态（设计 6.1）。
	verifyExp, verifyShown []byte
	// verifyConfirmed：同一组“期望内容加打印结果”连续两次重载都一样（M6-4b-3 补审时定：见一次就放弃会把“别人刚好在
	// 中间写了别的内容”误当成“打印格式对不上”，锚点会一直留着别人的内容）。只有确认了才不再重载。
	verifyConfirmed bool
	// paused 由 Flush 置上：暂停期间 Step 什么都不做，Protected 为 false；Apply 解除（M2-7c）。
	paused bool
	// restored 由 ApplyRestored 置上（M6-11a）：之后每轮的期望是“回国拦截”那一组（ReturnBlock），不是接管规则；
	// Protected 一直为 false（这组不是断网保护），不调用 OnApplied（开机时 guard 用的参数仍是接管的那一份），
	// 加载后不清状态。Apply、Flush 解除。rbOK 是这一模式下最近一轮是否成功。
	restored, rbOK bool
}

// ReturnBlockConfig 是回国拦截那一组的配置（地址段是编译进程序的 AnthropicNets）。
func ReturnBlockConfig() Config { return Config{ReturnBlock: AnthropicNets} }

// ApplyRestored 解除暂停、切到回国拦截模式并立即跑一轮（M6-11a）：锚点经一次加载从接管规则换成这一组（没有空窗），
// pf 没启用时启用（新 token 照样经 OnToken 交出）。
func (p *Patrol) ApplyRestored(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.restored { // 从接管模式换过来：上一模式的复核记录不适用
		p.verifyExp, p.verifyShown, p.verifyConfirmed = nil, nil, false
	}
	p.paused, p.restored, p.protected = false, true, false
	return p.step(ctx)
}

// RestoredMode 报告巡检是否处于回国拦截模式（ApplyRestored 之后、Apply 或 Flush 之前）。
func (p *Patrol) RestoredMode() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.restored && !p.paused
}

// ReturnBlockActive 报告回国拦截那一组是否在生效（处于这一模式，且最近一轮成功）。
func (p *Patrol) ReturnBlockActive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.restored && !p.paused && p.rbOK
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
			p.lastExpected, p.lastCfg = rules, c
			return rules, nil
		}
		err = rerr
	}
	p.emit(Event{Kind: EvNetError, Detail: "读不到当前网络或生成规则失败，沿用上一次的期望"})
	if p.lastExpected != nil {
		return p.lastExpected, nil
	}
	// 从来没算出过：最小配置（不放行局域网和网桥）。
	p.lastCfg = Config{UtunName: p.utun(), User: p.User}
	return Render(p.lastCfg)
}

// Apply 解除暂停并立即跑一轮（状态机设置好 utun 之后调用；pf 的应用都经同一个 Patrol，M2-7c）。
func (p *Patrol) Apply(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.restored {
		p.verifyExp, p.verifyShown, p.verifyConfirmed = nil, nil, false
	}
	p.paused, p.restored, p.rbOK = false, false, false
	return p.step(ctx)
}

// Flush 清空锚点，忘掉上一次应用的规则，并把巡检置为暂停：之后的 Step 不再把规则加载回去，直到下一次 Apply。
// 清空失败时同样保持暂停（锚点可能还在，由调用方重试）。
func (p *Patrol) Flush(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused, p.protected = true, false
	p.restored, p.rbOK = false, false
	p.lastExpected, p.lastApplied = nil, nil
	p.verifyExp, p.verifyShown, p.verifyConfirmed = nil, nil, false
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
	if p.restored {
		return p.stepRestored(ctx)
	}
	exp, err := p.expected(ctx)
	if err != nil {
		p.protected = false
		p.emit(Event{Kind: EvEnsureError, Detail: "连最小配置也生成不了"})
		return err
	}
	if p.verifyConfirmed && p.verifyShown != nil && bytes.Equal(exp, p.verifyExp) {
		if st, serr := p.C.Status(ctx); serr == nil && st.Enabled && st.Referenced && bytes.Equal(st.AnchorRules, p.verifyShown) {
			// 和上一轮复核不一致时一模一样：不 Load、不 Kill、不计修复，仍判保护失效。
			p.protected, p.softFails, p.repairStreak = false, 0, 0
			p.emit(Event{Kind: EvVerifyFailed, Detail: "与上一轮相同，不再重载"})
			return ErrVerifyFailed
		}
	}
	prevExp, prevShown := p.verifyExp, p.verifyShown
	p.verifyExp, p.verifyShown, p.verifyConfirmed = nil, nil, false
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
		if p.OnApplied != nil {
			p.OnApplied(p.lastCfg)
		}
	case errors.Is(err, ErrAnchorNotReferenced):
		p.protected, p.softFails = false, 0
		// Ensure 只在复核通过（或不需要加载）之后才返回这个错误，所以锚点里已经是期望的规则。
		p.lastApplied = exp
		p.emit(Event{Kind: EvNotReferenced, Detail: "别的软件替换了系统防火墙规则"})
	case errors.Is(err, ErrVerifyFailed):
		p.protected, p.softFails = false, 0
		p.verifyExp, p.verifyShown = exp, r.ShownAfterLoad
		// 第一次不一致：下一轮再完整重载一次；重载后打印结果和上次一模一样才认定格式对不上，之后不再重载。
		p.verifyConfirmed = prevShown != nil && bytes.Equal(prevExp, exp) && bytes.Equal(prevShown, r.ShownAfterLoad)
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

// stepRestored 是回国拦截模式的一轮（调用方持锁）：期望是固定的那一组；复核不一致、主规则集不引用锚点都只记事件。
func (p *Patrol) stepRestored(ctx context.Context) error {
	p.protected = false
	nets := AnthropicNets
	if p.ReturnBlockNets != nil {
		nets = p.ReturnBlockNets()
	}
	if len(nets) == 0 { // 不能退回成零值配置（那会生成接管规则）
		p.rbOK = false
		p.emit(Event{Kind: EvEnsureError, Detail: "回国拦截没有可用的地址段"})
		return errors.New("回国拦截没有可用的地址段")
	}
	exp, err := Render(Config{ReturnBlock: nets})
	if err != nil {
		p.rbOK = false
		p.emit(Event{Kind: EvEnsureError, Detail: "生成回国拦截规则失败"})
		return err
	}
	// 防空转（architect 2026-11-27：和接管模式同一套）：同一组“期望加打印结果”连续两次重载都一样，就不再重载。
	if p.verifyConfirmed && p.verifyShown != nil && bytes.Equal(exp, p.verifyExp) {
		if st, serr := p.C.Status(ctx); serr == nil && st.Enabled && st.Referenced && bytes.Equal(st.AnchorRules, p.verifyShown) {
			p.rbOK = false
			p.emit(Event{Kind: EvVerifyFailed, Detail: "回国拦截：与上一轮相同，不再重载"})
			return ErrVerifyFailed
		}
	}
	prevExp, prevShown := p.verifyExp, p.verifyShown
	p.verifyExp, p.verifyShown, p.verifyConfirmed = nil, nil, false
	r, err := p.C.EnsureNoKill(ctx, exp, p.lastApplied)
	if r.NewToken != "" {
		p.OnToken(r.NewToken)
		p.emit(Event{Kind: EvEnabled})
	}
	if r.Loaded {
		p.emit(Event{Kind: EvReloaded, WasAbsent: r.WasAbsent, Foreign: r.Foreign, Detail: "回国拦截"})
		if p.KillReturnBlockStates && (r.WasAbsent || r.Foreign) {
			for _, n := range nets {
				if kerr := p.C.KillStatesTo(ctx, n); kerr != nil {
					p.emit(Event{Kind: EvEnsureError, Detail: "清掉到回国拦截地址段的状态失败"})
				}
			}
		}
	}
	p.rbOK = err == nil
	switch {
	case err == nil, errors.Is(err, ErrAnchorNotReferenced):
		p.lastApplied = exp
		if err != nil {
			p.emit(Event{Kind: EvNotReferenced, Detail: "别的软件替换了系统防火墙规则（回国拦截不生效）"})
		}
	case errors.Is(err, ErrVerifyFailed):
		p.verifyExp, p.verifyShown = exp, r.ShownAfterLoad
		p.verifyConfirmed = prevShown != nil && bytes.Equal(prevExp, exp) && bytes.Equal(prevShown, r.ShownAfterLoad)
		p.emit(Event{Kind: EvVerifyFailed, Detail: "回国拦截"})
	default:
		p.emit(Event{Kind: EvEnsureError, Detail: "回国拦截没能加载"})
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
