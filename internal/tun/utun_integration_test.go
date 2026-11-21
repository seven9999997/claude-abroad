//go:build integration && darwin

package tun

// 真实 utun 的测试：需要 root（sudo go test -tags integration -run Utun ./internal/tun/）。
// 只创建一个 utun，不配地址、不配路由，不影响网络。会话里不跑，由用户执行。

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestUtunOpenAndName(t *testing.T) {
	d, err := OpenUtun(-1, 1500)
	if err != nil {
		t.Fatalf("打开 utun（需要 root）：%v", err)
	}
	defer d.Close()
	if !strings.HasPrefix(d.Name(), "utun") || d.MTU() != 1500 {
		t.Fatalf("名称或 MTU 不对：%s %d", d.Name(), d.MTU())
	}
	t.Logf("打开了 %s", d.Name())
}

func TestUtunCloseWakesBlockedRead(t *testing.T) {
	d, err := OpenUtun(-1, 1500)
	if err != nil {
		t.Fatalf("打开 utun（需要 root）：%v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := d.Read(make([]byte, 2000))
		done <- err
	}()
	time.Sleep(100 * time.Millisecond) // 让 Read 阻塞住（没有地址和路由，不会有包）
	start := time.Now()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("Read 应返回 ErrClosed：%v", err)
		}
		t.Logf("Close 后 %s 唤醒了 Read", time.Since(start))
	case <-time.After(time.Second):
		t.Fatal("Close 后 1 秒内 Read 没有返回")
	}
	if err := d.Close(); err != nil {
		t.Fatalf("重复 Close 应幂等：%v", err)
	}
	if _, err := d.Write([]byte{0x45, 0, 0, 20}); !errors.Is(err, ErrClosed) {
		t.Fatalf("关闭后 Write 应返回 ErrClosed：%v", err)
	}
}

// 读缓冲比 4+MTU 多 1 字节，用来识别被截断的超长包（M2-5b 第 0 步）。
// 真实的超长包要在配了地址和路由之后才能造出来（M2-9 的脚本），这里只核对缓冲的大小。
func TestUtunReadBufferDetectsTruncation(t *testing.T) {
	d, err := OpenUtun(-1, 1280)
	if err != nil {
		t.Fatalf("打开 utun（需要 root）：%v", err)
	}
	defer d.Close()
	if got := len(d.(*utun).rbuf); got != utunHeaderLength+1280+1 {
		t.Fatalf("读缓冲应为 4+MTU+1：%d", got)
	}
}
