package daemon

import (
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
