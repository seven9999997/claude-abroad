package agent

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// prepareInitramfs 在假根目录里放启动模块清单、模块和 /sys/block 下的盘。
func prepareInitramfs(t *testing.T, f *fakeSys, disks ...string) {
	modDir := "/lib/modules/" + testKver + "/kernel/"
	writeFile(t, filepath.Join(f.root, "/etc/abroad/boot-modules"),
		[]byte(modDir+"drivers/block/virtio_blk.ko\n"+modDir+"fs/squashfs/squashfs.ko\n"))
	writeFile(t, filepath.Join(f.root, modDir+"drivers/block/virtio_blk.ko"), []byte("\x7fELFvirtio_blk"))
	writeFile(t, filepath.Join(f.root, modDir+"fs/squashfs/squashfs.ko"), []byte("\x7fELFsquashfs"))
	for _, d := range disks {
		writeFile(t, filepath.Join(f.root, "/sys/block", d, "size"), []byte("1"))
	}
}

func bootSequence() []string {
	return []string{
		"mount devtmpfs /dev devtmpfs 0xa ",
		"mount proc /proc proc 0xa ",
		"mount sysfs /sys sysfs 0xa ",
		"load virtio_blk",
		"load squashfs",
		// 盘按名字顺序挂，认盘看内容
	}
}

func TestBootFullSequence(t *testing.T) {
	for name, c := range map[string]struct{ root, modloop string }{
		"正常顺序": {"vda", "vdb"},
		"盘序颠倒": {"vdb", "vda"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeSys(t)
			f.disks = map[string]string{c.root: "root", c.modloop: "modloop"}
			prepareInitramfs(t, f, "vda", "vdb")
			err := Boot(f.env())
			if err == nil || !strings.Contains(err.Error(), "switch_root") {
				t.Fatalf("fake 的 switch_root 返回后 Boot 应报错：%v", err)
			}
			want := append(bootSequence(),
				"mount /dev/vda /mnt/vda squashfs 0x1 ",
				"mount /dev/vdb /mnt/vdb squashfs 0x1 ",
				"load overlay",
				"mount tmpfs /mnt/rw tmpfs 0x6 size=32m,mode=0755",
				"mount overlay /newroot overlay 0x0 lowerdir=/mnt/"+c.root+",upperdir=/mnt/rw/upper,workdir=/mnt/rw/work",
				"move /dev /newroot/dev",
				"move /proc /newroot/proc",
				"move /sys /newroot/sys",
				"move /mnt/"+c.modloop+" /newroot/modloop",
				"switch_root /newroot /sbin/abroad-agent init",
			)
			if got := f.Calls(); !reflect.DeepEqual(got, want) {
				t.Fatalf("调用顺序不对：\n实际 %q\n应为 %q", got, want)
			}
		})
	}
}

func TestBootDiskRetry(t *testing.T) {
	f := newFakeSys(t)
	f.disks = map[string]string{"vda": "root", "vdb": "modloop"}
	f.mountFail["vdb"] = 3 // 设备节点晚出现
	prepareInitramfs(t, f, "vda")
	env := f.env()
	sleeps := 0
	env.Sleep = func(time.Duration) {
		sleeps++
		if sleeps == 2 { // 第二块盘晚出现
			writeFile(t, filepath.Join(f.root, "/sys/block/vdb/size"), []byte("1"))
		}
	}
	if err := Boot(env); !strings.Contains(err.Error(), "switch_root") {
		t.Fatalf("盘晚出现后应能继续：%v", err)
	}
	if n := strings.Count(strings.Join(f.Calls(), "\n"), "mount /dev/vda "); n != 1 {
		t.Errorf("已认出的盘不应重复挂载，实际 %d 次", n)
	}
}

func TestBootDiskMissing(t *testing.T) {
	for name, c := range map[string]struct {
		disks map[string]string
		want  string
	}{
		"缺根盘":       {map[string]string{"vda": "modloop", "vdb": "other"}, "根盘"},
		"缺 modloop": {map[string]string{"vda": "root"}, "modloop"},
		"没有盘":       {map[string]string{}, "没有发现任何 virtio 磁盘"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeSys(t)
			f.disks = c.disks
			var names []string
			for d := range c.disks {
				names = append(names, d)
			}
			prepareInitramfs(t, f, names...)
			env := f.env()
			now := time.Unix(0, 0)
			env.Now = func() time.Time { return now }
			env.Sleep = func(d time.Duration) { now = now.Add(d) }
			err := Boot(env)
			if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), "switch_root") {
				t.Fatalf("应超时并指出缺%s：%v", c.want, err)
			}
			if now.Sub(time.Unix(0, 0)) < DiskTimeout {
				t.Errorf("应等满 %s，实际 %s", DiskTimeout, now.Sub(time.Unix(0, 0)))
			}
			for _, call := range f.Calls() {
				if strings.HasPrefix(call, "mount overlay") || strings.HasPrefix(call, "switch_root") {
					t.Errorf("缺盘时不应继续：%s", call)
				}
			}
		})
	}
}

func TestBootModuleFailures(t *testing.T) {
	f := newFakeSys(t)
	f.disks = map[string]string{"vda": "root", "vdb": "modloop"}
	f.loadFail["squashfs"] = errBoom
	prepareInitramfs(t, f, "vda", "vdb")
	if err := Boot(f.env()); err == nil || !strings.Contains(err.Error(), "squashfs") {
		t.Fatalf("启动模块失败应致命：%v", err)
	}

	f = newFakeSys(t)
	f.disks = map[string]string{"vda": "root", "vdb": "modloop"}
	f.loadFail["overlay"] = errBoom
	prepareInitramfs(t, f, "vda", "vdb")
	if err := Boot(f.env()); err == nil || !strings.Contains(err.Error(), "overlay") {
		t.Fatalf("overlay 失败应致命：%v", err)
	}

	f = newFakeSys(t)
	if err := Boot(f.env()); err == nil || !strings.Contains(err.Error(), "启动模块清单") {
		t.Fatalf("缺启动模块清单应报错：%v", err)
	}
}
