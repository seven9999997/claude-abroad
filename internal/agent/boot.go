package agent

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"claude-abroad/internal/guestabi"
	"claude-abroad/internal/kmod"
)

// DiskTimeout 是 boot 等待根盘和 modloop 出现的上限。
const DiskTimeout = 10 * time.Second

// Boot 是 initramfs 里的 /init（设计文档 7.1）：
// 挂 devtmpfs、proc、sysfs → 加载启动模块 → 等盘并按内容认出根盘和 modloop →
// 从 modloop 加载 overlay → tmpfs + overlay 组装 /newroot → 把 /dev、/proc、/sys、modloop
// 移进去 → switch_root 并执行 /sbin/abroad-agent init。
//
// 只在出错时返回；调用方（PID 1）随后关机。
func Boot(e *Env) error {
	for _, m := range []struct{ src, dst, fs string }{
		{"devtmpfs", "/dev", "devtmpfs"},
		{"proc", "/proc", "proc"},
		{"sysfs", "/sys", "sysfs"},
	} {
		if err := e.mkdir(m.dst); err != nil {
			return err
		}
		if err := e.Sys.Mount(m.src, m.dst, m.fs, MountNoSuid|MountNoExec, ""); err != nil {
			return fmt.Errorf("挂载 %s：%w", m.dst, err)
		}
	}
	kver, err := e.Sys.KernelRelease()
	if err != nil {
		return fmt.Errorf("取内核版本：%w", err)
	}
	e.logf("boot：内核 %s", kver)

	if err := loadBootModules(e); err != nil {
		return err
	}
	disks, err := findDisks(e, kver)
	if err != nil {
		return err
	}
	e.logf("boot：根盘 %s，modloop %s", disks.root, disks.modloop)

	modDir := path.Join("/mnt", disks.modloop, "modules", kver)
	rs, err := kmod.Loader{Dir: os.DirFS(e.path(modDir)), Load: e.Sys.LoadModule}.LoadNames([]string{"overlay"})
	if err != nil {
		return fmt.Errorf("从 modloop 加载 overlay：%w", err)
	}
	for _, r := range rs {
		if !r.OK {
			return fmt.Errorf("从 modloop 加载 %s 失败：%s", r.Name, r.Error)
		}
	}

	newRoot := guestabi.NewRoot
	for _, d := range []string{"/mnt/rw", newRoot} {
		if err := e.mkdir(d); err != nil {
			return err
		}
	}
	if err := e.Sys.Mount("tmpfs", "/mnt/rw", "tmpfs", MountNoSuid|MountNoDev, "size="+guestabi.OverlaySize+",mode=0755"); err != nil {
		return fmt.Errorf("挂载 overlay 的可写层：%w", err)
	}
	for _, d := range []string{"/mnt/rw/upper", "/mnt/rw/work"} {
		if err := e.mkdir(d); err != nil {
			return err
		}
	}
	lower := path.Join("/mnt", disks.root)
	opts := "lowerdir=" + lower + ",upperdir=/mnt/rw/upper,workdir=/mnt/rw/work"
	if err := e.Sys.Mount("overlay", newRoot, "overlay", 0, opts); err != nil {
		return fmt.Errorf("挂载 overlay：%w", err)
	}

	for _, m := range []struct{ src, dst string }{
		{"/dev", "/dev"},
		{"/proc", "/proc"},
		{"/sys", "/sys"},
		{path.Join("/mnt", disks.modloop), guestabi.ModloopDir},
	} {
		dst := path.Join(newRoot, m.dst)
		if err := e.mkdir(dst); err != nil {
			return err
		}
		if err := e.Sys.MoveMount(m.src, dst); err != nil {
			return fmt.Errorf("把 %s 移到新根目录：%w", m.src, err)
		}
	}
	e.logf("boot：切换到根盘")
	if err := e.Sys.SwitchRoot(newRoot, []string{guestabi.AgentPath, "init"}); err != nil {
		return fmt.Errorf("switch_root：%w", err)
	}
	return errors.New("switch_root 意外返回")
}

func loadBootModules(e *Env) error {
	b, err := os.ReadFile(e.path(guestabi.BootModulesList))
	if err != nil {
		return fmt.Errorf("读取启动模块清单：%w", err)
	}
	for _, p := range strings.Fields(string(b)) {
		img, err := os.ReadFile(e.path(p))
		if err != nil {
			return fmt.Errorf("读取启动模块 %s：%w", p, err)
		}
		if err := e.Sys.LoadModule(img); err != nil && !errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("加载启动模块 %s：%w", kmod.ModuleName(p), err)
		}
	}
	return nil
}

type disks struct{ root, modloop string }

// findDisks 在 DiskTimeout 内反复扫 /sys/block/vd*，把新出现的盘只读挂成 squashfs，
// 按内容认：有 sbin/abroad-agent 的是根盘，有 modules/<内核版本>/modules.dep 的是 modloop。
// 不靠 vda/vdb 的顺序。两块都认出来才返回。
func findDisks(e *Env, kver string) (disks, error) {
	var d disks
	tried := map[string]string{} // 盘 → 上次挂载失败的原因（空表示已挂上）
	deadline := e.now().Add(DiskTimeout)
	for {
		names, _ := filepath.Glob(e.path("/sys/block/vd*"))
		sort.Strings(names)
		for _, full := range names {
			name := filepath.Base(full)
			if reason, ok := tried[name]; ok && reason == "" {
				continue
			}
			mnt := path.Join("/mnt", name)
			if err := e.mkdir(mnt); err != nil {
				return d, err
			}
			if err := e.Sys.Mount("/dev/"+name, mnt, "squashfs", MountReadOnly, ""); err != nil {
				tried[name] = err.Error() // 设备节点可能还没出现，下一轮再试
				continue
			}
			tried[name] = ""
			switch {
			case exists(e.path(path.Join(mnt, guestabi.AgentPath))):
				d.root = name
			case exists(e.path(path.Join(mnt, "modules", kver, "modules.dep"))):
				d.modloop = name
			default:
				e.logf("boot：%s 既不是根盘也不是 modloop，忽略", name)
			}
		}
		if d.root != "" && d.modloop != "" {
			return d, nil
		}
		if !e.now().Before(deadline) {
			return d, diskError(d, tried)
		}
		e.sleep(200 * time.Millisecond)
	}
}

func diskError(d disks, tried map[string]string) error {
	var missing []string
	if d.root == "" {
		missing = append(missing, "根盘（含 "+guestabi.AgentPath+"）")
	}
	if d.modloop == "" {
		missing = append(missing, "modloop（含当前内核版本的模块）")
	}
	var seen []string
	for n, r := range tried {
		if r == "" {
			r = "已挂载"
		}
		seen = append(seen, n+"："+r)
	}
	sort.Strings(seen)
	if len(seen) == 0 {
		seen = []string{"没有发现任何 virtio 磁盘"}
	}
	return fmt.Errorf("等了 %s 仍找不到%s；看到的盘：%s", DiskTimeout, strings.Join(missing, "和"), strings.Join(seen, "；"))
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
