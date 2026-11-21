package kmod

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"syscall"
)

// Result 是一个模块的加载结果。
type Result struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Soft 表示这是软依赖（modules.softdep 的 pre:）：失败不算错，Failed 不列它。
	Soft bool `json:"soft,omitempty"`
}

// Loader 从一个模块目录（内含 modules.dep，即 /lib/modules/<内核版本>）加载模块。
// Load 把解压后的模块镜像交给内核（Linux 上是 init_module）；返回 EEXIST 视为已加载。
type Loader struct {
	Dir  fs.FS
	Load func(image []byte) error
}

// LoadNames 按依赖顺序加载 names 及其依赖。modules.builtin 里的（内置进内核的）跳过，
// 缺这个文件时当作空。依赖解析失败时返回错误、不加载任何模块；
// 之后每个模块单独加载，失败的记在结果里并继续，依赖它的模块会跟着失败。
func (l Loader) LoadNames(names []string) ([]Result, error) {
	b, err := fs.ReadFile(l.Dir, "modules.dep")
	if err != nil {
		return nil, fmt.Errorf("读取 modules.dep：%w", err)
	}
	deps, err := ParseModulesDep(strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	builtin := map[string]bool{}
	if b, err := fs.ReadFile(l.Dir, "modules.builtin"); err == nil {
		for _, p := range strings.Fields(string(b)) {
			builtin[ModuleName(p)] = true
		}
	}
	order, err := deps.Resolve(names, builtin)
	if err != nil {
		return nil, err
	}
	soft := map[string][]string{}
	if b, err := fs.ReadFile(l.Dir, "modules.softdep"); err == nil {
		soft = ParseModulesSoftdep(strings.NewReader(string(b)))
	}
	alias := map[string][]string{}
	if len(soft) > 0 {
		if b, err := fs.ReadFile(l.Dir, "modules.alias"); err == nil {
			alias = ParseModulesAlias(strings.NewReader(string(b)))
		}
	}
	byName := map[string]string{}
	for p := range deps {
		byName[ModuleName(p)] = p
	}
	failed := map[string]bool{}
	done := map[string]bool{}     // 已经尝试过（成功或失败）的模块路径
	softSeen := map[string]bool{} // 已经处理过的软依赖名字（防环）
	var out []Result
	var load func(p string, isSoft bool)
	// loadSoft 尝试加载 mod 的软依赖：名字先当模块名，再当别名；每个候选连同它的硬依赖一起加载。失败只记录。
	loadSoft := func(mod string) {
		for _, name := range soft[mod] {
			if softSeen[name] {
				continue
			}
			softSeen[name] = true
			cands := []string{strings.ReplaceAll(name, "-", "_")}
			if _, ok := byName[cands[0]]; !ok {
				cands = alias[name]
			}
			for _, c := range cands {
				if builtin[c] {
					continue
				}
				o, err := deps.Resolve([]string{c}, builtin)
				if err != nil {
					out = append(out, Result{Name: c, Soft: true, Error: err.Error()})
					continue
				}
				for _, p := range o {
					load(p, true)
				}
			}
		}
	}
	load = func(p string, isSoft bool) {
		if done[p] {
			return
		}
		done[p] = true
		loadSoft(ModuleName(p)) // 先软依赖
		r := Result{Name: ModuleName(p), Soft: isSoft}
		if err := l.loadOne(deps, p, failed); err != nil {
			failed[p] = true
			r.Error = err.Error()
		} else {
			r.OK = true
		}
		out = append(out, r)
	}
	for _, p := range order {
		load(p, false)
	}
	return out, nil
}

func (l Loader) loadOne(deps ModuleDeps, p string, failed map[string]bool) error {
	for _, d := range deps[p] {
		if failed[d] {
			return fmt.Errorf("依赖的 %s 没有加载成功", ModuleName(d))
		}
	}
	data, err := fs.ReadFile(l.Dir, p)
	if err != nil {
		return fmt.Errorf("读取模块文件：%w", err)
	}
	img, err := DecompressModule(p, data)
	if err != nil {
		return err
	}
	if err := l.Load(img); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("加载 %s：%w", ModuleName(p), err)
	}
	return nil
}

// Failed 返回结果里加载失败的模块名。
func Failed(rs []Result) []string {
	var out []string
	for _, r := range rs {
		if !r.OK && !r.Soft { // 软依赖失败不算错
			out = append(out, r.Name)
		}
	}
	return out
}
