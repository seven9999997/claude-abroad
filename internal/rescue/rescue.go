// Package rescue 是应急恢复程序 abroad-rescue 的逻辑（M6-5b，m6-plan v2.7 的 8 条，设计 10.3）。
//
// 只在两种情况下用：abroadd 没在运行；状态文件损坏。abroadd 还活着（拿不到状态目录的排他锁，或本地接口连得上）时拒绝，
// 什么都不改。固定顺序，任何一步失败就停（已完成的不回滚，再执行一次从头走，结果相同）：
//
//  1. 列出要结束的 Claude 进程（和回国同一套判定），用户确认；
//  2. 结束 Claude：确认过的进程没能结束时中止，不碰网络；
//  3. 状态文件原子写成“已回国”（之后 abroadd 被拉起来不会重新接管；待还原的原值先留在文件里）；
//  4. 清空本项目自己的 pf 锚点，释放自己记下的启用引用（状态文件里的、开机 guard 留下的）；
//  5. 删自己的 DNS 键和接管路由；
//  6. 按待还原记录恢复 IPv6、时区、语言；记录读不出来时这一步不动，明确提示手动核对。
//
// 不删解析锁和 Claude Code 的托管设置（那是管控，只有卸载才解除）；不关闭 pf、不碰别人的规则；不停止、不结束 abroadd，
// 不调用系统服务管理命令；不联网、不监听。所有系统操作经 Deps 注入：会话里只用假的实现测试。
package rescue

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"strings"

	"claude-abroad/internal/claudeproc"
	"claude-abroad/internal/pf"
	"claude-abroad/internal/state"
	"claude-abroad/internal/sysnet"
)

// 退出码。
const (
	ExitOK      = 0 // 走完了（或本来就不需要）
	ExitFailed  = 1 // 某一步失败，停在那里
	ExitUsage   = 2 // 参数不对
	ExitRefused = 3 // 前提不满足（不是 root、abroadd 还在运行、你没有确认），什么都没改
)

// EmergencyDoc 是应急文档的位置（输出里提示用）。
const EmergencyDoc = "docs/emergency.md（仓库里），或应用包里的“应急恢复”文档"

// Claude 是“预览 + 结束”（claudeproc.Service）。
type Claude interface {
	Preview(ctx context.Context) (claudeproc.Preview, error)
	KillClaude(ctx context.Context) error
}

// PF 是本项目自己的锚点：清空、加载（回国拦截那一组，M6-11b）、释放启用引用。没有关闭 pf 的方法。
type PF interface {
	Flush(ctx context.Context) error
	Load(ctx context.Context, rules []byte) error
	Release(ctx context.Context, token string) error
}

// DNS、Routes、IPv6 只要撤销的那一个方法（sysnet 的真实实现都满足）。
type (
	DNS interface {
		Remove(ctx context.Context) error
	}
	Routes interface {
		Delete(ctx context.Context, dst netip.Prefix) error
	}
	IPv6 interface {
		Restore(ctx context.Context, service string, s sysnet.IPv6Setting) error
	}
)

// Deps 是应急程序用到的系统操作（真实实现只在 cmd/abroad-rescue 里组装）。
type Deps struct {
	Store  *state.Store
	Claude Claude
	PF     PF
	DNS    DNS
	Routes Routes
	IPv6   IPv6
	Locale sysnet.MacRegion // 可为 nil：不还原地区
	// GuardTokens 取出开机 guard 留下的启用引用（并删掉记录）；可为 nil。
	GuardTokens func() ([]string, error)
	// DaemonAlive 报告 abroadd 的本地接口连得上（abroadd 正常时应改用普通的回国）。
	DaemonAlive func(ctx context.Context) bool
	// IsRoot 报告是否以 root 运行。
	IsRoot func() bool
	// Confirm 给用户看清单后要确认（Yes 时不调用）。返回 false 表示不继续。
	Confirm func(p claudeproc.Preview) bool
	Yes     bool // 跳过确认（清单照样打印）
	Out     io.Writer
}

// Run 执行一次应急恢复，返回退出码。
func Run(ctx context.Context, d Deps) int {
	say := func(format string, a ...any) { fmt.Fprintf(d.Out, format+"\n", a...) }
	// ---- 前提：root、abroadd 不在 ----
	if d.IsRoot == nil || !d.IsRoot() {
		say("应急恢复要以 root 运行：sudo abroad-rescue。什么都没改。")
		return ExitRefused
	}
	release, err := d.Store.Lock()
	if err != nil {
		if errors.Is(err, state.ErrLocked) {
			say("后台服务 abroadd 还在运行（它持有状态文件的锁）。应急程序只在它不在时使用：")
			say("  - 它正常时，请用应用里的“回国”或 sudo abroad restore；")
			say("  - 它卡住时，先按 %s 停掉后台服务，再执行本程序。", EmergencyDoc)
			say("什么都没改。")
			return ExitRefused
		}
		say("锁定状态文件失败：%v。什么都没改。", err)
		return ExitFailed
	}
	defer release()
	if d.DaemonAlive != nil && d.DaemonAlive(ctx) {
		say("后台服务的本地接口还连得上：请用应用里的“回国”或 sudo abroad restore。什么都没改。")
		return ExitRefused
	}

	// ---- 读状态文件：损坏时“待还原记录”读不出来 ----
	f, lerr := d.Store.Load()
	records := lerr == nil
	pending, hasPending, perr := d.Store.LoadPending()
	if perr != nil {
		hasPending = false
	}
	if records && f.State == state.Restored && !leftovers(f, state.ReturnBlockMarked(d.Store.Dir) && state.ChooseReturnBlock(d.Store.Dir).Load) && !hasPending {
		say("状态已经是“已回国”，没有要恢复的。")
		return ExitOK
	}
	if records {
		say("状态文件：%s。", f.State)
	} else {
		say("状态文件读不出来（%v）：网络照常恢复，时区、语言和 IPv6 的原值读不到，这几项不动。", lerr)
	}

	// ---- 1. 预览并确认 ----
	pv, err := d.Claude.Preview(ctx)
	if err != nil {
		say("第 1 步 列出 Claude 进程失败：%v。什么都没改。请手动退出 Claude 后再试。", err)
		return ExitFailed
	}
	say("第 1 步 要结束的 Claude 进程：")
	if len(pv.Processes) == 0 {
		say("  （没有）")
	}
	for _, p := range pv.Processes {
		say("  %d  %s", p.PID, p.App)
	}
	for _, p := range pv.Unverified {
		say("  没能确认、不会结束：%d  %s（%s）", p.PID, p.App, p.Note)
	}
	if !d.Yes && (d.Confirm == nil || !d.Confirm(pv)) {
		say("你没有确认，什么都没改。")
		return ExitRefused
	}
	// ---- 2. 结束 Claude：失败就中止，不碰网络 ----
	if err := d.Claude.KillClaude(ctx); err != nil {
		say("第 2 步 结束 Claude 失败：%v", err)
		say("网络没有改动（还开着的 Claude 会在网络恢复的那一刻用本机的 IP 重连）。请手动退出 Claude（活动监视器里强制退出）后再执行本程序。")
		return ExitFailed
	}
	say("第 2 步 已结束 Claude")
	// ---- 3. 状态文件写成已回国（原值先留着） ----
	if records {
		err = d.Store.Update(func(x *state.File) error { x.State, x.Origin, x.Step = state.Restored, "", 0; return nil })
	} else {
		err = d.Store.Save(state.File{Version: state.Version, State: state.Restored})
	}
	if err != nil {
		return fail(say, 3, "把状态文件写成“已回国”", err)
	}
	say("第 3 步 状态文件已写成“已回国”（之后后台服务被拉起来也不会重新接管）")
	// ---- 4. 自己的锚点：清空并释放启用引用；首次准备做过、没卸载时换成回国拦截那一组并留一个（M6-11b） ----
	toks := append([]string(nil), f.PFTokens...)
	if d.GuardTokens != nil {
		if gt, err := d.GuardTokens(); err == nil {
			toks = append(toks, gt...)
		}
	}
	choice := state.ChooseReturnBlock(d.Store.Dir) // 和 abroadd、guard 同一个来源（M6-11b）
	rb := state.ReturnBlockMarked(d.Store.Dir) && choice.Load
	if state.ReturnBlockMarked(d.Store.Dir) && choice.Note != "" {
		say("第 4 步 %s", choice.Note)
	}
	var keep []string
	if rb {
		rules, err := pf.Render(choice.Config())
		if err == nil {
			err = d.PF.Load(ctx, rules) // 一次加载替换锚点里的内容，没有空窗
		}
		if err != nil {
			return fail(say, 4, "把本项目的 pf 锚点换成回国拦截", err)
		}
		if n := len(toks); n > 0 { // 留最新的一个（开机 guard 的排在最后），其余释放
			keep, toks = toks[n-1:], toks[:n-1:n-1]
		}
	} else if err := d.PF.Flush(ctx); err != nil {
		return fail(say, 4, "清空本项目的 pf 锚点", err)
	}
	left := 0
	for _, t := range toks {
		if err := d.PF.Release(ctx, t); err != nil {
			left++ // 尽力而为：锚点已经清空，pf 保持启用无害，重启后消失（设计 6.2）
		}
	}
	if records || rb {
		if err := d.Store.Update(func(x *state.File) error { x.PFTokens, x.PFEnablePending = keep, false; return nil }); err != nil {
			return fail(say, 4, "记下留下的启用引用", err)
		}
	}
	switch {
	case rb:
		say("第 4 步 已把本项目的 pf 锚点换成回国拦截（只拦 Anthropic 的地址段，卸载时解除），留下 %d 个启用引用、释放了 %d 个（%d 个没能释放）", len(keep), len(toks)-left, left)
	case left > 0:
		say("第 4 步 已清空本项目的 pf 锚点；有 %d 个启用引用没能释放：pf 可能保持启用但锚点为空，无害，重启后消失", left)
	default:
		say("第 4 步 已清空本项目的 pf 锚点，释放了 %d 个启用引用（没有关闭 pf，没有动别人的规则）", len(toks))
	}
	// ---- 5. DNS 键和接管路由 ----
	if err := ignoreNotFound(d.DNS.Remove(ctx)); err != nil {
		return fail(say, 5, "删除本项目的 DNS 设置", err)
	}
	for _, dst := range append(slices.Clone(state.TakeoverRoutes), sysnet.LegacyTakeoverRoutes...) {
		if err := ignoreNotFound(d.Routes.Delete(ctx, dst)); err != nil {
			return fail(say, 5, "删除接管路由 "+dst.String(), err)
		}
	}
	say("第 5 步 已删除本项目的 DNS 设置和接管路由")
	// ---- 6. IPv6、时区、语言 ----
	if !records {
		say("第 6 步 待还原记录读不出来：IPv6、时区和语言没有还原，请手动核对（系统设置 → 网络 → 详细信息 → TCP/IP；通用 → 日期与时间、语言与地区）。见 %s。", EmergencyDoc)
		say("应急恢复完成（网络部分）。")
		return ExitOK
	}
	var notes []string
	for _, svc := range sortedKeys(f.IPv6Original) {
		orig := f.IPv6Original[svc]
		if orig.Mode == sysnet.IPv6Off || orig.Mode == sysnet.IPv6Unknown {
			continue // 启用时没动过它
		}
		if err := d.IPv6.Restore(ctx, svc, orig); err != nil {
			if errors.Is(err, sysnet.ErrNotFound) {
				continue // 网络服务已经不在了
			}
			return fail(say, 6, "恢复网络服务“"+svc+"”的 IPv6", err)
		}
	}
	var p state.RegionPending
	if d.Locale != nil {
		switch {
		case f.Region.Target != nil && f.Region.Original != nil:
			p, _ = state.RestoreRegion(ctx, d.Locale, *f.Region.Original)
		case hasPending:
			cur, err := d.Locale.Read(ctx)
			if err != nil {
				p = pending
			} else {
				p, _ = state.RestoreRegion(ctx, d.Locale, pending.Apply(cur))
			}
		}
	}
	if len(p.Items) > 0 {
		p.V = 1
		if err := d.Store.SavePending(p); err != nil {
			notes = append(notes, "没能还原的项也没能记下")
		}
		say("第 6 步 已恢复 IPv6；时区或语言有几项没能还原：%s。后台服务下次启动后会重试；也可以手动改回。%s", p.Labels(), strings.Join(notes, ""))
	} else {
		if hasPending {
			_ = d.Store.DeletePending()
		}
		say("第 6 步 已恢复 IPv6、时区和语言")
	}
	// 全部做完：清掉原值（下一个周期重拍）；回国拦截留下的那个启用引用记着（卸载时释放）。
	if err := d.Store.MarkRestoredKeep(keep); err != nil {
		return fail(say, 6, "清理状态文件里的原值", err)
	}
	say("应急恢复完成。解析锁和 Claude Code 的托管设置没有动（那是管控，只有卸载才解除）。")
	return ExitOK
}

func fail(say func(string, ...any), step int, what string, err error) int {
	say("第 %d 步 %s失败：%v", step, what, err)
	say("停在这一步（已完成的不回滚）。处理后再执行一次本程序，会从头走一遍；还是不行就按 %s 手动恢复。", EmergencyDoc)
	return ExitFailed
}

// leftovers 报告“已回国”的状态文件里还有没处理完的东西（上一次应急恢复中途停下）。
// leftovers 报告“已回国”的状态文件里还有没收拾的东西。保留回国拦截时（rb）留一个启用引用是正常的。
func leftovers(f state.File, rb bool) bool {
	keep := 0
	if rb {
		keep = 1
	}
	return len(f.PFTokens) > keep || len(f.IPv6Original) > 0 || f.Region.Original != nil || f.Region.Target != nil
}

func ignoreNotFound(err error) error {
	if errors.Is(err, sysnet.ErrNotFound) {
		return nil
	}
	return err
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
