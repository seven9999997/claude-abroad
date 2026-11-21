package kmod

import (
	"bufio"
	"io"
	"strings"
)

// 软依赖（m4-plan v2.1、v2.8 决定 7）：modules.softdep 里 “softdep <模块> pre: <名字>… post: <名字>…”。
// 加载一个模块之前，先尝试加载它 pre: 里的名字；软依赖失败只记在结果里，不算错（目标模块失败才算）。
// pre: 里写的常是别名（例如 libcrc32c 的 “pre: crc32c”，crc32c 是加密算法的别名，对应 crc32c_generic 等模块），
// 所以还要读 modules.alias 里不带通配符的别名。

// ParseModulesSoftdep 解析 modules.softdep，返回 模块名 → pre: 里的名字（按出现顺序）。
func ParseModulesSoftdep(r io.Reader) map[string][]string {
	out := map[string][]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 || f[0] != "softdep" {
			continue
		}
		mod := strings.ReplaceAll(f[1], "-", "_")
		pre := false
		for _, w := range f[2:] {
			switch w {
			case "pre:":
				pre = true
			case "post:":
				pre = false
			default:
				if pre {
					out[mod] = append(out[mod], w)
				}
			}
		}
	}
	return out
}

// ParseModulesAlias 解析 modules.alias 里不带通配符的别名：别名 → 模块名（同一个别名有多个模块时都留着，按出现顺序）。
func ParseModulesAlias(r io.Reader) map[string][]string {
	out := map[string][]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 3 || f[0] != "alias" || strings.ContainsAny(f[1], "*?[") {
			continue
		}
		out[f[1]] = append(out[f[1]], strings.ReplaceAll(f[2], "-", "_"))
	}
	return out
}
