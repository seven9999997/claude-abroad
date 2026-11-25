package daemon

import (
	"strings"

	"claude-abroad/internal/resolverlock"
)

// checkResolverLock 做一次解析锁自检（M6-3，m6-plan v2.4 决定 5）：只读；写入过但有文件不在、被改、权限不对时
// 记一条事件（“第一层未生效”）；结果变化时才记。不自动重写（重写是敏感操作，要本人授权）。
func (d *Daemon) checkResolverLock() {
	lk := d.opts.Sys.ResolverLock
	if lk == nil {
		return
	}
	st := lk.Check()
	sum := lockSummary(st)
	d.resolverMu.Lock()
	changed := sum != d.resolverLast
	d.resolverLast = sum
	d.resolverMu.Unlock()
	if !changed {
		return
	}
	switch {
	case st.Written && !st.Effective:
		d.events.Add("resolver_lock", "not_effective", "Claude 域名解析锁（第一层）未生效："+sum)
	case st.Effective:
		d.events.Add("resolver_lock", "effective", "")
	}
}

func lockSummary(st resolverlock.Status) string {
	if !st.Written {
		return "未写入"
	}
	var parts []string
	for _, p := range st.Problems {
		parts = append(parts, p.Domain+" "+p.Kind)
	}
	if len(st.Conflicts) > 0 {
		parts = append(parts, "冲突 "+strings.Join(st.Conflicts, "、"))
	}
	if len(parts) == 0 {
		return "正常"
	}
	return strings.Join(parts, "；")
}
