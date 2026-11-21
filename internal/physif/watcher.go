package physif

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Watcher 的常量。
const (
	// MaxStale：值的年龄超过它就不再直接用，必须同步查到新值（查询失败也一样，失败即断）。
	MaxStale = 30 * time.Second
	// FailCacheFor：查询失败的结果缓存这么久（没有可用旧值时直接返回错误，不每次都起子进程）。
	FailCacheFor = time.Second
)

// Watcher 缓存物理网卡查询结果，语义写死如下（M2-2，architect 2026-11-18）：
//   - 年龄 < Every：直接返回，不刷新；
//   - Every ≤ 年龄 ≤ MaxStale：立即返回旧值，同时在后台刷新（同一时刻只跑一个）；
//   - 年龄 > MaxStale、从没查到过、或刚 Invalidate：同步查，并发调用合并成一次；
//   - 查询失败：旧值在 MaxStale 内继续用，超过就返回错误；没有可用旧值时，失败结果缓存 FailCacheFor；
//     绝不返回空名（调用方不会退回“不绑定”）；
//   - 查询一律用自带超时的 context（QueryTimeout），不跟调用方的 ctx 走；调用方的 ctx 只控制等多久。
//
// 查到的名字是 utun、lo、bridge、vmenet 开头的，一律当查询失败。
type Watcher struct {
	query func(context.Context) (string, error)
	every time.Duration
	// Now 可注入（测试用），默认 time.Now。
	Now func() time.Time

	mu       sync.Mutex
	name     string
	at       time.Time // 最近一次成功的时间
	have     bool
	invalid  bool // Invalidate 之后，旧值不再可用，直到下一次查询成功
	err      error
	errAt    time.Time
	inflight chan struct{}
	// gen 在每次 Invalidate 时加一；查询记下起查时的 gen，结束时 gen 变了就丢弃结果
	// （作废之前起的查询可能返回刚失效的网卡名，不能把作废冲掉）。
	gen uint64
}

// NewWatcher 返回一个 Watcher。every 是值的“新鲜期”（abroad-vm 用 2 秒）。
func NewWatcher(q func(context.Context) (string, error), every time.Duration) *Watcher {
	return &Watcher{query: q, every: every}
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// Invalidate 让旧值作废，下一次 Current 同步查询（绑定网卡失败时调用：网卡可能刚变了）。
func (w *Watcher) Invalidate() {
	w.mu.Lock()
	w.invalid, w.err = true, nil
	w.gen++
	w.mu.Unlock()
}

// Last 返回最近一次查到的网卡名（没查到过或已作废时为空），只用于诊断。网卡名不算敏感信息。
func (w *Watcher) Last() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.have || w.invalid {
		return ""
	}
	return w.name
}

// startLocked 起一次查询（调用方持锁，且 w.inflight 为 nil）。
func (w *Watcher) startLocked() chan struct{} {
	done := make(chan struct{})
	w.inflight = done
	g := w.gen
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), QueryTimeout)
		defer cancel()
		name, err := w.query(ctx)
		if err == nil {
			err = checkName(name)
		}
		w.mu.Lock()
		switch {
		case w.gen != g: // 起查之后被作废过：结果作废，下一次 Current 重新查
		case err == nil:
			w.name, w.at, w.have, w.invalid, w.err = name, w.now(), true, false, nil
		default:
			w.err, w.errAt = err, w.now()
		}
		w.inflight = nil
		w.mu.Unlock()
		close(done)
	}()
	return done
}

// Current 按上面的语义返回当前物理网卡名。
func (w *Watcher) Current(ctx context.Context) (string, error) {
	if w.query == nil {
		return "", errors.New("没有配置物理网卡查询")
	}
	for {
		w.mu.Lock()
		now := w.now()
		if w.have && !w.invalid {
			age := now.Sub(w.at)
			if age < w.every {
				name := w.name
				w.mu.Unlock()
				return name, nil
			}
			if age <= MaxStale {
				// 后台刷新；上次失败不到 FailCacheFor 时先不重试。
				if w.inflight == nil && !(w.err != nil && now.Sub(w.errAt) < FailCacheFor) {
					w.startLocked()
				}
				name := w.name
				w.mu.Unlock()
				return name, nil
			}
		}
		// 同步路径：没有可用旧值。
		if w.inflight == nil && w.err != nil && now.Sub(w.errAt) < FailCacheFor {
			err := w.err
			w.mu.Unlock()
			return "", err
		}
		done := w.inflight
		if done == nil {
			done = w.startLocked()
		}
		w.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		// 查询结束后重新判断：成功就走上面的新鲜路径；失败就在下一轮返回缓存的错误。
		w.mu.Lock()
		failed := w.err != nil && (!w.have || w.invalid || w.now().Sub(w.at) > MaxStale)
		err := w.err
		w.mu.Unlock()
		if failed {
			return "", err
		}
	}
}
