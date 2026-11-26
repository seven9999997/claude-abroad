package state

import (
	"context"
	"time"

	"claude-abroad/internal/gatesm"
	"claude-abroad/internal/region"
	"claude-abroad/internal/sysnet"
)

// PF 是状态机对 pf 的需要。真实实现是 pfadapter.go 的 PatrolPF：所有应用都经同一个 pf.Patrol（M2-7c 决定 1），
// “将要启用”标记和新 token 经 Controller 的钩子落盘（决定 2）。
type PF interface {
	// Apply 按 utun（"" 表示还没有 utun，规则里不输出 utun 那一行）应用 pf 并解除巡检暂停。
	Apply(ctx context.Context, utun string) error
	// Flush 清空锚点并暂停巡检（之后巡检不会把规则加载回去）。
	Flush(ctx context.Context) error
	// ApplyRestored 把锚点经一次加载换成“回国拦截”那一组（M6-11a，没有空窗），解除暂停，巡检之后按这一组核对。
	// pf 没启用时启用；这时拿到的新 token 取代状态文件里的全部旧 token（pf 关过，旧的都已失效）。
	ApplyRestored(ctx context.Context) error
	// Release 释放一个启用引用。token 已失效时返回包着 pf.ErrTokenInvalid 的错误（状态机只用它区分事件文案）。
	Release(ctx context.Context, token string) error
	// SaveUnsaved 把拿到了、但追加进状态文件失败的 token 补存（回滚和恢复开始前调用）。
	SaveUnsaved() error
}

// Tun 管理 utun 设备（配地址和 MTU 属于真实实现，放 7d 或 M2-9）。
type Tun interface {
	Open(ctx context.Context) (name string, err error)
	// Close 关掉当前的 utun；没有打开时返回 nil。
	Close(ctx context.Context) error
	// Name 是当前打开的 utun 名，没有时为 ""。
	Name() string
	// Alive 报告 utun 是否还活着（设备意外消失时为 false）。设备死后 Name 可能仍返回旧名字，
	// 所以调用方判断“要不要重建”时看 Alive，不只看名字是否为空（rebuild 会先 Close 再 Open）。
	Alive() bool
}

// VM 是虚拟机的启停（幂等）。
type VM interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// ExitCheck 是启用第 ⑤ 步的出口检查：返回 nil 表示明确通过；不通过和无法判断都返回错误
// （m4-plan v2.2 决定 4：第 ⑤ 步无法判断按失败处理并回滚）。运行中的复查不经这里，由 Recheck 负责。
type ExitCheck interface {
	Check(ctx context.Context) error
}

// Gate 是闸门（关闭时走代理的新连接一律拒绝；gatesm.Gate 满足）。打开要等复查放行，状态机只加、清原因。
// 状态机只动 state 这个原因，监护只动 takeover（m4-plan v2.2 决定 1、5）。
type Gate interface {
	AddReason(r gatesm.Reason, detail string)
	ClearReason(r gatesm.Reason)
	IsOpen() bool
}

// Recheck 是出口复查循环（gatesm.Rechecker 满足）：启用第 ⑧ 步用第 ⑤ 步的结果放行或补一轮；
// 监护清掉 takeover、Recover 到 ENABLED 后立即补一轮。
type Recheck interface {
	AfterEnable(passAt time.Time)
	Kick()
}

// Baseline 报告有没有可用的出口基准（m4-plan v2.2 决定 6：没有不能启用）。可选：nil 时不检查（开发用的最小检查模式）。
type Baseline interface {
	Ready() error
}

// Teardown 断开所有走代理的连接（真实顺序 DropAll → DoH Close → Dialer Close 在 M2-9 组装）。
type Teardown interface {
	Teardown(ctx context.Context)
}

// Killer 强制结束 Claude 相关进程，并确认已全部退出（设计 10.1）。
type Killer interface {
	KillClaude(ctx context.Context) error
}

// Preflight 是启用前的冲突检查（设计 10.4，M2-8）。只读。
type Preflight interface {
	Check(ctx context.Context) error
}

// Deps 是状态机的全部依赖（都注入，测试用假实现）。
// DataState 是数据面的“状态接收方”（m3-plan v2.7 决定 3）：网络栈据此判定停用时只放行 offline_allow 的直连、
// 并在状态变化时重判在途的流。只分“启用”和“其他”两种。可选：Deps.DataState 为 nil 时不通知。
type DataState interface {
	SetEnabled(enabled bool)
}

// VMRegion 把目标地区下发给虚拟机（agent 的 PUT /v1/region：写时区和语言，有变化时重启引擎）。
type VMRegion interface {
	SetRegion(ctx context.Context, t region.Target) error
}

// EnvCheck 是环境检测（M4-6a，envcheck.Monitor 满足）：启用第 ⑦ 步在探测窗口里跑全部硬性项；Kick 立即补一轮。
type EnvCheck interface {
	AtEnable(ctx context.Context) error
	Kick()
}

// TargetSource 给出启用时的目标地区（daemon：锁定的目标；还没有时按出口基准确定并锁定）。ok 为 false 表示没有目标
// （开发用的最小模式没有出口基准），这时不做地区同步。
type TargetSource interface {
	Target(ctx context.Context) (t region.Target, ok bool, err error)
}

type Deps struct {
	PF        PF
	Tun       Tun
	Routes    sysnet.Routes
	DNS       sysnet.DNS
	IPv6      sysnet.IPv6
	VM        VM
	Check     ExitCheck
	Gate      Gate
	Recheck   Recheck
	Baseline  Baseline // 可选
	Teardown  Teardown
	Killer    Killer
	Preflight Preflight
	DataState DataState // 可选
	// Env 是环境检测（M4-6a，可选：nil 时不做第 ⑦ 步）。
	Env EnvCheck
	// 地区（M4-5）。三个都非 nil 才做地区同步。
	Locale   sysnet.MacRegion
	VMRegion VMRegion
	Targets  TargetSource
	// ReturnBlock 报告回国后是否保留“回国拦截”（M6-11a，设计 10.2）：打开了启动管控、首次准备做过、没在卸载也没卸载过。
	// nil 或返回假时回国行为和以前完全一样（清空锚点、释放全部启用引用）。每次回国、恢复时读一次。
	ReturnBlock func() bool
}

// EventKind 是状态机事件的种类。
type EventKind string

const (
	EvRollback             EventKind = "rollback"               // 启用失败，开始回滚
	EvRollbackFailed       EventKind = "rollback_failed"        // 回滚也失败了，状态留在 ENABLING
	EvIPv6Unknown          EventKind = "ipv6_unknown"           // 网络服务的 IPv6 设置没法原样还原，不关它（pf 照样拦 inet6）
	EvServiceGone          EventKind = "service_gone"           // 快照里的网络服务已经不存在，跳过
	EvTokenUnknown         EventKind = "token_unknown"          // 恢复时“将要启用”标记仍在，可能漏了一个启用引用
	EvTokenReleaseFailed   EventKind = "token_release_failed"   // 锚点清空后释放启用引用失败（已从文件去掉，继续恢复）
	EvTokenUnsaved         EventKind = "token_unsaved"          // 状态文件写不进去，标记或 token 没能落盘（照样启用 pf）
	EvReturnBlockFailed    EventKind = "return_block_failed"    // 已回国：回国拦截那一组没能加载（巡检会再试）
	EvCorrupt              EventKind = "corrupt"                // 状态文件损坏，按“非 RESTORED”加载 pf
	EvOrigDNSUnknown       EventKind = "orig_dns_unknown"       // 启用时读不出本机 DNS（直连按域名会失败，M3-7）
	EvRegionUserSkipped    EventKind = "region_user_skipped"    // 没有人登录或登录的换了人：跳过用户级的语言和地区格式
	EvRegionSynced         EventKind = "region_synced"          // 地区已同步（Detail 是目标，不含原值）
	EvRegionRestored       EventKind = "region_restored"        // 地区已写回原值
	EvRegionPending        EventKind = "region_pending"         // 恢复时时区或语言有几项没能还原（Detail 只有项目名）
	EvRegionPendingCleared EventKind = "region_pending_cleared" // 待还原的项重试成功，文件已删除
	EvRegionPendingCorrupt EventKind = "region_pending_corrupt" // 待还原文件读不出来，已忽略
)

// Event 是一条状态机事件。Detail 只含不敏感的信息（步骤、网络服务名、错误类别）。
type Event struct {
	Kind   EventKind
	Detail string
}
