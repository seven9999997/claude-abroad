package imagebuild

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- 构造样本：每段是一个不带结束块的 tar 片段，gzip 后首尾相接 ----

type tarItem struct {
	name, link string
	typ        byte
	data       []byte
	mode       int64
}

func segment(t *testing.T, items []tarItem) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, it := range items {
		h := &tar.Header{Name: it.name, Linkname: it.link, Typeflag: it.typ, Mode: it.mode, Size: int64(len(it.data)), Format: tar.FormatGNU}
		if it.typ == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if h.Typeflag != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write(it.data)
		}
	}
	tw.Flush() // 不写结束块（apk 的段就是这样）
	var z bytes.Buffer
	zw := gzip.NewWriter(&z)
	zw.Write(raw.Bytes())
	zw.Close()
	return z.Bytes()
}

func testKey(t *testing.T) (*rsa.PrivateKey, APKKeys) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	pub, err := ParseAPKKey(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	if err != nil {
		t.Fatal(err)
	}
	return k, APKKeys{"test-key.rsa.pub": pub}
}

func sigSegment(t *testing.T, k *rsa.PrivateKey, signed []byte, sha256Alg bool) []byte {
	t.Helper()
	var sig []byte
	var err error
	name := ".SIGN.RSA.test-key.rsa.pub"
	if sha256Alg {
		d := sha256.Sum256(signed)
		sig, err = rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, d[:])
		name = ".SIGN.RSA256.test-key.rsa.pub"
	} else {
		d := sha1.Sum(signed)
		sig, err = rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA1, d[:])
	}
	if err != nil {
		t.Fatal(err)
	}
	return segment(t, []tarItem{{name: name, data: sig}})
}

func makeIndex(t *testing.T, k *rsa.PrivateKey, text string, sha256Alg bool) []byte {
	ctl := segment(t, []tarItem{{name: "DESCRIPTION", data: []byte("test")}, {name: "APKINDEX", data: []byte(text)}})
	return append(sigSegment(t, k, ctl, sha256Alg), ctl...)
}

// makeAPK 构造 .apk：控制段（.PKGINFO + 脚本）和数据段；datahash 按实际数据段算（bad 为真时故意写错）。
func makeAPK(t *testing.T, k *rsa.PrivateKey, name, ver string, scripts []string, data []tarItem, bad bool) []byte {
	t.Helper()
	ds := segment(t, data)
	dh := sha256.Sum256(ds)
	h := hex.EncodeToString(dh[:])
	if bad {
		h = strings.Repeat("0", 64)
	}
	ctlItems := []tarItem{{name: ".PKGINFO", data: []byte("pkgname = " + name + "\npkgver = " + ver + "\ndatahash = " + h + "\n")}}
	for _, s := range scripts {
		ctlItems = append(ctlItems, tarItem{name: s, data: []byte("#!/bin/sh\nexit 0\n"), mode: 0o755})
	}
	ctl := segment(t, ctlItems)
	out := append(sigSegment(t, k, ctl, false), ctl...)
	return append(out, ds...)
}

// ---- 索引的签名 ----

func TestVerifyAPKINDEX(t *testing.T) {
	k, keys := testKey(t)
	text := "P:a\nV:1.0-r0\nA:aarch64\n\n"
	for _, alg := range []bool{false, true} {
		b := makeIndex(t, k, text, alg)
		got, err := VerifyAPKINDEX(b, keys)
		if err != nil || string(got) != text {
			t.Fatalf("签名应通过（sha256=%v）：%v", alg, err)
		}
	}
	// 内容被改一个字节：签名不通过。
	b := makeIndex(t, k, text, false)
	b2 := append([]byte(nil), b...)
	segs, _ := splitGzipMembers(b, 4)
	tampered := append(append([]byte(nil), segs[0]...), segment(t, []tarItem{{name: "DESCRIPTION", data: []byte("test")}, {name: "APKINDEX", data: []byte("P:evil\nV:1\n\n")}})...)
	if _, err := VerifyAPKINDEX(tampered, keys); !errors.Is(err, ErrSignature) {
		t.Fatalf("内容被换掉时签名应不通过：%v", err)
	}
	_ = b2
	// 公钥不在信任列表里。
	_, other := testKey(t)
	if _, err := VerifyAPKINDEX(b, other); err == nil {
		t.Fatal("签名公钥不在信任列表里时应拒绝")
	} else if !errors.Is(err, ErrSignature) {
		t.Fatal(err)
	}
	// 没有签名段。
	ctl := segment(t, []tarItem{{name: "APKINDEX", data: []byte(text)}})
	if _, err := VerifyAPKINDEX(append(ctl, ctl...), keys); !errors.Is(err, ErrSignature) {
		t.Fatalf("没有签名时应拒绝：%v", err)
	}
}

// ---- 解析与依赖闭包 ----

func TestParseAPKINDEX(t *testing.T) {
	pk, err := ParseAPKINDEX(strings.NewReader("P:a\nV:1.0-r0\nD:b so:libc.musl-aarch64.so.1\np:cmd:a=1.0-r0\nC:Q1xx\nS:10\nk:5\n\nP:b\nV:2\n"))
	if err != nil || len(pk) != 2 || pk[0].Priority != 5 || pk[0].Size != 10 || len(pk[0].Depends) != 2 || pk[1].Name != "b" {
		t.Fatalf("%+v %v", pk, err)
	}
	if _, err := ParseAPKINDEX(strings.NewReader("P:a\n\n")); err == nil {
		t.Fatal("缺 V: 应报错")
	}
	if _, err := ParseAPKINDEX(strings.NewReader("bad line\n")); err == nil {
		t.Fatal("行格式不对应报错")
	}
}

func TestCompareAPKVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0-r0", "1.0-r0", 0}, {"1.0-r1", "1.0-r0", 1}, {"1.10", "1.9", 1}, {"2.6.20-r0", "2.6.3-r9", 1},
		{"6.5_p20250503-r0", "6.5_p20250101-r0", 1}, {"1.0", "1.0.1", -1}, {"2026f-r0", "2026e-r0", 1},
	}
	for _, c := range cases {
		if got := CompareAPKVersion(c.a, c.b); got != c.want {
			t.Errorf("%s vs %s：%d，期望 %d", c.a, c.b, got, c.want)
		}
	}
}

func TestResolveAPK(t *testing.T) {
	idx, _ := ParseAPKINDEX(strings.NewReader(strings.Join([]string{
		"P:app\nV:1.0-r0\nD:liba cmd:tool so:libx.so.1 libv>=2\n",
		"P:liba\nV:1.0-r0\nD:libb\n",
		"P:libb\nV:1.0-r0\nD:liba\n", // 循环依赖
		"P:toolpkg\nV:3.0-r0\np:cmd:tool=3.0-r0\n",
		"P:libxpkg\nV:1.0-r0\np:so:libx.so.1=1.0\n",
		"P:libxalt\nV:1.0-r0\np:so:libx.so.1=1.0\nk:10\n", // 优先级高
		"P:libv\nV:1.5-r0\n",
		"P:libv\nV:2.1-r0\n",
		"P:needsold\nV:1\nD:libv<1\n",
		"P:needsmissing\nV:1\nD:nothere\n",
	}, "\n")))
	got, err := ResolveAPK(idx, []string{"app"}, map[string]string{"/bin/sh": "0"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range got {
		names = append(names, p.Name+"-"+p.Version)
	}
	want := "app-1.0-r0 liba-1.0-r0 libb-1.0-r0 libv-2.1-r0 libxalt-1.0-r0 toolpkg-3.0-r0"
	if strings.Join(names, " ") != want {
		t.Fatalf("闭包：%s\n期望：%s", strings.Join(names, " "), want)
	}
	if _, err := ResolveAPK(idx, []string{"needsold"}, nil); err == nil || !strings.Contains(err.Error(), "版本约束") {
		t.Fatalf("版本约束不满足应报错：%v", err)
	}
	if _, err := ResolveAPK(idx, []string{"needsmissing"}, nil); err == nil || !strings.Contains(err.Error(), "nothere") {
		t.Fatalf("缺包应报错并指出是谁：%v", err)
	}
	// 已装的不再装。
	got, _ = ResolveAPK(idx, []string{"liba"}, map[string]string{"libb": "1.0-r0"})
	if len(got) != 1 || got[0].Name != "liba" {
		t.Fatalf("已装的 libb 不应再装：%+v", got)
	}
}

// 录制的索引（Alpine 3.22 main/aarch64 的一部分）和 minirootfs 的已装列表：闭包与 2026-11-19 的固定结果一致。
func TestResolveRecorded(t *testing.T) {
	ib, _ := os.ReadFile("testdata/apk/APKINDEX-excerpt")
	db, _ := os.ReadFile("testdata/apk/installed-3.22.8")
	idx, err := ParseAPKINDEX(bytes.NewReader(ib))
	if err != nil {
		t.Fatal(err)
	}
	inst, err := InstalledFromDB(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	inst["/bin/sh"] = "0"
	got, err := ResolveAPK(idx, []string{"openvpn", "wireguard-tools-wg", "nftables", "iproute2", "tzdata", "musl-locales"}, inst)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 24 {
		t.Fatalf("闭包应是 24 个包：%d", len(got))
	}
	for _, p := range got {
		if p.Name == "bash" || p.Name == "wireguard-tools-wg-quick" || p.Name == "libcrypto3" || p.Name == "musl" {
			t.Fatalf("不该装 %s（不要 wg-quick；libcrypto3、musl 已在 minirootfs 里）", p.Name)
		}
	}
	// wg-quick 会带进 bash 和 resolvconf：这正是不用它的原因之一（R5）。
	q, err := ResolveAPK(idx, []string{"wireguard-tools-wg-quick"}, inst)
	if err == nil {
		for _, p := range q {
			if p.Name == "bash" {
				return
			}
		}
	}
	t.Logf("wg-quick 的闭包：%v %v", q, err)
}

// ---- 解包 ----

func TestExtractAPK(t *testing.T) {
	k, keys := testKey(t)
	b := makeAPK(t, k, "demo", "1.0-r0", []string{".pre-install", ".post-install"}, []tarItem{
		{name: "usr/", typ: tar.TypeDir, mode: 0o755},
		{name: "usr/bin/demo", data: []byte("ELF"), mode: 0o755},
		{name: "usr/bin/demo2", typ: tar.TypeLink, link: "usr/bin/demo"},
		{name: "usr/lib/libd.so", typ: tar.TypeSymlink, link: "libd.so.1"},
		{name: "usr/lib/up", typ: tar.TypeSymlink, link: "../../etc/x"},
	}, false)
	if n, err := VerifyAPK(b, keys); err != nil || n != "test-key.rsa.pub" {
		t.Fatalf("包的签名：%s %v", n, err)
	}
	tr := NewTree()
	m, err := ExtractAPK(b, tr)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "demo" || m.Version != "1.0-r0" || strings.Join(m.Scripts, ",") != ".post-install,.pre-install" || m.Files != 4 {
		t.Fatalf("%+v", m)
	}
	if e, ok := tr.Get("/usr/bin/demo"); !ok || string(e.Data) != "ELF" || e.Mode.Perm() != 0o755 {
		t.Fatal("文件没解对")
	}
	if e, ok := tr.Get("/usr/bin/demo2"); !ok || string(e.Data) != "ELF" {
		t.Fatal("硬链接应复制成独立文件")
	}
	// 安装脚本只记录，不进树。
	if _, ok := tr.Get("/.pre-install"); ok {
		t.Fatal("安装脚本不应进根文件系统")
	}
	// datahash 不对。
	if _, err := ExtractAPK(makeAPK(t, k, "demo", "1", nil, []tarItem{{name: "a", data: []byte("x")}}, true), NewTree()); err == nil || !strings.Contains(err.Error(), "数据段") {
		t.Fatalf("datahash 不符应拒绝：%v", err)
	}
	// 拒绝的路径。
	for name, it := range map[string]tarItem{
		"越界路径":    {name: "usr/../../etc/passwd", data: []byte("x")},
		"绝对路径":    {name: "/etc/passwd", data: []byte("x")},
		"指向根外的链接": {name: "usr/lib/evil", typ: tar.TypeSymlink, link: "../../../etc/shadow"},
		"硬链接越界":   {name: "usr/bin/h", typ: tar.TypeLink, link: "../../x"},
	} {
		if _, err := ExtractAPK(makeAPK(t, k, "evil", "1", nil, []tarItem{it}, false), NewTree()); err == nil {
			t.Errorf("%s 应被拒绝", name)
		}
	}
	// 段数不对。
	if _, err := ExtractAPK(b[:len(b)/3], NewTree()); err == nil {
		t.Fatal("截断的包应报错")
	}
}

// 录制的真实 .apk（三段式，Alpine 的签名）：签名、控制段校验和与索引一致、能解出 /usr/bin/wg。
func TestRecordedAPK(t *testing.T) {
	b, err := os.ReadFile("testdata/apk/wireguard-tools-wg-1.0.20250521-r0.apk")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := LoadAPKKeys(filepath.Join("..", "..", "guest", "keys"))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := VerifyAPK(b, keys); err != nil || !strings.Contains(n, "alpine-devel@lists.alpinelinux.org") {
		t.Fatalf("真实包的签名：%s %v", n, err)
	}
	ib, _ := os.ReadFile("testdata/apk/APKINDEX-excerpt")
	idx, _ := ParseAPKINDEX(bytes.NewReader(ib))
	for _, p := range idx {
		if p.Name == "wireguard-tools-wg" {
			if err := CheckAPKAgainstIndex(b, p); err != nil {
				t.Fatal(err)
			}
			p.Checksum = "Q1AAAA"
			if err := CheckAPKAgainstIndex(b, p); err == nil {
				t.Fatal("校验和不符应报错")
			}
		}
	}
	tr := NewTree()
	if _, err := ExtractAPK(b, tr); err != nil {
		t.Fatal(err)
	}
	if _, ok := tr.Get("/usr/bin/wg"); !ok {
		t.Fatal("应解出 /usr/bin/wg")
	}
}

// ---- 安装脚本的等价动作 ----

func TestScriptEquivalents(t *testing.T) {
	tr := NewTree()
	tr.AddFile("/etc/passwd", 0o644, []byte("root:x:0:0:root:/root:/bin/sh\nnobody:x:65534:65534:nobody:/:/sbin/nologin\nx:x:100:101::/:/bin/false\n"))
	tr.AddFile("/etc/group", 0o644, []byte("root:x:0:root\nx:x:101:\n"))
	tr.AddSymlink("/bin/ip", "/bin/busybox")
	list := []PinnedPackage{
		{Name: "openvpn", Scripts: []string{".pre-install"}},
		{Name: "iproute2", Scripts: []string{".post-install"}},
		{Name: "nftables", Scripts: []string{".post-upgrade"}},
		{Name: "other", Scripts: []string{".pre-deinstall"}},
	}
	if err := applyScriptEquivalents(tr, list); err != nil {
		t.Fatal(err)
	}
	pw, _ := tr.Get("/etc/passwd")
	gr, _ := tr.Get("/etc/group")
	if !strings.Contains(string(pw.Data), "openvpn:x:102:102:openvpn:/dev/null:/sbin/nologin\n") || !strings.Contains(string(gr.Data), "openvpn:x:102:openvpn\n") {
		t.Fatalf("应取 100–999 里第一个空闲 ID：\n%s\n%s", pw.Data, gr.Data)
	}
	if _, ok := tr.Get("/bin/ip"); ok {
		t.Fatal("/bin/ip 指向 busybox 的链接应删掉")
	}
	// 再来一次不重复加。
	applyScriptEquivalents(tr, list)
	pw, _ = tr.Get("/etc/passwd")
	if strings.Count("\n"+string(pw.Data), "\nopenvpn:") != 1 {
		t.Fatal("已存在时不应重复加用户")
	}
	// 没核对过的脚本：构建失败。
	if err := applyScriptEquivalents(tr, []PinnedPackage{{Name: "newpkg", Scripts: []string{".post-install"}}}); err == nil || !strings.Contains(err.Error(), "还没有核对过") {
		t.Fatalf("没核对过的安装脚本应让构建失败：%v", err)
	}
}

// ---- 清单与差异 ----

func TestManifestPackages(t *testing.T) {
	base := `{"alpine":"3.22.8","artifacts":[` +
		`{"name":"netboot","version":"1","url":"https://x/a","sha256":"` + strings.Repeat("a", 64) + `","license":"","source":""},` +
		`{"name":"minirootfs","version":"1","url":"https://x/b","sha256":"` + strings.Repeat("a", 64) + `","license":"","source":""},` +
		`{"name":"sing-box","version":"1","url":"https://x/c","sha256":"` + strings.Repeat("a", 64) + `","license":"","source":""},` +
		`{"name":"mihomo","version":"1","url":"https://x/d","sha256":"` + strings.Repeat("a", 64) + `","license":"","source":""}]`
	ok := base + `,"packages":{"repos":["https://r/main/aarch64"],"keys":"guest/keys","want":["a"],"list":[{"name":"a","version":"1","url":"https://r/a-1.apk","sha256":"` + strings.Repeat("b", 64) + `"}]}}`
	if _, err := ParseManifest([]byte(ok)); err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(ok, strings.Repeat("b", 64), strings.Repeat("b", 63)+"G", 1)
	if _, err := ParseManifest([]byte(bad)); err == nil {
		t.Fatal("包的 SHA-256 不合法应拒绝")
	}
	if _, err := ParseManifest([]byte(strings.Replace(ok, "https://r/main", "http://r/main", 1))); err == nil {
		t.Fatal("仓库不是 https 应拒绝")
	}
}

func TestDiffPinned(t *testing.T) {
	old := []PinnedPackage{{Name: "a", Version: "1", SHA256: "x"}, {Name: "b", Version: "1", SHA256: "y"}, {Name: "c", Version: "1", SHA256: "z"}}
	cur := []PinnedPackage{{Name: "a", Version: "2", SHA256: "x2"}, {Name: "b", Version: "1", SHA256: "y2"}, {Name: "d", Version: "1"}}
	got := strings.Join(DiffPinned(old, cur), "\n")
	for _, w := range []string{"~ a 1 → 2", "! b 1：版本没变但 SHA-256 变了", "- c 1", "+ d 1"} {
		if !strings.Contains(got, w) {
			t.Fatalf("差异里应有 %q：\n%s", w, got)
		}
	}
	if len(DiffPinned(cur, cur)) != 0 {
		t.Fatal("相同时应没有差异")
	}
}
