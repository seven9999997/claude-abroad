package daemon

import (
	"context"
	"errors"
	"path/filepath"

	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/exitpolicy"
	"claude-abroad/internal/usererror"
)

// BaselineFile 是状态目录里的出口基准文件。
const BaselineFile = "baseline.json"

// Prober 做一轮出口检查（*exitcheck.Checker 满足；测试注入假的）。
type Prober interface {
	Check(ctx context.Context, exit string, o exitcheck.Options) exitcheck.Report
}

// BaselineService 是出口基准（m4-plan v2.2 决定 6）：建立（经主代理跑一轮带位置查询的检查，按规则保存，0600），
// 以及启用前确认有可用的基准（state.Baseline）。
type BaselineService struct {
	Prober Prober
	Path   string
}

// NewBaselineService 返回状态目录下的基准服务。
func NewBaselineService(p Prober, stateDir string) *BaselineService {
	return &BaselineService{Prober: p, Path: filepath.Join(stateDir, BaselineFile)}
}

// Ready 实现 state.Baseline。
func (b *BaselineService) Ready() error {
	_, err := exitcheck.LoadBaseline(b.Path)
	return err
}

// BaselineSummary 是建立基准的应答（不含出口 IP、城市）。
type BaselineSummary struct {
	Rule     exitpolicy.Rule `json:"rule"`
	Country  string          `json:"country"`
	Timezone string          `json:"timezone,omitempty"`
	// Borrowed：这次检查用的虚拟机是临时启动的，检查完已经关闭（未启用时，docs/vm-borrow-plan.md v2）。
	Borrowed bool `json:"borrowed,omitempty"`
}

// Establish 建立基准：HTTPS、出口 IP、大陆段三项都要明确通过，出口不在大陆，才保存。
func (b *BaselineService) Establish(ctx context.Context, rule exitpolicy.Rule) (BaselineSummary, error) {
	base, err := b.Probe(ctx, rule)
	if err != nil {
		return BaselineSummary{}, err
	}
	return b.Save(base)
}

// Save 保存 Probe 得到的基准。
func (b *BaselineService) Save(base exitcheck.Baseline) (BaselineSummary, error) {
	if err := exitcheck.SaveBaseline(b.Path, base); err != nil {
		return BaselineSummary{}, usererror.New("保存出口基准", err.Error(), "请检查状态目录的权限。", err)
	}
	return BaselineSummary{Rule: base.Rule, Country: base.Country, Timezone: base.Timezone}, nil
}

// summarizeBaseline 是基准的摘要（不含出口 IP）。
func summarizeBaseline(base exitcheck.Baseline) BaselineSummary {
	return BaselineSummary{Rule: base.Rule, Country: base.Country, Timezone: base.Timezone}
}

// Probe 跑一轮带位置查询的检查并得出基准，不保存（应用主代理的两步确认：第一次调用不改任何东西）。
func (b *BaselineService) Probe(ctx context.Context, rule exitpolicy.Rule) (exitcheck.Baseline, error) {
	if rule == "" {
		rule = exitpolicy.RuleSameCountry
	}
	if !rule.Valid() {
		return exitcheck.Baseline{}, usererror.New("建立出口基准", "出口变化规则不认识："+string(rule), "规则只能是 fixed_ip、same_city、same_country、any。", nil)
	}
	rep := b.Prober.Check(ctx, "main", exitcheck.Options{WantGeo: true})
	for _, name := range []string{"https", "ip", "mainland"} {
		if it, ok := rep.Item(name); !ok || it.Result != exitcheck.Pass {
			pf := newProbeFailure(rep)
			reason, next := "这一轮检查没有明确通过（"+failedItems(rep)+"）", "请确认主代理可用、出口不在中国大陆，再试一次。"
			switch {
			case pf.OnlyChannel:
				reason, next = "到虚拟机的数据通道打不开（虚拟机没有运行或刚刚退出）", "请在“引擎与虚拟机”页看虚拟机的状态，稍后再试。"
			case pf.Unreachable:
				reason, next = "主代理不通（"+failedItems(rep)+"）", "请确认主代理节点可用后再试。"
			}
			return exitcheck.Baseline{}, usererror.New("建立出口基准", reason, next, pf)
		}
	}
	base, err := exitcheck.BaselineFromReport(rep, rule)
	if err != nil {
		return exitcheck.Baseline{}, usererror.New("建立出口基准", err.Error(), "请换一个主代理节点，或换一条出口变化规则。", err)
	}
	return base, nil
}

// ProbeFailure 是出口检查没有明确通过（docs/vm-borrow-plan.md v2 第 1 节第 1 步）。Report 只在内存里（含出口 IP），
// Error 只给中文的项名和原因。
type ProbeFailure struct {
	Report exitcheck.Report
	// Unreachable：没通过的项全是网络类原因（超时、连不上、限流、格式、通道）——“主代理不通”，不是明确不通过。
	Unreachable bool
	// OnlyChannel：没通过的项全是“数据通道打不开”——到虚拟机的通道本身不通（虚拟机没有运行）。
	OnlyChannel bool
}

func (p *ProbeFailure) Error() string { return "检查没有明确通过：" + failedItems(p.Report) }

// ErrProbeNotPassed 让 errors.Is 能认出出口检查没有明确通过。
var ErrProbeNotPassed = errors.New("检查没有明确通过")

func (p *ProbeFailure) Is(target error) bool { return target == ErrProbeNotPassed }

func newProbeFailure(rep exitcheck.Report) *ProbeFailure {
	pf := &ProbeFailure{Report: rep, Unreachable: true, OnlyChannel: true}
	n := 0
	for _, it := range rep.Items {
		if it.Result == exitcheck.Pass || it.Name == "udp" || it.Name == "geo" || it.Name == "rule" && it.Cause == exitcheck.CauseNoBaseline {
			continue
		}
		n++
		if !exitcheck.NetworkCause(it.Cause) {
			pf.Unreachable = false
		}
		if it.Cause != exitcheck.CauseChannelDown {
			pf.OnlyChannel = false
		}
	}
	if n == 0 {
		pf.Unreachable, pf.OnlyChannel = false, false
	}
	return pf
}
