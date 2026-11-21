package procinfo

import (
	"encoding/binary"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPlistSamples(t *testing.T) {
	// 本机真实应用的 Bundle ID，只保留这一个键，经 plutil 生成两种格式。
	for f, want := range map[string]string{
		"testdata/Safari.xml1.plist":        "com.apple.Safari",
		"testdata/Calculator.binary1.plist": "com.apple.calculator",
	} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := BundleIDFromPlist(b); err != nil || got != want {
			t.Errorf("%s：%q %v", f, got, err)
		}
	}
}

func TestFileBundles(t *testing.T) {
	dir := t.TempDir()
	app := dir + "/X.app"
	if err := os.MkdirAll(app+"/Contents", 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile("testdata/Safari.xml1.plist")
	if err := os.WriteFile(app+"/Contents/Info.plist", b, 0o644); err != nil {
		t.Fatal(err)
	}
	var fb FileBundles
	if id, err := fb.Read(app); err != nil || id != "com.apple.Safari" {
		t.Fatalf("%q %v", id, err)
	}
	// 修改时间变了就重读。
	b2, _ := os.ReadFile("testdata/Calculator.binary1.plist")
	_ = os.WriteFile(app+"/Contents/Info.plist", b2, 0o644)
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(app+"/Contents/Info.plist", future, future)
	if id, err := fb.Read(app); err != nil || id != "com.apple.calculator" {
		t.Fatalf("修改后应重读：%q %v", id, err)
	}
	if _, err := fb.Read(dir + "/Missing.app"); err == nil {
		t.Fatal("没有 Info.plist 应报错")
	}
	// 超过 1 MiB。
	big := dir + "/Big.app"
	_ = os.MkdirAll(big+"/Contents", 0o755)
	_ = os.WriteFile(big+"/Contents/Info.plist", make([]byte, MaxPlistSize+1), 0o644)
	if _, err := fb.Read(big); err == nil {
		t.Fatal("超过 1 MiB 应报错")
	}
}

// bp 构造 bplist00：objects 是编码好的对象，引用和偏移都用 1 或 2 字节。
func bp(top int, objects ...[]byte) []byte {
	b := []byte("bplist00")
	var offs []int
	for _, o := range objects {
		offs = append(offs, len(b))
		b = append(b, o...)
	}
	table := len(b)
	for _, o := range offs {
		b = binary.BigEndian.AppendUint16(b, uint16(o))
	}
	tr := make([]byte, 32)
	tr[6], tr[7] = 2, 1
	binary.BigEndian.PutUint64(tr[8:], uint64(len(objects)))
	binary.BigEndian.PutUint64(tr[16:], uint64(top))
	binary.BigEndian.PutUint64(tr[24:], uint64(table))
	return append(b, tr...)
}

func astr(s string) []byte {
	if len(s) < 15 {
		return append([]byte{0x50 | byte(len(s))}, s...)
	}
	return append([]byte{0x5f, 0x10, byte(len(s))}, s...) // 长度 ≥15 时后面跟一个 1 字节的整数
}

func TestPlistRejects(t *testing.T) {
	good := bp(0, []byte{0xd1, 1, 2}, astr(bundleIDKey), astr("com.x.y"))
	if id, err := BundleIDFromPlist(good); err != nil || id != "com.x.y" {
		t.Fatalf("构造的正例：%q %v", id, err)
	}
	// UTF-16 的值也认。
	u16 := []byte{0x67}
	for _, r := range "com.x.z" {
		u16 = binary.BigEndian.AppendUint16(u16, uint16(r))
	}
	if id, err := BundleIDFromPlist(bp(0, []byte{0xd1, 1, 2}, astr(bundleIDKey), u16)); err != nil || id != "com.x.z" {
		t.Fatalf("UTF-16：%q %v", id, err)
	}
	deep := func(n int) []byte { // 嵌套 n 层的数组，最里层是 dict
		objs := [][]byte{{0xd1, 1, 2}, astr(bundleIDKey), astr("a.b")}
		top := 0
		for i := 0; i < n; i++ {
			objs = append(objs, []byte{0xa1, byte(top)})
			top = len(objs) - 1
		}
		return bp(top, objs...)
	}
	cases := map[string][]byte{
		"超过 1 MiB":  append([]byte("bplist00"), make([]byte, MaxPlistSize)...),
		"嵌套超过 16 层": deep(20),
		"对象数超限": func() []byte {
			b := bp(0, []byte{0xd0})
			binary.BigEndian.PutUint64(b[len(b)-24:], maxPlistObjects+1)
			return b
		}(),
		"偏移表越界": func() []byte {
			b := good
			b = append([]byte(nil), b...)
			binary.BigEndian.PutUint64(b[len(b)-8:], uint64(len(b)))
			return b
		}(),
		"偏移越界": func() []byte {
			b := append([]byte(nil), good...)
			binary.BigEndian.PutUint16(b[len(b)-32-6:], 0xffff)
			return b
		}(),
		"引用成环":          bp(0, []byte{0xd1, 1, 2}, astr(bundleIDKey), []byte{0xa1, 2}),
		"键不是字符串":        bp(0, []byte{0xd1, 1, 2}, []byte{0x10, 5}, astr("a.b")),
		"顶层不是 dict":     bp(0, []byte{0xa1, 1}, astr("x")),
		"没有这个键":         bp(0, []byte{0xd1, 1, 2}, astr("Other"), astr("a.b")),
		"值不是字符串":        bp(0, []byte{0xd1, 1, 2}, astr(bundleIDKey), []byte{0x10, 5}),
		"引用越界":          bp(0, []byte{0xd1, 1, 9}, astr(bundleIDKey), astr("a.b")),
		"trailer 太短":    []byte("bplist00short"),
		"XML 嵌套过深":      []byte(`<plist><dict><key>K</key>` + strings.Repeat("<array>", 20) + strings.Repeat("</array>", 20) + `</dict></plist>`),
		"XML 对象太多":      []byte(`<plist><dict>` + strings.Repeat("<key>a</key><true/>", maxPlistObjects/2+10) + `</dict></plist>`),
		"XML 顶层不是 dict": []byte(`<plist><array><string>x</string></array></plist>`),
		"XML 格式坏":       []byte(`<plist><dict><key>CFBundleIdentifier</key><string>a.b</dict>`),
		"XML 重复的键":      []byte(`<plist><dict><key>CFBundleIdentifier</key><string>a.b</string><key>CFBundleIdentifier</key><string>c.d</string></dict></plist>`),
	}
	for name, b := range cases {
		if id, err := BundleIDFromPlist(b); err == nil {
			t.Errorf("%s：应报错，得到 %q", name, id)
		}
	}
	// 嵌套 10 层（不超限）：错误应是“顶层不是 dict”，而不是深度超限。
	if _, err := BundleIDFromPlist(deep(10)); err == nil || strings.Contains(err.Error(), "嵌套太深") {
		t.Errorf("嵌套 10 层不应判成深度超限：%v", err)
	}
}

func FuzzPlist(f *testing.F) {
	for _, p := range []string{"testdata/Safari.xml1.plist", "testdata/Calculator.binary1.plist"} {
		b, _ := os.ReadFile(p)
		f.Add(b)
	}
	f.Add(bp(0, []byte{0xd1, 1, 2}, astr(bundleIDKey), astr("com.x.y")))
	f.Add(bp(0, []byte{0xa2, 0, 1}, astr("x")))
	f.Fuzz(func(t *testing.T, b []byte) {
		var id string
		var err error
		best := time.Hour
		for i := 0; i < 3; i++ {
			start := time.Now()
			id, err = BundleIDFromPlist(b)
			best = min(best, time.Since(start))
			if best <= 50*time.Millisecond {
				break
			}
		}
		if best > 50*time.Millisecond {
			t.Fatalf("处理用了 %v（%d 字节）", best, len(b))
		}
		if err == nil && len(id) > 4096 {
			t.Fatal("名字超长")
		}
	})
}
