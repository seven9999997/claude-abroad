// Package fakedns 是 fake-IP DNS（设计 4.3）。本文件是地址池：域名 ↔ 假地址的稳定映射。
//
// M2-4a 阶段只依赖标准库（有源码检查测试守着）；DNS 服务（miekg/dns）在 M2-4b。
// 不 import usernet、managedproxy、physif。
package fakedns

import (
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// 地址池的常量（设计 4.3）。
const (
	// MinKeep：映射在最近一次使用后至少保留这么久，才可能被淘汰。
	MinKeep = 30 * time.Minute
	// MaxAge：超过这么久没用（且没有连接在用）的映射在落盘循环里清掉（设计 4.3）。
	MaxAge = 30 * 24 * time.Hour
	// PruneEvery 是落盘循环里清理的默认间隔。
	PruneEvery = 24 * time.Hour
	// MaxName 是域名（规范化后）的最大长度。
	MaxName = 253
)

var (
	// ErrPoolFull 是地址池满、且没有可以淘汰的映射（都不满 MinKeep 或正在被连接使用）。
	ErrPoolFull = errors.New("fake-IP 地址池已满")
	// ErrBadName 是域名不合法。
	ErrBadName = errors.New("域名不合法")
)

// DefaultFirst、DefaultLast 是默认的可分配范围：198.18.0.0/15 里去掉 198.18.0.0/24
// （198.18.0.1 是 utun、198.18.0.2 是 DNS 等保留地址）和 198.19.255.255。
var (
	DefaultFirst = netip.MustParseAddr("198.18.1.0")
	DefaultLast  = netip.MustParseAddr("198.19.255.254")
)

type entry struct {
	name  string
	local bool // 域名命中 local_domains（m3-plan v2：本地域名也分配假地址，连接时强制直连）
	idx   uint32
	last  time.Time // 最近使用（查询、连接活动、连接释放）的墙上时间
	refs  int
	elem  *list.Element // 在 lru 里的位置；refs > 0 时为 nil（正在用的映射不参与淘汰）
}

// Pool 是 fake-IP 地址池，并发安全。
//
//   - 同一个域名尽量总是分到同一个地址：起点由域名的 sha256 决定（重启后不变），冲突顺延；
//   - 池满时淘汰“没有连接在用、最近使用距今不少于 MinKeep”里最久没用的那个，直接复用它的地址；
//     没有可淘汰的就返回 ErrPoolFull，绝不提前复用；
//   - 淘汰和找空位都是 O(1) 均摊（LRU 链表 + “下一个空位”并查集）。
type Pool struct {
	// Now 是 release、Prune 用的时钟（测试注入），默认 time.Now。
	Now func() time.Time
	// PruneInterval 是 Run 里清理旧映射的间隔，默认 PruneEvery（测试注入）。
	PruneInterval time.Duration

	first uint32 // 第一个可分配地址（IPv4 的 uint32 形式）
	size  uint32 // 可分配地址数

	mu     sync.Mutex
	byName map[string]*entry
	byIdx  []*entry
	lru    *list.List // 只含 refs == 0 的映射，表头最久没用
	next   []uint32   // 并查集：next[i] 指向 i 之后（含 i）可能空着的位置；空位置 next[i] == i
	used   uint32

	evictions, fulls uint64
	dirty            uint64 // 每次改动加一（落盘用）
	// epoch 在每次 reset（Load、LoadFile）时加一。Acquire 记下当时的 epoch，release 时 epoch 变了就什么都不做：
	// 旧 entry 已不在新表里，再放进新的 LRU 会让两个域名共用一个假地址。
	epoch uint64
	// localMatch 判定域名是否属于 local_domains（nil 表示没有本地域名）。
	localMatch func(name string) bool
}

// Mapping 是一条映射：域名和它是否是本地域名。
type Mapping struct {
	Name  string
	Local bool
}

// SetLocalMatcher 设定“本地域名”的判定函数，并按它重新标记已有的映射（名单变化时调用）。
func (p *Pool) SetLocalMatcher(fn func(name string) bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.localMatch = fn
	for _, e := range p.byName {
		if l := p.isLocalLocked(e.name); l != e.local {
			e.local = l
			p.dirty++
		}
	}
}

func (p *Pool) isLocalLocked(name string) bool { return p.localMatch != nil && p.localMatch(name) }

// IsLocalName 判定域名是否属于 local_domains（DNS 服务据此让本地域名优先于“不使用假地址”的名单）。
func (p *Pool) IsLocalName(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.isLocalLocked(name)
}

// NewPool 返回可分配范围为 [first, last]（含两端，IPv4）的地址池。
func NewPool(first, last netip.Addr) (*Pool, error) {
	if !first.Is4() || !last.Is4() || last.Less(first) {
		return nil, errors.New("地址池范围不对")
	}
	f, l := v4(first), v4(last)
	p := &Pool{first: f, size: l - f + 1}
	p.reset()
	return p, nil
}

// DefaultPool 返回默认范围（198.18.1.0–198.19.255.254）的地址池。
func DefaultPool() *Pool {
	p, _ := NewPool(DefaultFirst, DefaultLast)
	return p
}

func (p *Pool) reset() {
	p.byName = map[string]*entry{}
	p.byIdx = make([]*entry, p.size)
	p.lru = list.New()
	p.next = make([]uint32, p.size)
	for i := range p.next {
		p.next[i] = uint32(i)
	}
	p.used = 0
	p.epoch++
	p.dirty++
}

func v4(a netip.Addr) uint32 { b := a.As4(); return binary.BigEndian.Uint32(b[:]) }

func (p *Pool) addr(idx uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], p.first+idx)
	return netip.AddrFrom4(b)
}

// index 把地址换成池内下标；不在可分配范围里时 ok 为 false。
func (p *Pool) index(a netip.Addr) (uint32, bool) {
	a = a.Unmap()
	if !a.Is4() {
		return 0, false
	}
	v := v4(a)
	if v < p.first || v-p.first >= p.size {
		return 0, false
	}
	return v - p.first, true
}

// Contains 报告 a 是否在可分配范围里。
func (p *Pool) Contains(a netip.Addr) bool { _, ok := p.index(a); return ok }

// Normalize 规范化域名：小写、去掉末尾的一个点。空名、"."、超过 MaxName、含非法字符或空标签的返回 ErrBadName。
// 允许字母、数字、'-'、'_'（SRV、DMARC 这类名字），每个标签 1–63 字节。
func Normalize(name string) (string, error) {
	n := strings.ToLower(strings.TrimSuffix(name, "."))
	if n == "" || len(n) > MaxName {
		return "", ErrBadName
	}
	for _, label := range strings.Split(n, ".") {
		if label == "" || len(label) > 63 {
			return "", ErrBadName
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return "", ErrBadName
			}
		}
	}
	return n, nil
}

// start 是域名的起点下标（sha256，重启后不变）。
func (p *Pool) start(name string) uint32 {
	h := sha256.Sum256([]byte(name))
	return binary.BigEndian.Uint32(h[:4]) % p.size
}

// findFree 返回从 i 开始（循环）的第一个空位置；调用方保证还有空位。
func (p *Pool) findFree(i uint32) uint32 {
	root := i
	for p.next[root] != root {
		root = p.next[root]
	}
	for p.next[i] != root { // 路径压缩
		nx := p.next[i]
		p.next[i] = root
		i = nx
	}
	return root
}

// occupy 占用空位置 i：把它并到下一个位置。
func (p *Pool) occupy(i uint32) {
	p.next[i] = (i + 1) % p.size
	p.used++
}

// touchLocked 刷新最近使用时间；正在 LRU 里的移到表尾。比现有时间早的 now 不回拨。
func (p *Pool) touchLocked(e *entry, now time.Time) {
	if now.After(e.last) {
		e.last = now
	}
	if e.elem != nil {
		p.lru.MoveToBack(e.elem)
	}
	p.dirty++
}

// age 是映射距今多久没用；最近使用时间比 now 还晚（时钟回拨）时按 0 处理。
func age(e *entry, now time.Time) time.Duration {
	if e.last.After(now) {
		return 0
	}
	return now.Sub(e.last)
}

// Assign 返回域名对应的假地址，必要时分配。
func (p *Pool) Assign(name string, now time.Time) (netip.Addr, error) {
	n, err := Normalize(name)
	if err != nil {
		return netip.Addr{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.byName[n]; e != nil {
		p.touchLocked(e, now)
		return p.addr(e.idx), nil
	}
	if p.used < p.size {
		i := p.findFree(p.start(n))
		p.occupy(i)
		e := &entry{name: n, idx: i, last: now, local: p.isLocalLocked(n)}
		e.elem = p.lru.PushBack(e)
		p.byName[n], p.byIdx[i] = e, e
		p.dirty++
		return p.addr(i), nil
	}
	// 池满：表头是没有连接在用的映射里最久没用的那个。
	front := p.lru.Front()
	if front == nil || age(front.Value.(*entry), now) < MinKeep {
		p.fulls++
		return netip.Addr{}, ErrPoolFull
	}
	e := front.Value.(*entry)
	delete(p.byName, e.name)
	e.name, e.last, e.local = n, now, p.isLocalLocked(n)
	p.byName[n] = e
	p.lru.MoveToBack(front)
	p.evictions++
	p.dirty++
	return p.addr(e.idx), nil
}

// Lookup 只读地查地址对应的域名；不在可分配范围、没有映射时返回 false。
func (p *Pool) Lookup(a netip.Addr) (string, bool) {
	i, ok := p.index(a)
	if !ok {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.byIdx[i]; e != nil {
		return e.name, true
	}
	return "", false
}

// Acquire 在同一把锁里查到地址对应的域名并把引用数加一（连接建立时调用，避免“刚查到就被淘汰”）。
// 有引用的映射不会被淘汰。release 幂等，释放时刷新最近使用时间（用 p.Now，默认墙上时间）。
func (p *Pool) Acquire(a netip.Addr, now time.Time) (name string, release func(), ok bool) {
	m, release, ok := p.AcquireMapping(a, now)
	return m.Name, release, ok
}

// AcquireMapping 同 Acquire，另外给出映射是否带“本地域名”标记（网络栈据此设 rules.Dest.Local）。
func (p *Pool) AcquireMapping(a netip.Addr, now time.Time) (m Mapping, release func(), ok bool) {
	i, ok := p.index(a)
	if !ok {
		return Mapping{}, nil, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.byIdx[i]
	if e == nil {
		return Mapping{}, nil, false
	}
	p.touchLocked(e, now)
	e.refs++
	if e.elem != nil {
		p.lru.Remove(e.elem)
		e.elem = nil
	}
	var once sync.Once
	epoch := p.epoch
	return Mapping{Name: e.name, Local: e.local}, func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.epoch != epoch { // 之后加载过新表：这个引用属于旧表，什么都不做
				return
			}
			e.refs--
			p.touchLocked(e, p.clock())
			if e.refs == 0 {
				e.elem = p.lru.PushBack(e)
			}
		})
	}, true
}

func (p *Pool) clock() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Prune 删掉没有连接在用、且超过 maxAge 没用的映射，返回删掉的条数。删完重建“下一个空位”并查集，
// 腾出的地址能被新域名按哈希起点重新占用。有引用的映射保留，epoch 不变（旧的 release 仍然有效）。
func (p *Pool) Prune(now time.Time, maxAge time.Duration) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	removed := 0
	for el := p.lru.Front(); el != nil; {
		e := el.Value.(*entry)
		if age(e, now) <= maxAge {
			break // LRU 有序：后面的都更新
		}
		nx := el.Next()
		p.lru.Remove(el)
		e.elem = nil
		delete(p.byName, e.name)
		p.byIdx[e.idx] = nil
		p.used--
		removed++
		el = nx
	}
	if removed > 0 {
		for i := range p.next {
			if p.byIdx[i] == nil {
				p.next[i] = uint32(i)
			} else {
				p.next[i] = uint32((i + 1) % len(p.next))
			}
		}
		p.dirty++
	}
	return removed
}

// Stats 是地址池的计数（不含域名）。
type Stats struct {
	Mappings  int    `json:"mappings"`
	Evictions uint64 `json:"evictions"`
	Full      uint64 `json:"full"`
}

// Stats 返回计数快照。
func (p *Pool) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{Mappings: len(p.byName), Evictions: p.evictions, Full: p.fulls}
}

func (p *Pool) String() string {
	return fmt.Sprintf("fakedns.Pool(%s–%s)", p.addr(0), p.addr(p.size-1))
}
