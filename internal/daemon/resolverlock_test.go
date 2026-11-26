package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claude-abroad/internal/resolverlock"
)

func lockSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	m := map[string]string{}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		info, _ := e.Info()
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		m[e.Name()] = info.Mode().String() + "|" + info.ModTime().String() + "|" + string(b)
	}
	return m
}

// d、e：状态接口带解析锁的四个字段；启用、停用、再启用、回国全流程不碰锁目录（内容和修改时间不变）；
// 自检发现被改时记“未生效”事件，不自动重写。
func TestResolverLockInDaemon(t *testing.T) {
	e := newEnv(t)
	lockDir := filepath.Join(filepath.Dir(e.dir), "resolver")
	lk := &resolverlock.Lock{Dir: lockDir, RecordDir: filepath.Dir(e.dir)}
	if _, err := lk.Write(); err != nil {
		t.Fatal(err)
	}
	before := lockSnapshot(t, lockDir)
	o := e.opts()
	o.Sys.ResolverLock = lk
	d := e.start(o)
	st := status(t, d)
	if st.ResolverLock == nil || !st.ResolverLock.Written || !st.ResolverLock.Effective || st.ResolverLock.Form != "default" ||
		strings.Join(st.ResolverLock.Domains, ",") != strings.Join(resolverlock.Domains(), ",") || len(st.ResolverLock.Domains) != 5 {
		t.Fatalf("状态 %+v", st.ResolverLock)
	}
	_, raw := call(t, d, "GET", "/v1/status", nil)
	if strings.Contains(string(raw), lockDir) || strings.Contains(string(raw), "198.18.0.2") {
		t.Fatalf("状态里不应有路径和锁的内容：%s", raw)
	}
	post(t, d, "/v1/enable", 200)
	post(t, d, "/v1/disable", 200)
	post(t, d, "/v1/enable", 200)
	post(t, d, "/v1/restore", 200)
	if after := lockSnapshot(t, lockDir); len(after) != len(before) {
		t.Fatalf("锁目录被改了：%v → %v", before, after)
	} else {
		for k, v := range before {
			if after[k] != v {
				t.Fatalf("锁文件 %s 被改了", k)
			}
		}
	}
	// 被人改了：状态显示未生效、记事件；不自动重写
	_ = os.WriteFile(filepath.Join(lockDir, "claude.ai"), []byte("nameserver 8.8.8.8\n"), 0o644)
	st = status(t, d)
	if st.ResolverLock.Effective || len(st.ResolverLock.Problems) != 1 || st.ResolverLock.Problems[0].Kind != "modified" {
		t.Fatalf("被改后 %+v", st.ResolverLock)
	}
	d.checkResolverLock()
	if !hasEvent(d, "resolver_lock", "not_effective") {
		t.Fatal("应记“未生效”事件")
	}
	if b, _ := os.ReadFile(filepath.Join(lockDir, "claude.ai")); string(b) != "nameserver 8.8.8.8\n" {
		t.Fatal("不应自动重写")
	}
}

// 没接解析锁（开发工具、旧测试）：状态里没有这一项。
func TestNoResolverLock(t *testing.T) {
	e := newEnv(t)
	d := e.start(e.opts())
	if status(t, d).ResolverLock != nil {
		t.Fatal("没接时不应有解析锁状态")
	}
}
