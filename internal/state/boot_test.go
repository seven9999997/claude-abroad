package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claude-abroad/internal/pf"
)

// guardBoot 以 guard 进程的身份跑一次 Boot（新 Store 持锁），返回期间的 pfctl 调用。
func (b *bed) guardBoot(t *testing.T) ([]string, error) {
	t.Helper()
	b.release()
	st, rel := b.newProcStore()
	_, pw := b.newPF(st)
	b.resetTimeline()
	err := Boot(bg, st, pw, b.onEvent)
	var calls []string
	for _, s := range b.tl() {
		if strings.HasPrefix(s, "pf ") {
			calls = append(calls, s)
		}
	}
	rel()
	b.spawn()
	return calls, err
}

// e/g. 状态文件不存在或 RESTORED：Boot 没有任何 pfctl 调用。
func TestBootRestoredNoop(t *testing.T) {
	b := newBed(t)
	calls, err := b.guardBoot(t)
	if err != nil || len(calls) != 0 {
		t.Fatalf("文件不存在：%v %v", calls, err)
	}
	mustOK(t, b.m.Enable(bg))
	mustOK(t, b.m.Restore(bg))
	b.fpf.Reset()
	calls, err = b.guardBoot(t)
	if err != nil || len(calls) != 0 {
		t.Fatalf("RESTORED：%v %v", calls, err)
	}
}

func writeCorrupt(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// g. 损坏的状态文件：Boot 加载 pf、有事件、不崩溃；Recover 确保 pf 已加载、返回 ErrCorrupt，不改网络。
func TestCorruptStateFile(t *testing.T) {
	b := newBed(t)
	b.mu.Lock()
	b.noCheck = true // 不变量检查要读状态文件，损坏时读不出来
	b.mu.Unlock()
	writeCorrupt(t, b.dir)
	calls, err := b.guardBoot(t)
	if err != nil {
		t.Fatalf("Boot：%v", err)
	}
	if !b.fpf.Active(pf.DefaultAnchor) || !b.blocked() || index(calls, has("-F states")) < 0 {
		t.Fatalf("Boot 没加载 pf：%v", calls)
	}
	if b.countEvents(EvCorrupt) != 1 || b.countEvents(EvTokenUnsaved) == 0 {
		t.Fatal("没有事件")
	}
	// Recover。
	b.resetTimeline()
	err = b.m.Recover(bg)
	if !errors.Is(err, ErrCorrupt) || !isUserError(err) {
		t.Fatalf("%v", err)
	}
	if !b.fpf.Active(pf.DefaultAnchor) {
		t.Fatal("pf 应生效")
	}
	for _, s := range b.tl() {
		if strings.HasPrefix(s, "net ") || strings.HasPrefix(s, "store ") {
			t.Fatalf("损坏时不应改网络或写状态文件：%v", b.tl())
		}
	}
	if !b.net.Snapshot().Equal(b.init) {
		t.Fatal("网络被改了")
	}
}

// g. 状态文件是符号链接：同样处理，且不跟随链接写入。
func TestCorruptSymlink(t *testing.T) {
	b := newBed(t)
	b.mu.Lock()
	b.noCheck = true
	b.mu.Unlock()
	victim := filepath.Join(t.TempDir(), "victim.json")
	const orig = `{"version":1,"state":"RESTORED","step":0}`
	if err := os.WriteFile(victim, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(b.dir, FileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.guardBoot(t); err != nil {
		t.Fatal(err)
	}
	if !b.fpf.Active(pf.DefaultAnchor) || b.countEvents(EvCorrupt) != 1 {
		t.Fatal("符号链接时应按损坏处理并加载 pf")
	}
	if err := b.m.Recover(bg); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("%v", err)
	}
	if got, _ := os.ReadFile(victim); string(got) != orig {
		t.Fatalf("链接指向的文件被改了：%s", got)
	}
	if fi, _ := os.Lstat(filepath.Join(b.dir, FileName)); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("链接被替换了")
	}
}
