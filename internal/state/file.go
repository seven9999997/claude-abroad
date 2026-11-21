// Package state 是 Claude 出国的状态机和状态文件（设计第 10 节，M2-7）。本文件是状态文件：
// state.json 的读写。读到不对的内容一律报 ErrCorrupt，绝不当成 RESTORED（否则开机时不加载 pf）。
//
// 状态文件里不写出口 IP、订阅地址、凭据。
package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"claude-abroad/internal/platform"
	"claude-abroad/internal/region"
	"claude-abroad/internal/sysnet"
	"claude-abroad/internal/usererror"
)

// State 是总状态（设计第 10 节）。
type State string

const (
	Restored  State = "RESTORED"
	Enabling  State = "ENABLING"
	Enabled   State = "ENABLED"
	Disabled  State = "DISABLED"
	Restoring State = "RESTORING"
)

func (s State) valid() bool {
	switch s {
	case Restored, Enabling, Enabled, Disabled, Restoring:
		return true
	}
	return false
}

// IPv6Setting 是一个网络服务原来的 IPv6 设置（定义在 sysnet，这里是别名）。
type IPv6Setting = sysnet.IPv6Setting

// Version 是当前的文件格式版本。
const Version = 1

// MaxSize 是状态文件的大小上限。
const MaxSize = 1 << 20

// FileName 是状态文件名。
const FileName = "state.json"

// File 是状态文件的内容。
type File struct {
	Version int   `json:"version"`
	State   State `json:"state"`
	Origin  State `json:"origin,omitempty"` // 从 RESTORED 还是 DISABLED 启用（失败时回滚的目标）
	Step    int   `json:"step"`             // 当前步骤序号（每一步执行前先写）
	// PFTokens 是 pf 启用引用（追加保存，恢复时全部释放）。
	PFTokens []string `json:"pf_tokens,omitempty"`
	// PFEnablePending 在调用 pfctl -E 之前置 true 并落盘，拿到 token 后与 token 一起落盘时清掉。
	// 恢复时看到它为 true，说明可能漏掉了一个 token（崩溃在 -E 成功与落盘之间）。
	PFEnablePending bool   `json:"pf_enable_pending,omitempty"`
	Utun            string `json:"utun,omitempty"`
	// IPv6Original 是各网络服务原来的 IPv6 设置；一次 RESTORED → ENABLING 周期内只写一次。
	IPv6Original      map[string]IPv6Setting `json:"ipv6_original,omitempty"`
	IPv6SnapshotTaken bool                   `json:"ipv6_snapshot_taken,omitempty"`
	// OrigDNS 是启用前本机的 DNS 服务器（给 M3 的直连解析用）。
	OrigDNS []netip.Addr `json:"orig_dns,omitempty"`
	// Region 是地区（M4-5）：原值、本周期锁定的目标、同步进行中的标记。
	Region RegionFile `json:"region,omitzero"`
}

// RegionFile 是状态文件里的地区（m4-plan M4-5 v2.4）：
//   - Original：Mac 原来的地区设置。只在从 RESTORED 启用时记（从 DISABLED 再启用时不重拍）；恢复时按它逐项写回；
//   - Target：本周期写进 Mac 的目标（启用第 ⑥ 步、运行中的同步之前先落盘）。有它才需要在恢复时写回原值；
//   - Syncing：ENABLED 下运行中的同步（应用主代理、切换地区）进行中；崩溃后 Recover 接着写完。
type RegionFile struct {
	Original *sysnet.MacLocale `json:"original,omitempty"`
	Target   *region.Target    `json:"target,omitempty"`
	Syncing  bool              `json:"syncing,omitempty"`
}

func (f File) validate() error {
	if f.Version != Version {
		return fmt.Errorf("不认识的版本 %d", f.Version)
	}
	if !f.State.valid() {
		return errors.New("不认识的状态")
	}
	switch f.Origin {
	case "", Restored, Disabled:
	default:
		return errors.New("来源状态只能是 RESTORED 或 DISABLED")
	}
	if f.Step < 0 {
		return errors.New("步骤序号为负")
	}
	for _, tok := range f.PFTokens {
		if _, err := strconv.ParseUint(tok, 10, 64); err != nil {
			return errors.New("pf token 不是数字")
		}
	}
	for _, s := range f.IPv6Original {
		if !s.Valid() {
			return errors.New("IPv6 原值不合法")
		}
	}
	if t := f.Region.Target; t != nil {
		if err := t.Validate(); err != nil {
			return errors.New("地区目标不合法")
		}
	}
	if o := f.Region.Original; o != nil {
		if region.ValidTimezone(o.Timezone) != nil {
			return errors.New("地区原值不合法")
		}
	}
	return nil
}

// ErrCorrupt 是状态文件存在但内容或属性不对（不能当成 RESTORED）。
var ErrCorrupt = errors.New("状态文件已损坏")

func corrupt(why string) error {
	return usererror.New("读取状态文件", "文件已损坏或不安全（"+why+"）",
		"为了安全，Claude 出国按“未恢复”处理并保持断网保护；请执行 sudo abroad restore --force，或重新安装后执行一键卸载。",
		ErrCorrupt)
}

// Store 读写一个目录里的 state.json。目录必须是 0700 且属主是 UID（开发模式是当前用户，正式是 root）。
type Store struct {
	Dir string
	UID int

	mu sync.Mutex
	// beforeRename 测试注入：在 rename 之前调用，返回错误则放弃这次写入。
	beforeRename func() error
	// syncDir 测试注入：替换目录 fsync。
	syncDir func(dir string) error
	// afterSave 测试注入：每次成功写入之后调用（持锁，不能再调用 Store 的方法）。
	afterSave func(File)
	// afterPending 测试注入：待还原文件写入、删除之后调用（崩溃注入点）。
	afterPending func(string)

	lockMu sync.Mutex
	lockFd *os.File // 持有跨进程锁时非 nil
}

func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// NewStore 返回属主为当前用户的 Store（开发模式）。
func NewStore(dir string) *Store { return &Store{Dir: dir, UID: os.Getuid()} }

func (s *Store) path() string { return filepath.Join(s.Dir, FileName) }

func ownerOK(info fs.FileInfo, uid int) bool {
	owner, err := platform.FileOwner(info)
	return err == nil && owner == uid
}

// checkDir 检查目录：不存在返回 (false, nil)。
func (s *Store) checkDir() (bool, error) {
	info, err := os.Lstat(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, corrupt("读不了目录")
	}
	if !info.IsDir() {
		return false, corrupt("状态目录不是目录")
	}
	if info.Mode().Perm() != 0o700 {
		return false, corrupt("状态目录权限不是 0700")
	}
	if !ownerOK(info, s.UID) {
		return false, corrupt("状态目录属主不对")
	}
	return true, nil
}

// ensureDir 检查状态目录，不存在时以 0700 创建。
func (s *Store) ensureDir() error {
	ok, err := s.checkDir()
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("建状态目录：%w", err)
	}
	if err := os.Chmod(s.Dir, 0o700); err != nil {
		return err
	}
	_, err = s.checkDir()
	return err
}

// Load 读状态文件。文件（或目录）不存在时返回 RESTORED；内容或属性不对返回 ErrCorrupt（usererror 包装）。
func (s *Store) Load() (File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

func (s *Store) load() (File, error) {
	fresh := File{Version: Version, State: Restored}
	ok, err := s.checkDir()
	if err != nil {
		return File{}, err
	}
	if !ok {
		return fresh, nil
	}
	fd, err := platform.OpenNoFollow(s.path(), os.O_RDONLY, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return fresh, nil
	}
	if err != nil {
		return File{}, corrupt("打不开（可能是符号链接）")
	}
	defer fd.Close()
	info, err := fd.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return File{}, corrupt("不是普通文件")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return File{}, corrupt("权限过宽")
	}
	if !ownerOK(info, s.UID) {
		return File{}, corrupt("属主不对")
	}
	data, err := io.ReadAll(io.LimitReader(fd, MaxSize+1))
	if err != nil {
		return File{}, corrupt("读取出错")
	}
	if len(data) > MaxSize {
		return File{}, corrupt("超过大小上限")
	}
	var f File
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return File{}, corrupt("不是合法的 JSON 或有未知字段")
	}
	if dec.More() {
		return File{}, corrupt("末尾有多余内容")
	}
	if err := f.validate(); err != nil {
		return File{}, corrupt(err.Error())
	}
	return f, nil
}

// Save 原子地写入状态文件：临时文件（O_NOFOLLOW、O_EXCL，0600）→ fsync → rename → 目录 fsync。
// 目录不存在时以 0700 创建；目标是符号链接时拒绝。
func (s *Store) Save(f File) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.save(f)
}

func (s *Store) save(f File) error {
	if f.Version == 0 {
		f.Version = Version
	}
	if err := f.validate(); err != nil {
		return fmt.Errorf("写状态文件：内容不合法：%w", err)
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := s.ensureDir(); err != nil {
		return err
	}
	if info, err := os.Lstat(s.path()); err == nil && !info.Mode().IsRegular() {
		return corrupt("目标不是普通文件（可能是符号链接），拒绝写入")
	}
	tmp := filepath.Join(s.Dir, fmt.Sprintf(".%s.%d.%d.tmp", FileName, os.Getpid(), time.Now().UnixNano()))
	fd, err := platform.OpenNoFollow(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("写状态文件：%w", err)
	}
	done := false
	defer func() {
		if !done {
			fd.Close()
			os.Remove(tmp)
		}
	}()
	if _, err := fd.Write(data); err != nil {
		return fmt.Errorf("写状态文件：%w", err)
	}
	if err := fd.Sync(); err != nil {
		return fmt.Errorf("写状态文件：%w", err)
	}
	if err := fd.Close(); err != nil {
		return fmt.Errorf("写状态文件：%w", err)
	}
	if s.beforeRename != nil {
		if err := s.beforeRename(); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, s.path()); err != nil {
		return fmt.Errorf("写状态文件：%w", err)
	}
	done = true
	// 目录 fsync 失败时 rename 可能没有真正落盘（断电后读到旧状态）：必须报错，调用方不得继续下一步。
	sync := s.syncDir
	if sync == nil {
		sync = fsyncDir
	}
	if err := sync(s.Dir); err != nil {
		return fmt.Errorf("写状态文件：目录落盘失败：%w", err)
	}
	if s.afterSave != nil {
		s.afterSave(f)
	}
	return nil
}

// Update 读、改、写（在同一把锁里）。
func (s *Store) Update(fn func(*File) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return err
	}
	if err := fn(&f); err != nil {
		return err
	}
	return s.save(f)
}

// TakeIPv6Snapshot 只在本周期还没拍过快照时写入 IPv6 原值，返回是否写入（重入时不覆盖：
// 那时网络服务的 IPv6 可能已经被关掉了）。
func (s *Store) TakeIPv6Snapshot(orig map[string]IPv6Setting) (bool, error) {
	wrote := false
	err := s.Update(func(f *File) error {
		if f.IPv6SnapshotTaken {
			return nil
		}
		f.IPv6Original = orig
		f.IPv6SnapshotTaken = true
		wrote = true
		return nil
	})
	return wrote && err == nil, err
}

// MarkRestored 写入干净的 RESTORED（清掉快照、token 等，下一个周期可以重新拍快照）。
// 只能在 pf 清除之后调用（顺序约束由状态机保证）。
func (s *Store) MarkRestored() error {
	return s.Save(File{Version: Version, State: Restored})
}

// SetEnablePending 在调用 pfctl -E 之前置 pending 并落盘。
func (s *Store) SetEnablePending() error {
	return s.Update(func(f *File) error { f.PFEnablePending = true; return nil })
}

// AddToken 追加一个 pf 启用引用并清掉 pending，一起落盘。不去重、不丢已有的 token。
func (s *Store) AddToken(tok string) error {
	return s.Update(func(f *File) error {
		f.PFTokens = append(f.PFTokens, tok)
		f.PFEnablePending = false
		return nil
	})
}

// ClearEnablePending 清掉 pending（pf 本来就启用着、没有拿到新 token 时用）。
func (s *Store) ClearEnablePending() error {
	return s.Update(func(f *File) error { f.PFEnablePending = false; return nil })
}
