package resolverlock

import "claude-abroad/internal/platform"

// ErrUnsupported：Windows 上的对应做法（NRPT）另做（Windows 版暂停）。真实模式在 Windows 上本来就不运行，
// 这里只给出明确的“本平台尚未支持”，不假装成功。
var ErrUnsupported = platform.Unsupported("Claude 域名解析锁")
