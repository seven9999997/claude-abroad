package imagebuild

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTreeAddAndOrder(t *testing.T) {
	tr := NewTree()
	must(t, tr.AddFile("/usr/bin/x", 0o755, []byte("x")))
	must(t, tr.AddSymlink("/bin/sh", "/bin/busybox"))
	must(t, tr.AddDir("/a-b", 0o700))
	must(t, tr.AddFile("/a/b", 0o600, nil))
	var paths []string
	for _, e := range tr.Entries() {
		paths = append(paths, e.Path)
	}
	want := "/ /a /a/b /a-b /bin /bin/sh /usr /usr/bin /usr/bin/x"
	if strings.Join(paths, " ") != want {
		t.Fatalf("顺序不对：%v", paths)
	}
	if e, _ := tr.Get("/usr"); !e.IsDir() || e.Mode.Perm() != 0o755 {
		t.Fatal("上级目录应自动补成 0755")
	}
	if err := tr.AddFile("/usr", 0o644, nil); err == nil {
		t.Fatal("不能用文件替换目录")
	}
	if err := tr.AddFile("/usr/bin/x/y", 0o644, nil); err == nil {
		t.Fatal("不能把文件放在文件下面")
	}
	if err := tr.Add(Entry{Path: "/p", Mode: fs.ModeNamedPipe}); err == nil {
		t.Fatal("不支持的类型应报错")
	}
	tr.Remove("/usr")
	if _, ok := tr.Get("/usr/bin/x"); ok {
		t.Fatal("Remove 应连同子项删除")
	}
}

func TestCPIORoundTrip(t *testing.T) {
	tr := NewTree()
	must(t, tr.AddFile("/init", 0o755, []byte("#!/bin/sh\n")))
	must(t, tr.AddFile("/etc/secret", 0o600, []byte("abc")))
	must(t, tr.Add(Entry{Path: "/etc/owned", Mode: 0o644, UID: 501, GID: 20, Data: []byte("z")}))
	must(t, tr.AddSymlink("/bin/sh", "busybox"))
	must(t, tr.AddCharDev("/dev/console", 0o600, 5, 1))
	must(t, tr.Add(Entry{Path: "/bin/su", Mode: 0o755 | fs.ModeSetuid, Data: []byte("su")}))
	var buf bytes.Buffer
	must(t, WriteCPIO(&buf, tr))
	raw := buf.Bytes()
	if len(raw)%4 != 0 {
		t.Fatalf("cpio 总长 %d 不是 4 的倍数", len(raw))
	}
	got, err := ReadCPIO(bytes.NewReader(raw))
	must(t, err)

	// 自己按格式逐项解析：顺序、mode、属主为 0、对齐、以 TRAILER!!! 结尾。
	off := 0
	var names []string
	for {
		if off%4 != 0 {
			t.Fatalf("条目头偏移 %d 没有 4 字节对齐", off)
		}
		h := raw[off : off+cpioHdrLen]
		field := func(i int) int {
			v, _ := strconv.ParseUint(string(h[6+8*i:14+8*i]), 16, 32)
			return int(v)
		}
		if string(h[:6]) != "070701" || field(2) != 0 || field(3) != 0 {
			t.Fatalf("偏移 %d 的头不对：%q", off, h[:30])
		}
		ns, size := field(11), field(6)
		name := string(raw[off+cpioHdrLen : off+cpioHdrLen+ns-1])
		names = append(names, name)
		off += cpioHdrLen + ns + pad4(cpioHdrLen+ns)
		off += size + pad4(size)
		if name == cpioTrailer {
			break
		}
	}
	if off != len(raw) {
		t.Fatalf("TRAILER!!! 之后还有 %d 字节", len(raw)-off)
	}
	want := "bin bin/sh bin/su dev dev/console etc etc/owned etc/secret init TRAILER!!!"
	if strings.Join(names, " ") != want {
		t.Fatalf("条目顺序不对：%v", names)
	}

	by := map[string]CPIOEntry{}
	for _, e := range got {
		by[e.Path] = e
		if e.UID != 0 || e.GID != 0 {
			t.Errorf("%s 属主应为 0:0（WriteCPIO 固定属主）", e.Path)
		}
	}
	if e := by["/init"]; e.Mode.Perm() != 0o755 || string(e.Data) != "#!/bin/sh\n" {
		t.Errorf("/init 不对：%v %q", e.Mode, e.Data)
	}
	if e := by["/etc/secret"]; e.Mode.Perm() != 0o600 || string(e.Data) != "abc" {
		t.Errorf("/etc/secret 不对：%v", e.Mode)
	}
	if e := by["/bin/sh"]; !e.IsSymlink() || e.Link != "busybox" {
		t.Errorf("/bin/sh 不对：%+v", e.Entry)
	}
	if e := by["/dev/console"]; !e.IsCharDev() || e.DevMajor != 5 || e.DevMinor != 1 {
		t.Errorf("/dev/console 不对：%+v", e.Entry)
	}
	if e := by["/bin/su"]; e.Mode&fs.ModeSetuid == 0 {
		t.Error("setuid 位丢了")
	}
}

func TestReadCPIOErrors(t *testing.T) {
	var buf bytes.Buffer
	tr := NewTree()
	must(t, tr.AddFile("/a", 0o644, []byte("hello")))
	must(t, WriteCPIO(&buf, tr))
	raw := buf.Bytes()
	if _, err := ReadCPIO(bytes.NewReader(raw[:len(raw)-20])); err == nil {
		t.Error("截断的 cpio 应报错")
	}
	bad := append([]byte(nil), raw...)
	copy(bad, "070707")
	if _, err := ReadCPIO(bytes.NewReader(bad)); err == nil {
		t.Error("旧格式魔数应报错")
	}
}

// fakeImage 构造一个最小的"arm64 Image"：64 字节头，偏移 56 处是魔数。
func fakeImage() []byte {
	b := make([]byte, 4096)
	copy(b[56:], arm64Magic)
	copy(b[64:], "kernel-body")
	return b
}

func gz(t *testing.T, b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(b)
	must(t, zw.Close())
	return buf.Bytes()
}

func zboot(payload []byte, comp string) []byte {
	const off = 512
	b := make([]byte, off+len(payload)+64)
	copy(b, "MZ")
	copy(b[4:], "zimg")
	binary.LittleEndian.PutUint32(b[8:], off)
	binary.LittleEndian.PutUint32(b[12:], uint32(len(payload)))
	copy(b[24:], comp)
	copy(b[off:], payload)
	return b
}

func TestExtractImage(t *testing.T) {
	img := fakeImage()
	for name, in := range map[string][]byte{
		"已解压":   img,
		"gzip":  gz(t, img),
		"zboot": zboot(gz(t, img), "gzip"),
	} {
		out, err := ExtractImage(in)
		if err != nil || !bytes.Equal(out, img) {
			t.Errorf("%s：解出失败 %v", name, err)
		}
	}
	notImage := make([]byte, 4096)
	for name, in := range map[string][]byte{
		"未知压缩":       zboot(gz(t, img), "zstd"),
		"解压后魔数不对":    gz(t, notImage),
		"zboot 魔数不对": zboot(gz(t, notImage), "gzip"),
		"随机数据":       []byte("not a kernel at all, definitely not"),
		"偏移越界":       func() []byte { b := zboot(gz(t, img), "gzip"); binary.LittleEndian.PutUint32(b[12:], 1<<30); return b }(),
	} {
		if _, err := ExtractImage(in); err == nil {
			t.Errorf("%s：应报错", name)
		}
	}
}

func shaHex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestManifestValidation(t *testing.T) {
	good := func() string {
		var parts []string
		for _, n := range RequiredArtifacts {
			parts = append(parts, `{"name":"`+n+`","version":"1","url":"https://example.com/`+n+`.tar.gz","sha256":"`+strings.Repeat("a", 64)+`","license":"x","source":"y"}`)
		}
		return `{"alpine":"3.22.8","artifacts":[` + strings.Join(parts, ",") + `]}`
	}
	if _, err := ParseManifest([]byte(good())); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]string{
		"缺 sha":  strings.Replace(good(), strings.Repeat("a", 64), "", 1),
		"sha 大写": strings.Replace(good(), strings.Repeat("a", 64), strings.Repeat("A", 64), 1),
		"http":   strings.Replace(good(), "https://", "http://", 1),
		"未知字段":   strings.Replace(good(), `"alpine"`, `"extra":1,"alpine"`, 1),
		"缺必需条目":  strings.Replace(good(), `"name":"mihomo"`, `"name":"other"`, 1),
		"重复":     strings.Replace(good(), `"name":"mihomo"`, `"name":"sing-box"`, 1),
	} {
		if _, err := ParseManifest([]byte(s)); err == nil {
			t.Errorf("%s：应报错", name)
		}
	}
}

func TestRepoManifestIsValid(t *testing.T) {
	m, err := LoadManifest(filepath.Join("..", "..", "guest", "manifest.json"))
	if err != nil {
		t.Fatalf("仓库里的 guest/manifest.json 无效：%v", err)
	}
	if a, _ := m.Get("netboot"); !strings.HasSuffix(a.FileName(), ".tar.gz") {
		t.Errorf("netboot 文件名不对：%s", a.FileName())
	}
}

func TestFetch(t *testing.T) {
	body := []byte(strings.Repeat("payload", 1000))
	mux := http.NewServeMux()
	mux.HandleFunc("/ok.bin", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) })
	mux.HandleFunc("/404.bin", http.NotFound)
	mux.HandleFunc("/cut.bin", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body[:100])
		if hj, ok := w.(http.Hijacker); ok {
			c, _, _ := hj.Hijack()
			c.Close()
		}
	})
	release := make(chan struct{})
	mux.HandleFunc("/stall.bin", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body[:100])
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	defer close(release)
	dir := t.TempDir()
	ctx := context.Background()

	a := Artifact{Name: "ok", URL: srv.URL + "/ok.bin", SHA256: shaHex(body)}
	p, err := Fetch(ctx, srv.Client(), a, dir)
	must(t, err)
	if b, _ := os.ReadFile(p); !bytes.Equal(b, body) {
		t.Fatal("下载内容不对")
	}
	// 已存在且校验通过：不再请求（换一个会失败的客户端也能成功）。
	if _, err := Fetch(ctx, &http.Client{Transport: failingTransport{}}, a, dir); err != nil {
		t.Fatalf("已缓存的文件应直接使用：%v", err)
	}

	bad := Artifact{Name: "bad", URL: srv.URL + "/ok.bin", SHA256: strings.Repeat("0", 64)}
	_ = os.Remove(p)
	if _, err := Fetch(ctx, srv.Client(), bad, dir); !errors.Is(err, ErrChecksum) {
		t.Fatalf("SHA-256 不符应返回 ErrChecksum：%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ok.bin")); !os.IsNotExist(err) {
		t.Fatal("校验失败时不应留下目标文件")
	}

	cut := Artifact{Name: "cut", URL: srv.URL + "/cut.bin", SHA256: shaHex(body)}
	if _, err := Fetch(ctx, srv.Client(), cut, dir); err == nil {
		t.Fatal("中途断开应报错")
	}
	if _, err := os.Stat(filepath.Join(dir, "cut.bin")); !os.IsNotExist(err) {
		t.Fatal("中途断开不应留下目标文件")
	}

	// 服务器发了一部分就不动了：按空闲超时中止，不留文件。
	old := IdleTimeout
	IdleTimeout = 300 * time.Millisecond
	defer func() { IdleTimeout = old }()
	stall := Artifact{Name: "stall", URL: srv.URL + "/stall.bin", SHA256: shaHex(body)}
	if _, err := Fetch(ctx, srv.Client(), stall, dir); err == nil {
		t.Fatal("长时间没有数据应中止")
	}
	if _, err := os.Stat(filepath.Join(dir, "stall.bin")); !os.IsNotExist(err) {
		t.Fatal("空闲超时不应留下目标文件")
	}

	nf := Artifact{Name: "nf", URL: srv.URL + "/404.bin", SHA256: shaHex(body)}
	if _, err := Fetch(ctx, srv.Client(), nf, dir); err == nil {
		t.Fatal("404 应报错")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".part") {
			t.Fatalf("不应留下临时文件：%s", e.Name())
		}
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("不应发起请求")
}

// tarGz 用条目表生成 tar.gz（测试用）。
func tarGz(t *testing.T, items []tar.Header, data map[string][]byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, h := range items {
		h := h
		if d, ok := data[h.Name]; ok {
			h.Size = int64(len(d))
		}
		must(t, tw.WriteHeader(&h))
		if d, ok := data[h.Name]; ok {
			_, _ = tw.Write(d)
		}
	}
	must(t, tw.Close())
	must(t, zw.Close())
	return buf.Bytes()
}

func TestAddTarGzAndExtract(t *testing.T) {
	b := tarGz(t, []tar.Header{
		{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "./bin/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "./bin/busybox", Typeflag: tar.TypeReg, Mode: 0o755},
		{Name: "./bin/sh", Typeflag: tar.TypeSymlink, Linkname: "/bin/busybox", Mode: 0o777},
		{Name: "./bin/ash", Typeflag: tar.TypeLink, Linkname: "./bin/busybox", Mode: 0o755},
		{Name: "./etc/shadow", Typeflag: tar.TypeReg, Mode: 0o640, Gid: 42},
		{Name: "./dev/null", Typeflag: tar.TypeChar, Mode: 0o666, Devmajor: 1, Devminor: 3},
	}, map[string][]byte{"./bin/busybox": []byte("BB"), "./etc/shadow": []byte("root:*")})
	tr := NewTree()
	must(t, AddTarGz(tr, bytes.NewReader(b)))
	if e, ok := tr.Get("/bin/ash"); !ok || string(e.Data) != "BB" {
		t.Error("硬链接应复制为独立文件")
	}
	if e, _ := tr.Get("/bin/sh"); !e.IsSymlink() || e.Link != "/bin/busybox" {
		t.Error("符号链接不对")
	}
	if e, _ := tr.Get("/etc/shadow"); e.Mode.Perm() != 0o640 || e.GID != 42 {
		t.Error("权限位或属组没保留")
	}
	if _, ok := tr.Get("/dev/null"); ok {
		t.Error("设备节点应跳过")
	}
	got, err := ExtractFiles(bytes.NewReader(b), []string{"bin/busybox", "etc/shadow"})
	if err != nil || string(got["bin/busybox"]) != "BB" {
		t.Fatalf("ExtractFiles：%v", err)
	}
	if _, err := ExtractFiles(bytes.NewReader(b), []string{"boot/vmlinuz-virt"}); err == nil || !strings.Contains(err.Error(), "boot/vmlinuz-virt") {
		t.Errorf("缺文件应报错并指出是哪个：%v", err)
	}
	evil := tarGz(t, []tar.Header{{Name: "../etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644}}, nil)
	if err := AddTarGz(NewTree(), bytes.NewReader(evil)); err == nil {
		t.Error("越界路径应拒绝")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
