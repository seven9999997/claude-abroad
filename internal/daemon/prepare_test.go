package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claude-abroad/internal/claudecfg"
	"claude-abroad/internal/resolverlock"
)

type prepEnv struct {
	*env
	lock *resolverlock.Lock
	cs   *claudecfg.Settings
}

func newPrepEnv(t *testing.T) *prepEnv {
	t.Helper()
	e := &prepEnv{env: newEnv(t)}
	base := filepath.Dir(e.dir)
	_ = os.MkdirAll(e.dir, 0o700)
	e.lock = &resolverlock.Lock{Dir: filepath.Join(base, "resolver"), RecordDir: e.dir}
	e.cs = &claudecfg.Settings{Dir: filepath.Join(base, "ClaudeCode", "managed-settings.d"), RecordDir: e.dir}
	return e
}

func (e *prepEnv) start(testOnly bool) *Daemon {
	o := e.opts()
	o.Sys.ResolverLock, o.Sys.ClaudeSettings = e.lock, e.cs
	o.Sys.FirstRun, o.Sys.PrepareTestOnly = true, testOnly
	return e.env.start(o)
}

func (e *prepEnv) restart(t *testing.T, d *Daemon, testOnly bool) *Daemon {
	t.Helper()
	if err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	return e.start(testOnly)
}

// 第一次启动写入解析锁和托管设置、记下标记；之后重启不再写（被删掉的不自动写回，自检报告）。
func TestPrepareFirstRun(t *testing.T) {
	e := newPrepEnv(t)
	d := e.start(false)
	st := status(t, d)
	if st.Prepare == nil || !st.Prepare.Done {
		t.Fatalf("首次准备应完成：%+v", st.Prepare)
	}
	if st.ResolverLock == nil || !st.ResolverLock.Effective {
		t.Fatalf("解析锁应生效：%+v", st.ResolverLock)
	}
	if st.ClaudeCode == nil || st.ClaudeCode.Settings == nil || !st.ClaudeCode.Settings.Effective {
		t.Fatalf("托管设置应生效：%+v", st.ClaudeCode)
	}
	if _, err := os.Stat(filepath.Join(e.dir, PreparedFile)); err != nil {
		t.Fatal("应有 prepared 标记")
	}
	if !hasEvent(d, "prepare", "done") {
		t.Fatal("应有 prepare:done 事件")
	}
	// 删掉一个锁文件再重启：不自动写回（静态；只在卸载时删），自检报告 missing
	_ = os.Remove(filepath.Join(e.lock.Dir, "claude.ai"))
	d = e.restart(t, d, false)
	st = status(t, d)
	if !st.Prepare.Done || st.ResolverLock.Effective {
		t.Fatalf("重启后不应重写：prepare=%+v lock=%+v", st.Prepare, st.ResolverLock)
	}
	if hasEvent(d, "prepare", "done") {
		t.Fatal("重启后不应再做首次准备")
	}
}

// 卸载之后重启 abroadd：不写回。
func TestPrepareSkippedAfterUninstall(t *testing.T) {
	e := newPrepEnv(t)
	_ = os.WriteFile(filepath.Join(e.dir, UninstalledFile), []byte("x\n"), 0o600)
	d := e.start(false)
	st := status(t, d)
	if st.Prepare == nil || st.Prepare.Done || len(st.Prepare.Notes) == 0 {
		t.Fatalf("已卸载时不应准备：%+v", st.Prepare)
	}
	if _, err := os.Stat(e.lock.Dir); err == nil {
		t.Fatal("已卸载时不应写解析锁")
	}
	if _, err := os.Stat(e.cs.Dir); err == nil {
		t.Fatal("已卸载时不应写托管设置")
	}
}

// 只管测试目标时不写托管设置。
func TestPrepareTestOnly(t *testing.T) {
	e := newPrepEnv(t)
	e.lock.Domains = []string{resolverlock.TestDomain}
	d := e.start(true)
	st := status(t, d)
	if !st.Prepare.Done {
		t.Fatalf("%+v", st.Prepare)
	}
	if _, err := os.Stat(filepath.Join(e.lock.Dir, resolverlock.TestDomain)); err != nil {
		t.Fatal("应写测试域名的锁")
	}
	if _, err := os.Stat(filepath.Join(e.lock.Dir, "claude.ai")); err == nil {
		t.Fatal("只管测试域名时不应锁 claude.ai")
	}
	if _, err := os.Stat(e.cs.Dir); err == nil {
		t.Fatal("只管测试目标时不应写托管设置")
	}
}

// 写失败：不记标记，下次启动重试，修好后完成。
func TestPrepareRetriesAfterFailure(t *testing.T) {
	e := newPrepEnv(t)
	// 解析锁目录的位置被一个普通文件占着：MkdirAll 失败
	_ = os.WriteFile(e.lock.Dir, []byte("x"), 0o644)
	d := e.start(false)
	st := status(t, d)
	if st.Prepare.Done || !hasEvent(d, "prepare", "resolver_lock_failed") {
		t.Fatalf("应失败并记事件：%+v", st.Prepare)
	}
	if _, err := os.Stat(filepath.Join(e.dir, PreparedFile)); err == nil {
		t.Fatal("失败时不应记标记")
	}
	_ = os.Remove(e.lock.Dir)
	d = e.restart(t, d, false)
	if st = status(t, d); !st.Prepare.Done || !st.ResolverLock.Effective {
		t.Fatalf("重试应完成：%+v %+v", st.Prepare, st.ResolverLock)
	}
}

// 有别人的同名文件：不覆盖，算完成，说明里写明。
func TestPrepareConflictNotOverwritten(t *testing.T) {
	e := newPrepEnv(t)
	_ = os.MkdirAll(e.lock.Dir, 0o755)
	other := []byte("nameserver 9.9.9.9\n")
	_ = os.WriteFile(filepath.Join(e.lock.Dir, "claude.ai"), other, 0o644)
	d := e.start(false)
	st := status(t, d)
	if !st.Prepare.Done || !strings.Contains(strings.Join(st.Prepare.Notes, ""), "claude.ai") {
		t.Fatalf("%+v", st.Prepare)
	}
	if b, _ := os.ReadFile(filepath.Join(e.lock.Dir, "claude.ai")); string(b) != string(other) {
		t.Fatal("别人的文件被覆盖了")
	}
}

// 默认（测试、假系统）不做首次准备。
func TestPrepareOffByDefault(t *testing.T) {
	e := newPrepEnv(t)
	o := e.opts()
	o.Sys.ResolverLock, o.Sys.ClaudeSettings = e.lock, e.cs
	d := e.env.start(o)
	if st := status(t, d); st.Prepare != nil {
		t.Fatalf("%+v", st.Prepare)
	}
	if _, err := os.Stat(e.lock.Dir); err == nil {
		t.Fatal("没打开首次准备时不应写")
	}
}
