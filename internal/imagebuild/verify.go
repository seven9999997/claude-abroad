package imagebuild

import (
	"bufio"
	"bytes"
	"claude-abroad/internal/guestabi"
	"claude-abroad/internal/kmod"
	"compress/gzip"
	"debug/elf"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Problem 是 verify 发现的一个问题：哪一项、什么问题。
type Problem struct {
	Item   string
	Detail string
}

func (p Problem) String() string { return p.Item + "：" + p.Detail }

// Verify 离线检查 outDir 里的镜像文件，只读不写。返回全部问题（没有问题时为空）。
func Verify(outDir string, rootfs RootFS) []Problem {
	var probs []Problem
	add := func(item, format string, a ...any) {
		probs = append(probs, Problem{Item: item, Detail: fmt.Sprintf(format, a...)})
	}
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(outDir, name))
		if err != nil {
			add(name, "读取失败：%v", err)
			return nil
		}
		return b
	}

	// build.json 与 SHA256SUMS。
	var info BuildInfo
	if b := read(OutInfo); b != nil {
		if err := json.Unmarshal(b, &info); err != nil {
			add(OutInfo, "不是有效的 JSON：%v", err)
		}
	}
	sums := map[string]string{}
	if b := read(OutSums); b != nil {
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) == 2 {
				sums[f[1]] = f[0]
			}
		}
	}
	for _, name := range []string{OutKernel, OutInitramfs, OutModloop, OutRootFS} {
		want, ok := sums[name]
		if !ok {
			add(OutSums, "缺少 %s 的校验值", name)
			continue
		}
		got, err := sha256File(filepath.Join(outDir, name))
		if err != nil {
			add(name, "读取失败：%v", err)
		} else if got != want {
			add(name, "SHA-256 与 SHA256SUMS 不符（文件被改动或构建中断）")
		}
	}

	// 内核。
	if b := read(OutKernel); b != nil && !IsArm64Image(b) {
		add(OutKernel, "不是未压缩的 arm64 Image（偏移 56 处没有 ARM\\x64 魔数）")
	}

	// initramfs：/init 可执行、/dev/console、启动模块齐全且是 ELF。
	var initAgent, rootAgent []byte
	if b := read(OutInitramfs); b != nil {
		initAgent = verifyInitramfs(b, info, add)
	}

	// modloop：squashfs 魔数。
	if b := read(OutModloop); b != nil && (len(b) < 4 || string(b[:4]) != "hsqs") {
		add(OutModloop, "不是 squashfs 镜像")
	}

	// 根镜像回读。
	if rootfs != nil {
		t, err := rootfs.Read(filepath.Join(outDir, OutRootFS))
		if err != nil {
			add(OutRootFS, "无法读回：%v", err)
		} else {
			verifyRoot(t, add)
			if e, ok := t.Get(AgentPath); ok {
				rootAgent = e.Data
			}
		}
	}

	// 真 agent（不是占位程序）：两份都是静态链接的 linux/arm64 程序且内容相同。
	if !info.AgentIsStub && initAgent != nil {
		if err := CheckAgentELF(initAgent); err != nil {
			add(OutInitramfs, "/init：%v", err)
		}
		if rootfs != nil && rootAgent != nil && !bytes.Equal(initAgent, rootAgent) {
			add(OutRootFS, "%s 与 initramfs 的 /init 不是同一个程序", AgentPath)
		}
	}
	return probs
}

func verifyInitramfs(b []byte, info BuildInfo, add func(string, string, ...any)) (agent []byte) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		add(OutInitramfs, "不是 gzip：%v", err)
		return nil
	}
	entries, err := ReadCPIO(zr)
	if err != nil {
		add(OutInitramfs, "cpio 无法解析：%v", err)
		return nil
	}
	by := map[string]CPIOEntry{}
	for _, e := range entries {
		by[e.Path] = e
		if e.UID != 0 || e.GID != 0 {
			add(OutInitramfs, "%s 的属主不是 0:0", e.Path)
		}
	}
	if e, ok := by["/init"]; !ok || !e.IsRegular() || e.Mode.Perm()&0o111 == 0 {
		add(OutInitramfs, "缺少可执行的 /init")
	} else {
		agent = e.Data
	}
	if e, ok := by["/dev/console"]; !ok || !e.IsCharDev() || e.DevMajor != 5 || e.DevMinor != 1 {
		add(OutInitramfs, "缺少 /dev/console（字符设备 5:1）")
	}
	list, ok := by[BootModulesList]
	if !ok {
		add(OutInitramfs, "缺少 %s", BootModulesList)
		return agent
	}
	mods := strings.Fields(string(list.Data))
	if len(mods) == 0 {
		add(OutInitramfs, "%s 是空的", BootModulesList)
	}
	for _, m := range mods {
		e, ok := by[m]
		switch {
		case !ok:
			add(OutInitramfs, "缺少模块 %s", m)
		case !bytes.HasPrefix(e.Data, []byte("\x7fELF")):
			add(OutInitramfs, "模块 %s 不是 ELF 文件", m)
		}
	}
	have := map[string]bool{}
	for _, m := range mods {
		have[kmod.ModuleName(m)] = true
	}
	for _, need := range BootModules {
		if !have[need] {
			add(OutInitramfs, "启动模块里没有 %s", need)
		}
	}
	if len(info.BootModules) > 0 && strings.Join(info.BootModules, "\n") != strings.Join(mods, "\n") {
		add(OutInitramfs, "模块清单与 build.json 不一致")
	}
	return agent
}

// CheckRunnableOn 检查一个 aarch64 程序在镜像 t 里能否运行：静态链接直接通过；
// 动态链接时，解释器（PT_INTERP）和 DT_NEEDED 里的库都要在 /lib 或 /usr/lib 下。
func CheckRunnableOn(t *Tree, b []byte) error {
	f, err := elf.NewFile(bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("不是 ELF 程序：%v", err)
	}
	defer f.Close()
	if f.Machine != elf.EM_AARCH64 {
		return fmt.Errorf("不是 aarch64 程序（%v）", f.Machine)
	}
	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		raw, err := io.ReadAll(p.Open())
		if err != nil {
			return err
		}
		interp := strings.TrimRight(string(raw), "\x00")
		if !treeHasFile(t, interp) {
			return fmt.Errorf("缺少动态链接器 %s（多半是 glibc 版程序，Alpine 用 musl）", interp)
		}
	}
	libs, err := f.ImportedLibraries()
	if err != nil {
		return nil // 没有动态段：静态链接
	}
	for _, l := range libs {
		if !treeHasFile(t, "/lib/"+l) && !treeHasFile(t, "/usr/lib/"+l) {
			return fmt.Errorf("缺少动态库 %s", l)
		}
	}
	return nil
}

// treeHasFile 判断路径存在（跟随一层符号链接）。
func treeHasFile(t *Tree, p string) bool {
	e, ok := t.Get(p)
	if !ok {
		return false
	}
	if e.IsSymlink() {
		target := e.Link
		if !strings.HasPrefix(target, "/") {
			target = path.Join(path.Dir(p), target)
		}
		e, ok = t.Get(target)
	}
	return ok && e.IsRegular()
}

// CheckAgentELF 检查 abroad-agent 是 64 位小端的 aarch64 Linux 程序，并且是静态链接的
// （没有 PT_INTERP：initramfs 里没有动态加载器，用 cgo 构建的程序在那里起不来）。
func CheckAgentELF(b []byte) error {
	f, err := elf.NewFile(bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("不是 ELF 程序：%v", err)
	}
	defer f.Close()
	if f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB || f.Machine != elf.EM_AARCH64 {
		return fmt.Errorf("不是 linux/arm64 程序（%v %v %v），请用 GOOS=linux GOARCH=arm64 构建", f.Class, f.Data, f.Machine)
	}
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return fmt.Errorf("不是可执行文件（%v）", f.Type)
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("是动态链接的程序，请用 CGO_ENABLED=0 构建")
		}
	}
	return nil
}

func verifyRoot(t *Tree, add func(string, string, ...any)) {
	if e, ok := t.Get("/"); !ok || e.UID != 0 || e.GID != 0 {
		add(OutRootFS, "根目录属主不是 0:0")
	}
	for _, p := range []string{AgentPath, SingBoxPath, MihomoPath, "/bin/busybox"} {
		e, ok := t.Get(p)
		if !ok || !e.IsRegular() || e.Mode.Perm()&0o111 == 0 || e.UID != 0 || e.GID != 0 {
			add(OutRootFS, "缺少 %s，或不可执行，或属主不是 0:0", p)
		}
	}
	// 引擎必须能在 Alpine（musl）上运行：解释器和依赖的动态库都要在镜像里。
	// （sing-box 的 linux-arm64 包是 glibc 动态链接的，要用 -musl 包。）
	for _, p := range []string{SingBoxPath, MihomoPath} {
		if e, ok := t.Get(p); ok && e.IsRegular() {
			if err := CheckRunnableOn(t, e.Data); err != nil {
				add(OutRootFS, "%s 在这个镜像里跑不起来：%v", p, err)
			}
		}
	}
	// abroad-agent 用 /sbin/ip（busybox）配网卡。
	if e, ok := t.Get("/sbin/ip"); !ok || !(e.IsSymlink() || e.IsRegular() && e.Mode.Perm()&0o111 != 0) {
		add(OutRootFS, "缺少 /sbin/ip（abroad-agent 用它配置网卡）")
	}
	if e, ok := t.Get("/bin/sh"); !ok || !e.IsSymlink() {
		add(OutRootFS, "/bin/sh 不是符号链接")
	}
	if e, ok := t.Get("/etc/resolv.conf"); !ok || !e.IsRegular() || len(e.Data) != 0 {
		add(OutRootFS, "/etc/resolv.conf 应为空文件（虚拟机不使用网关 DNS）")
	}
	for _, d := range []string{guestabi.ModloopDir, "/run", "/proc", "/sys", "/dev", "/tmp"} {
		if e, ok := t.Get(d); !ok || !e.IsDir() {
			add(OutRootFS, "缺少目录 %s", d)
		}
	}
}
