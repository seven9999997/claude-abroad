package daemon

import (
	"context"
	"errors"
	"fmt"

	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/gatesm"
	"claude-abroad/internal/state"
	"claude-abroad/internal/usererror"
)

// ErrExitCheckFailed 标记出口检查“明确不通过”（收到了不是预期的应答、出口在大陆、违反出口变化规则）。
// state.ExitCheck 的其余错误（连不上、超时、通道不可用）在复查里按“无法判断”处理。
var ErrExitCheckFailed = errors.New("出口检查明确不通过")

// TriChecker 是能直接给出三种结果的出口检查（FullExitCheck 满足）。
type TriChecker interface {
	CheckResult(ctx context.Context) (gatesm.Result, string)
}

// GeoTriChecker 是能顺带给出这轮出口的国家和时区的三态检查（FullExitCheck 满足；M4-5 的出口漂移只发事件）。
type GeoTriChecker interface {
	CheckResultGeo(ctx context.Context) (res gatesm.Result, detail, country, tz string)
}

// recheckFunc 把系统的出口检查适配成复查用的三态：先确认虚拟机在（Recover、监护不再启动它），再检查。
// onGeo 非 nil 且检查能给出国家、时区时调用（只用来比对目标地区、发事件）。
// readyReporter 是能只读报告是否健康的虚拟机（vmmgr.Manager 实现）。复查不能经 Start：
// Start 表示“需要虚拟机”，会让停止重启的管理器重新开始拉起。
type readyReporter interface{ Ready() error }

func recheckFunc(vm state.VM, ex state.ExitCheck, onGeo func(country, tz string)) gatesm.CheckFunc {
	return func(ctx context.Context) (gatesm.Result, string) {
		if r, ok := vm.(readyReporter); ok { // 由 abroadd 管理的虚拟机：只读当前是否健康，不触发拉起（M6-2a）
			if err := r.Ready(); err != nil {
				return gatesm.Unknown, "虚拟机没有就绪"
			}
		} else if err := vm.Start(ctx); err != nil {
			return gatesm.Unknown, "虚拟机没有就绪"
		}
		if gc, ok := ex.(GeoTriChecker); ok {
			res, detail, country, tz := gc.CheckResultGeo(ctx)
			if onGeo != nil && res != gatesm.Unknown {
				onGeo(country, tz)
			}
			return res, detail
		}
		if tc, ok := ex.(TriChecker); ok {
			return tc.CheckResult(ctx)
		}
		err := ex.Check(ctx)
		switch {
		case err == nil:
			return gatesm.Pass, ""
		case errors.Is(err, ErrExitCheckFailed):
			return gatesm.Fail, usererror.Reason(err)
		}
		return gatesm.Unknown, usererror.Reason(err)
	}
}

// FullExitCheck 是真实模式的出口检查（m4-plan M4-2、M4-3）：exitcheck 的整轮检查，按状态目录里的出口基准比对。
// 启用第 ⑤ 步要求明确通过；复查用三态。基准读不出来时按“无法判断”（没有基准的启用在第 ⓪ 步就被拒了）。
type FullExitCheck struct {
	Checker  *exitcheck.Checker
	Baseline *BaselineService
}

func (f FullExitCheck) run(ctx context.Context) exitcheck.Report {
	var base *exitcheck.Baseline
	if b, err := exitcheck.LoadBaseline(f.Baseline.Path); err == nil {
		base = &b
	}
	return f.Checker.Check(ctx, "main", exitcheck.Options{Base: base})
}

// Check 实现 state.ExitCheck：明确通过才返回 nil。
func (f FullExitCheck) Check(ctx context.Context) error {
	rep := f.run(ctx)
	if rep.Verdict == exitcheck.Pass {
		return nil
	}
	err := fmt.Errorf("出口检查%s：%s", map[exitcheck.Result]string{exitcheck.Fail: "不通过", exitcheck.Unknown: "无法判断"}[rep.Verdict], failedItems(rep))
	if rep.Verdict == exitcheck.Fail {
		err = fmt.Errorf("%w：%s", ErrExitCheckFailed, failedItems(rep))
	}
	return usererror.New("出口检查", usererror.Reason(err), "请确认主代理可用、出口不在大陆且符合出口变化规则，再试一次。", err)
}

// CheckResultGeo 实现 GeoTriChecker。
func (f FullExitCheck) CheckResultGeo(ctx context.Context) (gatesm.Result, string, string, string) {
	rep := f.run(ctx)
	res, detail := verdictOf(rep)
	return res, detail, rep.Country, rep.Timezone
}

// CheckResult 实现 TriChecker。
func (f FullExitCheck) CheckResult(ctx context.Context) (gatesm.Result, string) {
	return verdictOf(f.run(ctx))
}

func verdictOf(rep exitcheck.Report) (gatesm.Result, string) {
	switch rep.Verdict {
	case exitcheck.Pass:
		return gatesm.Pass, ""
	case exitcheck.Fail:
		return gatesm.Fail, failedItems(rep)
	}
	return gatesm.Unknown, failedItems(rep)
}

// failedItems 列出没通过的项（只有项名和原因，不含地址）。
func failedItems(rep exitcheck.Report) string {
	var out string
	for _, it := range rep.Items {
		if it.Result != exitcheck.Pass && it.Name != "udp" && it.Name != "geo" {
			if out != "" {
				out += "；"
			}
			out += it.Name + "（" + string(it.Cause) + "）"
		}
	}
	return out
}
