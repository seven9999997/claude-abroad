package agent

import (
	"bytes"
	"claude-abroad/internal/redact"
	"strings"
	"sync"
)

const (
	// RingLogBytes 是每个引擎日志缓冲的上限（设计文档 7.1：引擎日志只放内存）。
	RingLogBytes = 256 << 10
	// MaxLogLine 是单行上限，超过的截断。
	MaxLogLine = 4 << 10
)

// RingLog 是按行存、按字节封顶的环形日志。写入时逐行脱敏；满了丢最旧的行并计数。
// 实现 io.Writer，可以直接接子进程的 stdout/stderr。并发安全。
type RingLog struct {
	mu      sync.Mutex
	max     int
	lines   []string
	size    int
	dropped int64
	partial []byte
}

// NewRingLog 返回上限为 maxBytes 的环形日志。
func NewRingLog(maxBytes int) *RingLog { return &RingLog{max: maxBytes} }

func (r *RingLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			r.partial = append(r.partial, p...)
			if len(r.partial) > MaxLogLine { // 没有换行的超长输出：先按一行收下
				r.addLocked(string(r.partial))
				r.partial = r.partial[:0]
			}
			break
		}
		r.partial = append(r.partial, p[:i]...)
		r.addLocked(string(r.partial))
		r.partial = r.partial[:0]
		p = p[i+1:]
	}
	return n, nil
}

func (r *RingLog) addLocked(line string) {
	line = strings.TrimRight(line, "\r")
	if len(line) > MaxLogLine {
		line = line[:MaxLogLine] + "…[截断]"
	}
	line = Redact(line)
	r.lines = append(r.lines, line)
	r.size += len(line) + 1
	for r.size > r.max && len(r.lines) > 0 {
		r.size -= len(r.lines[0]) + 1
		r.lines = r.lines[1:]
		r.dropped++
	}
}

// Tail 返回最后 n 行（n<=0 时全部）和累计丢弃的行数。
func (r *RingLog) Tail(n int) ([]string, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	lines := r.lines
	if n > 0 && n < len(lines) {
		lines = lines[len(lines)-n:]
	}
	return append([]string(nil), lines...), r.dropped
}

// Redact 是 redact.String 的别名（保留旧名，引擎日志写入时调用）。
func Redact(s string) string { return redact.String(s) }
