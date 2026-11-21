package vmhost

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"claude-abroad/internal/guestabi"
)

// Timing 是生命周期的各项时长。测试里换成很短的值。
type Timing struct {
	Watchdog        time.Duration // 启动后这么久 health 仍不通就强停，算一次失败
	ProbeEvery      time.Duration // 启动阶段的 health 探测间隔
	ProbeTimeout    time.Duration // 单次 health 探测（含拨号）
	ShutdownTimeout time.Duration // POST /v1/shutdown（含拨号）
	StopWait        time.Duration // 发出关机请求后等虚拟机停止的上限，超时强停
	StableFor       time.Duration // health 通后连续运行这么久，退避才复位
	BackoffMin      time.Duration
	BackoffMax      time.Duration
	PatrolEvery     time.Duration // health 通过之后的巡检间隔
	PatrolFailures  int           // 巡检连续失败这么多次就强停
}

// DefaultTiming 是正式的取值（docs/m1-plan.md M1-3、M1-4a）。
var DefaultTiming = Timing{
	Watchdog:        30 * time.Second,
	ProbeEvery:      500 * time.Millisecond,
	ProbeTimeout:    2 * time.Second,
	ShutdownTimeout: 3 * time.Second,
	StopWait:        10 * time.Second,
	StableFor:       60 * time.Second,
	BackoffMin:      time.Second,
	BackoffMax:      30 * time.Second,
	PatrolEvery:     5 * time.Second,
	PatrolFailures:  3,
}

// MaxHealthBody 是 health 应答体的读取上限。
const MaxHealthBody = 64 << 10

// Supervisor 管理虚拟机的生命周期：
//   - 启动后在 Watchdog 内等 health 通，不通就强停；
//   - health 通后每 PatrolEvery 巡检一次，连续 PatrolFailures 次不通就强停
//     （刚从睡眠唤醒的那一轮不计失败）；
//   - health 里的 boot_id 变了，说明虚拟机原地重启过（内核复位时 VZ 不通知宿主），
//     不强停，但重新触发 OnReady，稳定期从头算；
//   - 意外退出按 BackoffMin→BackoffMax 翻倍退避后重启；health 通后连续运行满 StableFor 才复位；
//   - 从没成功启动过时，创建或启动失败直接返回错误（确定性错误，不重试）；
//   - 强停后虚拟机仍没停：返回 ErrStuck，不再起第二台；
//   - ctx 结束时优雅停止：POST /v1/shutdown，等 StopWait，超时强停；期间 Force 收到信号立即强停。
type Supervisor struct {
	Factory Factory
	Spec    Spec
	Timing  Timing
	Log     func(format string, a ...any)
	Force   <-chan struct{} // 优雅停止期间收到就立即强停（第二次 Ctrl-C）

	// OnReady 在每次开机 health 首次通过时、以及 boot_id 变化时调用，在单独的 goroutine 里运行。
	// ctx 在这台虚拟机停止或 boot_id 再次变化时取消。主代理配置、对时都挂在这里重新下发。
	//
	// 并发约定：取消旧 ctx 时不等旧的回调返回，Run 重启下一台时也不等，所以新旧两个回调可能
	// 同时在跑；回调里经 Dial 拨到的总是当前这台。实现必须遵守 ctx、可重入、幂等。
	OnReady func(ctx context.Context, bootID string)

	// Health、Shutdown 可替换（测试用）；默认经 DialVsock(1081) 发 HTTP 请求。
	Health   func(ctx context.Context, m Machine) (bootID string, err error)
	Shutdown func(ctx context.Context, m Machine) error
	// OnWake 在巡检判定“刚从睡眠唤醒”的那一轮调用（例如立即对时）。不得阻塞。
	OnWake func()
	// Now 返回墙上时间（不带单调时钟读数，用于识别睡眠唤醒）；默认 time.Now().Round(0)。
	Now func() time.Time

	mu      sync.Mutex
	cur     Machine
	running bool
}

// ErrNotRunning 是虚拟机不在运行（启动中、退避中、已停止）时 Dial 的错误。
var ErrNotRunning = errors.New("虚拟机不在运行")

// ErrStuck 是强停后虚拟机仍没有停止。此时不再起第二台，abroad-vm 以非 0 退出。
var ErrStuck = errors.New("强停后虚拟机仍没有停止，为避免同时运行两台虚拟机，abroad-vm 退出")

// Dial 拨当前这台虚拟机的 vsock 端口；不在运行时立即返回 ErrNotRunning。
func (s *Supervisor) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	s.mu.Lock()
	m, ok := s.cur, s.running
	s.mu.Unlock()
	if !ok {
		return nil, ErrNotRunning
	}
	return m.DialVsock(ctx, port)
}

func (s *Supervisor) setCurrent(m Machine) {
	s.mu.Lock()
	s.cur, s.running = m, m != nil
	s.mu.Unlock()
}

func (s *Supervisor) logf(format string, a ...any) {
	if s.Log != nil {
		s.Log(format, a...)
	}
}

// timing 逐字段兜底：没填的字段用默认值（避免 PatrolEvery=0 空转、PatrolFailures=0 一次失败就强停）。
func (s *Supervisor) timing() Timing {
	t, d := s.Timing, DefaultTiming
	for _, f := range []struct{ v, def *time.Duration }{
		{&t.Watchdog, &d.Watchdog}, {&t.ProbeEvery, &d.ProbeEvery}, {&t.ProbeTimeout, &d.ProbeTimeout},
		{&t.ShutdownTimeout, &d.ShutdownTimeout}, {&t.StopWait, &d.StopWait}, {&t.StableFor, &d.StableFor},
		{&t.BackoffMin, &d.BackoffMin}, {&t.BackoffMax, &d.BackoffMax}, {&t.PatrolEvery, &d.PatrolEvery},
	} {
		if *f.v <= 0 {
			*f.v = *f.def
		}
	}
	if t.PatrolFailures <= 0 {
		t.PatrolFailures = d.PatrolFailures
	}
	return t
}

// wallNow 返回去掉单调时钟读数的当前时间：macOS 上单调时钟在睡眠期间不走，
// 只有墙上时间能看出“中间睡过一觉”。
func (s *Supervisor) wallNow() time.Time {
	if s.Now != nil {
		return s.Now().Round(0)
	}
	return time.Now().Round(0)
}

func (s *Supervisor) health(ctx context.Context, m Machine) (string, error) {
	if s.Health != nil {
		return s.Health(ctx, m)
	}
	return httpHealth(ctx, m)
}

// Run 一直运行到 ctx 结束（返回 nil）、首次启动失败或强停无效（返回错误）。
func (s *Supervisor) Run(ctx context.Context) error {
	t := s.timing()
	backoff := t.BackoffMin
	started := false
	for {
		m, err := s.Factory(s.Spec)
		if err == nil {
			err = m.Start()
		}
		if err != nil {
			if !started {
				return err
			}
			s.logf("启动虚拟机失败：%v", err)
		} else {
			started = true
			s.setCurrent(m)
			s.logf("虚拟机已启动")
			res := s.watch(ctx, m, t)
			s.setCurrent(nil)
			if res.err != nil {
				return res.err
			}
			if res.stopped {
				return nil
			}
			if !res.healthyAt.IsZero() && time.Since(res.healthyAt) >= t.StableFor {
				backoff = t.BackoffMin
			}
			s.logf("虚拟机意外停止（%v）", m.Err())
		}
		s.logf("%s 后重启虚拟机", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, t.BackoffMax)
	}
}

type watchResult struct {
	stopped   bool      // 因 ctx 结束而停止
	healthyAt time.Time // 最近一次开始“健康”的时间（boot_id 变化时重置）
	err       error     // ErrStuck
}

type monitorEvent struct {
	kind   int // evHealthy、evBootChanged、evPatrolFailed
	bootID string
}

const (
	evHealthy = iota
	evBootChanged
	evPatrolFailed
)

// watch 管一台虚拟机从启动到停止。
func (s *Supervisor) watch(ctx context.Context, m Machine, t Timing) watchResult {
	mctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan monitorEvent, 4)
	go s.monitor(mctx, m, t, events)

	var res watchResult
	readyCancel := func() {}
	defer func() { readyCancel() }()
	ready := func(id string) {
		readyCancel()
		if s.OnReady == nil {
			readyCancel = func() {}
			return
		}
		rctx, rcancel := context.WithCancel(mctx)
		readyCancel = rcancel
		go s.OnReady(rctx, id)
	}

	watchdog := time.NewTimer(t.Watchdog)
	defer watchdog.Stop()
	for {
		select {
		case <-ctx.Done():
			if !s.gracefulStop(m, t) {
				res.err = ErrStuck
			}
			res.stopped = true
			return res
		case <-m.Done():
			return res
		case ev := <-events:
			switch ev.kind {
			case evHealthy:
				watchdog.Stop()
				res.healthyAt = time.Now()
				s.logf("虚拟机 health 已通（boot_id %s）", ev.bootID)
				ready(ev.bootID)
			case evBootChanged:
				res.healthyAt = time.Now()
				s.logf("虚拟机原地重启过（boot_id 变为 %s），重新下发就绪回调", ev.bootID)
				ready(ev.bootID)
			case evPatrolFailed:
				s.logf("巡检连续 %d 次 health 不通，强停虚拟机", t.PatrolFailures)
				if !s.forceStop(m, t) {
					res.err = ErrStuck
				}
				res.healthyAt = time.Time{}
				return res
			}
		case <-watchdog.C:
			s.logf("启动 %s 后 health 仍不通，强停虚拟机", t.Watchdog)
			if !s.forceStop(m, t) {
				res.err = ErrStuck
			}
			return watchResult{err: res.err}
		}
	}
}

// monitor 先在启动阶段反复探测直到 health 第一次通过，然后转入巡检。
func (s *Supervisor) monitor(ctx context.Context, m Machine, t Timing, events chan<- monitorEvent) {
	send := func(ev monitorEvent) bool {
		select {
		case events <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}
	probe := func() (string, error) {
		hctx, cancel := context.WithTimeout(ctx, t.ProbeTimeout)
		defer cancel()
		return s.health(hctx, m)
	}
	sleep := func(d time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(d):
			return true
		}
	}

	var bootID string
	for {
		id, err := probe()
		if err == nil {
			bootID = id
			if !send(monitorEvent{kind: evHealthy, bootID: id}) {
				return
			}
			break
		}
		if !sleep(t.ProbeEvery) {
			return
		}
	}

	fails := 0
	last := s.wallNow()
	for {
		if !sleep(t.PatrolEvery) {
			return
		}
		now := s.wallNow()
		gap := now.Sub(last)
		last = now
		// 正常情况下 gap ≈ PatrolEvery + 上一轮探测耗时（最多 ProbeTimeout）；
		// 明显超过这个范围才是睡过一觉。
		woke := gap > t.ProbeTimeout+3*t.PatrolEvery
		if woke {
			s.logf("距上次巡检 %s，判断刚从睡眠唤醒，失败计数清零", gap.Round(time.Second))
			fails = 0
			if s.OnWake != nil {
				s.OnWake()
			}
		}
		id, err := probe()
		if err != nil {
			if woke {
				continue // 唤醒后的第一轮不计失败
			}
			fails++
			if fails >= t.PatrolFailures {
				send(monitorEvent{kind: evPatrolFailed})
				return
			}
			continue
		}
		fails = 0
		if id != bootID {
			bootID = id
			if !send(monitorEvent{kind: evBootChanged, bootID: id}) {
				return
			}
		}
	}
}

// gracefulStop 请求关机，等 StopWait，超时或收到 Force 就强停。返回虚拟机是否已停止。
func (s *Supervisor) gracefulStop(m Machine, t Timing) bool {
	shutdown := s.Shutdown
	if shutdown == nil {
		shutdown = httpShutdown
	}
	s.logf("请求虚拟机关机")
	sctx, cancel := context.WithTimeout(context.Background(), t.ShutdownTimeout)
	err := shutdown(sctx, m)
	cancel()
	if err != nil {
		s.logf("关机请求失败（%v），强停", err)
		return s.forceStop(m, t)
	}
	timer := time.NewTimer(t.StopWait)
	defer timer.Stop()
	select {
	case <-m.Done():
		s.logf("虚拟机已关机")
		return true
	case <-timer.C:
		s.logf("%s 内没有关机，强停", t.StopWait)
	case <-s.Force:
		s.logf("再次收到停止信号，强停")
	}
	return s.forceStop(m, t)
}

// forceStop 强停并等最多 StopWait，返回虚拟机是否真的停了。
func (s *Supervisor) forceStop(m Machine, t Timing) bool {
	if err := m.ForceStop(); err != nil {
		s.logf("强停失败：%v", err)
	}
	timer := time.NewTimer(t.StopWait)
	defer timer.Stop()
	select {
	case <-m.Done():
		return true
	case <-timer.C:
		s.logf("强停后 %s 仍没有停止", t.StopWait)
		return false
	}
}

// vsockRoundTrip 经 vsock 1081 发一个 HTTP 请求（Connection: close），返回状态码和应答体
// （最多读 MaxHealthBody+1 字节，调用方据此判断是否超限）。
func vsockRoundTrip(ctx context.Context, m Machine, method, path string) (int, []byte, error) {
	return ctlRoundTrip(ctx, func(ctx context.Context) (net.Conn, error) { return m.DialVsock(ctx, guestabi.CtlPort) },
		method, path, nil)
}

// ctlRoundTrip 用 dial 拨一条到控制面的连接，发一个 HTTP 请求。
func ctlRoundTrip(ctx context.Context, dial func(context.Context) (net.Conn, error), method, path string, body []byte) (int, []byte, error) {
	c, err := dial(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://abroad-agent"+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Close = true
	if err := req.Write(c); err != nil {
		return 0, nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, MaxHealthBody+1))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, out, nil
}

func httpHealth(ctx context.Context, m Machine) (string, error) {
	code, body, err := vsockRoundTrip(ctx, m, http.MethodGet, "/v1/health")
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("health 返回 %d", code)
	}
	if len(body) > MaxHealthBody {
		return "", fmt.Errorf("health 应答超过 %d 字节", MaxHealthBody)
	}
	var h struct {
		BootID string `json:"boot_id"`
	}
	if err := json.Unmarshal(body, &h); err != nil {
		return "", fmt.Errorf("health 应答不是有效的 JSON：%w", err)
	}
	if h.BootID == "" {
		return "", errors.New("health 应答里没有 boot_id")
	}
	return h.BootID, nil
}

func httpShutdown(ctx context.Context, m Machine) error {
	code, _, err := vsockRoundTrip(ctx, m, http.MethodPost, "/v1/shutdown")
	if err != nil {
		return err
	}
	if code != http.StatusAccepted {
		return fmt.Errorf("shutdown 返回 %d", code)
	}
	return nil
}
