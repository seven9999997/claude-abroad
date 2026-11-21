//go:build integration && darwin

package main

import (
	"net"
	"testing"
	"time"
)

// IP_BOUND_IF 绑 lo0 连本机监听端口：能通说明绑定生效且不需要 root。
func TestBoundDialLoopback(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
		}
	}()
	ctrl, err := boundControl("lo0")
	if err != nil {
		t.Fatal(err)
	}
	if r := dial("tcp4", ln.Addr().String(), ctrl, "-", 2*time.Second); !r.OK {
		t.Fatal(r)
	}
}
