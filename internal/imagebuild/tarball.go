package imagebuild

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
)

const maxTarFile = 512 << 20

// tarName 规范化 tar 里的路径：去掉开头的 "./" 和 "/"，拒绝 ".."。
func tarName(n string) (string, error) {
	for _, seg := range strings.Split(n, "/") {
		if seg == ".." {
			return "", fmt.Errorf("tar 里有越界路径 %q", n)
		}
	}
	return path.Clean("/" + strings.TrimPrefix(n, "./")), nil
}

func gzipTar(r io.Reader) (*tar.Reader, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("不是 gzip 文件：%w", err)
	}
	return tar.NewReader(zr), nil
}

// ExtractFiles 从 tar.gz 里取出指定的文件（路径不带开头的 ./），一次扫描完成。
// 任一文件缺失都报错并列出缺失项。
func ExtractFiles(r io.Reader, want []string) (map[string][]byte, error) {
	tr, err := gzipTar(r)
	if err != nil {
		return nil, err
	}
	need := map[string]bool{}
	for _, w := range want {
		need["/"+strings.TrimPrefix(w, "/")] = true
	}
	out := map[string][]byte{}
	var seen []string // 包里实际有的、看起来相关的路径，缺文件时列出来
	for len(out) < len(need) {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		n, err := tarName(h.Name)
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && strings.HasSuffix(n, "-virt") && len(seen) < 20 {
			seen = append(seen, strings.TrimPrefix(n, "/"))
		}
		if !need[n] || h.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxTarFile+1))
		if err != nil {
			return nil, err
		}
		if len(b) > maxTarFile {
			return nil, fmt.Errorf("%s 超过 %d MiB", n, maxTarFile>>20)
		}
		out[strings.TrimPrefix(n, "/")] = b
	}
	var missing []string
	for _, w := range want {
		if _, ok := out[strings.TrimPrefix(w, "/")]; !ok {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		hint := ""
		if len(seen) > 0 {
			hint = "；包里以 -virt 结尾的文件有：" + strings.Join(seen, "、")
		}
		return nil, fmt.Errorf("压缩包里缺少：%s%s", strings.Join(missing, "、"), hint)
	}
	return out, nil
}

// AddTarGz 把 tar.gz 里的所有项加入树，保留权限位和属主。硬链接复制为独立文件。
// 设备节点、FIFO 等跳过（/dev 在虚拟机里由 devtmpfs 提供）。
func AddTarGz(t *Tree, r io.Reader) error {
	tr, err := gzipTar(r)
	if err != nil {
		return err
	}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		n, err := tarName(h.Name)
		if err != nil {
			return err
		}
		mode := fs.FileMode(h.Mode).Perm()
		if h.Mode&0o4000 != 0 {
			mode |= fs.ModeSetuid
		}
		if h.Mode&0o2000 != 0 {
			mode |= fs.ModeSetgid
		}
		if h.Mode&0o1000 != 0 {
			mode |= fs.ModeSticky
		}
		e := Entry{Path: n, UID: h.Uid, GID: h.Gid}
		switch h.Typeflag {
		case tar.TypeDir:
			e.Mode = fs.ModeDir | mode
		case tar.TypeReg:
			b, err := io.ReadAll(io.LimitReader(tr, maxTarFile+1))
			if err != nil {
				return err
			}
			if len(b) > maxTarFile {
				return fmt.Errorf("%s 过大", n)
			}
			e.Mode, e.Data = mode, b
		case tar.TypeSymlink:
			e.Mode, e.Link = fs.ModeSymlink|0o777, h.Linkname
		case tar.TypeLink:
			target, err := tarName(h.Linkname)
			if err != nil {
				return err
			}
			src, ok := t.Get(target)
			if !ok || !src.IsRegular() {
				return fmt.Errorf("%s 是指向 %s 的硬链接，但目标不存在", n, target)
			}
			e.Mode, e.Data = mode, src.Data
		default:
			continue
		}
		if err := t.Add(e); err != nil {
			return err
		}
	}
}
