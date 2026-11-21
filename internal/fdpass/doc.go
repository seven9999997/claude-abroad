// Package fdpass 经 unix socket 用 SCM_RIGHTS 传递文件描述符（design 6.3）。
// 一次 Send 发一个带长度前缀的短消息，可以附带一个 fd；Recv 收一个消息和至多一个 fd。
// 任何异常（多个 fd、控制消息被截断、fd 出现在消息中间）都关掉收到的全部 fd 再报错，不泄漏。
//
// 传输层只有 unix 实现（fdpass.go、peer_*.go）；Windows 上是一律失败的占位（fdpass_windows.go，W-1）。
package fdpass

// MaxMsg 是默认的消息上限（不含 2 字节长度）；MaxLimit 是允许的最大上限（2 字节长度能表示的范围内，
// 直连拨号模块的配置请求用，m3-plan v2.8 决定 1）。
const (
	MaxMsg   = 1024
	MaxLimit = 64 << 10
)
