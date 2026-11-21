package tun

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestPipeBothWaysKeepsBoundaries(t *testing.T) {
	a, b := Pipe(1500)
	defer a.Close()
	for i, pkt := range [][]byte{{1}, {2, 3}, bytes.Repeat([]byte{4}, 1500)} {
		if _, err := a.Write(pkt); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 2000)
		n, err := b.Read(buf)
		if err != nil || !bytes.Equal(buf[:n], pkt) {
			t.Fatalf("第 %d 个包边界不对：%d %v", i, n, err)
		}
	}
	b.Write([]byte{9, 9})
	buf := make([]byte, 10)
	if n, _ := a.Read(buf); n != 2 {
		t.Fatal("反方向也应互通")
	}
	if _, err := a.Write(make([]byte, 1501)); !errors.Is(err, ErrTooBig) {
		t.Fatal("超过 MTU 应报错")
	}
	a.Write([]byte{1, 2, 3})
	if _, err := b.Read(make([]byte, 2)); !errors.Is(err, ErrShortBuffer) {
		t.Fatal("缓冲区比包小应报错，不截断")
	}
}

func TestPipeWriteCopies(t *testing.T) {
	a, b := Pipe(100)
	pkt := []byte{1, 2, 3}
	a.Write(pkt)
	pkt[0] = 9
	buf := make([]byte, 10)
	b.Read(buf)
	if buf[0] != 1 {
		t.Fatal("Write 应拷贝一份")
	}
}

func TestPipeCloseWakesBoth(t *testing.T) {
	a, b := Pipe(100)
	errs := make(chan error, 2)
	go func() { _, err := a.Read(make([]byte, 10)); errs <- err }()
	go func() { _, err := b.Read(make([]byte, 10)); errs <- err }()
	time.Sleep(20 * time.Millisecond)
	b.Close()
	for range 2 {
		select {
		case err := <-errs:
			if !errors.Is(err, ErrClosed) {
				t.Fatalf("应返回 ErrClosed：%v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Close 应唤醒两端阻塞的 Read")
		}
	}
	if _, err := a.Write([]byte{1}); !errors.Is(err, ErrClosed) {
		t.Fatal("关闭后 Write 应返回 ErrClosed")
	}
	if a.Close() != nil || b.Close() != nil {
		t.Fatal("Close 应幂等")
	}
	// 队列满时阻塞的 Write 也会被唤醒。
	c, d := Pipe(100)
	for range PipeQueue {
		c.Write([]byte{1})
	}
	done := make(chan error, 1)
	go func() { _, err := c.Write([]byte{1}); done <- err }()
	time.Sleep(20 * time.Millisecond)
	d.Close()
	if err := <-done; !errors.Is(err, ErrClosed) {
		t.Fatalf("阻塞的 Write 应被唤醒：%v", err)
	}
}

func TestPipeConcurrent(t *testing.T) {
	a, b := Pipe(100)
	defer a.Close()
	const n = 2000
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range n / 4 {
				a.Write([]byte(fmt.Sprintf("%d-%d", w, i)))
			}
		}()
	}
	got := 0
	buf := make([]byte, 100)
	for got < n {
		if _, err := b.Read(buf); err != nil {
			t.Fatal(err)
		}
		got++
	}
	wg.Wait()
}
