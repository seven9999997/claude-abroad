// Package claudedomains 记录 Claude 进程实际访问的、不在内置置顶后缀之下的域名（M6-12，m6-plan v2.8）。
//
// 只观察，不自动改名单：是不是 Anthropic 自有由人看过证据后改源码追加（程序不联网去查）。程序只给线索：域名、次数、
// 首次和最近一次出现的时间。条数有上限，满了淘汰最久没出现的。存在状态目录里只有 root 可读的文件（0600）；
// 不记路径、请求内容、地址。Claude Code 会替用户抓取任意网页，所以清单里会有用户访问过的网站：诊断包默认只放条数。
package claudedomains

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// FileName 是记录文件名（状态目录里）。
const FileName = "claude-domains.json"

// DefaultMax 是默认的条数上限。
const DefaultMax = 200

// maxNameLen 是域名的长度上限（DNS 名字最长 253）。
const maxNameLen = 253

// Entry 是一个域名的记录。
type Entry struct {
	Domain string    `json:"domain"`
	Count  int64     `json:"count"`
	First  time.Time `json:"first"`
	Last   time.Time `json:"last"`
}

type fileFormat struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

// Store 是记录（并发安全）。Dir 为空时只在内存里记。
type Store struct {
	Dir string
	Max int
	Now func() time.Time

	mu      sync.Mutex
	m       map[string]*Entry
	dirty   bool
	loaded  bool
	saveErr error
}

func (s *Store) max() int {
	if s.Max <= 0 {
		return DefaultMax
	}
	return s.Max
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Store) path() string { return filepath.Join(s.Dir, FileName) }

// Normalize 把域名规范成小写、去掉末尾的点；不像域名的（空、太长、带空白或斜杠、是 IP）返回空。
func Normalize(name string) string {
	n := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	if n == "" || len(n) > maxNameLen || !strings.Contains(n, ".") {
		return ""
	}
	for _, r := range n {
		ok := r == '-' || r == '.' || r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if !ok {
			return ""
		}
	}
	if isNumeric(n) {
		return "" // IPv4 字面量
	}
	return n
}

func isNumeric(n string) bool {
	for _, r := range n {
		if r != '.' && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func (s *Store) loadLocked() {
	if s.loaded {
		return
	}
	s.loaded = true
	s.m = map[string]*Entry{}
	if s.Dir == "" {
		return
	}
	b, err := os.ReadFile(s.path())
	if err != nil {
		return
	}
	var f fileFormat
	if json.Unmarshal(b, &f) != nil || f.Version != 1 {
		return // 读不出来：从空开始（只是线索，不影响任何判定）
	}
	for _, e := range f.Entries {
		if d := Normalize(e.Domain); d != "" {
			e := e
			e.Domain = d
			s.m[d] = &e
		}
	}
	s.evictLocked()
}

// Record 记一次访问。返回这个域名是不是第一次出现（调用方据此决定要不要马上落盘）。
func (s *Store) Record(name string) bool {
	d := Normalize(name)
	if d == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	now := s.now()
	e, ok := s.m[d]
	if !ok {
		e = &Entry{Domain: d, First: now}
		s.m[d] = e
	}
	e.Count++
	e.Last = now
	s.dirty = true
	s.evictLocked()
	return !ok
}

// evictLocked：超过上限时淘汰最久没出现的。
func (s *Store) evictLocked() {
	for len(s.m) > s.max() {
		var oldest *Entry
		for _, e := range s.m {
			if oldest == nil || e.Last.Before(oldest.Last) || (e.Last.Equal(oldest.Last) && e.Domain < oldest.Domain) {
				oldest = e
			}
		}
		delete(s.m, oldest.Domain)
	}
}

// List 返回全部记录（最近出现的在前）。
func (s *Store) List() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	out := make([]Entry, 0, len(s.m))
	for _, e := range s.m {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Last.Equal(out[j].Last) {
			return out[i].Last.After(out[j].Last)
		}
		return out[i].Domain < out[j].Domain
	})
	return out
}

// Count 是记录的条数（诊断包默认只放它）。
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	return len(s.m)
}

// Clear 清空（内存和文件）。
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loaded, s.m, s.dirty = true, map[string]*Entry{}, false
	if s.Dir == "" {
		return nil
	}
	if err := os.Remove(s.path()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Flush 有改动时写盘（原子写，0600）。没有改动时什么都不做。
func (s *Store) Flush() error {
	s.mu.Lock()
	if !s.dirty || s.Dir == "" {
		s.mu.Unlock()
		return nil
	}
	f := fileFormat{Version: 1}
	for _, e := range s.m {
		f.Entries = append(f.Entries, *e)
	}
	s.dirty = false
	s.mu.Unlock()
	sort.Slice(f.Entries, func(i, j int) bool { return f.Entries[i].Domain < f.Entries[j].Domain })
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, ".claude-domains-*")
	if err != nil {
		s.markDirty()
		return err
	}
	name := tmp.Name()
	if err := tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(append(b, '\n'))
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, s.path())
	}
	if err != nil {
		os.Remove(name)
		s.markDirty()
	}
	return err
}

func (s *Store) markDirty() { s.mu.Lock(); s.dirty = true; s.mu.Unlock() }
