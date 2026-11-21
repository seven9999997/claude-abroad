// Package imagebuild 组装虚拟机镜像（设计文档 7.1）：未压缩内核、initramfs、
// 只读根镜像。全部用纯 Go 在 macOS 上完成，不依赖 Docker/Lima。
//
// 根目录先用内存里的条目表（Tree）描述，再交给写入器生成镜像，这样属主、
// 权限位、符号链接完全由我们决定，不受宿主文件系统影响。
package imagebuild

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// Entry 是根目录里的一项。
type Entry struct {
	Path string      // 绝对路径，以 / 开头，不以 / 结尾（根目录除外）
	Mode fs.FileMode // 类型位（目录、符号链接、普通文件）+ 权限位
	UID  int
	GID  int
	Link string // 符号链接目标
	Data []byte // 普通文件内容
	// 字符设备的主、次设备号（只有 initramfs 的 /dev/console 用到）。
	DevMajor, DevMinor uint32
}

// IsDir、IsSymlink、IsRegular 判断条目类型。
func (e *Entry) IsDir() bool     { return e.Mode.IsDir() }
func (e *Entry) IsSymlink() bool { return e.Mode&fs.ModeSymlink != 0 }
func (e *Entry) IsRegular() bool { return e.Mode.IsRegular() }
func (e *Entry) IsCharDev() bool {
	return e.Mode&fs.ModeDevice != 0 && e.Mode&fs.ModeCharDevice != 0
}

// Tree 是一棵内存里的目录树。
type Tree struct {
	entries map[string]*Entry
}

// NewTree 返回只有根目录（0755 root:root）的树。
func NewTree() *Tree {
	return &Tree{entries: map[string]*Entry{"/": {Path: "/", Mode: fs.ModeDir | 0o755}}}
}

func cleanPath(p string) (string, error) {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	c := path.Clean(p)
	if strings.Contains(c, "\x00") {
		return "", fmt.Errorf("路径含 NUL：%q", p)
	}
	return c, nil
}

// Add 加入或替换一项，缺失的上级目录按 0755 root:root 自动补上。
// 不允许用非目录替换已有目录，也不允许把文件放在非目录下面。
func (t *Tree) Add(e Entry) error {
	p, err := cleanPath(e.Path)
	if err != nil {
		return err
	}
	e.Path = p
	if e.Mode&0o7000 != 0 {
		return fmt.Errorf("%s：setuid/setgid/sticky 要用 fs.ModeSetuid 等标志，不能写在权限位里", p)
	}
	if p == "/" {
		if !e.IsDir() {
			return fmt.Errorf("根目录必须是目录")
		}
		t.entries["/"] = &e
		return nil
	}
	switch {
	case e.IsDir(), e.IsRegular(), e.IsCharDev():
	case e.IsSymlink():
		if e.Link == "" || len(e.Link) > 4095 {
			return fmt.Errorf("%s：符号链接目标为空或超过 4095 字节", p)
		}
	default:
		return fmt.Errorf("%s：不支持的文件类型 %v", p, e.Mode.Type())
	}
	if old, ok := t.entries[p]; ok && old.IsDir() && !e.IsDir() {
		return fmt.Errorf("%s：已经是目录，不能替换成文件", p)
	}
	if err := t.mkdirAll(path.Dir(p)); err != nil {
		return err
	}
	t.entries[p] = &e
	return nil
}

func (t *Tree) mkdirAll(dir string) error {
	if e, ok := t.entries[dir]; ok {
		if !e.IsDir() {
			return fmt.Errorf("%s 不是目录", dir)
		}
		return nil
	}
	if err := t.mkdirAll(path.Dir(dir)); err != nil {
		return err
	}
	t.entries[dir] = &Entry{Path: dir, Mode: fs.ModeDir | 0o755}
	return nil
}

// AddFile、AddDir、AddSymlink 是常用的简写，属主都是 root:root。
func (t *Tree) AddFile(p string, perm fs.FileMode, data []byte) error {
	return t.Add(Entry{Path: p, Mode: perm.Perm(), Data: data})
}
func (t *Tree) AddDir(p string, perm fs.FileMode) error {
	return t.Add(Entry{Path: p, Mode: fs.ModeDir | perm.Perm()})
}
func (t *Tree) AddSymlink(p, target string) error {
	return t.Add(Entry{Path: p, Mode: fs.ModeSymlink | 0o777, Link: target})
}
func (t *Tree) AddCharDev(p string, perm fs.FileMode, major, minor uint32) error {
	return t.Add(Entry{Path: p, Mode: fs.ModeDevice | fs.ModeCharDevice | perm.Perm(), DevMajor: major, DevMinor: minor})
}

// Get 返回某一项。
func (t *Tree) Get(p string) (*Entry, bool) {
	c, err := cleanPath(p)
	if err != nil {
		return nil, false
	}
	e, ok := t.entries[c]
	return e, ok
}

// Remove 删除一项（目录连同其下所有项）。
func (t *Tree) Remove(p string) {
	c, err := cleanPath(p)
	if err != nil || c == "/" {
		return
	}
	for k := range t.entries {
		if k == c || strings.HasPrefix(k, c+"/") {
			delete(t.entries, k)
		}
	}
}

// Entries 按路径排序返回所有项（父目录总在子项之前）。
func (t *Tree) Entries() []*Entry {
	out := make([]*Entry, 0, len(t.entries))
	for _, e := range t.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return pathLess(out[i].Path, out[j].Path) })
	return out
}

// pathLess 按路径分段比较，保证 "/a" < "/a/b" < "/a-b"。
func pathLess(a, b string) bool {
	as, bs := strings.Split(a, "/"), strings.Split(b, "/")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if as[i] != bs[i] {
			return as[i] < bs[i]
		}
	}
	return len(as) < len(bs)
}

// Len 返回条目数（含根目录）。
func (t *Tree) Len() int { return len(t.entries) }
