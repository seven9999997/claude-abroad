package imagebuild

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"debug/elf"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testKver = "6.12.115-0-virt"

// fakeInputs 生成一套假的镜像输入（结构与真实文件一致，内容很小）。
func fakeInputs(t *testing.T) map[string][]byte {
	modDir := "lib/modules/" + testKver + "/"
	modELF := func(s string) []byte { return gz(t, []byte("\x7fELF"+s)) }
	alpineTree := NewTree()
	must(t, alpineTree.AddFile("/init", 0o755, []byte("alpine init")))
	must(t, alpineTree.AddFile("/"+modDir+"modules.dep",
		0o644, []byte("kernel/drivers/block/virtio_blk.ko.gz:\nkernel/fs/squashfs/squashfs.ko.gz:\nkernel/drivers/virtio/virtio_mmio.ko.gz:\n")))
	must(t, alpineTree.AddFile("/"+modDir+"kernel/drivers/block/virtio_blk.ko.gz", 0o644, modELF("blk")))
	must(t, alpineTree.AddFile("/"+modDir+"kernel/fs/squashfs/squashfs.ko.gz", 0o644, modELF("sq")))
	must(t, alpineTree.AddFile("/"+modDir+"kernel/drivers/virtio/virtio_mmio.ko.gz", 0o644, modELF("mmio")))
	var cpio bytes.Buffer
	must(t, WriteCPIO(&cpio, alpineTree))

	netboot := tarGz(t, []tar.Header{
		{Name: "boot/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "boot/vmlinuz-virt", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "boot/initramfs-virt", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "boot/modloop-virt", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "boot/config-6.12.115-0-virt", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "boot/vmlinuz-lts", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string][]byte{
		"boot/vmlinuz-virt":           zboot(gz(t, fakeImage()), "gzip"),
		"boot/initramfs-virt":         gz(t, cpio.Bytes()),
		"boot/modloop-virt":           append([]byte("hsqs"), make([]byte, 4092)...),
		"boot/config-6.12.115-0-virt": []byte("CONFIG_SQUASHFS=m\n"),
		"boot/vmlinuz-lts":            []byte("ignored"),
	})
	minirootfs := tarGz(t, []tar.Header{
		{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "./bin/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "./bin/busybox", Typeflag: tar.TypeReg, Mode: 0o755},
		{Name: "./bin/sh", Typeflag: tar.TypeSymlink, Linkname: "/bin/busybox", Mode: 0o777},
		{Name: "./sbin/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "./sbin/ip", Typeflag: tar.TypeSymlink, Linkname: "/bin/busybox", Mode: 0o777},
		{Name: "./etc/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "./etc/resolv.conf", Typeflag: tar.TypeSymlink, Linkname: "../run/resolv.conf", Mode: 0o777},
		{Name: "./tmp/", Typeflag: tar.TypeDir, Mode: 0o1777},
	}, map[string][]byte{"./bin/busybox": []byte("\x7fELFbusybox")})
	singbox := tarGz(t, []tar.Header{
		{Name: "sing-box-1.14.4-linux-arm64/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "sing-box-1.14.4-linux-arm64/LICENSE", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "sing-box-1.14.4-linux-arm64/sing-box", Typeflag: tar.TypeReg, Mode: 0o755},
	}, map[string][]byte{"sing-box-1.14.4-linux-arm64/LICENSE": []byte("GPL"), "sing-box-1.14.4-linux-arm64/sing-box": fakeELF(elf.EM_AARCH64, false)})
	return map[string][]byte{
		"netboot.tar.gz":    netboot,
		"minirootfs.tar.gz": minirootfs,
		"sing-box.tar.gz":   singbox,
		"mihomo.gz":         gz(t, fakeELF(elf.EM_AARCH64, false)),
	}
}

// setupBuild 起一个 TLS 测试服务器提供假输入，写出对应的清单。
func setupBuild(t *testing.T) (BuildOptions, *httptest.Server) {
	inputs := fakeInputs(t)
	mux := http.NewServeMux()
	for name, b := range inputs {
		b := b
		mux.HandleFunc("/"+name, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(b) })
	}
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	m := Manifest{Alpine: "3.22.8"}
	for _, x := range []struct{ name, file string }{
		{"netboot", "netboot.tar.gz"}, {"minirootfs", "minirootfs.tar.gz"},
		{"sing-box", "sing-box.tar.gz"}, {"mihomo", "mihomo.gz"},
	} {
		m.Artifacts = append(m.Artifacts, Artifact{Name: x.name, Version: "test", URL: srv.URL + "/" + x.file, SHA256: shaHex(inputs[x.file])})
	}
	dir := t.TempDir()
	mp := filepath.Join(dir, "manifest.json")
	mb, _ := json.Marshal(m)
	must(t, os.WriteFile(mp, mb, 0o644))
	return BuildOptions{ManifestPath: mp, OutDir: filepath.Join(dir, "out"), Client: srv.Client(), RootFS: Squashfs{}}, srv
}

func TestBuildAndVerify(t *testing.T) {
	o, _ := setupBuild(t)
	o.Agent = fakeELF(elf.EM_AARCH64, false)
	info, err := Build(context.Background(), o)
	must(t, err)
	if info.KernelVersion != testKver || info.AgentIsStub {
		t.Fatalf("build.json 不对：%+v", info)
	}
	if got := strings.Join(info.BootModules, " "); !strings.Contains(got, "virtio_blk.ko") || !strings.Contains(got, "squashfs.ko") || strings.Contains(got, "mmio") {
		t.Fatalf("启动模块不对：%s", got)
	}
	if probs := Verify(o.OutDir, Squashfs{}); len(probs) != 0 {
		t.Fatalf("刚构建的镜像应通过 verify：%v", probs)
	}

	// 根镜像内容。
	root, err := Squashfs{}.Read(filepath.Join(o.OutDir, OutRootFS))
	must(t, err)
	if e, _ := root.Get(AgentPath); !bytes.Equal(e.Data, o.Agent) || e.Mode.Perm() != 0o755 {
		t.Error("根镜像里的 agent 不对")
	}
	if e, _ := root.Get("/etc/resolv.conf"); e == nil || !e.IsRegular() || len(e.Data) != 0 {
		t.Error("resolv.conf 应替换为空的普通文件")
	}
	if e, _ := root.Get("/tmp"); e.Mode&os.ModeSticky == 0 {
		t.Error("/tmp 的 sticky 位丢了")
	}

	// 输出目录里不留临时文件，下载缓存在 cache/ 下。
	entries, _ := os.ReadDir(o.OutDir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".part") {
			t.Errorf("留下了临时文件 %s", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(o.OutDir, CacheDirName, "netboot.tar.gz")); err != nil {
		t.Error("下载缓存应在 cache/ 下")
	}
}

func TestBuildStubAgent(t *testing.T) {
	o, _ := setupBuild(t)
	info, err := Build(context.Background(), o)
	must(t, err)
	if !info.AgentIsStub {
		t.Error("没给 agent 时应标记为占位")
	}
}

func TestVerifyDetectsDamage(t *testing.T) {
	o, _ := setupBuild(t)
	o.Agent = fakeELF(elf.EM_AARCH64, false)
	_, err := Build(context.Background(), o)
	must(t, err)
	clean := t.TempDir()
	copyDir(t, o.OutDir, clean)

	cases := map[string]struct {
		damage func(dir string)
		item   string
	}{
		"改内核一个字节":   {func(d string) { flipByte(t, filepath.Join(d, OutKernel), 56) }, OutKernel},
		"删 modloop": {func(d string) { _ = os.Remove(filepath.Join(d, OutModloop)) }, OutModloop},
		"删一个启动模块": {func(d string) {
			rewriteInitramfs(t, filepath.Join(d, OutInitramfs), func(tr *Tree) {
				tr.Remove("/lib/modules/" + testKver + "/kernel/fs/squashfs/squashfs.ko")
			})
		}, OutInitramfs},
		"sing-box 是 glibc 版": {func(d string) {
			rewriteRootFS(t, d, func(tr *Tree) {
				tr.Remove(SingBoxPath)
				must(t, tr.AddFile(SingBoxPath, 0o755, fakeELF(elf.EM_AARCH64, true)))
			})
		}, OutRootFS},
		"根镜像缺 /sbin/ip": {func(d string) { rewriteRootFS(t, d, func(tr *Tree) { tr.Remove("/sbin/ip") }) }, OutRootFS},
		"根镜像被截断":        {func(d string) { truncate(t, filepath.Join(d, OutRootFS), 200) }, OutRootFS},
	}
	for name, c := range cases {
		dir := t.TempDir()
		copyDir(t, clean, dir)
		c.damage(dir)
		probs := Verify(dir, Squashfs{})
		found := false
		for _, p := range probs {
			if p.Item == c.item {
				found = true
			}
		}
		if !found {
			t.Errorf("%s：verify 应指出 %s 有问题，实际：%v", name, c.item, probs)
		}
	}
}

func TestBuildFailsOnChecksumMismatch(t *testing.T) {
	o, _ := setupBuild(t)
	b, _ := os.ReadFile(o.ManifestPath)
	var m Manifest
	must(t, json.Unmarshal(b, &m))
	m.Artifacts[1].SHA256 = strings.Repeat("0", 64)
	b, _ = json.Marshal(m)
	must(t, os.WriteFile(o.ManifestPath, b, 0o644))
	if _, err := Build(context.Background(), o); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("校验不符应失败并说明原因：%v", err)
	}
	if _, err := os.Stat(filepath.Join(o.OutDir, OutRootFS)); !os.IsNotExist(err) {
		t.Error("失败时不应生成根镜像")
	}
}

func copyDir(t *testing.T, src, dst string) {
	entries, err := os.ReadDir(src)
	must(t, err)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		must(t, err)
		must(t, os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644))
	}
}

func flipByte(t *testing.T, p string, off int) {
	b, err := os.ReadFile(p)
	must(t, err)
	b[off] ^= 0xff
	must(t, os.WriteFile(p, b, 0o644))
}

func truncate(t *testing.T, p string, n int64) { must(t, os.Truncate(p, n)) }

// rewriteInitramfs 改写 initramfs 并同步更新 SHA256SUMS，模拟"校验和对得上但内容缺项"。
func rewriteInitramfs(t *testing.T, p string, f func(*Tree)) {
	b, err := os.ReadFile(p)
	must(t, err)
	zr, err := gzip.NewReader(bytes.NewReader(b))
	must(t, err)
	entries, err := ReadCPIO(zr)
	must(t, err)
	tr := NewTree()
	for _, e := range entries {
		must(t, tr.Add(e.Entry))
	}
	f(tr)
	var cpio bytes.Buffer
	must(t, WriteCPIO(&cpio, tr))
	must(t, os.WriteFile(p, gz(t, cpio.Bytes()), 0o644))
	dir := filepath.Dir(p)
	sums, _ := os.ReadFile(filepath.Join(dir, OutSums))
	h, _ := sha256File(p)
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(sums)), "\n") {
		if strings.HasSuffix(l, "  "+OutInitramfs) {
			l = h + "  " + OutInitramfs
		}
		out = append(out, l)
	}
	must(t, os.WriteFile(filepath.Join(dir, OutSums), []byte(strings.Join(out, "\n")+"\n"), 0o644))
}

// rewriteRootFS 改写根镜像并同步更新 SHA256SUMS。
func rewriteRootFS(t *testing.T, dir string, f func(*Tree)) {
	p := filepath.Join(dir, OutRootFS)
	tr, err := Squashfs{}.Read(p)
	must(t, err)
	f(tr)
	must(t, Squashfs{}.Write(p, tr))
	sums, _ := os.ReadFile(filepath.Join(dir, OutSums))
	h, _ := sha256File(p)
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(sums)), "\n") {
		if strings.HasSuffix(l, "  "+OutRootFS) {
			l = h + "  " + OutRootFS
		}
		out = append(out, l)
	}
	must(t, os.WriteFile(filepath.Join(dir, OutSums), []byte(strings.Join(out, "\n")+"\n"), 0o644))
}
