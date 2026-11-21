package imagebuild

import (
	"bufio"
	"bytes"
	"claude-abroad/internal/guestabi"
	"claude-abroad/internal/kmod"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"claude-abroad/internal/usererror"
)

// RootFS 是根镜像的写入和回读。实现见 squashfs.go；做成接口是为了在写入库
// 不满足要求（属主、权限位、符号链接）时可以整体替换。
type RootFS interface {
	// Write 把树写成镜像文件 dst。
	Write(dst string, t *Tree) error
	// Read 把镜像读回成树（verify 和回读测试用）。
	Read(src string) (*Tree, error)
}

// 输出文件名（guest/out/ 下）。
const (
	OutKernel    = "Image"
	OutInitramfs = "initramfs.gz"
	OutRootFS    = "rootfs.img"
	OutModloop   = "modloop"
	OutSums      = "SHA256SUMS"
	OutInfo      = "build.json"
	CacheDirName = "cache"
)

// netboot 包里要用的文件。
var netbootFiles = []string{"boot/vmlinuz-virt", "boot/initramfs-virt", "boot/modloop-virt"}

// BootModules 是 initramfs 里必须带的模块：挂载根盘（virtio-blk 上的只读镜像）
// 和 modloop 需要。其余模块（vsock、virtio_net、overlay 等）由 agent 从 modloop 加载。
var BootModules = []string{"virtio_blk", "squashfs"}

// 镜像里的固定路径。
const (
	AgentPath       = guestabi.AgentPath
	BootModulesList = guestabi.BootModulesList // initramfs 里，按加载顺序列出模块路径
	SingBoxPath     = guestabi.SingBoxPath
	MihomoPath      = guestabi.MihomoPath
)

// BuildOptions 控制一次构建。
type BuildOptions struct {
	ManifestPath string
	OutDir       string       // 通常是 guest/out
	Agent        []byte       // abroad-agent（linux/arm64）；为空时放占位脚本（M1-2a）
	Client       *http.Client // 下载用
	RootFS       RootFS
	Log          func(format string, a ...any)
}

// BuildInfo 写入 build.json，verify 和 vmhost 读取。
type BuildInfo struct {
	KernelVersion string            `json:"kernel_version"`
	BootModules   []string          `json:"boot_modules"` // initramfs 内的路径，按加载顺序
	Artifacts     map[string]string `json:"artifacts"`    // 名称 → 版本
	AgentIsStub   bool              `json:"agent_is_stub"`
	Packages      map[string]string `json:"packages,omitempty"` // 装进根文件系统的软件包：名字 → 版本
	Files         map[string]string `json:"files"`              // 输出文件 → SHA-256
}

// PlaceholderAgent 是 M1-2a 阶段的占位 /init：真正的 agent 在 M1-2b 装入。
var PlaceholderAgent = []byte("#!/bin/sh\necho 'abroad-agent 占位程序：请用 M1-2b 之后的版本重新构建镜像' >&2\nexit 1\n")

// Build 下载输入、组装并写出全部镜像文件。
func Build(ctx context.Context, o BuildOptions) (*BuildInfo, error) {
	logf := o.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if len(o.Agent) > 0 {
		if err := CheckAgentELF(o.Agent); err != nil {
			return nil, usererror.New("检查 abroad-agent", err.Error(),
				"请用 GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags \"-s -w\" -o guest/out/abroad-agent ./cmd/abroad-agent 构建。", err)
		}
	}
	m, err := LoadManifest(o.ManifestPath)
	if err != nil {
		return nil, err
	}
	cache := filepath.Join(o.OutDir, CacheDirName)
	files := map[string]string{}
	info := &BuildInfo{Artifacts: map[string]string{}, Files: map[string]string{}}
	for _, name := range RequiredArtifacts {
		a, _ := m.Get(name)
		logf("下载并校验 %s %s", a.Name, a.Version)
		p, err := Fetch(ctx, o.Client, a, cache)
		if err != nil {
			return nil, err
		}
		files[name] = p
		info.Artifacts[name] = a.Version
	}

	logf("解包 netboot")
	nb, err := extractFromFile(files["netboot"], netbootFiles)
	if err != nil {
		return nil, usererror.New("解包 netboot", err.Error(), "netboot 包的结构可能变了，请核对 guest/manifest.json 里的版本。", err)
	}
	img, err := ExtractImage(nb["boot/vmlinuz-virt"])
	if err != nil {
		return nil, usererror.New("解出内核", err.Error(), "请核对 guest/manifest.json 里的 netboot 版本。", err)
	}

	logf("从 Alpine 的 initramfs 取启动模块")
	initrd, kver, mods, err := buildInitramfs(nb["boot/initramfs-virt"], agentOrStub(o.Agent))
	if err != nil {
		return nil, usererror.New("生成 initramfs", err.Error(), "请核对 guest/manifest.json 里的 netboot 版本。", err)
	}
	info.KernelVersion, info.BootModules = kver, mods
	info.AgentIsStub = len(o.Agent) == 0

	logf("组装根文件系统")
	root, err := buildRootTree(files["minirootfs"], files["sing-box"], files["mihomo"], agentOrStub(o.Agent))
	if err != nil {
		return nil, usererror.New("组装根文件系统", err.Error(), "请核对 guest/manifest.json 里的 minirootfs 和引擎版本。", err)
	}
	if ps := m.Packages; ps != nil && len(ps.List) > 0 {
		info.Packages = map[string]string{}
		logf("装软件包（%d 个，安装脚本不执行）", len(ps.List))
		for _, p := range ps.List {
			fp, err := Fetch(ctx, o.Client, p.Artifact(), filepath.Join(cache, "apk"))
			if err != nil {
				return nil, err
			}
			b, err := os.ReadFile(fp)
			if err != nil {
				return nil, err
			}
			meta, err := ExtractAPK(b, root)
			if err != nil {
				return nil, usererror.New("装软件包 "+p.Name, err.Error(), "请重新运行 abroad-imagebuild pin-packages，或核对 guest/manifest.json。", err)
			}
			if meta.Name != p.Name || meta.Version != p.Version {
				return nil, usererror.New("装软件包 "+p.Name, fmt.Sprintf("包里写的是 %s %s，与清单不符", meta.Name, meta.Version),
					"请重新运行 abroad-imagebuild pin-packages。", nil)
			}
			info.Packages[p.Name] = p.Version
		}
		if err := applyScriptEquivalents(root, ps.List); err != nil {
			return nil, usererror.New("处理安装脚本的等价动作", err.Error(), "这是构建工具的问题，请把输出贴给开发者。", err)
		}
	}

	if err := os.MkdirAll(o.OutDir, 0o755); err != nil {
		return nil, usererror.Wrap("创建输出目录", err, "请检查仓库目录的写权限。")
	}
	write := func(name string, data []byte) error {
		return writeFileAtomic(filepath.Join(o.OutDir, name), data)
	}
	if err := write(OutKernel, img); err != nil {
		return nil, err
	}
	if err := write(OutInitramfs, initrd); err != nil {
		return nil, err
	}
	if err := write(OutModloop, nb["boot/modloop-virt"]); err != nil {
		return nil, err
	}
	logf("写根镜像")
	tmp := filepath.Join(o.OutDir, "."+OutRootFS+".part")
	_ = os.Remove(tmp)
	if err := o.RootFS.Write(tmp, root); err != nil {
		_ = os.Remove(tmp)
		return nil, usererror.New("写根镜像", err.Error(), "这是构建工具的问题，请把输出贴给开发者。", err)
	}
	if err := os.Rename(tmp, filepath.Join(o.OutDir, OutRootFS)); err != nil {
		return nil, err
	}

	var sums strings.Builder
	for _, name := range []string{OutKernel, OutInitramfs, OutModloop, OutRootFS} {
		h, err := sha256File(filepath.Join(o.OutDir, name))
		if err != nil {
			return nil, err
		}
		info.Files[name] = h
		fmt.Fprintf(&sums, "%s  %s\n", h, name)
	}
	if err := write(OutSums, []byte(sums.String())); err != nil {
		return nil, err
	}
	ib, _ := json.MarshalIndent(info, "", "  ")
	if err := write(OutInfo, append(ib, '\n')); err != nil {
		return nil, err
	}
	return info, nil
}

func agentOrStub(a []byte) []byte {
	if len(a) == 0 {
		return PlaceholderAgent
	}
	return a
}

func extractFromFile(p string, want []string) (map[string][]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ExtractFiles(bufio.NewReaderSize(f, 1<<20), want)
}

// buildInitramfs 从 Alpine 的 initramfs 里取出启动模块（解压成 .ko），
// 和 agent（作为 /init）一起打成新的 initramfs。返回 gzip 后的 cpio、内核版本、模块路径。
func buildInitramfs(alpineInitrd, agent []byte) ([]byte, string, []string, error) {
	if bytes.HasPrefix(alpineInitrd, []byte{0x28, 0xb5, 0x2f, 0xfd}) {
		return nil, "", nil, fmt.Errorf("Alpine 的 initramfs 是 zstd 压缩，构建工具目前只支持 gzip")
	}
	zr, err := gzip.NewReader(bytes.NewReader(alpineInitrd))
	if err != nil {
		return nil, "", nil, fmt.Errorf("Alpine 的 initramfs 不是 gzip：%w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(zr, 1<<30))
	if err != nil {
		return nil, "", nil, fmt.Errorf("解压 Alpine 的 initramfs：%w", err)
	}
	entries, err := ReadCPIOAll(raw)
	if err != nil {
		return nil, "", nil, fmt.Errorf("解析 Alpine 的 initramfs：%w", err)
	}
	byPath := map[string]*CPIOEntry{}
	for i := range entries {
		byPath[entries[i].Path] = &entries[i]
	}
	kver, err := findKernelVersion(entries)
	if err != nil {
		return nil, "", nil, err
	}
	modDir := "/lib/modules/" + kver
	dep, ok := byPath[modDir+"/modules.dep"]
	if !ok {
		return nil, "", nil, fmt.Errorf("Alpine 的 initramfs 里没有 %s/modules.dep", modDir)
	}
	deps, err := kmod.ParseModulesDep(bytes.NewReader(dep.Data))
	if err != nil {
		return nil, "", nil, err
	}
	builtin := map[string]bool{}
	if b, ok := byPath[modDir+"/modules.builtin"]; ok {
		for _, l := range strings.Fields(string(b.Data)) {
			builtin[kmod.ModuleName(l)] = true
		}
	}
	order, err := deps.Resolve(BootModules, builtin)
	if err != nil {
		return nil, "", nil, err
	}

	t := NewTree()
	for _, d := range []string{"/dev", "/proc", "/sys", "/run", "/mnt", guestabi.NewRoot, guestabi.ModloopDir, "/etc/abroad"} {
		if err := t.AddDir(d, 0o755); err != nil {
			return nil, "", nil, err
		}
	}
	if err := t.AddCharDev("/dev/console", 0o600, 5, 1); err != nil {
		return nil, "", nil, err
	}
	if err := t.AddFile("/init", 0o755, agent); err != nil {
		return nil, "", nil, err
	}
	var list []string
	for _, rel := range order {
		src, ok := byPath[modDir+"/"+rel]
		if !ok {
			return nil, "", nil, fmt.Errorf("Alpine 的 initramfs 里缺少模块文件 %s", rel)
		}
		ko, err := kmod.DecompressModule(rel, src.Data)
		if err != nil {
			return nil, "", nil, err
		}
		dst := modDir + "/" + kmod.UncompressedName(rel)
		if err := t.AddFile(dst, 0o644, ko); err != nil {
			return nil, "", nil, err
		}
		list = append(list, dst)
	}
	if err := t.AddFile(BootModulesList, 0o644, []byte(strings.Join(list, "\n")+"\n")); err != nil {
		return nil, "", nil, err
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err := WriteCPIO(zw, t); err != nil {
		return nil, "", nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, "", nil, err
	}
	return buf.Bytes(), kver, list, nil
}

func findKernelVersion(entries []CPIOEntry) (string, error) {
	var vers []string
	for _, e := range entries {
		if e.IsDir() && path.Dir(e.Path) == "/lib/modules" {
			vers = append(vers, path.Base(e.Path))
		}
	}
	sort.Strings(vers)
	switch len(vers) {
	case 0:
		return "", fmt.Errorf("Alpine 的 initramfs 里没有 /lib/modules/<版本> 目录")
	case 1:
		return vers[0], nil
	}
	return "", fmt.Errorf("Alpine 的 initramfs 里有多个内核版本：%s", strings.Join(vers, "、"))
}

// buildRootTree 组装根文件系统：minirootfs + 两个引擎 + agent。
func buildRootTree(minirootfs, singbox, mihomo string, agent []byte) (*Tree, error) {
	t := NewTree()
	f, err := os.Open(minirootfs)
	if err != nil {
		return nil, err
	}
	err = AddTarGz(t, bufio.NewReaderSize(f, 1<<20))
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("读取 minirootfs：%w", err)
	}
	if _, ok := t.Get("/bin/busybox"); !ok {
		return nil, fmt.Errorf("minirootfs 里没有 /bin/busybox")
	}

	sb, err := singBoxBinary(singbox)
	if err != nil {
		return nil, err
	}
	mh, err := gunzipFile(mihomo)
	if err != nil {
		return nil, fmt.Errorf("解压 mihomo：%w", err)
	}
	for _, x := range []struct {
		p    string
		perm os.FileMode
		data []byte
	}{
		{SingBoxPath, 0o755, sb},
		{MihomoPath, 0o755, mh},
		{AgentPath, 0o755, agent},
		{"/etc/resolv.conf", 0o644, nil}, // 4.5：虚拟机不使用网关 DNS
		{"/etc/hostname", 0o644, []byte("abroad\n")},
	} {
		t.Remove(x.p) // minirootfs 里的 resolv.conf 可能是符号链接
		if err := t.AddFile(x.p, x.perm, x.data); err != nil {
			return nil, err
		}
	}
	for _, d := range []string{guestabi.ModloopDir, "/run", "/proc", "/sys", "/dev", "/tmp"} {
		if e, ok := t.Get(d); !ok || !e.IsDir() {
			if err := t.AddDir(d, 0o755); err != nil {
				return nil, err
			}
		}
	}
	return t, nil
}

// singBoxBinary 从 sing-box 的发行包里取出可执行文件（包内路径是 sing-box-<版本>-linux-arm64/sing-box）。
func singBoxBinary(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tr, err := gzipTar(bufio.NewReader(f))
	if err != nil {
		return nil, fmt.Errorf("读取 sing-box 发行包：%w", err)
	}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("sing-box 发行包里没有 sing-box 可执行文件")
		}
		if err != nil {
			return nil, err
		}
		if path.Base(h.Name) == "sing-box" && h.Typeflag == '0' {
			return io.ReadAll(io.LimitReader(tr, maxTarFile))
		}
	}
}

func gunzipFile(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(bufio.NewReader(f))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(zr, maxTarFile))
}

func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeFileAtomic(p string, data []byte) error {
	tmp := p + ".part"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		_ = os.Remove(tmp)
		return usererror.Wrap("写 "+filepath.Base(p), err, "请检查仓库目录的写权限和磁盘空间。")
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return usererror.Wrap("写 "+filepath.Base(p), err, "请检查仓库目录的写权限。")
	}
	return nil
}
