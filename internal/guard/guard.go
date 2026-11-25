// Package guard 是开机时按状态文件加载断网规则的逻辑（M6-2b，m6-plan v2.3 的 8 条，design 10.2、11.4）。
// cmd/abroad-guard 是它的入口：读状态文件和 abroadd 另存的参数，决定要不要加载，加载后退出。
//
// 只做三件事：读状态文件、生成规则（pf.Render，和 abroadd 同一个函数）、加载到本项目自己的锚点并取一个启用引用。
// 不联网、不监听、不常驻、不读机密，不依赖虚拟机和 abroadd 在不在。pf 操作只有“加载到自己的锚点”和“启用”
// （源码检查守着）；不清空、不关闭 pf、不碰别的锚点。
//
// 和 abroadd 共用的三个文件：
//   - 状态目录里的 guard-params.json：abroadd 每次成功应用规则后原子另存“不带 utun 的那一组”的参数，guard 只读；
//   - 运行时目录里的 pf-loaded：abroadd 本次开机已经加载过规则的标记，内容是本次开机的标识（重启后不再匹配）；
//   - 运行时目录里的 guard-pf-tokens：guard 取得的启用引用，abroadd 收养进状态文件，回国时和自己的一起释放。
package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"claude-abroad/internal/pf"
	"claude-abroad/internal/state"
)

// 文件名。
const (
	ParamsFile = "guard-params.json"
	MarkerFile = "pf-loaded"
	TokenFile  = "guard-pf-tokens"
)

// ParamsVersion 是参数文件的版本。
const ParamsVersion = 1

// Params 是 abroadd 另存给 guard 的参数（不带 utun：开机时还没有 utun）。不含任何机密。
type Params struct {
	Version int      `json:"version"`
	User    string   `json:"user"`
	LAN     []string `json:"lan,omitempty"`
	VMIfs   []string `json:"vm_ifs,omitempty"`
}

// ParamsFrom 由 abroadd 应用的规则配置得到参数（去掉 utun）。
func ParamsFrom(c pf.Config) Params {
	p := Params{Version: ParamsVersion, User: c.User, VMIfs: append([]string(nil), c.VMIfs...)}
	for _, l := range c.LAN {
		p.LAN = append(p.LAN, l.String())
	}
	return p
}

// Config 把参数还原成规则配置（不带 utun）。任何一项不合法都返回错误（由调用方按“损坏”处理）。
func (p Params) Config() (pf.Config, error) {
	if p.Version != ParamsVersion {
		return pf.Config{}, fmt.Errorf("参数文件版本 %d 不认识", p.Version)
	}
	if p.User == "" {
		return pf.Config{}, errors.New("参数文件缺放行用户")
	}
	c := pf.Config{User: p.User, VMIfs: append([]string(nil), p.VMIfs...)}
	for _, s := range p.LAN {
		pr, err := netip.ParsePrefix(s)
		if err != nil {
			return pf.Config{}, errors.New("参数文件里的局域网段不合法")
		}
		c.LAN = append(c.LAN, pr)
	}
	if _, err := pf.Render(c); err != nil { // 用同一个函数校验（用户名、私有范围、网桥名）
		return pf.Config{}, fmt.Errorf("参数文件不合法：%w", err)
	}
	return c, nil
}

// MaxParamsSize 是参数文件的读取上限。
const MaxParamsSize = 16 << 10

// WriteParams 原子写参数文件（0600，同目录临时文件加改名）。abroadd 调用。
func WriteParams(dir string, p Params) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return writeAtomic(dir, ParamsFile, append(b, '\n'))
}

// ReadParams 读参数文件：不是普通文件、太大、权限对组和其他人开放、解析失败、缺项或不合法都返回错误。
func ReadParams(dir string) (pf.Config, error) {
	b, err := readSmall(filepath.Join(dir, ParamsFile), MaxParamsSize)
	if err != nil {
		return pf.Config{}, err
	}
	var p Params
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return pf.Config{}, errors.New("参数文件解析失败")
	}
	return p.Config()
}

func readSmall(path string, max int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > max || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s 不是普通文件、太大或权限太宽", filepath.Base(path))
	}
	return os.ReadFile(path)
}

func writeAtomic(dir, name string, b []byte) error {
	f, err := os.CreateTemp(dir, "."+name+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
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

// WriteMarker 写“本次开机 abroadd 已加载过规则”的标记（abroadd 调用；bootID 为空时不写）。
func WriteMarker(runDir, bootID string) error {
	if bootID == "" {
		return errors.New("没有开机标识")
	}
	return writeAtomic(runDir, MarkerFile, []byte(bootID+"\n"))
}

// markerMatches：标记在、内容等于本次开机的标识时为 true。读不出、没有标识、不匹配都是 false（按“没加载过”处理）。
func markerMatches(runDir, bootID string) bool {
	if bootID == "" {
		return false
	}
	b, err := readSmall(filepath.Join(runDir, MarkerFile), 256)
	return err == nil && strings.TrimSpace(string(b)) == bootID
}

// AppendToken 把 guard 取得的启用引用追加进运行时目录的文件（guard 调用）。
func AppendToken(runDir, tok string) error {
	path := filepath.Join(runDir, TokenFile)
	var old []byte
	if b, err := readSmall(path, 4096); err == nil {
		old = b
	}
	return writeAtomic(runDir, TokenFile, append(old, []byte(tok+"\n")...))
}

// TakeTokens 读出并删掉 guard 留下的启用引用（abroadd 调用，收养进状态文件）。没有文件时返回 nil。
func TakeTokens(runDir string) ([]string, error) {
	path := filepath.Join(runDir, TokenFile)
	b, err := readSmall(path, 4096)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var toks []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			toks = append(toks, l)
		}
	}
	return toks, os.Remove(path)
}

// Outcome 是一次运行的结果。
type Outcome int

const (
	Loaded     Outcome = iota // 加载了断网规则
	NotNeeded                 // 不需要加载（回国、全新安装、abroadd 本次开机已经加载过）
	LoadFailed                // 要加载但失败了
)

// 退出码（三种可区分）。
const (
	ExitLoaded    = 0
	ExitNotNeeded = 3
	ExitFailed    = 1
)

// Code 是结果对应的退出码。
func (o Outcome) Code() int {
	switch o {
	case Loaded:
		return ExitLoaded
	case NotNeeded:
		return ExitNotNeeded
	}
	return ExitFailed
}

// Deps 是一次运行的依赖。
type Deps struct {
	StateDir string // abroadd 的状态目录（state.json、guard-params.json）
	RunDir   string // 运行时目录（pf-loaded、guard-pf-tokens）
	PF       *pf.Controller
	BootID   func() (string, error) // 本次开机的标识
	Logf     func(format string, a ...any)
}

func (d Deps) logf(format string, a ...any) {
	if d.Logf != nil {
		d.Logf(format, a...)
	}
}

// Run 按状态文件决定是否加载断网规则：
//   - 状态文件不存在（全新安装）、回国：什么都不做，不碰 pf；
//   - 已启用、停用、启用中、恢复中、损坏：加载“只断网、不接管”的那一组；abroadd 本次开机已经加载过时不动；
//   - 参数文件读不出、缺项或不合法：加载最严的一组（不放行任何例外）。
func Run(ctx context.Context, d Deps) (Outcome, error) {
	st := state.NewStore(d.StateDir)
	f, err := st.Load()
	switch {
	case err == nil && f.State == state.Restored:
		d.logf("状态是回国（或全新安装，没有状态文件）：不加载断网规则")
		return NotNeeded, nil
	case err == nil:
		d.logf("状态是 %s：需要断网规则", f.State)
	default:
		d.logf("状态文件读不出来（%s）：按损坏处理，加载断网规则", errClass(err))
	}
	boot := ""
	if d.BootID != nil {
		if b, err := d.BootID(); err == nil {
			boot = b
		}
	}
	if markerMatches(d.RunDir, boot) {
		d.logf("abroadd 本次开机已经加载过规则：不动")
		return NotNeeded, nil
	}
	cfg, perr := ReadParams(d.StateDir)
	if perr != nil {
		d.logf("参数文件读不出或不合法（%s）：加载最严的一组", errClass(perr))
		cfg = pf.Config{Lockdown: true}
	}
	rules, err := pf.Render(cfg)
	if err != nil {
		return LoadFailed, fmt.Errorf("生成规则：%w", err)
	}
	if err := d.PF.Load(ctx, rules); err != nil {
		return LoadFailed, fmt.Errorf("加载断网规则：%w", err)
	}
	tok, err := d.PF.Enable(ctx)
	if err != nil {
		return LoadFailed, fmt.Errorf("规则已加载，但启用 pf 失败：%w", err)
	}
	if err := AppendToken(d.RunDir, tok); err != nil {
		// 规则已经加载、pf 已经启用：保护已经生效；token 没存下只影响回国时能否释放这一个引用（重启后消失）。
		d.logf("启用引用没能写下（%s）：回国后 pf 可能保持启用但锚点为空，无害，重启后消失", errClass(err))
	}
	d.logf("已加载断网规则（%d 行）", bytes.Count(rules, []byte("\n")))
	return Loaded, nil
}

// errClass 给日志用的错误类别（不带路径和内容）。
func errClass(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "不存在"
	case errors.Is(err, fs.ErrPermission):
		return "没有权限"
	case errors.Is(err, state.ErrCorrupt):
		return "损坏"
	}
	return "格式不对"
}
