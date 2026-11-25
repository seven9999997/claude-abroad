package apiclient

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 后台对不认识的用户直接断开连接：不管在哪个时机断开，客户端都必须一律给出“被拒绝”（architect 判 M6-3：
// 以前漏了一种底层错误，提示时对时错）。三个时机：接受后立刻断、读到一半断、读完请求不应答就断；各循环多次。
func TestDisconnectAlwaysRejected(t *testing.T) {
	modes := map[string]func(c net.Conn){
		"接受后立刻断": func(c net.Conn) { c.Close() },
		"读到一半断": func(c net.Conn) {
			buf := make([]byte, 8)
			_, _ = c.Read(buf)
			c.Close()
		},
		"读完请求不应答就断": func(c net.Conn) {
			_, _ = http.ReadRequest(bufio.NewReader(c))
			c.Close()
		},
	}
	for name, h := range modes {
		t.Run(name, func(t *testing.T) {
			dir, _ := os.MkdirTemp("", "ac")
			defer os.RemoveAll(dir)
			p := filepath.Join(dir, "s")
			l, err := net.Listen("unix", p)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			go func() {
				for {
					c, err := l.Accept()
					if err != nil {
						return
					}
					go h(c)
				}
			}()
			c := &Client{Socket: p, Timeout: 5 * time.Second}
			body := make([]byte, 256<<10) // 大请求体：让“还没写完就被断开”的时机也出现
			for i := 0; i < 200; i++ {
				var b any
				if i%2 == 1 {
					b = body
				}
				_, err := c.Do(context.Background(), "PUT", "/v1/rules", b)
				if !errors.Is(err, ErrRejected) {
					t.Fatalf("第 %d 次：应识别为被拒绝，得到 %T %v", i, err, err)
				}
			}
		})
	}
}
