//go:build !darwin

package procinfo

import "errors"

var errUnsupported = errors.New("只支持 macOS")

// SysTable 在非 macOS 上不可用（让交叉编译通过）。
type SysTable struct{}

func (SysTable) Snapshot(string) ([]Entry, error) { return nil, errUnsupported }

// SysProcs 在非 macOS 上不可用。
type SysProcs struct{}

func (SysProcs) Info(int) (Proc, error) { return Proc{}, errUnsupported }

// List 在非 macOS 上不可用。
func (SysProcs) List() ([]Proc, error) { return nil, errUnsupported }

// Env 在非 macOS 上不可用。
func (SysProcs) Env(int) (EnvInfo, error) { return EnvInfo{}, errUnsupported }
