package devsocks

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"claude-abroad/internal/wire"
)

// fakeOpener 记录收到的帧头；每条流由测试控制另一端。
type fakeOpener struct {
	mu      sync.Mutex
	headers []wire.Header
	peers   chan net.Conn
}

func (f *fakeOpener) open(_ context.Context, h wire.Header) (net.Conn, error) {
	f.mu.Lock()
	f.headers = append(f.headers, h)
	f.mu.Unlock()
	a, b := net.Pipe()
	f.peers <- b
	return a, nil
}

func serve(t *testing.T, f *fakeOpener) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { (&Server{Open: f.open}).Serve(ctx, l); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return l.Addr().String()
}

func TestATYPDomainWithIPLiteral(t *testing.T) {
	f := &fakeOpener{peers: make(chan net.Conn, 4)}
	addr := serve(t, f)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write([]byte{5, 1, 0})
	io.ReadFull(c, make([]byte, 2))
	c.Write(append(append([]byte{5, 1, 0, 3, 11}, "203.0.113.5"...), 1, 187)) // ATYP=3 装 IP 字面量，端口 443
	rep2 := make([]byte, 10)
	io.ReadFull(c, rep2)
	f.mu.Lock()
	h := f.headers[0]
	f.mu.Unlock()
	if rep2[1] != 0 || h.Host != "" || h.IP != "203.0.113.5" || h.Port != 443 {
		t.Fatalf("ATYP=3 里的 IP 字面量应按 IP 处理：rep=%d %+v", rep2[1], h)
	}
}

func TestUDPStreamReopensAfterEnd(t *testing.T) {
	f := &fakeOpener{peers: make(chan net.Conn, 4)}
	addr := serve(t, f)
	relay := relayAddr(t, addr)
	defer relay.ctrl.Close()
	u, err := net.DialUDP("udp", nil, relay.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	pkt := append(socksUDPHeader("", netip.MustParseAddr("198.51.100.7"), 53), 'a')
	u.Write(pkt)
	first := <-f.peers
	dc := wire.NewDatagramConn(first)
	buf := make([]byte, 64)
	first.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := dc.ReadDatagram(buf); err != nil {
		t.Fatal(err)
	}
	first.Close() // 数据面那一端结束这条流
	time.Sleep(50 * time.Millisecond)
	u.Write(pkt)
	select {
	case second := <-f.peers:
		second.Close()
	case <-time.After(3 * time.Second):
		t.Fatal("流结束后，发往同一目标的数据报应重开一条流")
	}
	f.mu.Lock()
	n := len(f.headers)
	f.mu.Unlock()
	if n != 2 {
		t.Fatalf("应打开 2 条流，实际 %d", n)
	}
}

type assoc struct {
	ctrl net.Conn
	addr *net.UDPAddr
}

// relayAddr 发一次 UDP ASSOCIATE 并解析 BND（127.0.0.1:端口）。
func relayAddr(t *testing.T, socks string) assoc {
	c, err := net.Dial("tcp", socks)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte{5, 1, 0})
	var b [2]byte
	io.ReadFull(c, b[:])
	c.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0})
	rep := make([]byte, 10)
	io.ReadFull(c, rep)
	c.SetDeadline(time.Time{})
	if rep[1] != 0 {
		t.Fatalf("UDP ASSOCIATE 失败：%d", rep[1])
	}
	return assoc{ctrl: c, addr: &net.UDPAddr{IP: net.IP(rep[4:8]), Port: int(binary.BigEndian.Uint16(rep[8:10]))}}
}
