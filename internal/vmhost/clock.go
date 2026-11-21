package vmhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// ClockMaxRTT：对时请求的往返超过它就算失败——虚拟机被设成的时间已经偏了这么多。
const ClockMaxRTT = time.Second

// SyncResult 是一次对时的结果。
type SyncResult struct {
	OffsetMs int64 `json:"offset_ms"`
	Stepped  bool  `json:"stepped"`
}

// SyncClock 经 dial 拨控制面，用 PUT /v1/clock 把宿主的墙上时间下发给虚拟机（设计文档 7.1）。
func SyncClock(ctx context.Context, dial func(context.Context) (net.Conn, error), now func() time.Time) (SyncResult, error) {
	if now == nil {
		now = time.Now
	}
	// 用墙上时间算往返：macOS 的单调时钟在睡眠期间不走，往返中途睡了一觉时单调时钟看不出来。
	t0 := now().Round(0)
	body, _ := json.Marshal(map[string]int64{"unix_ms": t0.UnixMilli()})
	code, out, err := ctlRoundTrip(ctx, dial, http.MethodPut, "/v1/clock", body)
	rtt := now().Round(0).Sub(t0)
	if err != nil {
		return SyncResult{}, err
	}
	if code != http.StatusOK {
		return SyncResult{}, fmt.Errorf("对时返回 %d", code)
	}
	if rtt > ClockMaxRTT {
		return SyncResult{}, fmt.Errorf("对时往返 %s 超过 %s", rtt.Round(time.Millisecond), ClockMaxRTT)
	}
	var r SyncResult
	if err := json.Unmarshal(out, &r); err != nil {
		return SyncResult{}, errors.New("对时应答不是有效的 JSON")
	}
	return r, nil
}

// ClockKeeper 维持虚拟机时钟：启动立即对时；失败按 RetryMin→RetryMax 退避重试直到成功；
// 成功后每 Interval 一次；Wake 收到信号立即对一次。ctx 结束即返回。
// 对时失败只记日志，绝不强停虚拟机，也不影响巡检计数。
type ClockKeeper struct {
	Dial     func(context.Context) (net.Conn, error)
	Interval time.Duration
	RetryMin time.Duration
	RetryMax time.Duration
	Wake     <-chan struct{}
	Now      func() time.Time
	Logf     func(format string, a ...any)
}

func (k *ClockKeeper) logf(format string, a ...any) {
	if k.Logf != nil {
		k.Logf(format, a...)
	}
}

// Run 一直运行到 ctx 结束。
func (k *ClockKeeper) Run(ctx context.Context) {
	interval, rmin, rmax := k.Interval, k.RetryMin, k.RetryMax
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if rmin <= 0 {
		rmin = 2 * time.Second
	}
	if rmax <= 0 {
		rmax = 30 * time.Second
	}
	backoff := rmin
	for ctx.Err() == nil {
		sctx, cancel := context.WithTimeout(ctx, 2*ClockMaxRTT)
		r, err := SyncClock(sctx, k.Dial, k.Now)
		cancel()
		if ctx.Err() != nil {
			return
		}
		wait := interval
		if err != nil {
			k.logf("对时失败（%v），%s 后重试", err, backoff)
			wait = backoff
			backoff = min(backoff*2, rmax)
		} else {
			backoff = rmin
			if r.Stepped {
				k.logf("对时：虚拟机时钟偏差 %d 毫秒，已校正", r.OffsetMs)
			}
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-k.Wake:
			t.Stop()
		}
	}
}
