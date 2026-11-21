// Package procinfo 把一条连接识别成应用（design 5.1、m3-plan M3-3）：连接表 → 进程号 → 可执行文件路径 → 应用 ID，
// 同时判定它是不是 Claude 进程。
//
// 只用 sysctl 和只读地读文件，不执行外部命令，不打日志。识别不到一律返回 unknown，由 default 规则处理；
// 不泄漏靠的是置顶域名、Anthropic 地址段和 Claude 进程三条，不靠这里的准确率（design 5.3）。
package procinfo

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"time"

	"claude-abroad/internal/rules"
)

// Table 读一份连接表快照。network 是 "tcp" 或 "udp"。
type Table interface {
	Snapshot(network string) ([]Entry, error)
}

// Proc 是一个进程的信息。Path 是可执行文件的绝对路径；Args 读不到时为空。
// Start 是启动时间（和 PID 一起确定“还是同一个进程”，M4-4）；Name 是内核记的短名（路径读不到时也有）。
type Proc struct {
	PID, PPID int
	Path      string
	Args      []string
	Start     time.Time
	Name      string
}

// Lister 列出全部进程（M4-4）。读不到路径和参数的进程（属于别的用户而本进程不是 root）仍然列出，Path、Args 为空。
type Lister interface {
	List() ([]Proc, error)
}

// Procs 读进程信息。路径读不出来（进程已退出、属于别的用户而本进程不是 root）时返回错误。
type Procs interface {
	Info(pid int) (Proc, error)
}

// Result 是识别结果。
type Result struct {
	App    rules.AppID
	PID    int
	Path   string
	Claude bool
}

// Identifier 识别连接所属的应用。零值不可用，至少要有 Table 和 Procs。
type Identifier struct {
	Table         Table
	Procs         Procs
	Bundle        BundleReader
	InheritParent bool          // 5.1 的“继承父进程”：只改变 path: 应用的应用 ID
	Retries       int           // 查不到时的重试次数，默认 3
	RetryDelay    time.Duration // 默认 20 毫秒
	ShareFor      time.Duration // 快照在并发请求间共享的最长时间，默认 20 毫秒
	Now           func() time.Time

	mu    sync.Mutex
	snaps map[string]*snapshot
}

type snapshot struct {
	at      time.Time
	done    chan struct{}
	entries []Entry
	err     error
}

func (i *Identifier) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

func (i *Identifier) retries() (int, time.Duration) {
	n, d := i.Retries, i.RetryDelay
	if n <= 0 {
		n = 3
	}
	if d <= 0 {
		d = 20 * time.Millisecond
	}
	return n, d
}

// snapshot 取连接表。可以共享一份不超过 ShareFor、并且在 after 之后（严格晚于）才开始读的快照，
// 并发请求合并成一次读取。after 为零值时不限。重试传上一次快照的开始时间，复核传读出进程信息之后的时刻：
// 这样共享到的快照一定是“更新的”读取，等同于新读一次，只是并发时不重复读。
func (i *Identifier) snapshot(network string, after time.Time) ([]Entry, time.Time, error) {
	share := i.ShareFor
	if share <= 0 {
		share = 20 * time.Millisecond
	}
	i.mu.Lock()
	if i.snaps == nil {
		i.snaps = map[string]*snapshot{}
	}
	if s := i.snaps[network]; s != nil && i.now().Sub(s.at) <= share && (after.IsZero() || s.at.After(after)) {
		i.mu.Unlock()
		<-s.done
		return s.entries, s.at, s.err
	}
	s := &snapshot{at: i.now(), done: make(chan struct{})}
	i.snaps[network] = s
	i.mu.Unlock()
	s.entries, s.err = i.Table.Snapshot(network)
	close(s.done)
	return s.entries, s.at, s.err
}

var errNotFound = errors.New("连接表里没有这条连接")

// lookup 找流的归属进程：查不到就按 Retries、RetryDelay 重试（重试时重新读连接表）。
// start 是识别开始的时刻：被识别的连接在这之前就已存在，所以第一次只共享不早于 start 开始读的快照
// （更早的快照里不会有这条连接，用它只会白白多等一次重试）。
func (i *Identifier) lookup(ctx context.Context, network string, src, dst netip.AddrPort, start time.Time) (int, error) {
	n, delay := i.retries()
	prev := start.Add(-time.Nanosecond) // “严格晚于”它，即不早于 start
	for attempt := 0; ; attempt++ {
		after := prev // 第一次：不早于识别开始；重试：比上一次更新
		entries, at, err := i.snapshot(network, after)
		prev = at
		if err != nil {
			return 0, err
		}
		switch pid := match(entries, network, src, dst); {
		case pid > 0:
			return pid, nil
		case pid < 0:
			return 0, errors.New("匹配到多个进程")
		}
		if attempt >= n {
			return 0, errNotFound
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(delay):
		}
	}
}

// Identify 识别一条流（src 是应用一侧的地址，dst 是目的地址）。任何一步失败都返回 App=unknown。
// 识别结果不缓存：每条新连接都重新查（design 5.1）。
func (i *Identifier) Identify(ctx context.Context, network string, src, dst netip.AddrPort) Result {
	unknown := Result{App: rules.Unknown}
	pid, err := i.lookup(ctx, network, src, dst, i.now())
	if err != nil {
		return unknown
	}
	p, err := i.Procs.Info(pid)
	if err != nil || p.Path == "" {
		return unknown
	}
	// 进程号复用的复核：读出路径之后开始读的连接表里，连接仍须归这个进程号（只能缩小窗口，design 5.3）。
	entries, _, err := i.snapshot(network, i.now())
	if err != nil || match(entries, network, src, dst) != pid {
		return unknown
	}
	app := AppIDForPath(p.Path, i.Bundle)
	r := Result{App: app, PID: pid, Path: p.Path, Claude: i.claude(p, app)}
	if app == rules.Unknown {
		return Result{App: rules.Unknown, PID: pid, Path: p.Path, Claude: r.Claude}
	}
	if i.InheritParent && !r.Claude && isPathApp(app) {
		if t, ok := i.terminal(p); ok {
			r.App = t
		}
	}
	return r
}

func isPathApp(a rules.AppID) bool { return len(a) > 5 && a[:5] == "path:" }

// ancestors 沿父进程往上走，最多 MaxAncestors 级，对每一级（不含自己）调用 fn；fn 返回真时停。
// 父进程号读不出来、回到 0 或 1、出现环时停下。
func (i *Identifier) ancestors(p Proc, fn func(Proc) bool) {
	seen := map[int]bool{p.PID: true}
	ppid := p.PPID
	for depth := 1; depth <= MaxAncestors; depth++ {
		if ppid <= 1 || seen[ppid] {
			return
		}
		seen[ppid] = true
		a, err := i.Procs.Info(ppid)
		if err != nil {
			return // 读不出来：只按已经看过的判
		}
		if fn(a) {
			return
		}
		ppid = a.PPID
	}
}

func (i *Identifier) claude(p Proc, app rules.AppID) bool {
	if isClaudeProc(p, app) {
		return true
	}
	found := false
	i.ancestors(p, func(a Proc) bool {
		found = isClaudeProc(a, AppIDForPath(a.Path, i.Bundle))
		return found
	})
	return found
}

func (i *Identifier) terminal(p Proc) (rules.AppID, bool) {
	var t rules.AppID
	i.ancestors(p, func(a Proc) bool {
		id := AppIDForPath(a.Path, i.Bundle)
		if TerminalApps[id] {
			t = id
			return true
		}
		return false
	})
	return t, t != ""
}
