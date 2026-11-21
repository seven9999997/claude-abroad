package state

import (
	"errors"
	"strings"
	"testing"

	"claude-abroad/internal/gatesm"
	"claude-abroad/internal/usererror"
)

// f. 启用第 ⑦ 步：通过 → 成功；不通过 → 失败并回滚干净（地区也还原），错误里有修复办法，闸门从未打开。
// 第 ⑦ 步在地区同步之后、写 ENABLED 之前。
func TestEnableStep7(t *testing.T) {
	b := newBed(t)
	var stepAt int
	var regionDone bool
	b.env.atHook = func() {
		f := b.state()
		stepAt, regionDone = f.Step, b.vmr.n > 0
	}
	mustOK(t, b.m.Enable(bg))
	if b.env.atEn != 1 || stepAt != StepEnvCheck || !regionDone || b.state().State != Enabled {
		t.Fatalf("第 ⑦ 步：调用 %d 次，步骤 %d，地区已同步=%v", b.env.atEn, stepAt, regionDone)
	}
	mustOK(t, b.m.Restore(bg))

	b2 := newBed(t)
	b2.env.fail = usererror.New("环境检测", "这些项不通过：06 IPv6", "请按下面处理后再启用：06 IPv6：请执行“恢复本机网络”后重新启用。", errors.New("x"))
	err := b2.m.Enable(bg)
	if err == nil || !strings.Contains(err.Error(), "06 IPv6") || !strings.Contains(err.Error(), "恢复本机网络") {
		t.Fatalf("错误里应有不通过的项和修复办法：%v", err)
	}
	b2.assertClean(b2.init) // 地区也还原
	if b2.gate.Opens() != 0 {
		t.Fatal("闸门从未打开")
	}
}

// 没有配置环境检测时（最小模式）行为不变：不做第 ⑦ 步。
func TestEnableNoEnvCheck(t *testing.T) {
	b := newBed(t)
	b.m.D.Env = nil
	mustOK(t, b.m.Enable(bg))
	for _, s := range b.tl() {
		if strings.Contains(s, "step=7 ") {
			t.Fatal("没有环境检测时不应写第 ⑦ 步")
		}
	}
}

// 重启后（Recover 到 ENABLED）加 env_check 原因并立即补一轮环境检测：明确通过之前闸门不开。
func TestRecoverRequiresEnvCheck(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	b.crash()
	b.env.ok = false // 这一轮还没通过
	mustOK(t, b.m.Recover(bg))
	if !b.gate.has(gatesm.ReasonEnvCheck) || b.gate.IsOpen() || b.env.kicks != 1 {
		t.Fatal("重启后应等环境检测重新通过")
	}
	b.env.ok = true
	b.env.Kick()
	b.re.Kick()
	if !b.gate.IsOpen() {
		t.Fatal("通过后复查放行")
	}
}
