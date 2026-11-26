package daemon

import (
	"claude-abroad/internal/preflight"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	o.Sys.Preflight = PreflightHints{C: &preflight.Checker{R: cleanRunner{e.t}}} // 首次准备没完成时阻止启用（M6-8a 补审返工）
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

// M6-8a 补审返工：解析锁有别人的同名文件——不覆盖、不算完成（没有标记、done 为假、说明写明文件和下一步），启用被挡住；
// 处理掉之后不重启，“重新检测”（冲突检查）时再试一次就完成，启用通过。
func TestPrepareResolverConflictBlocksUntilFixed(t *testing.T) {
	e := newPrepEnv(t)
	_ = os.MkdirAll(e.lock.Dir, 0o755)
	other := []byte("nameserver 9.9.9.9\n")
	conflict := filepath.Join(e.lock.Dir, "claude.ai")
	_ = os.WriteFile(conflict, other, 0o644)
	d := e.start(false)
	st := status(t, d)
	notes := strings.Join(st.Prepare.Notes, "")
	if st.Prepare.Done || !strings.Contains(notes, "claude.ai") || !strings.Contains(notes, "重新检测") {
		t.Fatalf("冲突时不应算完成：%+v", st.Prepare)
	}
	if _, err := os.Stat(filepath.Join(e.dir, PreparedFile)); err == nil {
		t.Fatal("冲突时不应写完成标记")
	}
	if b, _ := os.ReadFile(conflict); string(b) != string(other) {
		t.Fatal("别人的文件被覆盖了")
	}
	code, b := callOp(t, d, "/v1/enable")
	if code != 422 || !strings.Contains(string(b), "首次准备没有完成") || !strings.Contains(string(b), "claude.ai") {
		t.Fatalf("首次准备没完成时应挡住启用：%d %s", code, b)
	}
	// 处理掉冲突，不重启：点“重新检测”（冲突检查）
	_ = os.Remove(conflict)
	if code, raw := call(t, d, "GET", "/v1/preflight", nil); code != 200 || strings.Contains(string(raw), "首次准备没有完成") {
		t.Fatalf("重新检测时应再试一次并完成：%d %s", code, raw)
	}
	if st := status(t, d); !st.Prepare.Done || !st.ResolverLock.Effective {
		t.Fatalf("应已完成：%+v %+v", st.Prepare, st.ResolverLock)
	}
	post(t, d, "/v1/enable", 200)
}

// 托管设置有冲突（别的分片定义了代理变量）：同样不算完成、挡住启用；解析锁已写的留着，重试时不重复写。
func TestPrepareSettingsConflictPartial(t *testing.T) {
	e := newPrepEnv(t)
	_ = os.MkdirAll(e.cs.Dir, 0o755)
	foreign := filepath.Join(e.cs.Dir, "corp.json")
	_ = os.WriteFile(foreign, []byte(`{"env":{"HTTPS_PROXY":"http://proxy.corp.example:8080"}}`), 0o644)
	d := e.start(false)
	st := status(t, d)
	if st.Prepare.Done || !strings.Contains(strings.Join(st.Prepare.Notes, ""), "corp.json 的 HTTPS_PROXY") {
		t.Fatalf("托管设置冲突时不应算完成：%+v", st.Prepare)
	}
	if !st.ResolverLock.Effective {
		t.Fatal("解析锁已写的应留着")
	}
	lockFile := filepath.Join(e.lock.Dir, "claude.ai")
	fi1, _ := os.Stat(lockFile)
	if code, b := callOp(t, d, "/v1/enable"); code != 422 || !strings.Contains(string(b), "corp.json") {
		t.Fatalf("应挡住启用：%d %s", code, b)
	}
	_ = os.Remove(foreign)
	time.Sleep(20 * time.Millisecond)
	post(t, d, "/v1/enable", 200) // 启用前的检查里再试一次，完成后放行
	if st := status(t, d); !st.Prepare.Done || st.ClaudeCode == nil || st.ClaudeCode.Settings == nil || !st.ClaudeCode.Settings.Effective {
		t.Fatalf("重试后应完成：%+v", st.Prepare)
	}
	if fi2, _ := os.Stat(lockFile); !fi2.ModTime().Equal(fi1.ModTime()) {
		t.Fatal("已写的解析锁重试时不应重写")
	}
}

// 完成之后才出现的冲突（别人后来写了文件）：完成标记不回退，行为和以前一样（自检报告）。
func TestPrepareConflictAfterDone(t *testing.T) {
	e := newPrepEnv(t)
	d := e.start(false)
	if !status(t, d).Prepare.Done {
		t.Fatal("应先完成")
	}
	_ = os.WriteFile(filepath.Join(e.lock.Dir, "claude.ai"), []byte("nameserver 9.9.9.9\n"), 0o644)
	if code, raw := call(t, d, "GET", "/v1/preflight", nil); code != 200 || strings.Contains(string(raw), "首次准备没有完成") {
		t.Fatalf("完成之后不应再有首次准备这一项：%d %s", code, raw)
	}
	if st := status(t, d); !st.Prepare.Done {
		t.Fatal("完成标记不应回退")
	}
	if _, err := os.Stat(filepath.Join(e.dir, PreparedFile)); err != nil {
		t.Fatal("标记应还在")
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
