// ssedemo 是 M5-1 探查 E4 的事件流演示服务：在 unix socket 上提供 GET /v1/events（Server-Sent Events），
// 发 3 个事件后结束。abroadd 的事件推送在 M5-2 才做，这里只验证 Swift 一侧能逐条读 SSE（含分块传输）。
//
//	go run ./scripts/m5/probe/ssedemo -sock <路径>
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	sock := flag.String("sock", "", "unix socket 路径")
	n := flag.Int("n", 3, "事件数")
	flag.Parse()
	if *sock == "" {
		fmt.Fprintln(os.Stderr, "用法：ssedemo -sock <路径>")
		os.Exit(2)
	}
	_ = os.Remove(*sock)
	ln, err := net.Listen("unix", *sock)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = os.Chmod(*sock, 0o600)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		fl, _ := w.(http.Flusher)
		states := []string{"ENABLING", "ENABLED", "DISABLED"}
		for i := 0; i < *n; i++ {
			fmt.Fprintf(w, "id: %d\nevent: state\ndata: {\"seq\":%d,\"kind\":\"state\",\"state\":%q,\"at\":%q}\n\n", i+1, i+1, states[i%len(states)], time.Now().UTC().Format(time.RFC3339))
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(150 * time.Millisecond)
		}
	})
	fmt.Println("listening", *sock)
	srv := &http.Server{Handler: mux}
	go func() { time.Sleep(30 * time.Second); _ = srv.Close() }() // 最多活 30 秒
	_ = srv.Serve(ln)
	_ = os.Remove(*sock)
}
