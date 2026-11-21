package fakedns

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// f. 落盘往返
func TestSaveLoadRoundTrip(t *testing.T) {
	p := DefaultPool()
	names := []string{"claude.ai", "api.anthropic.com", "example.com", "_dmarc.example.org"}
	addrs := map[string]netip.Addr{}
	for i, n := range names {
		addrs[n] = mustAssign(t, p, n, t0.Add(time.Duration(i)*time.Second))
	}
	var buf bytes.Buffer
	if err := p.Save(&buf); err != nil {
		t.Fatal(err)
	}
	q := DefaultPool()
	if err := q.Load(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	for i, n := range names {
		if got, ok := q.Lookup(addrs[n]); !ok || got != n {
			t.Fatalf("加载后 %s 应映射到 %s：%q", addrs[n], n, got)
		}
		q.mu.Lock()
		last, refs := q.byName[n].last, q.byName[n].refs
		q.mu.Unlock()
		if !last.Equal(t0.Add(time.Duration(i)*time.Second)) || refs != 0 {
			t.Fatalf("最近使用时间或引用数不对：%s %d", last, refs)
		}
	}
	if a := mustAssign(t, q, "claude.ai", t0.Add(time.Hour)); a != addrs["claude.ai"] {
		t.Fatal("老域名应得到原地址")
	}
	nw := mustAssign(t, q, "new.example.net", t0.Add(time.Hour))
	for _, a := range addrs {
		if a == nw {
			t.Fatal("新域名不应与已加载的地址相撞")
		}
	}
}

// build 按文件格式拼出内容（可故意写坏），校验和总是对的。
func build(version uint16, count uint32, recs []record, magic string) []byte {
	var b bytes.Buffer
	b.WriteString(magic)
	binary.Write(&b, binary.BigEndian, version)
	binary.Write(&b, binary.BigEndian, count)
	for _, r := range recs {
		a := r.addr.As4()
		b.Write(a[:])
		binary.Write(&b, binary.BigEndian, r.last.UnixNano())
		b.WriteByte(byte(len(r.name)))
		b.WriteString(r.name)
		if version >= 2 {
			var f byte
			if r.local {
				f = 1
			}
			b.WriteByte(f)
		}
	}
	sum := sha256.Sum256(b.Bytes())
	b.Write(sum[:])
	return b.Bytes()
}

// g. 坏文件
func TestLoadRejectsBadFiles(t *testing.T) {
	good := []record{{netip.MustParseAddr("198.18.1.1"), t0, "a.com", false}, {netip.MustParseAddr("198.18.1.2"), t0, "b.com", false}}
	valid := build(fileVersion, 2, good, fileMagic)
	flipped := append([]byte(nil), valid...)
	flipped[20] ^= 0xff
	cases := map[string][]byte{
		"截断":     valid[:len(valid)-5],
		"改一个字节":  flipped,
		"空文件":    {},
		"版本不对":   build(3, 2, good, fileMagic),
		"魔数不对":   build(fileVersion, 2, good, "XXXXXXXX"),
		"条数超过容量": build(fileVersion, DefaultPool().size+1, good, fileMagic),
		"条数比实际多": build(fileVersion, 3, good, fileMagic),
		"末尾多余":   build(fileVersion, 1, good, fileMagic),
		"地址越界":   build(fileVersion, 1, []record{{netip.MustParseAddr("198.18.0.2"), t0, "a.com", false}}, fileMagic),
		"地址不在网段": build(fileVersion, 1, []record{{netip.MustParseAddr("8.8.8.8"), t0, "a.com", false}}, fileMagic),
		"地址重复":   build(fileVersion, 2, []record{good[0], {good[0].addr, t0, "b.com", false}}, fileMagic),
		"域名重复":   build(fileVersion, 2, []record{good[0], {good[1].addr, t0, "a.com", false}}, fileMagic),
		"域名不合法":  build(fileVersion, 1, []record{{good[0].addr, t0, "A.COM", false}}, fileMagic),
		"域名带空格":  build(fileVersion, 1, []record{{good[0].addr, t0, "a b", false}}, fileMagic),
		"超过大小上限": make([]byte, MaxFileSize+10),
	}
	if err := DefaultPool().Load(bytes.NewReader(valid)); err != nil {
		t.Fatalf("拼出来的正例应能加载：%v", err)
	}
	for name, data := range cases {
		p := DefaultPool()
		mustAssign(t, p, "pre.existing.com", t0)
		err := p.Load(bytes.NewReader(data))
		if !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s：应报 ErrCorrupt，实际 %v", name, err)
		}
		if p.Stats().Mappings != 0 {
			t.Errorf("%s：失败后池应为空（全有或全无）", name)
		}
		if err != nil && (strings.Contains(err.Error(), "a.com") || strings.Contains(err.Error(), "pre.existing")) {
			t.Errorf("%s：错误信息里不应带域名：%v", name, err)
		}
	}
}

func FuzzLoad(f *testing.F) {
	good := []record{{netip.MustParseAddr("198.18.1.1"), t0, "a.com", false}}
	f.Add(build(fileVersion, 1, good, fileMagic))
	f.Add([]byte(fileMagic))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		p := DefaultPool()
		if err := p.Load(bytes.NewReader(data)); err != nil && p.Stats().Mappings != 0 {
			t.Fatal("失败后池应为空")
		}
	})
}

// h. 文件
func TestSaveFileAndLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fakeip.bin")
	p := DefaultPool()
	mustAssign(t, p, "claude.ai", t0)
	if err := p.SaveFile(path); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("权限应为 0600：%v %v", st.Mode(), err)
	}
	q := DefaultPool()
	if err := q.LoadFile(path); err != nil || q.Stats().Mappings != 1 {
		t.Fatalf("LoadFile：%v %+v", err, q.Stats())
	}
	if err := q.LoadFile(filepath.Join(dir, "missing.bin")); !errors.Is(err, ErrNoFile) || q.Stats().Mappings != 0 {
		t.Fatalf("文件不存在应返回 ErrNoFile 且池为空：%v", err)
	}
	link := filepath.Join(dir, "link.bin")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := q.LoadFile(link); err == nil || errors.Is(err, ErrNoFile) {
		t.Fatalf("符号链接应被拒：%v", err)
	}
	if err := q.LoadFile(dir); err == nil {
		t.Fatal("目录不是普通文件，应被拒")
	}

	ro := filepath.Join(dir, "ro")
	os.Mkdir(ro, 0o500)
	t.Cleanup(func() { os.Chmod(ro, 0o700) })
	if err := p.SaveFile(filepath.Join(ro, "fakeip.bin")); err == nil {
		t.Fatal("目录不可写时应报错")
	}
	entries, _ := os.ReadDir(ro)
	if len(entries) != 0 {
		t.Fatalf("失败时不应留下临时文件：%v", entries)
	}
	// 写到一半失败（目标是目录，rename 失败）：不留临时文件。
	target := filepath.Join(dir, "isdir")
	os.Mkdir(target, 0o700)
	os.WriteFile(filepath.Join(target, "x"), nil, 0o600)
	if err := p.SaveFile(target); err == nil {
		t.Fatal("rename 到非空目录应失败")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".fakeip-*"))
	if len(matches) != 0 {
		t.Fatalf("失败时不应留下临时文件：%v", matches)
	}
}

// i. Run
func TestRunSavesOnlyWhenDirty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fakeip.bin")
	p := DefaultPool()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var errs []error
	go func() {
		p.Run(ctx, path, 20*time.Millisecond, func(err error) { errs = append(errs, err) })
		close(done)
	}()
	time.Sleep(80 * time.Millisecond)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("没有改动不应写文件")
	}
	mustAssign(t, p, "a.com", t0)
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("有改动应在一个周期内落盘")
		}
		time.Sleep(5 * time.Millisecond)
	}
	st1, _ := os.Stat(path)
	time.Sleep(80 * time.Millisecond)
	if st2, _ := os.Stat(path); !st2.ModTime().Equal(st1.ModTime()) {
		t.Fatal("没有新改动不应重写")
	}
	for i := range 3 {
		mustAssign(t, p, fmt.Sprintf("late%d.com", i), t0)
	}
	cancel()
	<-done
	q := DefaultPool()
	if err := q.LoadFile(path); err != nil || q.Stats().Mappings != 4 {
		t.Fatalf("取消后应做最后一次落盘：%v %+v", err, q.Stats())
	}
	if len(errs) != 0 {
		t.Fatalf("不应有落盘错误：%v", errs)
	}
}

// 加载前拿到的 release 在加载后调用：不得碰新的映射表（否则两个域名会共用一个假地址）。
func TestReleaseAfterLoadIsNoop(t *testing.T) {
	p := smallPool(t, 2)
	clock := t0
	p.Now = func() time.Time { return clock }
	a := mustAssign(t, p, "a.com", t0)
	mustAssign(t, p, "b.com", t0)
	_, release, ok := p.Acquire(a, t0)
	if !ok {
		t.Fatal("Acquire 应成功")
	}
	var buf bytes.Buffer
	if err := p.Save(&buf); err != nil {
		t.Fatal(err)
	}
	if err := p.Load(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	release()
	release()
	check := func(when string) {
		t.Helper()
		if m := p.Stats().Mappings; m > 2 {
			t.Fatalf("%s：映射数 %d 超过池容量 2", when, m)
		}
	}
	check("release 后")
	now := t0
	for _, n := range []string{"c.com", "d.com", "e.com", "f.com"} {
		now = now.Add(31 * time.Minute)
		addr := mustAssign(t, p, n, now)
		if got, _ := p.Lookup(addr); got != n {
			t.Fatalf("%s 分到 %s，但 Lookup 回来是 %q（两个域名共用一个地址）", n, addr, got)
		}
		check(n)
	}
	// 新 epoch 的引用照常工作。
	addr := mustAssign(t, p, "g.com", now.Add(31*time.Minute))
	name, rel, ok := p.Acquire(addr, now.Add(31*time.Minute))
	if !ok || name != "g.com" {
		t.Fatal("加载后的 Acquire 应正常")
	}
	if _, err := p.Assign("h.com", now.Add(90*time.Minute)); err != nil {
		t.Fatalf("另一个映射可淘汰：%v", err)
	}
	if _, err := p.Assign("i.com", now.Add(91*time.Minute)); err == nil {
		// 2 个地址：g 在用、h 刚分配，不满 30 分钟 → 应池满
		t.Fatal("g 有引用、h 刚用过，应 ErrPoolFull")
	}
	clock = now.Add(92 * time.Minute)
	rel()
	p.mu.Lock()
	refs := p.byName["g.com"].refs
	p.mu.Unlock()
	if refs != 0 {
		t.Fatalf("新 epoch 的 release 应生效：refs=%d", refs)
	}
}

func TestLoadFileCleansTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fakeip.bin")
	os.WriteFile(filepath.Join(dir, ".fakeip-123.tmp"), []byte("leftover"), 0o600)
	keep := filepath.Join(dir, ".fakeip-link.tmp")
	os.Symlink(path, keep) // 符号链接不删（只删普通文件）
	other := filepath.Join(dir, "other.tmp")
	os.WriteFile(other, nil, 0o600)
	p := DefaultPool()
	if err := p.LoadFile(path); !errors.Is(err, ErrNoFile) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".fakeip-123.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("残留的临时文件应被删掉")
	}
	if _, err := os.Lstat(keep); err != nil {
		t.Fatal("符号链接不应被删")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("不匹配的文件不应被删")
	}
}

func TestRunPrunes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fakeip.bin")
	p := DefaultPool()
	clock := t0
	var mu sync.Mutex
	p.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	p.PruneInterval = 30 * time.Millisecond
	mustAssign(t, p, "old.com", t0.Add(-31*24*time.Hour)) // 启动时就该清掉
	mustAssign(t, p, "mid.com", t0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx, path, 10*time.Millisecond, nil); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for p.Stats().Mappings != 1 {
		if time.Now().After(deadline) {
			t.Fatal("启动时应清掉超过 30 天的映射")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	clock = t0.Add(31 * 24 * time.Hour) // 时间推进，下一个清理周期清掉 mid
	mu.Unlock()
	for p.Stats().Mappings != 0 {
		if time.Now().After(deadline) {
			t.Fatal("周期清理应清掉 mid.com")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	q := DefaultPool()
	if err := q.LoadFile(path); err != nil || q.Stats().Mappings != 0 {
		t.Fatalf("清理结果应落盘：%v %+v", err, q.Stats())
	}
}
