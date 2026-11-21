package tun

import "sync"

// PipeQueue 是 Pipe 每个方向的队列长度（包数）。
const PipeQueue = 256

type pipeShared struct {
	once   sync.Once
	closed chan struct{}
}

type pipeEnd struct {
	name string
	mtu  int
	in   chan []byte // 本端读
	out  chan []byte // 本端写（对端的 in）
	sh   *pipeShared
}

// Pipe 返回一对互通的内存设备（测试用）：a 写的包 b 读到，反之亦然。
//   - 包边界保持，Write 会拷贝一份；每个方向的队列满时 Write 阻塞；
//   - 超过 MTU 的包 Write 返回 ErrTooBig；
//   - 任一端 Close 后，两端的 Read、Write 都返回 ErrClosed（包括正在阻塞的）。
func Pipe(mtu int) (a, b Device) {
	ab, ba := make(chan []byte, PipeQueue), make(chan []byte, PipeQueue)
	sh := &pipeShared{closed: make(chan struct{})}
	return &pipeEnd{name: "pipe-a", mtu: mtu, in: ba, out: ab, sh: sh},
		&pipeEnd{name: "pipe-b", mtu: mtu, in: ab, out: ba, sh: sh}
}

func (p *pipeEnd) Read(b []byte) (int, error) {
	select {
	case <-p.sh.closed:
		return 0, ErrClosed
	default:
	}
	select {
	case pkt := <-p.in:
		if len(pkt) > len(b) {
			return 0, ErrShortBuffer
		}
		return copy(b, pkt), nil
	case <-p.sh.closed:
		return 0, ErrClosed
	}
}

func (p *pipeEnd) Write(b []byte) (int, error) {
	if len(b) > p.mtu {
		return 0, ErrTooBig
	}
	select {
	case <-p.sh.closed:
		return 0, ErrClosed
	default:
	}
	pkt := append([]byte(nil), b...)
	select {
	case p.out <- pkt:
		return len(b), nil
	case <-p.sh.closed:
		return 0, ErrClosed
	}
}

func (p *pipeEnd) MTU() int     { return p.mtu }
func (p *pipeEnd) Name() string { return p.name }

func (p *pipeEnd) Close() error {
	p.sh.once.Do(func() { close(p.sh.closed) })
	return nil
}
