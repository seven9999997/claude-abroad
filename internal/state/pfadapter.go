package state

import (
	"context"
	"errors"
	"sync"

	"claude-abroad/internal/pf"
)

// PatrolPF 把 pf.Patrol 适配成 PF（M2-7c 决定 1、2）：
//   - 状态机和后台巡检经同一个 Patrol 应用规则，“上一次应用的规则”只有一份；
//   - Controller.BeforeEnable 接到 Store.SetEnablePending，Patrol.OnToken 接到 Store.AddToken：
//     巡检自己发现 pf 被关、重新启用时，标记和 token 同样落盘；
//   - 状态文件损坏（ErrCorrupt）时照样调用 -E（保护优先），发事件；
//   - token 追加失败时留在内存，SaveUnsaved 补存（进程崩溃就丢了，由“将要启用”标记兜底）。
type PatrolPF struct {
	P       *pf.Patrol
	Store   *Store
	OnEvent func(Event) // 可为 nil

	// ExternalTokens 取走别处（开机时的 guard，M6-2b）取得的 pf 启用引用；收养进状态文件，回国时一起释放。可为 nil。
	ExternalTokens func() ([]string, error)

	mu        sync.Mutex
	restored  bool // 处于回国拦截模式（M6-11a）：新 token 取代全部旧 token
	utun      string
	unsaved   []string
	unsaveErr error // 最近一次 token 追加失败的原因（Apply 用来判断本次是否有 token 没存下来）
}

// NewPatrolPF 组装 Controller、Patrol 和状态文件的钩子。net 可为 nil（按最小配置）。
func NewPatrolPF(c *pf.Controller, store *Store, net pf.NetSource, user string, allowLAN func() bool, onEvent func(Event)) *PatrolPF {
	a := &PatrolPF{Store: store, OnEvent: onEvent}
	if net == nil {
		net = noNet{}
	}
	c.BeforeEnable = a.beforeEnable
	a.P = &pf.Patrol{C: c, Net: net, User: user, Utun: a.utunName, AllowLAN: allowLAN, OnToken: a.onToken}
	return a
}

type noNet struct{}

func (noNet) Snapshot(context.Context) (pf.NetSnapshot, error) { return pf.NetSnapshot{}, nil }

func (a *PatrolPF) emit(kind EventKind, detail string) {
	if a.OnEvent != nil {
		a.OnEvent(Event{Kind: kind, Detail: detail})
	}
}

func (a *PatrolPF) utunName() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.utun
}

func (a *PatrolPF) beforeEnable() error {
	err := a.Store.SetEnablePending()
	if errors.Is(err, ErrCorrupt) {
		a.emit(EvTokenUnsaved, "状态文件损坏，“将要启用”标记没能落盘，照样启用 pf")
		return nil
	}
	return err
}

// onToken 在 Patrol 持锁时被调用（不能再调用 Patrol 的方法）。
func (a *PatrolPF) onToken(tok string) {
	a.mu.Lock()
	restored := a.restored
	a.mu.Unlock()
	add := a.Store.AddToken
	if restored {
		// 回国拦截模式下拿到新 token 说明 pf 被关过：旧 token 都已失效，换成这一个。
		add = a.Store.ReplaceTokens
	}
	if err := add(tok); err != nil {
		a.mu.Lock()
		a.unsaved = append(a.unsaved, tok)
		a.unsaveErr = err
		a.mu.Unlock()
		a.emit(EvTokenUnsaved, "pf 启用引用没能写进状态文件，先留在内存")
	}
}

// Apply 实现 PF。本次取得的 token 没能落盘时返回错误（“拿到 token 立刻落盘”是状态机继续下一步的前提）；
// 状态文件损坏时例外：保护优先，照样返回 Patrol 的结果。
func (a *PatrolPF) Apply(ctx context.Context, utun string) error {
	a.mu.Lock()
	a.utun, a.restored = utun, false
	a.mu.Unlock()
	return a.apply(func() error { return a.P.Apply(ctx) })
}

// ApplyRestored 实现 PF（M6-11a）。
func (a *PatrolPF) ApplyRestored(ctx context.Context) error {
	a.mu.Lock()
	a.utun, a.restored = "", true
	a.mu.Unlock()
	return a.apply(func() error { return a.P.ApplyRestored(ctx) })
}

func (a *PatrolPF) apply(run func() error) error {
	a.mu.Lock()
	before := len(a.unsaved)
	a.mu.Unlock()
	err := run()
	a.mu.Lock()
	grew, uerr := len(a.unsaved) > before, a.unsaveErr
	a.mu.Unlock()
	if grew && !errors.Is(uerr, ErrCorrupt) {
		return errors.Join(err, uerr)
	}
	return err
}

// Flush 实现 PF。
func (a *PatrolPF) Flush(ctx context.Context) error {
	a.mu.Lock()
	a.utun, a.restored = "", false
	a.mu.Unlock()
	return a.P.Flush(ctx)
}

// Release 实现 PF。
func (a *PatrolPF) Release(ctx context.Context, token string) error { return a.P.C.Release(ctx, token) }

// AdoptExternal 把 guard 取得的启用引用收养进状态文件（abroadd 启动时、回国释放之前调用）。
func (a *PatrolPF) AdoptExternal() error {
	if a.ExternalTokens == nil {
		return nil
	}
	toks, err := a.ExternalTokens()
	if err != nil {
		a.emit(EvTokenUnsaved, "读不出开机时 guard 留下的 pf 启用引用")
		return err
	}
	for _, tok := range toks {
		if err := a.Store.AddToken(tok); err != nil {
			a.mu.Lock()
			a.unsaved = append(a.unsaved, tok)
			a.unsaveErr = err
			a.mu.Unlock()
			a.emit(EvTokenUnsaved, "开机时 guard 的 pf 启用引用没能写进状态文件，先留在内存")
		}
	}
	return nil
}

// SaveUnsaved 实现 PF。
func (a *PatrolPF) SaveUnsaved() error {
	a.mu.Lock()
	toks := a.unsaved
	a.mu.Unlock()
	if len(toks) == 0 {
		return nil
	}
	if err := a.Store.Update(func(f *File) error {
		f.PFTokens = append(f.PFTokens, toks...)
		f.PFEnablePending = false
		return nil
	}); err != nil {
		return err
	}
	a.mu.Lock()
	a.unsaved = a.unsaved[len(toks):]
	a.mu.Unlock()
	return nil
}
