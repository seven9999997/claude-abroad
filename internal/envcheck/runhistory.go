package envcheck

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"claude-abroad/internal/exitcheck"
)

// 环境检测的历史（补齐四，architect 2026-11-27，设计 8.1）：每一轮的时间、硬性项的总判定、每一项的标识和结果——不记每一项的
// 详细说明（里面可能带着当时环境的具体内容）。结果和上一条完全相同时不新增，只更新“最近一次看到”的时间；有上限，超出淘汰最旧的。
// 存在状态目录里只有 root 可读写的文件（原子写）；读不出时从空开始，不影响检测本身。时间一律 UTC。

const (
	RunHistoryFileName = "envcheck-history.json"
	RunHistoryMax      = 200
)

// RunItem 是一项的结果（只有标识和结果）。
type RunItem struct {
	ID     string           `json:"id"`
	Result exitcheck.Result `json:"result"`
}

// RunEntry 是一条历史：结果相同的连续几轮合成一条（At 是第一次、LastSeen 是最近一次）。
type RunEntry struct {
	At       time.Time        `json:"at"`
	LastSeen time.Time        `json:"last_seen"`
	Hard     exitcheck.Result `json:"hard"`
	Items    []RunItem        `json:"items"`
}

func (e RunEntry) same(o RunEntry) bool {
	if e.Hard != o.Hard || len(e.Items) != len(o.Items) {
		return false
	}
	for i := range e.Items {
		if e.Items[i] != o.Items[i] {
			return false
		}
	}
	return true
}

// RunHistory 是环境检测的历史，并发安全。
type RunHistory struct {
	Dir string
	Max int // 0 用 RunHistoryMax

	mu sync.Mutex
}

type runHistFile struct {
	V       int        `json:"v"`
	Entries []RunEntry `json:"entries"`
}

func (h *RunHistory) path() string { return filepath.Join(h.Dir, RunHistoryFileName) }

func (h *RunHistory) load() []RunEntry {
	b, err := readPrivate(h.path(), 4<<20)
	if err != nil {
		return nil
	}
	var f runHistFile
	if json.Unmarshal(b, &f) != nil || f.V != 1 {
		return nil
	}
	return f.Entries
}

func (h *RunHistory) save(es []RunEntry) error {
	b, err := json.Marshal(runHistFile{V: 1, Entries: es})
	if err != nil {
		return err
	}
	return writePrivate(h.path(), b)
}

// Record 记一轮。返回写盘的错误（调用方只记事件，不影响检测结果）。
func (h *RunHistory) Record(rep Report) error {
	if h == nil {
		return nil
	}
	e := RunEntry{At: rep.At.UTC(), LastSeen: rep.At.UTC(), Hard: rep.Hard, Items: []RunItem{}}
	for _, it := range rep.Items {
		e.Items = append(e.Items, RunItem{ID: it.ID, Result: it.Result})
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	es := h.load()
	if n := len(es); n > 0 && es[n-1].same(e) {
		es[n-1].LastSeen = e.LastSeen
	} else {
		es = append(es, e)
	}
	max := h.Max
	if max <= 0 {
		max = RunHistoryMax
	}
	if len(es) > max {
		es = es[len(es)-max:]
	}
	return h.save(es)
}

// List 按时间倒序返回全部条目（最新的在前）。
func (h *RunHistory) List() []RunEntry {
	if h == nil {
		return []RunEntry{}
	}
	h.mu.Lock()
	es := h.load()
	h.mu.Unlock()
	out := make([]RunEntry, 0, len(es))
	for i := len(es) - 1; i >= 0; i-- {
		out = append(out, es[i])
	}
	return out
}

// Clear 清空（删掉文件；不存在时什么都不做）。
func (h *RunHistory) Clear() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := os.Remove(h.path()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
