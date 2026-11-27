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
	reason := checkFailReason(rep)
	err := fmt.Errorf("出口检查%s：%s", map[exitcheck.Result]string{exitcheck.Fail: "不通过", exitcheck.Unknown: "无法判断"}[rep.Verdict], reason)
	if rep.Verdict == exitcheck.Fail {
		err = fmt.Errorf("%w：%s", ErrExitCheckFailed, reason)
	}
	// 原因直接用检查结果的文字（2026-11-27 真机：原来取 usererror.Reason(err)，对这种普通错误取到空串，
	// 用户看到“启用失败：出口检查失败：。”）。
	return usererror.New("出口检查", reason, "请确认主代理可用、出口不在大陆且符合出口变化规则，再试一次。", err)
}

// AllConnectFailedReason 是出口检查各项都连接失败时的原因（启用失败的提示）。
const AllConnectFailedReason = "经主代理连不上（各项都连接失败）：请看事件里的网络计数和现场快照"

// checkFailReason 是没有明确通过时给人看的原因：各项（不算 udp、geo）都是连接失败时给 AllConnectFailedReason，
// 否则列出没通过的项（failedItems）；万一什么都列不出来，也不留空。
func checkFailReason(rep exitcheck.Report) string {
	n, connect := 0, 0
	for _, it := range rep.Items {
		if it.Result != exitcheck.Pass && it.Name != "udp" && it.Name != "geo" {
			n++
			if it.Cause == exitcheck.CauseConnect {
				connect++
			}
		}
	}
	if n > 0 && connect == n {
		return AllConnectFailedReason
	}
	if s := failedItems(rep); s != "" {
		return s
	}
	return "检查没有给出明确结果"
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

// failedItems 列出没通过的项（中文的项名和原因，不含地址；原因码不拼进给人看的文字，docs/vm-borrow-plan.md v2 第 6 节）。
func failedItems(rep exitcheck.Report) string {
	var out string
	for _, it := range rep.Items {
		if it.Result != exitcheck.Pass && it.Name != "udp" && it.Name != "geo" {
			if out != "" {
				out += "；"
			}
			out += exitcheck.ItemLabel(it.Name) + "：" + exitcheck.CauseText(it.Cause)
		}
	}
	return out
}
