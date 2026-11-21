//go:build !unix

package claudeproc

import "claude-abroad/internal/platform"

// NewRealSignaler 在非 unix 上不可用（W-1：统一返回“本平台尚未支持”，不能拿到一个假装能发信号的实现）。
func NewRealSignaler() (Signaler, error) { return nil, platform.Unsupported("给 Claude 进程发信号") }
