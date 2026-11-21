// Package sniff 从连接的首包里识别去向（m3-plan M3-2）：TLS ClientHello 的 SNI（含 ECH 检测）、
// QUIC Initial 的 SNI、明文 HTTP 的 Host。
//
// 原则（m3-plan v2.3）：凡是说不清的，一律不能落到 NotTLS，因为只有 NotTLS 能让直连成立。
// 本包不打日志，返回的错误里不带读到的内容；除了传入的连接，不做任何网络操作。
package sniff

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"claude-abroad/internal/rules"
)

// Result 是识别结果。Name 是归一后的域名（rules.NormalizeName），只在 TLSName、QUICName、HTTPHost 时有值。
type Result struct {
	Kind rules.SniffKind
	Name string
}

const (
	// MaxPeek 是 TCP 首包最多读的字节数（分片的、带后量子密钥共享的大 ClientHello 都在这之内）。
	MaxPeek = 16 << 10
	// Timeout 是等首包的时限（design 4.2）。
	Timeout = 300 * time.Millisecond
)

// Classify 判定 TCP 已读到的字节。needMore 为真表示还不能下结论。
func Classify(b []byte) (r Result, needMore bool) {
	if len(b) == 0 {
		return Result{}, true
	}
	if MayBeTLS(b) {
		return TLS(b)
	}
	if maybeHTTP(b) {
		return HTTP(b)
	}
	return Result{Kind: rules.NotTLS}, false
}

// Final 是到时限（或对端不再发送）时按已读内容下的结论：一个字节都没有是 Timeout；
// TLS 记录头的前缀或没收完的 ClientHello 是 TLSNoName；其余（含没收完的 HTTP 头）是 NotTLS。
func Final(b []byte) Result {
	if r, more := Classify(b); !more {
		return r
	}
	switch {
	case len(b) == 0:
		return Result{Kind: rules.Timeout}
	case MayBeTLS(b):
		return Result{Kind: rules.TLSNoName}
	}
	return Result{Kind: rules.NotTLS}
}

// PeekTCP 从 conn 读首包，最多等 Timeout（或 ctx 更早的期限），能下结论就立即返回。
// 返回读到的全部字节（一个不多、一个不少），转发时要先发它们。返回前清掉自己设的读期限。
// ctx 取消时返回 ctx 的错误；对端关闭写方向（EOF）不算错误，按已读内容判定。
func PeekTCP(ctx context.Context, conn net.Conn) (Result, []byte, error) {
	deadline := time.Now().Add(Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	var mu sync.Mutex
	finished := false
	if err := conn.SetReadDeadline(deadline); err != nil {
		return Result{}, nil, errors.New("设置读期限失败")
	}
	stop := context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		if !finished {
			_ = conn.SetReadDeadline(time.Unix(1, 0)) // 让阻塞的 Read 立即返回
		}
	})
	defer func() {
		stop()
		mu.Lock()
		finished = true
		_ = conn.SetReadDeadline(time.Time{})
		mu.Unlock()
	}()

	buf := make([]byte, 0, 2048)
	chunk := make([]byte, 4096)
	for {
		n := min(len(chunk), MaxPeek-len(buf))
		if n <= 0 {
			return Final(buf), buf, nil
		}
		k, err := conn.Read(chunk[:n])
		buf = append(buf, chunk[:k]...)
		if k > 0 {
			if r, more := Classify(buf); !more {
				return r, buf, nil
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return Final(buf), buf, ctx.Err()
			}
			var ne net.Error
			if errors.Is(err, os.ErrDeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() || errors.Is(err, io.EOF) {
				return Final(buf), buf, nil
			}
			return Final(buf), buf, fmt.Errorf("读首包失败：%w", err)
		}
	}
}
