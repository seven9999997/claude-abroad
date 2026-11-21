package main

import (
	"io"
	"net"
	"testing"
	"time"

	"claude-abroad/internal/wire"
)

// 假的对端：读帧头，down 时回 exit_down，否则回 ok 并按标记回显。
func fakeFlowOpen(tag string, down bool) func() (io.ReadWriteCloser, error) {
	return func() (io.ReadWriteCloser, error) {
		a, b := net.Pipe()
		go func() {
			defer b.Close()
			h, err := wire.ReadHeader(b)
			if err != nil {
				return
			}
			if down {
				_ = wire.WriteReply(b, wire.Reply{Code: wire.CodeExitDown})
				return
			}
			_ = wire.WriteReply(b, wire.Reply{OK: true})
			if h.Net == wire.UDP {
				dc := wire.NewDatagramConn(b)
				buf := make([]byte, wire.MaxDatagram)
				n, err := dc.ReadDatagram(buf)
				if err == nil {
					_ = dc.WriteDatagram(append([]byte(tag+":"), buf[:n]...))
				}
				return
			}
			buf := make([]byte, 8)
			n, _ := io.ReadFull(b, buf)
			_, _ = b.Write(append([]byte(tag+":"), buf[:n]...))
			_, _ = io.Copy(b, b) // 之后的数据原样回显（--hold 时）
		}()
		return a, nil
	}
}

func TestRunFlows(t *testing.T) {
	h, _, err := buildProbe("x.sock", "wg", "tcp", "10.77.0.1:7", "", "t")
	if err != nil {
		t.Fatal(err)
	}
	if r := runFlows(fakeFlowOpen("wg", false), h, "wg", 50, 0); r.Good != 50 || len(r.Bad) != 0 {
		t.Fatalf("%+v", r)
	}
	if r := runFlows(fakeFlowOpen("ovpn", false), h, "wg", 5, 0); r.Good != 0 || len(r.Bad) != 5 {
		t.Fatalf("标记不对应算不符合：%+v", r)
	}
	if r := runFlows(fakeFlowOpen("wg", true), h, "wg", 3, 0); r.Good != 0 || r.Codes["exit_down"] != 3 {
		t.Fatalf("%+v", r)
	}
	hu, _, _ := buildProbe("x.sock", "wg", "udp", "10.77.0.1:7", "", "t")
	if r := runFlows(fakeFlowOpen("wg", false), hu, "wg", 10, 0); r.Good != 10 {
		t.Fatalf("%+v", r)
	}
}

func TestRunFlowsHold(t *testing.T) {
	h, _, _ := buildProbe("x.sock", "wg", "tcp", "10.77.0.1:7", "", "t")
	start := time.Now()
	if r := runFlows(fakeFlowOpen("wg", false), h, "wg", 3, 300*time.Millisecond); r.Good != 3 {
		t.Fatalf("%+v", r)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Fatal("--hold 应持续发数据")
	}
}
