package state

import (
	"context"

	"claude-abroad/internal/usererror"
)

// Boot 是开机 guard 的逻辑（设计 6.2，M2-7c 决定 3）：只依赖 Store 和 PF，调用方必须持有 Store 的跨进程锁。
//   - 状态文件不存在或是 RESTORED：不调用任何 pf 操作；
//   - 其余一律按“无 utun”的配置应用 pf（加载后清状态由 Ensure 负责），包括状态文件损坏和步骤 6 的 RESTORING；
//   - 状态文件损坏时标记和 token 存不进去，照样加载，发事件。
//
// cmd/abroad-guard 和 plist 在 M6。
func Boot(ctx context.Context, s *Store, p PF, onEvent func(Event)) error {
	if !s.HoldsLock() {
		return notLocked("开机加载断网保护")
	}
	f, err := s.Load()
	if err == nil && f.State == Restored {
		return nil
	}
	if err != nil && onEvent != nil {
		onEvent(Event{Kind: EvCorrupt, Detail: "状态文件损坏，按“未恢复”加载断网保护"})
	}
	if err := p.Apply(ctx, ""); err != nil {
		return usererror.Wrap("开机加载断网保护", err, "请执行 sudo abroad restore --force，或重启电脑。")
	}
	return nil
}
