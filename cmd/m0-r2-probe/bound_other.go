//go:build !darwin

package main

import "errors"

func bindToInterface(int, string, int) error {
	return errors.New("IP_BOUND_IF 只在 macOS 上可用")
}
