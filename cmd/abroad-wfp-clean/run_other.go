//go:build !windows

package main

import (
	"fmt"
	"io"
)

func run(_ options, _, stderr io.Writer) int {
	fmt.Fprintln(stderr, errNotWindows)
	return 3
}
