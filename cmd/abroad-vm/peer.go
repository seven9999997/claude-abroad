package main

import (
	"net"
	"os"

	"claude-abroad/internal/fdpass"
)

// peerListener 只接受 abroad-vm 自己的用户和 root 的连接（M4-8，m4-plan v2.12 决定 2：host.sock 能改出站白名单）。
// 取不到对端身份时一律断开（Windows 上 fdpass.PeerUID 一律失败，所以一律断开）。
type peerListener struct {
	net.Listener
	peerUID  func(*net.UnixConn) (uint32, error) // nil 用 fdpass.PeerUID（测试注入）
	self     uint32
	onReject func() // 测试用
}

func newPeerListener(l net.Listener) *peerListener {
	return &peerListener{Listener: l, self: uint32(os.Getuid())}
}

func (p *peerListener) Accept() (net.Conn, error) {
	for {
		c, err := p.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uc, ok := c.(*net.UnixConn)
		if !ok {
			c.Close()
			p.rejected()
			continue
		}
		get := p.peerUID
		if get == nil {
			get = fdpass.PeerUID
		}
		uid, err := get(uc)
		if err != nil || (uid != p.self && uid != 0) {
			c.Close()
			p.rejected()
			continue
		}
		return c, nil
	}
}

func (p *peerListener) rejected() {
	if p.onReject != nil {
		p.onReject()
	}
}
