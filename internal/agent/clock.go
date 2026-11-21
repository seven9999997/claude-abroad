package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// ClockStepThreshold：宿主与虚拟机的时间偏差不超过它时不动时钟。
const ClockStepThreshold = 500 * time.Millisecond

// MaxClockBody 是 PUT /v1/clock 请求体的上限。
const MaxClockBody = 256

// 接受的时间范围：[2025-01-01, 2100-01-01)。超出说明宿主时间或请求本身有问题，不改时钟。
var (
	clockMin = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	clockMax = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
)

// ClockStatus 是 /v1/health 的 clock 段。不影响 health 的 ok。
type ClockStatus struct {
	Synced       bool  `json:"synced"`
	Syncs        int64 `json:"syncs"`
	Steps        int64 `json:"steps"`
	LastOffsetMs int64 `json:"last_offset_ms"`
	NowUnixMs    int64 `json:"now_unix_ms"`
}

// Clock 处理 PUT /v1/clock（设计文档 7.1）：宿主经 vsock 下发自己的墙上时间，
// 偏差超过 ClockStepThreshold 就一步校正。客户机时钟在宿主睡眠期间不走，
// 代理协议（例如 vmess）对时间敏感，所以宿主在开机、唤醒和定期都会对时。
type Clock struct {
	Sys  System
	Now  func() time.Time
	Logf func(format string, a ...any)

	mu     sync.Mutex
	status ClockStatus
}

func (c *Clock) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Status 返回 clock 段。
func (c *Clock) Status() ClockStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.status
	st.NowUnixMs = c.now().UnixMilli()
	return st
}

func (c *Clock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", http.MethodPut)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxClockBody+1))
	if err != nil || len(body) > MaxClockBody {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体过大或读取失败"})
		return
	}
	var req struct {
		UnixMs *int64 `json:"unix_ms"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.UnixMs == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `请求体应为 {"unix_ms": <毫秒>}`})
		return
	}
	host := time.UnixMilli(*req.UnixMs)
	if host.Before(clockMin) || !host.Before(clockMax) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "时间不在 2025–2100 年之间"})
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	offset := host.Sub(c.now())
	stepped := false
	if offset > ClockStepThreshold || offset < -ClockStepThreshold {
		if err := c.Sys.SetTime(host); err != nil {
			if c.Logf != nil {
				c.Logf("对时：校正失败：%v", err)
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "设置时钟失败"})
			return
		}
		stepped = true
		c.status.Steps++
	}
	c.status.Synced = true
	c.status.Syncs++
	c.status.LastOffsetMs = offset.Milliseconds()
	if c.Logf != nil && stepped {
		c.Logf("对时：偏差 %d 毫秒，已校正", offset.Milliseconds())
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "offset_ms": offset.Milliseconds(), "stepped": stepped})
}
