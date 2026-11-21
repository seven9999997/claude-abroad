package kmod

import (
	"errors"
	"slices"
	"testing"
	"testing/fstest"
	"time"
)

// 构造的模块目录：nf_tables 硬依赖 libcrc32c；libcrc32c 的软依赖 pre: crc32c（别名 → crc32c_generic）；
// 另有成环的软依赖 a ↔ b，以及指向不存在的名字的软依赖。
func softFS() fstest.MapFS {
	mod := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	return fstest.MapFS{
		"modules.dep": mod("kernel/lib/libcrc32c.ko: \nkernel/crypto/crc32c_generic.ko: \nkernel/net/nf_tables.ko: kernel/lib/libcrc32c.ko\n" +
			"kernel/x/a.ko: \nkernel/x/b.ko: \nkernel/x/c.ko: \n"),
		"modules.softdep":                 mod("# 注释\nsoftdep libcrc32c pre: crc32c post: other\nsoftdep a pre: b\nsoftdep b pre: a\nsoftdep c pre: no_such_module\n"),
		"modules.alias":                   mod("alias crypto-crc32c crc32c_generic\nalias crc32c crc32c_generic\nalias pci:v*d* something\n"),
		"kernel/lib/libcrc32c.ko":         mod("libcrc32c"),
		"kernel/crypto/crc32c_generic.ko": mod("crc32c_generic"),
		"kernel/net/nf_tables.ko":         mod("nf_tables"),
		"kernel/x/a.ko":                   mod("a"),
		"kernel/x/b.ko":                   mod("b"),
		"kernel/x/c.ko":                   mod("c"),
	}
}

// a. 有 pre: 的先加载（经别名找到模块）；软依赖加载失败不算错；目标模块失败才报错；有环时不死循环。
func TestSoftdep(t *testing.T) {
	var loaded []string
	fail := map[string]bool{}
	l := Loader{Dir: softFS(), Load: func(img []byte) error {
		if fail[string(img)] {
			return errors.New("加载失败")
		}
		loaded = append(loaded, string(img))
		return nil
	}}
	rs, err := l.LoadNames([]string{"nf_tables"})
	if err != nil || !slices.Equal(loaded, []string{"crc32c_generic", "libcrc32c", "nf_tables"}) || len(Failed(rs)) != 0 {
		t.Fatalf("软依赖应先于 libcrc32c 加载：%v %v %+v", loaded, err, rs)
	}
	// 软依赖失败：不算错，目标照样加载。
	loaded, fail = nil, map[string]bool{"crc32c_generic": true}
	rs, _ = l.LoadNames([]string{"nf_tables"})
	if len(Failed(rs)) != 0 || !slices.Contains(loaded, "nf_tables") {
		t.Fatalf("软依赖失败不算错：%+v", rs)
	}
	var soft Result
	for _, r := range rs {
		if r.Name == "crc32c_generic" {
			soft = r
		}
	}
	if soft.OK || !soft.Soft {
		t.Fatalf("软依赖的失败要记在结果里：%+v", soft)
	}
	// 目标失败才报错。
	loaded, fail = nil, map[string]bool{"nf_tables": true}
	rs, _ = l.LoadNames([]string{"nf_tables"})
	if f := Failed(rs); !slices.Equal(f, []string{"nf_tables"}) {
		t.Fatalf("%v", f)
	}
	// 软依赖找不到：只记录。
	loaded, fail = nil, map[string]bool{}
	rs, _ = l.LoadNames([]string{"c"})
	if len(Failed(rs)) != 0 || !slices.Equal(loaded, []string{"c"}) {
		t.Fatalf("%v %+v", loaded, rs)
	}
	// 有环：a ↔ b，不死循环，各加载一次。
	done := make(chan []Result, 1)
	loaded = nil
	go func() { rs, _ := l.LoadNames([]string{"a"}); done <- rs }()
	select {
	case rs := <-done:
		if len(Failed(rs)) != 0 || len(loaded) != 2 {
			t.Fatalf("%v %+v", loaded, rs)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("软依赖成环时死循环")
	}
	// 没有 softdep 文件：和以前一样。
	fs := softFS()
	delete(fs, "modules.softdep")
	loaded = nil
	(Loader{Dir: fs, Load: func(img []byte) error { loaded = append(loaded, string(img)); return nil }}).LoadNames([]string{"nf_tables"})
	if !slices.Equal(loaded, []string{"libcrc32c", "nf_tables"}) {
		t.Fatalf("%v", loaded)
	}
}
