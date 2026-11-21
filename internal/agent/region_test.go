package agent

import (
	"bytes"
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeRegionFS 记下所有读写的路径（h：只写三个文件）。
type fakeRegionFS struct {
	mu     sync.Mutex
	files  map[string][]byte
	writes []string
	reads  []string
}

func (f *fakeRegionFS) ReadFile(p string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, p)
	b, ok := f.files[p]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return bytes.Clone(b), nil
}

func (f *fakeRegionFS) Replace(p string, data []byte, _ fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, p)
	f.files[p] = bytes.Clone(data)
	return nil
}

func newRegionFS() *fakeRegionFS {
	return &fakeRegionFS{files: map[string][]byte{
		"/usr/share/zoneinfo/Asia/Tokyo":          []byte("TZif tokyo"),
		"/usr/share/zoneinfo/America/Los_Angeles": []byte("TZif la"),
		"/usr/share/zoneinfo/Bogus/NotTZ":         []byte("hello"),
		RegionLocaltime:                           []byte("TZif utc"),
	}}
}

// h. 只写三个文件；写入后引擎重启一次；同样的值重复下发不重启；时区、语言不合法时拒绝。
func TestRegionApply(t *testing.T) {
	f := newRegionFS()
	restarts := 0
	r := &Region{FS: f, Restart: func(context.Context) { restarts++ }}
	changed, err := r.Apply(context.Background(), RegionRequest{Timezone: "Asia/Tokyo", Lang: "ja-JP"})
	if err != nil || !changed || restarts != 1 {
		t.Fatalf("%v %v %d", changed, err, restarts)
	}
	if !slices.Equal(f.writes, []string{RegionLocaltime, RegionTZ, RegionLocaleSh}) {
		t.Fatalf("只应写三个文件：%v", f.writes)
	}
	if string(f.files[RegionLocaltime]) != "TZif tokyo" || string(f.files[RegionTZ]) != "Asia/Tokyo\n" ||
		!strings.Contains(string(f.files[RegionLocaleSh]), "export LANG=ja_JP.UTF-8\nexport LC_ALL=ja_JP.UTF-8\n") {
		t.Fatalf("内容不对：%q", f.files[RegionLocaleSh])
	}
	for _, p := range f.reads {
		if p != RegionLocaltime && p != RegionTZ && p != RegionLocaleSh && !strings.HasPrefix(p, ZoneinfoDir+"/") {
			t.Fatalf("读了别的路径：%s", p)
		}
	}
	// 重复下发：不写、不重启。
	changed, err = r.Apply(context.Background(), RegionRequest{Timezone: "Asia/Tokyo", Lang: "ja-JP"})
	if err != nil || changed || restarts != 1 || len(f.writes) != 3 {
		t.Fatalf("重复下发不应重启：%v %d %v", changed, restarts, f.writes)
	}
	// 只改语言：只写 locale.sh，重启一次。
	if _, err := r.Apply(context.Background(), RegionRequest{Timezone: "Asia/Tokyo", Lang: "zh-Hant-TW"}); err != nil || restarts != 2 ||
		!slices.Equal(f.writes[3:], []string{RegionLocaleSh}) || !strings.Contains(string(f.files[RegionLocaleSh]), "LANG=zh_TW.UTF-8") {
		t.Fatalf("%v %d %v", err, restarts, f.writes)
	}
	// 不合法：拒绝，不写、不重启。
	for _, req := range []RegionRequest{
		{Timezone: "Mars/Base", Lang: "en-US"},        // 不在时区数据里
		{Timezone: "Bogus/NotTZ", Lang: "en-US"},      // 文件不是时区数据
		{Timezone: "../../etc/shadow", Lang: "en-US"}, // 路径
		{Timezone: "America/New York", Lang: "en-US"},
		{Timezone: "Asia/Tokyo", Lang: "ja_JP"},
		{Timezone: "Asia/Tokyo", Lang: "ja-JP\nexport X=1"},
	} {
		n := len(f.writes)
		if _, err := r.Apply(context.Background(), req); err == nil || len(f.writes) != n || restarts != 2 {
			t.Fatalf("%+v 应拒绝：%v", req, err)
		}
	}
}

func TestRegionHTTP(t *testing.T) {
	f := newRegionFS()
	r := &Region{FS: f}
	h := (&API{State: NewState("k"), Shutdown: func() {}, Region: r}).Handler()
	do := func(method, body string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/v1/region", strings.NewReader(body)))
		return rec.Code
	}
	if c := do(http.MethodPut, `{"timezone":"America/Los_Angeles","lang":"en-US"}`); c != 200 {
		t.Fatal(c)
	}
	if c := do(http.MethodPut, `{"timezone":"Asia/Tokyo","lang":"ja-JP","extra":1}`); c != 400 {
		t.Fatal(c)
	}
	if c := do(http.MethodPut, `{"timezone":"Nope/Nope","lang":"ja-JP"}`); c != 422 {
		t.Fatal(c)
	}
	if c := do(http.MethodGet, ``); c != 405 {
		t.Fatal(c)
	}
}

// 真实的 rootFS：替换符号链接时换掉链接本身，不写穿到链接目标。
func TestRootFSReplaceSymlink(t *testing.T) {
	root := t.TempDir()
	zone := filepath.Join(root, "usr/share/zoneinfo/Asia")
	if err := os.MkdirAll(zone, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(zone, "Singapore")
	os.WriteFile(target, []byte("TZif sg"), 0o644)
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	if err := os.Symlink(target, filepath.Join(root, "etc/localtime")); err != nil {
		t.Fatal(err)
	}
	if err := (rootFS{root}).Replace(RegionLocaltime, []byte("TZif tokyo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "TZif sg" {
		t.Fatal("不应写穿符号链接")
	}
	fi, _ := os.Lstat(filepath.Join(root, "etc/localtime"))
	if fi.Mode()&os.ModeSymlink != 0 || fi.Mode().Perm() != 0o644 {
		t.Fatalf("应换成普通文件 0644：%v", fi.Mode())
	}
	ents, _ := os.ReadDir(filepath.Join(root, "etc"))
	if len(ents) != 1 {
		t.Fatalf("不应留下临时文件：%v", ents)
	}
}
