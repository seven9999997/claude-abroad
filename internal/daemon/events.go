package daemon

import (
	"errors"
	"log"
	"sync"
	"time"

	"claude-abroad/internal/redact"
)

// EventRecord 是一条事件（只有种类、时间和不敏感的细节）。
type EventRecord struct {
	Source string    `json:"source"` // state、pf、supervise
	Kind   string    `json:"kind"`
	Time   time.Time `json:"time"`
	Detail string    `json:"detail,omitempty"`
}

// EventCap 是环形缓冲的容量。
const EventCap = 50

// Events 是事件的环形缓冲，同时写日志（经 redact）。
type Events struct {
	Log *log.Logger // 可为 nil
	Now func() time.Time

	mu  sync.Mutex
	buf []EventRecord
	hub eventHub
}

// Add 记一条事件。
func (e *Events) Add(source, kind, detail string) {
	now := time.Now
	if e.Now != nil {
		now = e.Now
	}
	detail = redact.String(detail)
	r := EventRecord{Source: source, Kind: kind, Time: now().Truncate(time.Second), Detail: detail}
	e.mu.Lock()
	e.buf = append(e.buf, r)
	if len(e.buf) > EventCap {
		e.buf = e.buf[len(e.buf)-EventCap:]
	}
	e.mu.Unlock()
	e.publish(r)
	if e.Log != nil {
		e.Log.Printf("事件 %s/%s %s", source, kind, detail)
	}
}

// List 返回最近的事件（旧的在前）。
func (e *Events) List() []EventRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]EventRecord(nil), e.buf...)
}

// ---- 事件推送（m5-plan v2.1 决定 6）----

// StreamEvent 是事件推送里的一条（Server-Sent Events 的 data）。内容沿用 EventRecord 的脱敏，
// 不含确认码、凭证、请求体（事件本来就只记种类和不敏感的细节）。Dropped 为真表示之前有事件因为客户端太慢被丢掉了。
type StreamEvent struct {
	Seq     uint64    `json:"seq"`
	Source  string    `json:"source"`
	Kind    string    `json:"kind"`
	Time    time.Time `json:"time"`
	Detail  string    `json:"detail,omitempty"`
	Dropped bool      `json:"dropped,omitempty"`
}

// 事件推送的上限。
const (
	StreamBuffer     = 64 // 每个订阅的缓冲
	MaxSubscriptions = 8  // 同时存在的订阅数
)

// ErrTooManySubs 是订阅数到了上限。
var ErrTooManySubs = errors.New("事件订阅数已到上限")

type subscriber struct {
	ch      chan StreamEvent
	dropped bool
}

type eventHub struct {
	mu   sync.Mutex
	seq  uint64
	subs map[*subscriber]bool
}

// Subscribe 订阅事件；返回的 cancel 必须调用。订阅数到上限时返回 ErrTooManySubs。
func (e *Events) Subscribe() (<-chan StreamEvent, func(), error) {
	e.hub.mu.Lock()
	defer e.hub.mu.Unlock()
	if e.hub.subs == nil {
		e.hub.subs = map[*subscriber]bool{}
	}
	if len(e.hub.subs) >= MaxSubscriptions {
		return nil, nil, ErrTooManySubs
	}
	s := &subscriber{ch: make(chan StreamEvent, StreamBuffer)}
	e.hub.subs[s] = true
	var once sync.Once
	return s.ch, func() {
		once.Do(func() {
			e.hub.mu.Lock()
			delete(e.hub.subs, s)
			e.hub.mu.Unlock()
		})
	}, nil
}

// Subscribers 是当前的订阅数（测试用）。
func (e *Events) Subscribers() int {
	e.hub.mu.Lock()
	defer e.hub.mu.Unlock()
	return len(e.hub.subs)
}

// publish 发给所有订阅：不阻塞，缓冲满了就丢掉，并在下一条成功送出的事件上标 Dropped。
func (e *Events) publish(r EventRecord) {
	e.hub.mu.Lock()
	defer e.hub.mu.Unlock()
	e.hub.seq++
	ev := StreamEvent{Seq: e.hub.seq, Source: r.Source, Kind: r.Kind, Time: r.Time, Detail: r.Detail}
	for s := range e.hub.subs {
		x := ev
		x.Dropped = s.dropped
		select {
		case s.ch <- x:
			s.dropped = false
		default:
			s.dropped = true
		}
	}
}
