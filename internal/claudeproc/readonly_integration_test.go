//go:build integration && darwin

package claudeproc

import (
	"os"
	"testing"
	"time"

	"claude-abroad/internal/procinfo"
)

// k. 只读实跑（m4-plan v2.3 验收 k）：以当前用户执行，只读 sysctl，签名查询只读执行 codesign。
// 不调用结束流程，不发任何信号；只输出数量。
func TestReadOnlyList(t *testing.T) {
	ps, err := procinfo.SysProcs{}.List()
	if err != nil {
		t.Fatal(err)
	}
	var me *procinfo.Proc
	for i := range ps {
		if ps[i].PID == os.Getpid() {
			me = &ps[i]
		}
	}
	if me == nil {
		t.Fatalf("列出的 %d 个进程里没有测试进程自己", len(ps))
	}
	if me.Path == "" || me.PPID != os.Getppid() {
		t.Fatalf("自己的路径或父进程不对：ppid=%d", me.PPID)
	}
	if age := time.Since(me.Start); age < 0 || age > 10*time.Minute {
		t.Fatalf("启动时间不合理：%v 之前", age)
	}
	info, err := procinfo.SysProcs{}.Info(os.Getpid())
	if err != nil || !info.Start.Equal(me.Start) {
		t.Fatalf("Info 和 List 的启动时间应一致：%v", err)
	}
	t.Logf("进程 %d 个", len(ps))
}

func TestReadOnlyScan(t *testing.T) {
	w := NewFakeWorld() // 只用作假的信号发送器：这里不调用结束流程
	sc := &Scanner{Procs: procinfo.SysProcs{}, Signer: CodesignSigner{}, Bundle: (&procinfo.FileBundles{}).Read, Self: os.Getpid()}
	k := &Killer{Scanner: sc, Info: procinfo.SysProcs{}.Info, Sig: w, Clock: w}
	_ = k // 构造得出来；不调用 Kill
	rep, err := sc.Scan()
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range rep.Targets {
		if x.PID == os.Getpid() {
			t.Fatal("清单里不应有自己")
		}
	}
	if len(w.SignalLog()) != 0 {
		t.Fatal("不应有任何信号")
	}
	t.Logf("要结束的 %d 个，没能确认的 %d 个", len(rep.Targets), len(rep.Unverified))
}
