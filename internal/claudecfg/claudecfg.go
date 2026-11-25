// Package claudecfg 是 Claude Code 的托管设置（设计 10.2 第四层，M6-3b，docs/fallback-plan.md v1.1 第 3.1 节）：
// 在 Claude Code 的托管设置分片目录里写一个我们自己的文件，env 里把代理变量指向 abroadd 的本机代理端口。
//
//   - 只写分片文件，主文件和别人的分片一概不碰；
//   - 六个变量大小写齐全：https_proxy、HTTPS_PROXY、http_proxy、HTTP_PROXY 指向同一个地址，no_proxy、NO_PROXY 只放回环；
//   - 写前校验是合法的 JSON，原子写入，普通用户可读（读不到时 Claude Code 静默不带这份策略）；
//   - 别的文件（主文件、别人的分片）定义了这六个变量中的任何一个就算冲突，不改别人的文件；
//   - 删除只删内容和记录一致的；自检只读，不自动重写。
//
// 本包不联网、不执行外部命令（源码检查）。目录由调用方给出：真实路径只在真实模式入口接上。
package claudecfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultPort 是本机代理端口（静态文件里写死，不随状态改；不用 7890、7891、8787）。
const DefaultPort = 47821

// FileName 是我们的分片文件名。
const FileName = "claude-abroad.json"

// MainFile 是主托管设置文件名（在分片目录的上一级；只读，用来查冲突）。
const MainFile = "managed-settings.json"

// RecordFile 是记录文件名（放在 abroadd 的状态目录里）。
const RecordFile = "claude-code-settings.json"

// Vars 是我们管的六个变量（按写入顺序）。
var Vars = []string{"https_proxy", "HTTPS_PROXY", "http_proxy", "HTTP_PROXY", "no_proxy", "NO_PROXY"}

// NoProxy 是绕过变量的值：只有回环地址（本机的开发服务器照常能连），不放任何公网域名。
const NoProxy = "localhost,127.0.0.1,::1"

// ProxyURL 是代理变量的值。
func ProxyURL(port uint16) string {
	return "http://" + netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port).String()
}

// Content 是分片文件的内容（逐字固定，变量按 Vars 的顺序），并校验是合法的 JSON。
func Content(port uint16) ([]byte, error) {
	if port == 0 {
		return nil, errors.New("端口不能是 0")
	}
	u := ProxyURL(port)
	var b bytes.Buffer
	b.WriteString("{\n  \"env\": {\n")
	for i, v := range Vars {
		val := u
		if strings.EqualFold(v, "no_proxy") {
			val = NoProxy
		}
		sep := ","
		if i == len(Vars)-1 {
			sep = ""
		}
		fmt.Fprintf(&b, "    %q: %q%s\n", v, val, sep)
	}
	b.WriteString("  }\n}\n")
	if !json.Valid(b.Bytes()) {
		return nil, errors.New("生成的托管设置不是合法的 JSON")
	}
	return b.Bytes(), nil
}

type record struct {
	Version    int    `json:"version"`
	Port       uint16 `json:"port"`
	Content    string `json:"content"`
	CreatedDir bool   `json:"created_dir,omitempty"` // 分片目录是我们建的（卸载时空了就一并删掉）
}

// Settings 是我们的托管设置分片。
type Settings struct {
	Dir       string // 分片目录（真实模式：Claude Code 托管设置目录下的 managed-settings.d；测试用临时目录）
	MainPath  string // 主托管设置文件（只读查冲突；空表示 Dir 的上一级下的 managed-settings.json）
	RecordDir string // 记录文件所在目录（abroadd 的状态目录）
	Port      uint16 // 0 用 DefaultPort
}

func (s *Settings) port() uint16 {
	if s.Port == 0 {
		return DefaultPort
	}
	return s.Port
}

func (s *Settings) mainPath() string {
	if s.MainPath != "" {
		return s.MainPath
	}
	return filepath.Join(filepath.Dir(s.Dir), MainFile)
}

func (s *Settings) path() string { return filepath.Join(s.Dir, FileName) }

func (s *Settings) readRecord() (*record, error) {
	b, err := os.ReadFile(filepath.Join(s.RecordDir, RecordFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r record
	if json.Unmarshal(b, &r) != nil || r.Version != 1 {
		return nil, errors.New("托管设置的记录文件损坏")
	}
	return &r, nil
}

func writeAtomic(dir, name string, b []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(dir, "."+name+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

// Conflict 是别的托管设置文件定义了我们管的变量（或解析不了）。
type Conflict struct {
	File string `json:"file"` // 文件名（不含目录）
	Var  string `json:"var"`  // 变量名；解析不了时为空
}

// ErrConflict 是有冲突（写入时不覆盖、不改别人的文件）。
var ErrConflict = errors.New("Claude Code 的托管设置有冲突：别的文件也定义了代理变量")

// Conflicts 列出别的托管设置文件（主文件、分片目录里别的 .json）里定义的六个变量；解析不了的文件也列出（Var 为空），
// 因为那会让 Claude Code 拒绝启动或不带策略。只读。
func (s *Settings) Conflicts() []Conflict {
	var out []Conflict
	check := func(path string) {
		b, err := os.ReadFile(path)
		if err != nil {
			return
		}
		var doc struct {
			Env map[string]any `json:"env"`
		}
		if json.Unmarshal(b, &doc) != nil {
			out = append(out, Conflict{File: filepath.Base(path)})
			return
		}
		var keys []string
		for k := range doc.Env {
			for _, v := range Vars {
				if k == v {
					keys = append(keys, k)
				}
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = append(out, Conflict{File: filepath.Base(path), Var: k})
		}
	}
	check(s.mainPath())
	ents, _ := os.ReadDir(s.Dir)
	for _, e := range ents {
		if e.Name() == FileName || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		check(filepath.Join(s.Dir, e.Name()))
	}
	return out
}

// Write 写入我们的分片（首次准备调用；可重入）。先写记录再写文件。已有内容相同、权限对的不重写（修改时间不变）。
// 有冲突时不写（返回 ErrConflict 和冲突清单）；同名文件内容既不是要写的、也不是记录里的，也不覆盖。
func (s *Settings) Write() ([]Conflict, error) {
	want, err := Content(s.port())
	if err != nil {
		return nil, err
	}
	if cs := s.Conflicts(); len(cs) > 0 {
		return cs, ErrConflict
	}
	rec, err := s.readRecord()
	if err != nil {
		return nil, err
	}
	cur, rerr := os.ReadFile(s.path())
	switch {
	case rerr == nil && !bytes.Equal(cur, want) && (rec == nil || !bytes.Equal(cur, []byte(rec.Content))):
		return []Conflict{{File: FileName}}, ErrConflict
	case rerr != nil && !errors.Is(rerr, fs.ErrNotExist):
		return nil, rerr
	}
	created := rec != nil && rec.CreatedDir
	if _, err := os.Stat(s.Dir); errors.Is(err, fs.ErrNotExist) {
		created = true
	}
	rb, _ := json.Marshal(record{Version: 1, Port: s.port(), Content: string(want), CreatedDir: created})
	if err := writeAtomic(s.RecordDir, RecordFile, append(rb, '\n'), 0o600); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return nil, err
	}
	if rerr == nil && bytes.Equal(cur, want) {
		if info, err := os.Lstat(s.path()); err == nil && info.Mode().Perm() == 0o644 {
			return nil, nil
		}
	}
	return nil, writeAtomic(s.Dir, FileName, want, 0o644)
}

// Remove 删除我们写的分片（卸载调用；可重入）：内容和记录一致才删，否则留下并返回 left=true。之后删记录。
func (s *Settings) Remove() (left bool, err error) {
	rec, err := s.readRecord()
	if err != nil || rec == nil {
		return false, err
	}
	info, err := os.Lstat(s.path())
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return false, err
	case !info.Mode().IsRegular():
		left = true
	default:
		cur, err := os.ReadFile(s.path())
		if err != nil {
			return false, err
		}
		if bytes.Equal(cur, []byte(rec.Content)) {
			if err := os.Remove(s.path()); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return false, err
			}
		} else {
			left = true
		}
	}
	if rec.CreatedDir && !left {
		_ = os.Remove(s.Dir) // 我们建的目录：空了才删得掉（非空时 Remove 失败，留下）
	}
	if err := os.Remove(filepath.Join(s.RecordDir, RecordFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return left, err
	}
	return left, nil
}

// Status 是托管设置的状态（状态接口用；不含路径）。
type Status struct {
	Written   bool       `json:"written"`             // 记录里写入过
	Effective bool       `json:"effective"`           // 写入过、自检全部通过、没有冲突
	Problem   string     `json:"problem,omitempty"`   // missing、modified、unreadable（普通用户读不到）、permission（组或其他人可写）
	Conflicts []Conflict `json:"conflicts,omitempty"` // 别的文件定义了代理变量
}

// Check 自检（只读，不写任何文件）。
func (s *Settings) Check() Status {
	var st Status
	rec, err := s.readRecord()
	st.Written = err == nil && rec != nil
	st.Conflicts = s.Conflicts()
	want, _ := Content(s.port())
	if st.Written {
		info, err := os.Lstat(s.path())
		switch {
		case err != nil:
			st.Problem = "missing"
		case !info.Mode().IsRegular():
			st.Problem = "modified"
		default:
			cur, rerr := os.ReadFile(s.path())
			p := info.Mode().Perm()
			switch {
			case rerr != nil || !bytes.Equal(cur, want):
				st.Problem = "modified"
			case p&0o004 == 0:
				st.Problem = "unreadable"
			case p&0o022 != 0:
				st.Problem = "permission"
			}
		}
	}
	st.Effective = st.Written && st.Problem == "" && len(st.Conflicts) == 0
	return st
}
