package claudedomains

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func clock() (func() time.Time, func(time.Duration)) {
	t := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	return func() time.Time { mu.Lock(); defer mu.Unlock(); return t }, func(d time.Duration) { mu.Lock(); t = t.Add(d); mu.Unlock() }
}

func TestRecordAndList(t *testing.T) {
	now, adv := clock()
	s := &Store{Now: now}
	if !s.Record("Statsig.Example.COM.") || s.Record("statsig.example.com") {
		t.Fatal("第一次出现返回 true，之后 false")
	}
	adv(time.Second)
	s.Record("cdn.example.net")
	l := s.List()
	if len(l) != 2 || l[0].Domain != "cdn.example.net" || l[1].Count != 2 || !l[1].First.Equal(now().Add(-time.Second)) {
		t.Fatalf("%+v", l)
	}
	for _, bad := range []string{"", "localhost", "1.2.3.4", "a b.com", "x/y.com", "203.0.113.7", strings.Repeat("a", 250) + ".com"} {
		if s.Record(bad) {
			t.Errorf("%q 不应记", bad)
		}
	}
	if s.Count() != 2 {
		t.Fatal(s.Count())
	}
}

func TestEvictLeastRecentlySeen(t *testing.T) {
	now, adv := clock()
	s := &Store{Now: now, Max: 3}
	for i := 0; i < 3; i++ {
		s.Record(fmt.Sprintf("d%d.example", i))
		adv(time.Second)
	}
	s.Record("d0.example") // d0 又出现了：最久没出现的是 d1
	adv(time.Second)
	s.Record("d3.example")
	var names []string
	for _, e := range s.List() {
		names = append(names, e.Domain)
	}
	if strings.Join(names, ",") != "d3.example,d0.example,d2.example" {
		t.Fatalf("应淘汰最久没出现的 d1：%v", names)
	}
}

func TestFlushLoadClear(t *testing.T) {
	dir := t.TempDir()
	now, _ := clock()
	s := &Store{Dir: dir, Now: now}
	s.Record("a.example")
	s.Record("a.example")
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("文件应是 0600：%v %v", fi, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, FileName))
	for _, bad := range []string{"/Users", "path", "203.0.113"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("文件里不应有 %q：%s", bad, b)
		}
	}
	s2 := &Store{Dir: dir}
	if l := s2.List(); len(l) != 1 || l[0].Count != 2 {
		t.Fatalf("重新读出：%+v", l)
	}
	if err := s2.Clear(); err != nil || s2.Count() != 0 {
		t.Fatal("清空")
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
		t.Fatal("清空后文件应删掉")
	}
	// 坏文件：从空开始
	_ = os.WriteFile(filepath.Join(dir, FileName), []byte("{"), 0o600)
	if (&Store{Dir: dir}).Count() != 0 {
		t.Fatal("坏文件应从空开始")
	}
	// 没改动时 Flush 不写
	s3 := &Store{Dir: t.TempDir()}
	if err := s3.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s3.Dir, FileName)); !os.IsNotExist(err) {
		t.Fatal("没改动不应写文件")
	}
}

func TestConcurrent(t *testing.T) {
	s := &Store{Dir: t.TempDir(), Max: 50}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s.Record(fmt.Sprintf("d%d.example", (i*j)%80))
				if j%50 == 0 {
					_ = s.Flush()
				}
			}
		}(i)
	}
	wg.Wait()
	if s.Count() > 50 {
		t.Fatal(s.Count())
	}
}
