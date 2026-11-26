package state

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"claude-abroad/internal/srccheck"
	"claude-abroad/internal/sysnet"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(filepath.Join(t.TempDir(), "st"))
}

func fullFile() File {
	return File{
		Version:         Version,
		State:           Enabling,
		Origin:          Disabled,
		Step:            4,
		PFTokens:        []string{"1234567890", "42"},
		PFEnablePending: true,
		Utun:            "utun7",
		IPv6Original: map[string]IPv6Setting{
			"Wi-Fi":      {Mode: sysnet.IPv6Automatic},
			"Ethernet":   {Mode: sysnet.IPv6Manual, Address: netip.MustParseAddr("fd00::5"), PrefixLen: 64, Router: netip.MustParseAddr("fd00::1")},
			"Ethernet 2": {Mode: sysnet.IPv6Manual, Address: netip.MustParseAddr("2001:db8::9"), PrefixLen: 48},
			"Thunder":    {Mode: sysnet.IPv6LinkLocal},
			"USB":        {Mode: sysnet.IPv6Off},
		},
		IPv6SnapshotTaken: true,
		OrigDNS:           []netip.Addr{netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("fe80::1")},
	}
}

// b. 往返。
func TestRoundTrip(t *testing.T) {
	s := newStore(t)
	f, err := s.Load()
	if err != nil || f.State != Restored || f.Version != Version {
		t.Fatalf("文件不存在应读到 RESTORED：%+v %v", f, err)
	}
	want := fullFile()
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("往返不等：\n%+v\n%+v", got, want)
	}
	// 四种模式都在里面，而且 Manual 的路由器可选。
	modes := map[sysnet.IPv6Mode]bool{}
	for _, v := range got.IPv6Original {
		modes[v.Mode] = true
	}
	if len(modes) != 4 {
		t.Fatalf("模式不全：%v", modes)
	}
	// 目录存在、文件不存在也是 RESTORED。
	if err := os.Remove(filepath.Join(s.Dir, FileName)); err != nil {
		t.Fatal(err)
	}
	if f, err := s.Load(); err != nil || f.State != Restored {
		t.Fatalf("%+v %v", f, err)
	}
}

func TestSaveRejectsInvalid(t *testing.T) {
	s := newStore(t)
	bad := []File{
		{State: "WHAT"},
		{State: Enabled, Origin: "X"},
		{State: Enabling, Origin: Enabled},
		{State: Enabling, Origin: Enabling},
		{State: Enabling, Origin: Restoring},
		{State: Enabled, Step: -1},
		{State: Enabled, PFTokens: []string{"abc"}},
		{State: Enabled, IPv6Original: map[string]IPv6Setting{"x": {Mode: sysnet.IPv6Manual}}},
		{State: Enabled, IPv6Original: map[string]IPv6Setting{"x": {Mode: sysnet.IPv6Off, PrefixLen: 64}}},
		{State: Enabled, IPv6Original: map[string]IPv6Setting{"x": {Mode: sysnet.IPv6Manual, Address: netip.MustParseAddr("10.0.0.1"), PrefixLen: 24}}},
	}
	for i, f := range bad {
		if err := s.Save(f); err == nil {
			t.Errorf("第 %d 个不合法内容被写入了", i)
		}
	}
	if _, err := os.Stat(filepath.Join(s.Dir, FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("不合法内容不应落盘")
	}
}

func checkCorrupt(t *testing.T, name string, s *Store) {
	t.Helper()
	f, err := s.Load()
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("%s：应返回 ErrCorrupt，实际 %+v %v", name, f, err)
	}
	if f.State == Restored {
		t.Fatalf("%s：读成了 RESTORED", name)
	}
	// usererror 的固定格式：“做什么失败：原因。下一步”。
	if msg := err.Error(); !strings.HasPrefix(msg, "读取状态文件失败：") || !strings.Contains(msg, "abroad-rescue") {
		t.Fatalf("%s：不是 usererror 的格式：%s", name, msg)
	}
}

// c. 各种坏文件都返回 ErrCorrupt，不读成 RESTORED。
func TestLoadCorrupt(t *testing.T) {
	writeRaw := func(t *testing.T, content string) *Store {
		s := newStore(t)
		if err := os.Mkdir(s.Dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.Dir, FileName), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return s
	}
	cases := map[string]string{
		"坏 JSON":        `{"version":1,"state":`,
		"空文件":           ``,
		"未知 version":    `{"version":2,"state":"ENABLED","step":0}`,
		"缺 version":     `{"state":"ENABLED","step":0}`,
		"未知 state":      `{"version":1,"state":"BROKEN","step":0}`,
		"未知字段":          `{"version":1,"state":"ENABLED","step":0,"exit_ip":"1.2.3.4"}`,
		"末尾多余内容":        `{"version":1,"state":"ENABLED","step":0}{}`,
		"不是对象":          `[]`,
		"来源是 ENABLED":   `{"version":1,"state":"ENABLING","origin":"ENABLED","step":0}`,
		"来源是 ENABLING":  `{"version":1,"state":"ENABLING","origin":"ENABLING","step":0}`,
		"来源是 RESTORING": `{"version":1,"state":"ENABLING","origin":"RESTORING","step":0}`,
		"IPv6 原值不合法":    `{"version":1,"state":"ENABLED","step":0,"ipv6_original":{"Wi-Fi":{"mode":"bogus"}}}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) { checkCorrupt(t, name, writeRaw(t, content)) })
	}

	t.Run("超 1 MiB", func(t *testing.T) {
		big := `{"version":1,"state":"ENABLED","step":0,"utun":"` + strings.Repeat("a", MaxSize) + `"}`
		checkCorrupt(t, "超 1 MiB", writeRaw(t, big))
	})
	t.Run("符号链接", func(t *testing.T) {
		s := newStore(t)
		if err := os.Mkdir(s.Dir, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "real.json")
		if err := os.WriteFile(target, []byte(`{"version":1,"state":"RESTORED","step":0}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(s.Dir, FileName)); err != nil {
			t.Fatal(err)
		}
		checkCorrupt(t, "符号链接", s)
	})
	t.Run("目录", func(t *testing.T) {
		s := newStore(t)
		if err := os.MkdirAll(filepath.Join(s.Dir, FileName), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(s.Dir, 0o700); err != nil {
			t.Fatal(err)
		}
		checkCorrupt(t, "目录", s)
	})
	t.Run("权限 0644", func(t *testing.T) {
		s := writeRaw(t, `{"version":1,"state":"ENABLED","step":0}`)
		if err := os.Chmod(filepath.Join(s.Dir, FileName), 0o644); err != nil {
			t.Fatal(err)
		}
		checkCorrupt(t, "权限 0644", s)
	})
	t.Run("目录权限 0755", func(t *testing.T) {
		s := writeRaw(t, `{"version":1,"state":"ENABLED","step":0}`)
		if err := os.Chmod(s.Dir, 0o755); err != nil {
			t.Fatal(err)
		}
		checkCorrupt(t, "目录权限 0755", s)
	})
	t.Run("目录权限 0755 且文件不存在", func(t *testing.T) {
		// 目录本身不安全时，不能因为文件不存在就当成 RESTORED。
		s := newStore(t)
		if err := os.Mkdir(s.Dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(s.Dir, 0o755); err != nil {
			t.Fatal(err)
		}
		checkCorrupt(t, "目录权限 0755 且文件不存在", s)
	})
	t.Run("属主不对", func(t *testing.T) {
		s := writeRaw(t, `{"version":1,"state":"ENABLED","step":0}`)
		s.UID = os.Getuid() + 1
		checkCorrupt(t, "属主不对", s)
	})
	t.Run("状态目录是符号链接", func(t *testing.T) {
		real := filepath.Join(t.TempDir(), "real")
		if err := os.Mkdir(real, 0o700); err != nil {
			t.Fatal(err)
		}
		s := newStore(t)
		if err := os.Symlink(real, s.Dir); err != nil {
			t.Fatal(err)
		}
		checkCorrupt(t, "状态目录是符号链接", s)
	})
}

func tempLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if e.Name() != FileName {
			out = append(out, e.Name())
		}
	}
	return out
}

// d. 原子写。
func TestSaveAtomic(t *testing.T) {
	t.Run("权限与残留", func(t *testing.T) {
		s := newStore(t)
		for i := range 3 {
			if err := s.Save(File{State: Enabled, Step: i}); err != nil {
				t.Fatal(err)
			}
		}
		info, err := os.Stat(filepath.Join(s.Dir, FileName))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("文件权限 %o", info.Mode().Perm())
		}
		dinfo, _ := os.Stat(s.Dir)
		if dinfo.Mode().Perm() != 0o700 {
			t.Fatalf("目录权限 %o", dinfo.Mode().Perm())
		}
		if left := tempLeftovers(t, s.Dir); len(left) != 0 {
			t.Fatalf("残留临时文件：%v", left)
		}
	})
	t.Run("目标是符号链接", func(t *testing.T) {
		s := newStore(t)
		if err := s.Save(File{State: Enabled}); err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(t.TempDir(), "victim")
		const orig = "不能被改"
		if err := os.WriteFile(victim, []byte(orig), 0o600); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(s.Dir, FileName)
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, p); err != nil {
			t.Fatal(err)
		}
		if err := s.Save(File{State: Disabled}); err == nil {
			t.Fatal("目标是符号链接时应拒绝")
		}
		if b, _ := os.ReadFile(victim); string(b) != orig {
			t.Fatalf("链接指向的文件被改了：%q", b)
		}
		if fi, _ := os.Lstat(p); fi.Mode()&os.ModeSymlink == 0 {
			t.Fatal("链接本身被替换了")
		}
		if left := tempLeftovers(t, s.Dir); len(left) != 0 {
			t.Fatalf("残留临时文件：%v", left)
		}
	})
	t.Run("rename 前失败", func(t *testing.T) {
		s := newStore(t)
		old := fullFile()
		if err := s.Save(old); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(filepath.Join(s.Dir, FileName))
		boom := errors.New("boom")
		s.beforeRename = func() error { return boom }
		if err := s.Save(File{State: Restored}); !errors.Is(err, boom) {
			t.Fatalf("应返回注入的错误：%v", err)
		}
		after, _ := os.ReadFile(filepath.Join(s.Dir, FileName))
		if string(before) != string(after) {
			t.Fatal("旧文件内容被改了")
		}
		s.beforeRename = nil
		if got, err := s.Load(); err != nil || !reflect.DeepEqual(got, old) {
			t.Fatalf("%+v %v", got, err)
		}
		if left := tempLeftovers(t, s.Dir); len(left) != 0 {
			t.Fatalf("残留临时文件：%v", left)
		}
	})
	t.Run("并发", func(t *testing.T) {
		s := newStore(t)
		if err := s.Save(File{State: Enabled, Utun: "utun0"}); err != nil {
			t.Fatal(err)
		}
		// 另一个 Store 实例（不共用锁）读同一个目录，模拟另一个进程：靠 rename 的原子性保证读到完整内容。
		other := NewStore(s.Dir)
		const writers, rounds = 4, 25
		valid := map[string]bool{"utun0": true}
		for w := range writers {
			for r := range rounds {
				valid[fmt.Sprintf("utun%d", 100+w*rounds+r)] = true
			}
		}
		var wg sync.WaitGroup
		errs := make(chan error, 64)
		for w := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for r := range rounds {
					n := 100 + w*rounds + r
					f := File{State: Enabled, Step: n, Utun: fmt.Sprintf("utun%d", n), PFTokens: []string{fmt.Sprint(n)}}
					if err := s.Save(f); err != nil {
						errs <- err
						return
					}
				}
			}()
		}
		stop := make(chan struct{})
		var rg sync.WaitGroup
		for _, rd := range []*Store{s, other} {
			rg.Add(1)
			go func() {
				defer rg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					f, err := rd.Load()
					if err != nil {
						errs <- err
						return
					}
					if !valid[f.Utun] || (f.Utun != "utun0" && (f.Utun != fmt.Sprintf("utun%d", f.Step) || len(f.PFTokens) != 1)) {
						errs <- fmt.Errorf("读到不完整的内容：%+v", f)
						return
					}
				}
			}()
		}
		wg.Wait()
		close(stop)
		rg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		if left := tempLeftovers(t, s.Dir); len(left) != 0 {
			t.Fatalf("残留临时文件：%v", left)
		}
	})
}

// e. IPv6 快照只写一次。
func TestIPv6SnapshotOnce(t *testing.T) {
	s := newStore(t)
	if err := s.Save(File{State: Enabling, Origin: Restored}); err != nil {
		t.Fatal(err)
	}
	first := map[string]IPv6Setting{"Wi-Fi": {Mode: sysnet.IPv6Automatic}, "Ethernet": {Mode: sysnet.IPv6Manual, Address: netip.MustParseAddr("fd00::5"), PrefixLen: 64}}
	if wrote, err := s.TakeIPv6Snapshot(first); err != nil || !wrote {
		t.Fatalf("第一次：%v %v", wrote, err)
	}
	// 重入：这时 IPv6 已经被关掉了，不能覆盖。
	second := map[string]IPv6Setting{"Wi-Fi": {Mode: sysnet.IPv6Off}, "Ethernet": {Mode: sysnet.IPv6Off}}
	if wrote, err := s.TakeIPv6Snapshot(second); err != nil || wrote {
		t.Fatalf("第二次：%v %v", wrote, err)
	}
	f, err := s.Load()
	if err != nil || !reflect.DeepEqual(f.IPv6Original, first) || !f.IPv6SnapshotTaken {
		t.Fatalf("%+v %v", f, err)
	}
	// 写 RESTORED 清掉后可以重新拍。
	if err := s.MarkRestored(); err != nil {
		t.Fatal(err)
	}
	if f, _ := s.Load(); f.IPv6SnapshotTaken || f.IPv6Original != nil || f.State != Restored {
		t.Fatalf("没清干净：%+v", f)
	}
	if wrote, err := s.TakeIPv6Snapshot(second); err != nil || !wrote {
		t.Fatalf("新周期：%v %v", wrote, err)
	}
	if f, _ := s.Load(); !reflect.DeepEqual(f.IPv6Original, second) {
		t.Fatalf("%+v", f)
	}
	// 读到坏文件时不写。
	if err := os.WriteFile(filepath.Join(s.Dir, FileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if wrote, err := s.TakeIPv6Snapshot(first); !errors.Is(err, ErrCorrupt) || wrote {
		t.Fatalf("坏文件：%v %v", wrote, err)
	}
}

// f. pending 与 token。
func TestEnablePendingAndTokens(t *testing.T) {
	s := newStore(t)
	if err := s.Save(File{State: Enabling, Origin: Restored, PFTokens: []string{"7"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnablePending(); err != nil {
		t.Fatal(err)
	}
	f, err := s.Load()
	if err != nil || !f.PFEnablePending || !reflect.DeepEqual(f.PFTokens, []string{"7"}) {
		t.Fatalf("置 pending：%+v %v", f, err)
	}
	if err := s.AddToken("7"); err != nil { // 同一个值也追加，不去重
		t.Fatal(err)
	}
	f, err = s.Load()
	if err != nil || f.PFEnablePending || !reflect.DeepEqual(f.PFTokens, []string{"7", "7"}) {
		t.Fatalf("追加 token：%+v %v", f, err)
	}
	if err := s.SetEnablePending(); err != nil {
		t.Fatal(err)
	}
	if err := s.AddToken("99"); err != nil {
		t.Fatal(err)
	}
	f, _ = s.Load()
	if f.PFEnablePending || !reflect.DeepEqual(f.PFTokens, []string{"7", "7", "99"}) || f.State != Enabling || f.Origin != Restored {
		t.Fatalf("%+v", f)
	}
	if err := s.AddToken("not-a-number"); err == nil {
		t.Fatal("非数字 token 应拒绝")
	}
	if f2, _ := s.Load(); !reflect.DeepEqual(f2, f) {
		t.Fatal("拒绝后文件被改了")
	}
}

// h. 源码检查。
func TestNoExecInSources(t *testing.T) { srccheck.NoExec(t, ".") }

// 7b 第 0 步：目录 fsync 失败时 Save 报错。
func TestSaveDirSyncFailure(t *testing.T) {
	s := newStore(t)
	boom := errors.New("boom")
	s.syncDir = func(string) error { return boom }
	if err := s.Save(File{State: Enabling, Origin: Restored}); !errors.Is(err, boom) {
		t.Fatalf("应返回目录落盘的错误：%v", err)
	}
	if err := s.Update(func(f *File) error { f.Step = 3; return nil }); !errors.Is(err, boom) {
		t.Fatalf("Update 也应返回：%v", err)
	}
	s.syncDir = nil
	if err := s.Save(File{State: Enabling, Origin: Restored}); err != nil {
		t.Fatal(err)
	}
}

func TestClearEnablePending(t *testing.T) {
	s := newStore(t)
	if err := s.Save(File{State: Enabling, Origin: Restored, PFTokens: []string{"5"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnablePending(); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearEnablePending(); err != nil {
		t.Fatal(err)
	}
	f, err := s.Load()
	if err != nil || f.PFEnablePending || !reflect.DeepEqual(f.PFTokens, []string{"5"}) || f.State != Enabling {
		t.Fatalf("%+v %v", f, err)
	}
}
