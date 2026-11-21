//go:build unix

package state

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// h. 同一目录第二个 Store 的 Lock 返回 ErrLocked；释放后可再拿；锁文件 0600。
func TestLockExclusive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "st")
	a, b := NewStore(dir), NewStore(dir)
	relA, err := a.Lock()
	mustOK(t, err)
	if !a.HoldsLock() || b.HoldsLock() {
		t.Fatal("HoldsLock 不对")
	}
	if _, err := b.Lock(); !errors.Is(err, ErrLocked) || !isUserError(err) {
		t.Fatalf("%v", err)
	}
	if _, err := a.Lock(); !errors.Is(err, ErrLocked) {
		t.Fatalf("同一个 Store 重复加锁：%v", err)
	}
	info, err := os.Stat(filepath.Join(dir, LockName))
	mustOK(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("锁文件权限 %o", info.Mode().Perm())
	}
	relA()
	relA() // 重复释放无害
	if a.HoldsLock() {
		t.Fatal("释放后仍持有")
	}
	relB, err := b.Lock()
	mustOK(t, err)
	relB()
}

// h. 持锁的文件描述符关闭后锁自动释放（另开一个 fd 模拟另一进程）。
func TestLockReleasedOnClose(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "st")
	s := NewStore(dir)
	mustOK(t, s.ensureDir())
	fd, err := os.OpenFile(filepath.Join(dir, LockName), os.O_RDWR|os.O_CREATE, 0o600)
	mustOK(t, err)
	mustOK(t, syscall.Flock(int(fd.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	if _, err := s.Lock(); !errors.Is(err, ErrLocked) {
		t.Fatalf("另一进程持锁时应拿不到：%v", err)
	}
	fd.Close() // “另一进程”退出
	rel, err := s.Lock()
	mustOK(t, err)
	rel()
}

// h. 锁文件是符号链接时拒绝。
func TestLockSymlink(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "st")
	s := NewStore(dir)
	mustOK(t, s.ensureDir())
	target := filepath.Join(t.TempDir(), "target")
	mustOK(t, os.WriteFile(target, nil, 0o600))
	mustOK(t, os.Symlink(target, filepath.Join(dir, LockName)))
	if _, err := s.Lock(); err == nil || !isUserError(err) {
		t.Fatalf("%v", err)
	}
	if s.HoldsLock() {
		t.Fatal("不应持有")
	}
}

// h. 没持锁时 Recover、Boot 拒绝执行且没有任何改动。
func TestRecoverBootRequireLock(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	b.crash()
	b.release()
	b.resetTimeline()
	if err := b.m.Recover(bg); !errors.Is(err, ErrNotLocked) || !isUserError(err) {
		t.Fatalf("Recover：%v", err)
	}
	st := NewStore(b.dir)
	_, pw := b.newPF(st)
	if err := Boot(bg, st, pw, b.onEvent); !errors.Is(err, ErrNotLocked) {
		t.Fatalf("Boot：%v", err)
	}
	if tl := b.tl(); len(tl) != 0 {
		t.Fatalf("没持锁却有改动：%v", tl)
	}
	b.release, _ = b.store.Lock()
}
