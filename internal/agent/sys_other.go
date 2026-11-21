//go:build !linux

package agent

import (
	"context"
	"net"
	"os"
	"syscall"
	"time"
)

// NewSystem 在非 Linux 平台上返回一个全部报错的实现，只为让 macOS 能编译和测试可移植部分。
func NewSystem() System { return unsupported{} }

type unsupported struct{}

func (unsupported) Mount(string, string, string, uintptr, string) error { return ErrUnsupported }
func (unsupported) MoveMount(string, string) error                      { return ErrUnsupported }
func (unsupported) LoadModule([]byte) error                             { return ErrUnsupported }
func (unsupported) KernelRelease() (string, error)                      { return "", ErrUnsupported }
func (unsupported) SwitchRoot(string, []string) error                   { return ErrUnsupported }
func (unsupported) Sync()                                               {}
func (unsupported) PowerOff()                                           {}
func (unsupported) DisableCtrlAltDel() error                            { return ErrUnsupported }
func (unsupported) Spawn([]string) (int, error)                         { return 0, ErrUnsupported }
func (unsupported) WaitAny() (WaitResult, error)                        { return WaitResult{}, ErrUnsupported }
func (unsupported) Kill(int, syscall.Signal) error                      { return ErrUnsupported }
func (unsupported) ListenVsock(uint32) (net.Listener, error)            { return nil, ErrUnsupported }
func (unsupported) SetTime(time.Time) error                             { return ErrUnsupported }
func (unsupported) LoopbackUp(context.Context) error                    { return ErrUnsupported }
func (unsupported) ConfigureNet(context.Context, NetConfig) error       { return ErrUnsupported }

// ShutdownSignals 是 PID 1 当作停机请求的信号。
var ShutdownSignals = []os.Signal{syscall.SIGTERM, syscall.SIGINT}
