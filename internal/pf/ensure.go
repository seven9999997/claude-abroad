package pf

import (
	"context"
	"errors"
	"fmt"
)

// EnsureResult 是一次 Ensure 做了什么。
type EnsureResult struct {
	Loaded       bool   // 加载了规则
	WasAbsent    bool   // 加载前锚点是空的
	KilledStates bool   // 清了状态（锚点原先不在，或被别人改过）
	Foreign      bool   // 锚点里的规则既不是期望的、也不是上一次我们应用的（别的软件改过）
	NewToken     string // pf 原本没启用、这次取得的启用引用（调用方要追加保存，恢复时全部释放）
	// ShownAfterLoad 是加载后复核时读到的锚点原始打印（复核不一致时巡检用它判断下一轮是否还是同一个样子）。
	ShownAfterLoad []byte
}

// ErrStatusUnavailable 包在读状态失败的错误外面：这时看不到 pf 的状态（可能是偶发），巡检连续 2 轮才判失效。
// 读到了状态、确认有坏状态但修复失败的，不带这个包装，巡检当轮就判失效。
var ErrStatusUnavailable = errors.New("读不到 pf 的状态")

// Ensure 让 pf 处于期望状态（设计 6.1、M2-6b）：
//   - Status 出错直接返回；
//   - 锚点为空：Load，紧接着 KillStates（加载规则之前已有的状态会让旧连接继续走）；
//   - 内容不同、且等于 previous（上一次我们应用的规则，非空）：只 Load（我们自己换了网络）；
//   - 内容不同、且不等于 previous（或 previous 为空）：别人改的，Load 后紧跟 KillStates，Foreign = true
//     （别人塞进的规则留下的状态仍能放行旧连接）；
//   - 一致：不动；
//   - pf 没启用：Enable，返回新 token；
//   - 有过 Load：再取一次 Status，Matcher 必须判一致，否则 ErrVerifyFailed（新规则保留，不 Flush）；
//   - 主规则集不再引用 com.apple/*：做完以上步骤后返回 ErrAnchorNotReferenced（处置在 M2-6c）。
func (c *Controller) Ensure(ctx context.Context, expected, previous []byte) (EnsureResult, error) {
	var r EnsureResult
	if c.Matcher == nil {
		return r, errors.New("没有配置 Matcher")
	}
	st, err := c.Status(ctx)
	if err != nil {
		return r, fmt.Errorf("%w：%w", ErrStatusUnavailable, err)
	}
	switch {
	case !c.Matcher.Present(st):
		if err := c.Load(ctx, expected); err != nil {
			return r, err
		}
		r.Loaded, r.WasAbsent = true, true
		if err := c.KillStates(ctx); err != nil {
			return r, err
		}
		r.KilledStates = true
	case !c.Matcher.Same(expected, st):
		if err := c.Load(ctx, expected); err != nil {
			return r, err
		}
		r.Loaded = true
		if len(previous) == 0 || !c.Matcher.Same(previous, st) {
			r.Foreign = true
			if err := c.KillStates(ctx); err != nil {
				return r, err
			}
			r.KilledStates = true
		}
	}
	if !st.Enabled {
		tok, err := c.Enable(ctx)
		if err != nil {
			return r, err
		}
		r.NewToken = tok
	}
	if r.Loaded {
		st2, err := c.Status(ctx)
		if err != nil {
			return r, err
		}
		r.ShownAfterLoad = st2.AnchorRules
		if !c.Matcher.Same(expected, st2) {
			return r, ErrVerifyFailed
		}
		st.Referenced = st2.Referenced
	}
	if !st.Referenced {
		return r, ErrAnchorNotReferenced
	}
	return r, nil
}
