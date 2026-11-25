// Package resolverlock 是 Claude 域名解析锁（设计 10.2 第一层，M6-3，m6-plan v2.4 的 8 条）：在 /etc/resolver/
// 下为内置的置顶域名各写一个文件，让这些域名只能由 Claude 出国的 DNS 解析。回国后虚拟网卡撤掉，解析失败，Claude 连不上。
//
//   - 只覆盖内置的置顶域名（rules.BuiltinPinnedBases，和规则引擎同一来源）；用户追加的 pinned_extra 不进锁文件；
//   - 文件内容做成参数：默认 nameserver 198.18.0.2，备选回环地址加固定端口（R18）；
//   - 静态：首次准备时写入、卸载时删除，回国、停用、退出应用都不碰；
//   - 不覆盖别人的文件：同名文件的内容既不是这次要写的、也不是记录里我们写过的，就报告冲突、不动它；
//     删除只删内容和记录一致的文件；
//   - 自检只读，不自动重写（重写是敏感操作）。
//
// 本包不联网、不执行外部命令（源码检查）。目录由调用方给出：真实的 /etc/resolver 只在真实模式入口接上。
package resolverlock

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"claude-abroad/internal/rules"
)

// Form 是锁文件的写法。
type Form struct {
	Addr netip.Addr
	Port uint16 // 0 表示不写 port 行（用 53）
}

// DefaultAddr 是设计 10.2 的锁地址：只有 Claude 出国的 DNS 应答它。
var DefaultAddr = netip.MustParseAddr("198.18.0.2")

// Default 是默认写法。
func Default() Form { return Form{Addr: DefaultAddr} }

// Loopback 是备选写法（R18）：回环地址加固定端口。已启用时需要的本机 DNS 监听等 R18 的结论选定后再做。
func Loopback(port uint16) Form { return Form{Addr: netip.MustParseAddr("127.0.0.1"), Port: port} }

// Name 是写法的名字（状态接口用）。
func (f Form) Name() string {
	if f.Port != 0 {
		return "loopback_port"
	}
	return "default"
}

func (f Form) valid() error {
	switch {
	case f.Addr == DefaultAddr && f.Port == 0:
		return nil
	case f.Addr.IsLoopback() && f.Addr.Is4() && f.Port > 0:
		return nil
	}
	return errors.New("锁文件的写法只能是默认地址，或回环地址加端口")
}

// Content 是锁文件的内容（逐字固定）。
func Content(f Form) ([]byte, error) {
	if err := f.valid(); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "nameserver %s\n", f.Addr)
	if f.Port != 0 {
		fmt.Fprintf(&b, "port %d\n", f.Port)
	}
	b.WriteString("timeout 1\n")
	return b.Bytes(), nil
}

// Domains 是要锁的域名：内置的置顶域名（基础域名；/etc/resolver/<域名> 同时管它的子域名）。
func Domains() []string { return rules.BuiltinPinnedBases() }

// TestDomain 是开发构建“只锁一个测试域名”时用的保留域名（.invalid 不会是真实域名）。
const TestDomain = "lock-test.claude-abroad.invalid"

// RecordFile 是记录文件名（放在 abroadd 的状态目录里）：记下我们写入的域名、写法和内容。
const RecordFile = "resolver-lock.json"

type record struct {
	Version int      `json:"version"`
	Form    string   `json:"form"`
	Domains []string `json:"domains"`
	Content string   `json:"content"`
}

// Lock 是一把解析锁。
type Lock struct {
	Dir       string   // 锁文件所在目录（真实模式 /etc/resolver；测试用临时目录）
	RecordDir string   // 记录文件所在目录（abroadd 的状态目录）
	Domains   []string // 为空时用 Domains()
	Form      Form     // 零值用 Default()
}

func (l *Lock) domains() []string {
	if len(l.Domains) == 0 {
		return Domains()
	}
	return l.Domains
}

func (l *Lock) form() Form {
	if !l.Form.Addr.IsValid() {
		return Default()
	}
	return l.Form
}

func validDomain(d string) bool {
	n, err := rules.NormalizeName(d)
	return err == nil && n == d && !strings.ContainsAny(d, "/\\") && !strings.HasPrefix(d, ".")
}

func (l *Lock) path(d string) string { return filepath.Join(l.Dir, d) }

func (l *Lock) readRecord() (*record, error) {
	b, err := os.ReadFile(filepath.Join(l.RecordDir, RecordFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r record
	if json.Unmarshal(b, &r) != nil || r.Version != 1 {
		return nil, errors.New("解析锁的记录文件损坏")
	}
	return &r, nil
}

func (l *Lock) writeRecord(r record) error {
	b, _ := json.Marshal(r)
	return writeAtomic(l.RecordDir, RecordFile, append(b, '\n'), 0o600)
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

// WriteResult 是一次写入的结果。
type WriteResult struct {
	Written   []string // 写好（或本来就是这个内容）的域名
	Conflicts []string // 已有别人的同名文件、没有覆盖的域名
}

// ours 判断已有文件的内容是不是我们写的：等于这次要写的，或等于记录里我们写过的。
func ours(cur, want []byte, rec *record) bool {
	return bytes.Equal(cur, want) || (rec != nil && rec.Content != "" && bytes.Equal(cur, []byte(rec.Content)))
}

// Write 写入锁文件（首次准备调用；可重入，重复写入结果相同）。先写记录再写文件：写到一半失败时，
// 再次 Write 或 Remove 都能收拾干净。已有别人的同名文件时不覆盖，记进 Conflicts；有冲突时返回 ErrConflict（其余照写）。
func (l *Lock) Write() (WriteResult, error) {
	var res WriteResult
	f := l.form()
	want, err := Content(f)
	if err != nil {
		return res, err
	}
	doms := l.domains()
	for _, d := range doms {
		if !validDomain(d) {
			return res, fmt.Errorf("域名不合法：%q", d)
		}
	}
	rec, err := l.readRecord()
	if err != nil {
		return res, err
	}
	// 记录先落盘（域名取并集：之前写过、这次没写的也要能被 Remove 收拾）
	all := slices.Clone(doms)
	if rec != nil {
		for _, d := range rec.Domains {
			if !slices.Contains(all, d) {
				all = append(all, d)
			}
		}
	}
	if err := l.writeRecord(record{Version: 1, Form: f.Name(), Domains: all, Content: string(want)}); err != nil {
		return res, err
	}
	if err := os.MkdirAll(l.Dir, 0o755); err != nil {
		return res, err
	}
	for _, d := range doms {
		cur, err := os.ReadFile(l.path(d))
		switch {
		case err == nil && !ours(cur, want, rec):
			res.Conflicts = append(res.Conflicts, d)
			continue
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return res, fmt.Errorf("读 %s 的锁文件：%w", d, err)
		case err == nil && bytes.Equal(cur, want):
			if info, serr := os.Lstat(l.path(d)); serr == nil && info.Mode().Perm() == 0o644 {
				res.Written = append(res.Written, d) // 已经是这个内容：不重写（修改时间不变）
				continue
			}
		}
		if err := writeAtomic(l.Dir, d, want, 0o644); err != nil {
			return res, fmt.Errorf("写 %s 的锁文件：%w", d, err)
		}
		res.Written = append(res.Written, d)
	}
	if len(res.Conflicts) > 0 {
		return res, ErrConflict
	}
	return res, nil
}

// ErrConflict 是有别人的同名文件、没有覆盖。
var ErrConflict = errors.New("解析锁有冲突：已有别人的同名文件，没有覆盖")

// RemoveResult 是一次删除的结果。
type RemoveResult struct {
	Removed []string // 删掉的域名
	Left    []string // 内容不是我们写的（别人的或被改过）、留下的域名
}

// Remove 删除我们写的锁文件（卸载调用；可重入）。只删内容和记录一致的；都删干净后删记录。
func (l *Lock) Remove() (RemoveResult, error) {
	var res RemoveResult
	rec, err := l.readRecord()
	if err != nil || rec == nil {
		return res, err
	}
	for _, d := range rec.Domains {
		if !validDomain(d) {
			continue
		}
		info, err := os.Lstat(l.path(d))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return res, err
		}
		if !info.Mode().IsRegular() { // 不是普通文件：不是我们写的，留下
			res.Left = append(res.Left, d)
			continue
		}
		cur, err := os.ReadFile(l.path(d))
		switch {
		case err != nil:
			return res, err
		case !bytes.Equal(cur, []byte(rec.Content)):
			res.Left = append(res.Left, d)
			continue
		}
		if err := os.Remove(l.path(d)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return res, err
		}
		res.Removed = append(res.Removed, d)
	}
	if err := os.Remove(filepath.Join(l.RecordDir, RecordFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, err
	}
	return res, nil
}

// Problem 是自检发现的一个问题。
type Problem struct {
	Domain string `json:"domain"`
	Kind   string `json:"kind"` // missing、modified、permission
}

// Status 是锁的状态（状态接口用；不含路径以外的敏感内容）。
type Status struct {
	Written   bool      `json:"written"`             // 记录里写入过
	Domains   []string  `json:"domains"`             // 锁的域名
	Form      string    `json:"form"`                // default、loopback_port
	Effective bool      `json:"effective"`           // 写入过且自检全部通过（第一层生效）
	Problems  []Problem `json:"problems,omitempty"`  // 自检发现的问题
	Conflicts []string  `json:"conflicts,omitempty"` // 有别人同名文件的域名（首次准备写不进去的）
}

// Check 自检（只读，不写任何文件）：每个域名的文件在不在、内容对不对、权限对不对（组和其他人不能写、所有人能读）；
// 另外列出有别人同名文件的域名。
func (l *Lock) Check() Status {
	f := l.form()
	st := Status{Domains: slices.Clone(l.domains()), Form: f.Name()}
	want, err := Content(f)
	if err != nil {
		return st
	}
	rec, rerr := l.readRecord()
	st.Written = rerr == nil && rec != nil
	for _, d := range st.Domains {
		info, err := os.Lstat(l.path(d))
		if err != nil {
			if st.Written {
				st.Problems = append(st.Problems, Problem{Domain: d, Kind: "missing"})
			}
			continue
		}
		cur, rerr := os.ReadFile(l.path(d))
		if !info.Mode().IsRegular() || rerr != nil || !bytes.Equal(cur, want) {
			if rerr == nil && info.Mode().IsRegular() && !ours(cur, want, rec) {
				st.Conflicts = append(st.Conflicts, d)
			}
			if st.Written {
				st.Problems = append(st.Problems, Problem{Domain: d, Kind: "modified"})
			}
			continue
		}
		if p := info.Mode().Perm(); p&0o022 != 0 || p&0o444 != 0o444 {
			st.Problems = append(st.Problems, Problem{Domain: d, Kind: "permission"})
		}
	}
	st.Effective = st.Written && len(st.Problems) == 0
	return st
}
