package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// handleEvents 是 GET /v1/events（Server-Sent Events，只读级别）：先发一行注释表示连上了，之后每条事件一个
// “id/event/data” 块。订阅数到上限时 503。客户端断开时退订、协程退出。
func (d *Daemon) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errBody{"这个连接不支持事件推送"})
		return
	}
	ch, cancel, err := d.events.Subscribe()
	if err != nil {
		if errors.Is(err, ErrTooManySubs) {
			writeJSON(w, http.StatusServiceUnavailable, errBody{"事件订阅数已到上限，请稍后再连"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, errBody{err.Error()})
		return
	}
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			b, _ := json.Marshal(ev)
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Source, b); err != nil {
				return
			}
			fl.Flush()
		}
	}
}
