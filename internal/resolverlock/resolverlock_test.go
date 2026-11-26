package resolverlock

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"claude-abroad/internal/rules"
)

func newLock(t *testing.T) *Lock {
	t.Helper()
	base := t.TempDir()
	rec := filepath.Join(base, "st")
	_ = os.Mkdir(rec, 0o700)
	return &Lock{Dir: filepath.Join(base, "resolver"), RecordDir: rec}
}

// a. 两种写法逐字固定；不合法的写法拒绝。
func TestContent(t *testing.T) {
	b, err := Content(Default())
	if err != nil || string(b) != "nameserver 198.18.0.2\ntimeout 1\n" {
		t.Fatalf("默认写法：%q %v", b, err)
	}
	b, err = Content(Loopback(5353))
	if err != nil || string(b) != "nameserver 127.0.0.1\nport 5353\ntimeout 1\n" {
		t.Fatalf("回环写法：%q %v", b, err)
	}
	for _, f := range []Form{Loopback(0), {Addr: DefaultAddr, Port: 53}, {}} {
		if _, err := Content(f); err == nil {
			t.Errorf("%+v 应拒绝", f)
		}
	}
	if Default().Name() != "default" || Loopback(1).Name() != "loopback_port" {
		t.Fatal("写法名字不对")
	}
}

// a. 域名清单等于内置的置顶域名（和规则引擎同一来源）；用户追加的 pinned_extra 不影响。
func TestDomainsFromBuiltinPinned(t *testing.T) {
	var bases []string
	for _, p := range rules.BuiltinPinned() {
		b := strings.TrimPrefix(p, "*.")
		if !slices.Contains(bases, b) {
			bases = append(bases, b)
		}
	}
	if !slices.Equal(Domains(), bases) || !slices.Equal(Domains(), []string{"claude.ai", "claude.com", "anthropic.com", "claudeusercontent.com", "claudemcpcontent.com"}) {
		t.Fatalf("锁的域名 %v，内置置顶 %v", Domains(), bases)
	}
	// pinned_extra 不进锁：解析一份带追加名单的规则，锁的域名不变
	if _, err := rules.Parse([]byte("pinned_extra: [example.org]\n")); err != nil {
		t.Fatalf("规则：%v", err)
	}
	if slices.Contains(Domains(), "example.org") {
		t.Fatal("用户追加的置顶域名不应进锁")
	}
}

// b. 写入后文件齐全、权限对；重复写入结果相同；Check 生效。
func TestWriteIdempotent(t *testing.T) {
	l := newLock(t)
	res, err := l.Write()
	if err != nil || len(res.Written) != len(Domains()) || len(res.Conflicts) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	want, _ := Content(Default())
	for _, d := range Domains() {
		info, err := os.Stat(filepath.Join(l.Dir, d))
		if err != nil || info.Mode().Perm() != 0o644 {
			t.Fatalf("%s：%v %v", d, err, info)
		}
		b, _ := os.ReadFile(filepath.Join(l.Dir, d))
		if string(b) != string(want) {
			t.Fatalf("%s 内容 %q", d, b)
		}
	}
	before := snapshot(t, l.Dir)
	if _, err := l.Write(); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, l.Dir); !mapsEqual(before, after) {
		t.Fatal("重复写入结果应相同")
	}
	if st := l.Check(); !st.Effective || !st.Written || st.Form != "default" || len(st.Problems) != 0 {
		t.Fatalf("自检 %+v", st)
	}
	// 换成回环写法再写：覆盖我们自己写的旧内容（不是冲突）
	l.Form = Loopback(5353)
	if res, err := l.Write(); err != nil || len(res.Written) != len(Domains()) {
		t.Fatalf("换写法：%+v %v", res, err)
	}
	if st := l.Check(); !st.Effective || st.Form != "loopback_port" {
		t.Fatalf("换写法后自检 %+v", st)
	}
}

// b. 已有别人的同名文件：不覆盖，报告是哪个域名；删除时留下别人的和被改过的。
func TestConflictsAndRemove(t *testing.T) {
	l := newLock(t)
	_ = os.MkdirAll(l.Dir, 0o755)
	other := []byte("nameserver 10.0.0.1\n")
	_ = os.WriteFile(filepath.Join(l.Dir, "claude.com"), other, 0o644)
	res, err := l.Write()
	if err != ErrConflict || !slices.Equal(res.Conflicts, []string{"claude.com"}) || len(res.Written) != len(Domains())-1 {
		t.Fatalf("%+v %v", res, err)
	}
	if b, _ := os.ReadFile(filepath.Join(l.Dir, "claude.com")); string(b) != string(other) {
		t.Fatal("别人的文件被覆盖了")
	}
	st := l.Check()
	if st.Effective || !slices.Equal(st.Conflicts, []string{"claude.com"}) {
		t.Fatalf("自检应报冲突：%+v", st)
	}
	// 我们写的 anthropic.com 被人改了
	_ = os.WriteFile(filepath.Join(l.Dir, "anthropic.com"), []byte("nameserver 1.1.1.1\n"), 0o644)
	rr, err := l.Remove()
	if err != nil || !slices.Equal(rr.Removed, []string{"claude.ai", "claudeusercontent.com", "claudemcpcontent.com"}) || !slices.Contains(rr.Left, "claude.com") || !slices.Contains(rr.Left, "anthropic.com") {
		t.Fatalf("删除 %+v %v", rr, err)
	}
	for _, d := range []string{"claude.com", "anthropic.com"} {
		if _, err := os.Stat(filepath.Join(l.Dir, d)); err != nil {
			t.Fatalf("%s 应留下", d)
		}
	}
	if _, err := os.Stat(filepath.Join(l.Dir, "claude.ai")); !os.IsNotExist(err) {
		t.Fatal("我们写的应删掉")
	}
	if _, err := os.Stat(filepath.Join(l.RecordDir, RecordFile)); !os.IsNotExist(err) {
		t.Fatal("删除后记录应删掉")
	}
	// 再删一次：无事可做
	if rr, err := l.Remove(); err != nil || len(rr.Removed)+len(rr.Left) != 0 {
		t.Fatalf("重复删除 %+v %v", rr, err)
	}
}

// b. 写到一半失败（第二个文件写不进去）：再次写入能补齐，删除能收拾干净。
func TestPartialWriteRecoverable(t *testing.T) {
	l := newLock(t)
	_ = os.MkdirAll(l.Dir, 0o755)
	// 让 claude.com 写不进去：同名的目录
	blocker := filepath.Join(l.Dir, "claude.com")
	_ = os.Mkdir(blocker, 0o755)
	if _, err := l.Write(); err == nil {
		t.Fatal("应写入失败")
	}
	if _, err := os.Stat(filepath.Join(l.Dir, "claude.ai")); err != nil {
		t.Fatal("第一个应已写入（用来验证收拾）")
	}
	// 删除能收拾已写的
	if rr, err := l.Remove(); err != nil || !slices.Contains(rr.Removed, "claude.ai") {
		t.Fatalf("删除 %+v %v", rr, err)
	}
	// 障碍去掉后再写：齐全
	_ = os.Remove(blocker)
	if res, err := l.Write(); err != nil || len(res.Written) != len(Domains()) {
		t.Fatalf("再写 %+v %v", res, err)
	}
}

// c. 自检：缺失、被改、权限不对各一条；自检不写任何文件。
func TestCheckProblems(t *testing.T) {
	l := newLock(t)
	if st := l.Check(); st.Written || st.Effective || len(st.Problems) != 0 {
		t.Fatalf("没写过：%+v", st)
	}
	if _, err := l.Write(); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(l.Dir, "claude.ai"))
	_ = os.WriteFile(filepath.Join(l.Dir, "claude.com"), []byte("nameserver 8.8.8.8\n"), 0o644)
	_ = os.Chmod(filepath.Join(l.Dir, "anthropic.com"), 0o666)
	before := snapshot(t, l.Dir)
	beforeRec, _ := os.ReadFile(filepath.Join(l.RecordDir, RecordFile))
	st := l.Check()
	kinds := map[string]string{}
	for _, p := range st.Problems {
		kinds[p.Domain] = p.Kind
	}
	if st.Effective || kinds["claude.ai"] != "missing" || kinds["claude.com"] != "modified" || kinds["anthropic.com"] != "permission" {
		t.Fatalf("自检 %+v", st)
	}
	if !mapsEqual(before, snapshot(t, l.Dir)) {
		t.Fatal("自检改动了锁目录")
	}
	if b, _ := os.ReadFile(filepath.Join(l.RecordDir, RecordFile)); string(b) != string(beforeRec) {
		t.Fatal("自检改动了记录")
	}
}

// 测试域名、不合法的域名。
func TestDomainsOverride(t *testing.T) {
	l := newLock(t)
	l.Domains = []string{TestDomain}
	if res, err := l.Write(); err != nil || !slices.Equal(res.Written, []string{TestDomain}) {
		t.Fatalf("%+v %v", res, err)
	}
	for _, bad := range []string{"../etc", "a/b", "", "Claude.AI"} {
		l2 := newLock(t)
		l2.Domains = []string{bad}
		if _, err := l2.Write(); err == nil {
			t.Errorf("%q 应拒绝", bad)
		}
	}
}

func snapshot(t *testing.T, dir string) map[string]string {
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

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// 卸载后不留空目录：锁目录是我们建的、删完空了就一并删掉；原来就有的留下。
func TestRemoveCleansCreatedDir(t *testing.T) {
	l := newLock(t)
	if _, err := l.Write(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.Dir); !os.IsNotExist(err) {
		t.Fatal("我们建的空目录应删掉")
	}
	l2 := newLock(t)
	_ = os.MkdirAll(l2.Dir, 0o755)
	_, _ = l2.Write()
	_, _ = l2.Remove()
	if _, err := os.Stat(l2.Dir); err != nil {
		t.Fatal("原来就有的目录不应删")
	}
}

// M6-12：名单变长后补写——只写新多出来的；以前写过、后来被删的不重写；新域名有别人的同名文件时不覆盖、报冲突；
// 没写过（没有记录）时不补写。
func TestBackfill(t *testing.T) {
	dir := t.TempDir()
	old := &Lock{Dir: filepath.Join(dir, "resolver"), RecordDir: dir, Domains: []string{"claude.ai", "claude.com"}}
	if miss, _ := old.Missing(); len(miss) != 0 {
		t.Fatalf("没有记录时不应补写：%v", miss)
	}
	if _, err := old.Write(); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(old.Dir, "claude.com")) // 以前写过、后来不见了
	cur := &Lock{Dir: old.Dir, RecordDir: dir, Domains: []string{"claude.ai", "claude.com", "claudeusercontent.com"}}
	miss, err := cur.Missing()
	if err != nil || !slices.Equal(miss, []string{"claudeusercontent.com"}) {
		t.Fatalf("Missing：%v %v", miss, err)
	}
	res, err := cur.Backfill()
	if err != nil || !slices.Equal(res.Written, []string{"claudeusercontent.com"}) {
		t.Fatalf("应只补写新多出来的：%+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(old.Dir, "claude.com")); !os.IsNotExist(err) {
		t.Fatal("以前写过、后来被删的不应自动重写")
	}
	if miss, _ := cur.Missing(); len(miss) != 0 {
		t.Fatalf("补写后记录应包含新域名：%v", miss)
	}
	// 新域名有别人的同名文件：不覆盖、报冲突
	more := &Lock{Dir: old.Dir, RecordDir: dir, Domains: []string{"claude.ai", "claude.com", "claudeusercontent.com", "claudemcpcontent.com"}}
	other := []byte("nameserver 10.0.0.1\n")
	_ = os.WriteFile(filepath.Join(old.Dir, "claudemcpcontent.com"), other, 0o644)
	if res, err := more.Backfill(); err != ErrConflict || !slices.Equal(res.Conflicts, []string{"claudemcpcontent.com"}) {
		t.Fatalf("冲突：%+v %v", res, err)
	}
	if b, _ := os.ReadFile(filepath.Join(old.Dir, "claudemcpcontent.com")); string(b) != string(other) {
		t.Fatal("别人的文件被覆盖了")
	}
	// 冲突处理掉之后再补写：能补上
	_ = os.Remove(filepath.Join(old.Dir, "claudemcpcontent.com"))
	if res, err := more.Backfill(); err != nil || !slices.Equal(res.Written, []string{"claudemcpcontent.com"}) {
		t.Fatalf("冲突处理掉后应能补上：%+v %v", res, err)
	}
}
