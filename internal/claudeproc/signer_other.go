//go:build !darwin

package claudeproc

import "errors"

// CodesignSigner 在非 macOS 上不可用。
type CodesignSigner struct{}

func (CodesignSigner) TeamID(string) (string, error) { return "", errors.New("只支持 macOS") }
