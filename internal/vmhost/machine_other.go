//go:build !darwin || !cgo

package vmhost

// NewVZ 在没有 Virtualization.framework 的平台上不可用。
func NewVZ(Spec) (Machine, error) { return nil, ErrNotSupported }
