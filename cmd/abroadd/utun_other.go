//go:build !darwin

package main

import (
	"errors"

	"claude-abroad/internal/tun"
)

func openUtun() (tun.Device, error) { return nil, errors.New("utun 只在 macOS 上可用") }
