// Package kmod 解析 modules.dep、按依赖排序、解压并加载 Linux 内核模块。
// 构建镜像（imagebuild）和虚拟机里的 abroad-agent 共用。
package kmod

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"path"
	"strings"
)

// ModuleDeps 是 modules.dep 的内容：模块相对路径 → 它依赖的模块相对路径。
type ModuleDeps map[string][]string

// ParseModulesDep 解析 modules.dep（每行 "kernel/a.ko.gz: kernel/b.ko.gz kernel/c.ko.gz"）。
func ParseModulesDep(r io.Reader) (ModuleDeps, error) {
	deps := ModuleDeps{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		mod, rest, ok := strings.Cut(line, ":")
		if !ok || mod == "" {
			return nil, fmt.Errorf("modules.dep 第 %d 行格式不对", n)
		}
		deps[mod] = strings.Fields(rest)
	}
	return deps, sc.Err()
}

// ModuleName 从路径取模块名：kernel/drivers/block/virtio_blk.ko.gz → virtio_blk。
// 内核把模块名里的 '-' 视为 '_'，这里统一成 '_'。
func ModuleName(p string) string {
	b := path.Base(p)
	if i := strings.Index(b, ".ko"); i >= 0 {
		b = b[:i]
	}
	return strings.ReplaceAll(b, "-", "_")
}

// Resolve 按加载顺序（依赖在前）返回所需模块的相对路径。
// 找不到的模块明确报错；名字在 builtin 里的（内置进内核的）跳过。
func (d ModuleDeps) Resolve(names []string, builtin map[string]bool) ([]string, error) {
	byName := map[string]string{}
	for p := range d {
		byName[ModuleName(p)] = p
	}
	var order []string
	state := map[string]int{} // 1 访问中，2 已完成
	var visit func(p string) error
	visit = func(p string) error {
		switch state[p] {
		case 1:
			return fmt.Errorf("模块依赖有环：%s", p)
		case 2:
			return nil
		}
		state[p] = 1
		for _, dep := range d[p] {
			if _, ok := d[dep]; !ok {
				return fmt.Errorf("modules.dep 里 %s 依赖的 %s 不存在", p, dep)
			}
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[p] = 2
		order = append(order, p)
		return nil
	}
	for _, n := range names {
		n = strings.ReplaceAll(n, "-", "_")
		if builtin[n] {
			continue
		}
		p, ok := byName[n]
		if !ok {
			return nil, fmt.Errorf("找不到内核模块 %s（modules.dep 里没有）", n)
		}
		if err := visit(p); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// DecompressModule 把 .ko.gz 解成 .ko；.ko 原样返回；其他压缩后缀明确报错。
func DecompressModule(p string, data []byte) ([]byte, error) {
	switch {
	case strings.HasSuffix(p, ".ko"):
		return data, nil
	case strings.HasSuffix(p, ".ko.gz"):
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("解压模块 %s：%w", p, err)
		}
		out, err := io.ReadAll(io.LimitReader(zr, 64<<20))
		if err != nil {
			return nil, fmt.Errorf("解压模块 %s：%w", p, err)
		}
		return out, nil
	}
	return nil, fmt.Errorf("不支持的模块压缩格式：%s（目前只支持 .ko 和 .ko.gz）", p)
}

// UncompressedName 把 x.ko.gz 改成 x.ko。
func UncompressedName(p string) string {
	if i := strings.Index(p, ".ko"); i >= 0 {
		return p[:i] + ".ko"
	}
	return p
}
