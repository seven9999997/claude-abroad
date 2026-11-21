// Package envcheck 是出国后的环境检测（design 8.1，m4-plan M4-6 v2.6）：从 Mac 本机应用的角度再查一遍出国环境。
// 全部按 design 8.1 自己实现，不复制 claude-switcher（AGPL）的任何代码、脚本或文案。
//
// 6a 只有框架、调度、与闸门的联动和 01、06、07、09、11 五项；其余在 6b。
//   - 每项三种结果（沿用 exitcheck.Result）：通过、不通过、无法判断；输出不含地址的说明和修复办法；
//   - 先后依赖：01（接管就绪）没有明确通过时，同一轮里要发网络探测的项都不执行，记“无法判断：接管未就绪”；
//   - 警告项可以由用户关闭（“已跳过”，不算通过、不参与判定），硬性项不能。
package envcheck

import (
	"context"
	"sort"
	"strings"
	"time"

	"claude-abroad/internal/exitcheck"
)

// Level 是检查项的级别。
type Level string

const (
	Hard Level = "hard" // 硬性：明确不通过时闸门关闭
	Warn Level = "warn" // 警告：只提示
)

// Skipped 是用户关闭了的警告项的结果。
const Skipped exitcheck.Result = "skipped"

// Outcome 是一项的结果。Detail、Fix 都不含地址。
type Outcome struct {
	Result exitcheck.Result
	Detail string
	Fix    string
}

// Item 是一个检查项。Network 为真表示它会发网络探测（01 没有明确通过时不执行）。
type Item struct {
	ID      string
	Name    string
	Level   Level
	Network bool
	Run     func(ctx context.Context) Outcome
}

// ItemResult 是一项在一轮里的结果（status、事件、日志用）。
type ItemResult struct {
	ID     string           `json:"id"`
	Name   string           `json:"name"`
	Level  Level            `json:"level"`
	Result exitcheck.Result `json:"result"`
	Detail string           `json:"detail,omitempty"`
	Fix    string           `json:"fix,omitempty"`
}

// Report 是一轮的结果。Hard 是硬性项的总判定：全部通过才是 Pass；有明确不通过是 Fail；否则 Unknown。
type Report struct {
	At    time.Time        `json:"at"`
	Items []ItemResult     `json:"items"`
	Hard  exitcheck.Result `json:"hard"`
}

// Failed 返回明确不通过的硬性项。
func (r Report) Failed() []ItemResult { return r.hardWith(exitcheck.Fail) }

// Unknown 返回无法判断的硬性项。
func (r Report) Unknown() []ItemResult { return r.hardWith(exitcheck.Unknown) }

func (r Report) hardWith(res exitcheck.Result) []ItemResult {
	var out []ItemResult
	for _, it := range r.Items {
		if it.Level == Hard && it.Result == res {
			out = append(out, it)
		}
	}
	return out
}

// Summary 是给事件和闸门原因用的一句话（只有项目编号和名字）。
func Summary(items []ItemResult) string {
	var out []string
	for _, it := range items {
		out = append(out, it.ID+" "+it.Name)
	}
	return strings.Join(out, "、")
}

// Fixes 是修复办法（编号 + 办法）。
func Fixes(items []ItemResult) string {
	var out []string
	for _, it := range items {
		if it.Fix != "" {
			out = append(out, it.ID+" "+it.Name+"："+it.Fix)
		}
	}
	return strings.Join(out, " ")
}

// TakeoverID 是接管就绪（01）的编号：它没有明确通过时，要发网络探测的项不执行。
const TakeoverID = "01"

// Clock 是可注入的时钟（重试的等待）。
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RetryDelays 是无法判断时的重试间隔（design 8.1）。
var RetryDelays = []time.Duration{5 * time.Second, 15 * time.Second, 45 * time.Second}

// Runner 跑一轮检测。
type Runner struct {
	Items    []Item
	Disabled func() map[string]bool // 用户关闭了的警告项（硬性项即使在里面也照跑）
	Clock    Clock
}

func (r *Runner) clock() Clock {
	if r.Clock != nil {
		return r.Clock
	}
	return realClock{}
}

// Round 跑一轮：先 01，再其余（按编号）。01 没有明确通过时，要发网络探测的项不执行。
func (r *Runner) Round(ctx context.Context) Report {
	return r.round(ctx, nil)
}

// round 只跑 only 里的项（nil 表示全部），其余沿用 prev 的结果。
func (r *Runner) round(ctx context.Context, prev map[string]ItemResult) Report {
	items := append([]Item(nil), r.Items...)
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].ID == TakeoverID || items[j].ID == TakeoverID {
			return items[i].ID == TakeoverID
		}
		return items[i].ID < items[j].ID
	})
	var off map[string]bool
	if r.Disabled != nil {
		off = r.Disabled()
	}
	rep := Report{At: r.clock().Now()}
	takeoverOK := true
	for _, it := range items {
		res := ItemResult{ID: it.ID, Name: it.Name, Level: it.Level}
		switch {
		case prev != nil && prev[it.ID].Result != exitcheck.Unknown && prev[it.ID].ID != "":
			res = prev[it.ID] // 重试时只重跑无法判断的项
		case it.Level == Warn && off[it.ID]:
			res.Result, res.Detail = Skipped, "已跳过（在设置里关掉了）"
		case it.Network && !takeoverOK:
			res.Result, res.Detail = exitcheck.Unknown, "无法判断：接管未就绪，没有发探测"
			res.Fix = "先处理 01 接管就绪的问题。"
		default:
			o := it.Run(ctx)
			if o.Result == "" {
				o.Result = exitcheck.Unknown
			}
			res.Result, res.Detail, res.Fix = o.Result, o.Detail, o.Fix
		}
		if it.ID == TakeoverID && res.Result != exitcheck.Pass {
			takeoverOK = false
		}
		rep.Items = append(rep.Items, res)
	}
	rep.Hard = exitcheck.Pass
	for _, it := range rep.Items {
		if it.Level != Hard {
			continue
		}
		switch it.Result {
		case exitcheck.Fail:
			rep.Hard = exitcheck.Fail
		case exitcheck.Unknown:
			if rep.Hard == exitcheck.Pass {
				rep.Hard = exitcheck.Unknown
			}
		}
	}
	return rep
}

// RunWithRetry 跑一轮；硬性项有无法判断（且没有明确不通过）时，按 RetryDelays 等待后只重跑无法判断的项（启用第 ⑦ 步）。
func (r *Runner) RunWithRetry(ctx context.Context) Report {
	rep := r.Round(ctx)
	for _, d := range RetryDelays {
		if rep.Hard != exitcheck.Unknown {
			return rep
		}
		if err := r.clock().Sleep(ctx, d); err != nil {
			return rep
		}
		prev := map[string]ItemResult{}
		for _, it := range rep.Items {
			prev[it.ID] = it
		}
		// 01 被重跑时，依赖它的网络项也要重跑：把它们从 prev 里去掉（它们的结果是“接管未就绪”的无法判断，本来就会重跑）。
		rep = r.round(ctx, prev)
	}
	return rep
}
