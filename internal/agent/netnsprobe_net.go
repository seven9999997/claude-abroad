//go:build abroaddev

package agent

import (
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// 探查的网络动作（在调用方已经进入的命名空间里执行；Linux 的 InNetns 负责切换）。

// probeNet 做一次探测，见 probeOps.Probe。
func probeNet(kind, addr string) error {
	switch kind {
	case "tcp-echo":
		c, err := net.DialTimeout("tcp4", addr, 2*time.Second)
		if err != nil {
			return err
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Write([]byte("ping")); err != nil {
			return err
		}
		b := make([]byte, 4)
		if _, err := io.ReadFull(c, b); err != nil {
			return err
		}
		if string(b) != "ping" {
			return errors.New("回显不对")
		}
		return nil
	case "udp-echo":
		c, err := net.Dial("udp4", addr)
		if err != nil {
			return err
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(1500 * time.Millisecond))
		if _, err := c.Write([]byte("ping")); err != nil {
			return err
		}
		b := make([]byte, 16)
		n, err := c.Read(b)
		if err != nil {
			return err
		}
		if string(b[:n]) != "ping" {
			return errors.New("应答不对")
		}
		return nil
	case "tcp-connect":
		c, err := net.DialTimeout("tcp4", addr, 3*time.Second)
		if err != nil {
			return err
		}
		return c.Close()
	case "udp-send":
		c, err := net.Dial("udp4", addr)
		if err != nil {
			return err
		}
		defer c.Close()
		_, err = c.Write([]byte("probe"))
		return err
	}
	return fmt.Errorf("不认识的探测 %q", kind)
}

// serveEchoHere 在当前命名空间里起回显服务（TCP 7、UDP 53）。
func serveEchoHere() (func(), error) {
	ln, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", echoPort))
	if err != nil {
		return nil, err
	}
	pc, err := net.ListenPacket("udp4", "0.0.0.0:53")
	if err != nil {
		ln.Close()
		return nil, err
	}
	go serveTCPEcho(ln)
	go serveUDPEcho(pc)
	return func() { ln.Close(); pc.Close() }, nil
}

func serveTCPEcho(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() { _, _ = io.Copy(c, c); c.Close() }()
	}
}

func serveUDPEcho(pc net.PacketConn) {
	b := make([]byte, 2048)
	for {
		n, a, err := pc.ReadFrom(b)
		if err != nil {
			return
		}
		_, _ = pc.WriteTo(b[:n], a)
	}
}
