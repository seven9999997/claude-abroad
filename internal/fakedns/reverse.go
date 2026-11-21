package fakedns

import (
	"container/list"
	"net/netip"
	"sync"
	"time"
)

// Reverse 的常量。
const (
	MaxReverse     = 4096
	MinReverseKeep = 30 * time.Minute
)

type rentry struct {
	addr    netip.Addr
	name    string
	expires time.Time
	elem    *list.Element
}

// Reverse 记“真实地址 → 域名”（不使用假地址的名单里的域名，经主代理解析出的真实地址），
// 供规则匹配用。上限 MaxReverse 条，满了淘汰最旧写入的；保留时间取 TTL 和 MinReverseKeep 中较长的。
// 并发安全。
type Reverse struct {
	mu  sync.Mutex
	m   map[netip.Addr]*rentry
	lru *list.List // 表头最旧
	max int
}

// NewReverse 返回上限为 MaxReverse 的 Reverse。
func NewReverse() *Reverse { return newReverse(MaxReverse) }

func newReverse(max int) *Reverse {
	return &Reverse{m: map[netip.Addr]*rentry{}, lru: list.New(), max: max}
}

// Put 记一条（再次写入同一地址时更新域名和过期时间，并移到最新）。
func (r *Reverse) Put(addr netip.Addr, name string, ttl time.Duration, now time.Time) {
	addr = addr.Unmap()
	exp := now.Add(max(ttl, MinReverseKeep))
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.m[addr]; e != nil {
		e.name, e.expires = name, exp
		r.lru.MoveToBack(e.elem)
		return
	}
	e := &rentry{addr: addr, name: name, expires: exp}
	e.elem = r.lru.PushBack(e)
	r.m[addr] = e
	for r.lru.Len() > r.max {
		old := r.lru.Front().Value.(*rentry)
		r.lru.Remove(old.elem)
		delete(r.m, old.addr)
	}
}

// Lookup 返回地址对应的域名；没有或已过期时 ok 为 false（过期的顺手删掉）。
func (r *Reverse) Lookup(addr netip.Addr, now time.Time) (string, bool) {
	addr = addr.Unmap()
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.m[addr]
	if e == nil {
		return "", false
	}
	if now.After(e.expires) {
		r.lru.Remove(e.elem)
		delete(r.m, addr)
		return "", false
	}
	return e.name, true
}

// Len 返回条数。
func (r *Reverse) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.m)
}
