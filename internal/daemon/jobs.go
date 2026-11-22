package daemon

import (
	"claude-abroad/internal/api"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"claude-abroad/internal/state"
	"claude-abroad/internal/usererror"
)

// 任务化（m5-plan v2.1 决定 5）：启用、停用带 ?async=1 时异步执行，返回任务号；不带时保持同步、行为完全相同。
// 任务与忙标记共用：已有任务在跑时新的异步请求回 409；任务执行时遇到别的操作在进行，任务失败、写明“忙”。
// 结果只在内存里，保留最近 JobKeep 个。任务里不保存请求体（确认码不出现在查询结果里）。

// JobKeep 是保留的任务数。
const JobKeep = 16

// 任务状态。
const (
	JobRunning   = "running"
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
)

// Job 是一个任务（查询结果）。
type Job struct {
	ID       string    `json:"id"`
	Op       string    `json:"op"` // enable、disable
	State    string    `json:"state"`
	Error    string    `json:"error,omitempty"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
}

type jobs struct {
	mu   sync.Mutex
	list []*Job // 旧的在前
}

func (j *jobs) running() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, x := range j.list {
		if x.State == JobRunning {
			return true
		}
	}
	return false
}

// start 新建一个进行中的任务；已有任务在跑时返回 false。
func (j *jobs) start(op string, now time.Time) (*Job, bool) {
	var b [8]byte
	_, _ = rand.Read(b[:])
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, x := range j.list {
		if x.State == JobRunning {
			return nil, false
		}
	}
	jb := &Job{ID: hex.EncodeToString(b[:]), Op: op, State: JobRunning, Started: now.Truncate(time.Second)}
	j.list = append(j.list, jb)
	if len(j.list) > JobKeep {
		// 只丢已经结束的旧任务（进行中的最多一个，就是刚加的这个）。
		j.list = j.list[len(j.list)-JobKeep:]
	}
	return jb, true
}

func (j *jobs) finish(jb *Job, err error, now time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	jb.Finished = now.Truncate(time.Second)
	if err != nil {
		jb.State, jb.Error = JobFailed, err.Error()
		if errors.Is(err, state.ErrBusy) {
			jb.Error = "忙：有别的操作在进行，请稍后再试。" + jb.Error
		}
		return
	}
	jb.State = JobSucceeded
}

func (j *jobs) get(id string) (Job, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, x := range j.list {
		if x.ID == id {
			return *x, true
		}
	}
	return Job{}, false
}

func (d *Daemon) now() time.Time {
	if d.opts.Clock != nil {
		return d.opts.Clock.Now()
	}
	return time.Now()
}

// wantAsync 判断请求是否要求异步（?async=1）；不认识的值回 400 并返回 ok=false。
func wantAsync(w http.ResponseWriter, r *http.Request) (async, ok bool) {
	switch r.URL.Query().Get("async") {
	case "", "0":
		return false, true
	case "1":
		return true, true
	}
	writeJSON(w, http.StatusBadRequest, errBody{"async 只能是 0 或 1"})
	return false, false
}

// run 执行启用、停用：同步时同 exec；异步时先在请求里做完“状态文件损坏”的检查，建任务、后台执行，回 202 和任务。
// giveBack 是执行遇到“忙”时放回确认码（只在同步路径上有意义）。
func (d *Daemon) run(w http.ResponseWriter, r *http.Request, op string, async bool, fn func(ctx context.Context) error, giveBack func()) {
	if !async {
		if err := d.exec(w, r, fn, false); errors.Is(err, state.ErrBusy) && giveBack != nil {
			giveBack()
		}
		return
	}
	if d.corrupt.Load() {
		if _, err := d.store.Load(); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, errBody{usererror.New("执行操作", "状态文件已损坏", "请执行 sudo abroad restore --force。", ErrCorruptMode).Error()})
			return
		}
		d.corrupt.Store(false)
	}
	jb, ok := d.jobs.start(op, d.now())
	if !ok {
		if giveBack != nil {
			giveBack()
		}
		writeErr(w, http.StatusConflict, api.ErrorBody{Code: api.CodeBusy, Error: "已经有一个任务在进行，请等它结束（GET /v1/jobs/{id}）"})
		return
	}
	go func() {
		err := fn(context.Background())
		d.jobs.finish(jb, err, d.now())
		if s, ok := d.jobs.get(jb.ID); ok {
			d.events.Add("job", "job_"+s.State, op)
		}
	}()
	snap, _ := d.jobs.get(jb.ID)
	writeJSON(w, http.StatusAccepted, snap)
}

func (d *Daemon) handleDisable(w http.ResponseWriter, r *http.Request) {
	async, ok := wantAsync(w, r)
	if !ok {
		return
	}
	if _, err := io.ReadAll(r.Body); err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, errBody{"请求体超过 64 KiB"})
		return
	}
	d.run(w, r, "disable", async, func(ctx context.Context) error { return d.m.Disable(ctx, false) }, nil)
}

func (d *Daemon) handleJobGet(w http.ResponseWriter, r *http.Request) {
	jb, ok := d.jobs.get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody{"没有这个任务（只保留最近 16 个，abroadd 重启后清空）"})
		return
	}
	writeJSON(w, http.StatusOK, jb)
}
