//go:build !windows

package apiclient

import (
	"errors"
	"syscall"
)

func isRefused(err error) bool { return errors.Is(err, syscall.ECONNREFUSED) }
