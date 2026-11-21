package imagebuild

import (
	"fmt"
	"strconv"
	"strings"
)

// 安装脚本的处理（R19）：脚本本身不执行；pin-packages 记下每个包有哪些脚本，这里逐个列出“等价动作”或“不需要”。
// 清单里出现这里没列出的脚本时构建失败，迫使人工看过新脚本再决定（避免静默漏掉必要的动作）。
//
// 2026-11-19 固定的包里有三个脚本（内容见 design 15 的 R19）：
//   - openvpn .pre-install：建系统组和用户 openvpn → 等价地写 /etc/group、/etc/passwd；
//   - iproute2 .post-install：/bin/ip 是指向 busybox 的链接时删掉 → 等价地删除；
//   - nftables .post-upgrade：只在升级时运行，全新安装不需要。
var scriptActions = map[string]func(*Tree) error{
	"openvpn/.pre-install":   func(t *Tree) error { return addSystemUser(t, "openvpn", "/dev/null", "/sbin/nologin") },
	"iproute2/.post-install": removeBusyboxLink("/bin/ip"),
	"nftables/.post-upgrade": nil, // 不需要
}

// 只在升级、卸载时运行的脚本，全新安装不需要。
var upgradeOnlyScripts = map[string]bool{".post-upgrade": true, ".pre-upgrade": true, ".pre-deinstall": true, ".post-deinstall": true}

func applyScriptEquivalents(t *Tree, list []PinnedPackage) error {
	for _, p := range list {
		for _, s := range p.Scripts {
			key := p.Name + "/" + s
			act, known := scriptActions[key]
			if !known {
				if upgradeOnlyScripts[s] {
					continue
				}
				return fmt.Errorf("软件包 %s 的安装脚本 %s 还没有核对过：请看过它的内容，在 internal/imagebuild/scripts.go 里写明等价动作或“不需要”", p.Name, s)
			}
			if act != nil {
				if err := act(t); err != nil {
					return fmt.Errorf("%s：%w", key, err)
				}
			}
		}
	}
	return nil
}

// addSystemUser 等价于 `addgroup -S <名>; adduser -S -D -H -h <home> -s <shell> -G <名> <名>`：
// 在 100–999 里取用户和组都没占用的最小 ID（确定性），已存在就不动。
func addSystemUser(t *Tree, name, home, shell string) error {
	pw, ok1 := t.Get("/etc/passwd")
	gr, ok2 := t.Get("/etc/group")
	if !ok1 || !ok2 || !pw.IsRegular() || !gr.IsRegular() {
		return fmt.Errorf("根文件系统里没有 /etc/passwd 或 /etc/group")
	}
	used := map[int]bool{}
	exists := false
	for _, f := range []string{string(pw.Data), string(gr.Data)} {
		for _, l := range strings.Split(f, "\n") {
			fs := strings.Split(l, ":")
			if len(fs) < 3 {
				continue
			}
			if fs[0] == name {
				exists = true
			}
			if n, err := strconv.Atoi(fs[2]); err == nil {
				used[n] = true
			}
		}
	}
	if exists {
		return nil
	}
	id := -1
	for i := 100; i < 1000; i++ {
		if !used[i] {
			id = i
			break
		}
	}
	if id < 0 {
		return fmt.Errorf("100–999 里没有空闲的 ID")
	}
	appendLine := func(e *Entry, line string) {
		d := string(e.Data)
		if d != "" && !strings.HasSuffix(d, "\n") {
			d += "\n"
		}
		e.Data = []byte(d + line + "\n")
	}
	appendLine(gr, fmt.Sprintf("%s:x:%d:%s", name, id, name))
	appendLine(pw, fmt.Sprintf("%s:x:%d:%d:%s:%s:%s", name, id, id, name, home, shell))
	if sh, ok := t.Get("/etc/shadow"); ok && sh.IsRegular() {
		appendLine(sh, name+":!::0:::::")
	}
	return nil
}

func removeBusyboxLink(p string) func(*Tree) error {
	return func(t *Tree) error {
		if e, ok := t.Get(p); ok && e.IsSymlink() && e.Link == "/bin/busybox" {
			t.Remove(p)
		}
		return nil
	}
}
