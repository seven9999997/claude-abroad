package procinfo

import (
	"errors"
	"sort"
	"sync"
	"sync/atomic"
)

// FakeTable 是测试用的连接表：按调用次数依次返回 Seq 里的快照（用完后一直返回最后一份）。
type FakeTable struct {
	mu    sync.Mutex
	Seq   map[string][][]Entry
	Err   error
	Calls atomic.Int64
	n     map[string]int
	Hook  func(network string) // 每次读取时调用（测试并发共享用）
}

func (f *FakeTable) Snapshot(network string) ([]Entry, error) {
	f.Calls.Add(1)
	if f.Hook != nil {
		f.Hook(network)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	if f.n == nil {
		f.n = map[string]int{}
	}
	seq := f.Seq[network]
	if len(seq) == 0 {
		return nil, nil
	}
	i := min(f.n[network], len(seq)-1)
	f.n[network]++
	return seq[i], nil
}

// FakeProcs 是测试用的进程表。
type FakeProcs struct {
	mu    sync.Mutex
	Procs map[int]Proc
	Calls atomic.Int64
}

func (f *FakeProcs) Info(pid int) (Proc, error) {
	f.Calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.Procs[pid]
	if !ok {
		return Proc{}, errors.New("进程不存在")
	}
	return p, nil
}

// List 按进程号顺序列出全部进程。
func (f *FakeProcs) List() ([]Proc, error) {
	f.Calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Proc, 0, len(f.Procs))
	for _, p := range f.Procs {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out, nil
}
