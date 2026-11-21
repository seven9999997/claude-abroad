package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"claude-abroad/internal/exitconf"
	"claude-abroad/internal/guestabi"
	"claude-abroad/internal/platform"
	"claude-abroad/internal/rules"
	"claude-abroad/internal/usererror"
)

// 分流出口的本机存储（M4-8，m4-plan v2.11 决定 1；design 7.4）：
//   - 状态目录下的 secrets/（0700，属主是状态目录的属主），一个出口一个文件 exit-<标签>.json（0600，原子写）；
//   - 文件里是标签、种类、配置原文；读出时再用 exitconf 校验一遍（坏文件跳过并发事件，不让它进虚拟机）；
//   - 对外只给摘要：标签、种类、端点个数和“已配置”，不给配置原文、密钥、端点主机名或地址。

// SecretsDir 是状态目录下放凭据的子目录。
const SecretsDir = "secrets"

// MaxExits 是分流出口的上限（和 agent 一致）。
const MaxExits = 16

// MaxExitBody 是 PUT /v1/exits/<标签> 的请求体上限（订阅最大 1 MiB，JSON 转义后会变大）。
const MaxExitBody = 2 << 20

var exitTagRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// StoredExit 是一个出口的完整内容（只在本机和下发给虚拟机时用）。
type StoredExit struct {
	Tag  string        `json:"tag"`
	Kind exitconf.Kind `json:"kind"`
	Conf string        `json:"conf"`
}

// ExitSummary 是对外的摘要（接口、status 用）：没有配置原文、密钥、端点主机名和地址。
type ExitSummary struct {
	Tag       string        `json:"tag"`
	Kind      exitconf.Kind `json:"kind"`
	Server    string        `json:"server"`    // 固定写“已配置”
	Endpoints int           `json:"endpoints"` // 服务器端点的个数
}

// ExitStore 管理 secrets/ 下的出口文件。并发安全。
type ExitStore struct {
	Dir string // secrets/ 的路径
	UID int    // 期望的属主
	// Write 写文件（默认 writeFileAtomic；测试可注入失败）。
	Write func(path string, b []byte) error

	mu    sync.Mutex
	exits map[string]StoredExit
}

// exitFile 是出口文件的路径。
func (s *ExitStore) exitFile(tag string) string { return filepath.Join(s.Dir, "exit-"+tag+".json") }

// ensureDir 建 secrets/（0700），并确认它是真目录、属主对、权限只给属主。
func (s *ExitStore) ensureDir() error {
	if err := os.Mkdir(s.Dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	fi, err := os.Lstat(s.Dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode()&fs.ModeSymlink != 0 {
		return errors.New("secrets 不是真目录")
	}
	owner, err := platform.FileOwner(fi)
	if err != nil || owner != s.UID {
		return errors.New("secrets 的属主不对或取不到")
	}
	if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(s.Dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// parseExit 用 exitconf 校验配置，返回端点个数。
func parseExit(kind exitconf.Kind, conf string) (int, error) {
	switch kind {
	case exitconf.WireGuard:
		w, err := exitconf.ParseWG([]byte(conf))
		return len(w.Endpoints()), err
	case exitconf.OpenVPN:
		o, err := exitconf.ParseOVPN([]byte(conf))
		return len(o.Endpoints()), err
	case exitconf.Clash:
		c, err := exitconf.ParseClash([]byte(conf))
		return len(c.Endpoints()), err
	}
	return 0, errors.New("种类只能是 wireguard、openvpn、clash")
}

// ValidTag 判断出口标签：小写字母开头，最多 32 位，不能叫 main。
func ValidTag(tag string) bool { return exitTagRe.MatchString(tag) && tag != guestabi.ExitMain }

// Load 读出 secrets/ 下全部出口。坏文件（读不出、不合法、标签和文件名对不上）不进内存，返回它们的说明（不含内容）。
func (s *ExitStore) Load() (bad []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exits = map[string]StoredExit{}
	ents, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := s.ensureDir(); err != nil {
		return nil, err
	}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasPrefix(name, "exit-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		tag := strings.TrimSuffix(strings.TrimPrefix(name, "exit-"), ".json")
		x, err := s.readOne(tag)
		if err != nil {
			bad = append(bad, fmt.Sprintf("出口 %s：%s", tag, usererror.Reason(err)))
			continue
		}
		s.exits[tag] = x
	}
	sort.Strings(bad)
	return bad, nil
}

func (s *ExitStore) readOne(tag string) (StoredExit, error) {
	var x StoredExit
	if !ValidTag(tag) {
		return x, errors.New("文件名里的标签不合法")
	}
	f, err := platform.OpenNoFollow(s.exitFile(tag), os.O_RDONLY, 0)
	if err != nil {
		return x, errors.New("读不出（可能是符号链接）")
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return x, errors.New("不是普通文件，或权限过宽")
	}
	if owner, err := platform.FileOwner(fi); err != nil || owner != s.UID {
		return x, errors.New("属主不对或取不到")
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxExitBody+1))
	if err != nil || len(b) > MaxExitBody {
		return x, errors.New("读不出或太大")
	}
	if err := json.Unmarshal(b, &x); err != nil {
		return x, errors.New("不是有效的 JSON")
	}
	if x.Tag != tag {
		return x, errors.New("文件里的标签和文件名对不上")
	}
	if _, err := parseExit(x.Kind, x.Conf); err != nil {
		return x, errors.New("配置不合法")
	}
	return x, nil
}

// ErrExitNotFound 是没有这个出口。
var ErrExitNotFound = errors.New("没有这个出口")

// Put 存一个出口（新增或替换）。调用方已校验（Validate）。
func (s *ExitStore) Put(x StoredExit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exits == nil {
		s.exits = map[string]StoredExit{}
	}
	if _, ok := s.exits[x.Tag]; !ok && len(s.exits) >= MaxExits {
		return fmt.Errorf("分流出口最多 %d 个", MaxExits)
	}
	if err := s.ensureDir(); err != nil {
		return err
	}
	b, err := json.Marshal(x)
	if err != nil {
		return err
	}
	write := s.Write
	if write == nil {
		write = writeFileAtomic
	}
	if err := write(s.exitFile(x.Tag), b); err != nil {
		return err
	}
	s.exits[x.Tag] = x
	return nil
}

// Delete 删一个出口的文件。
func (s *ExitStore) Delete(tag string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.exits[tag]; !ok {
		return ErrExitNotFound
	}
	if err := os.Remove(s.exitFile(tag)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	delete(s.exits, tag)
	return nil
}

// Has 报告出口是否已配置。
func (s *ExitStore) Has(tag string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.exits[tag]
	return ok
}

// All 返回全部出口（按标签排序；含配置原文，只给下发用）。
func (s *ExitStore) All() []StoredExit {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]StoredExit, 0, len(s.exits))
	for _, x := range s.exits {
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tag < out[j].Tag })
	return out
}

// Summaries 返回全部出口的摘要。
func (s *ExitStore) Summaries() []ExitSummary {
	out := []ExitSummary{}
	for _, x := range s.All() {
		out = append(out, summaryOf(x))
	}
	return out
}

// Summary 返回一个出口的摘要。
func (s *ExitStore) Summary(tag string) (ExitSummary, bool) {
	s.mu.Lock()
	x, ok := s.exits[tag]
	s.mu.Unlock()
	if !ok {
		return ExitSummary{}, false
	}
	return summaryOf(x), true
}

func summaryOf(x StoredExit) ExitSummary {
	n, _ := parseExit(x.Kind, x.Conf)
	return ExitSummary{Tag: x.Tag, Kind: x.Kind, Server: "已配置", Endpoints: n}
}

// RuleRefs 返回规则里引用 tag 的条目序号（从 1 开始）。
func RuleRefs(c *rules.Config, tag string) []int {
	var out []int
	for i, r := range c.Rules {
		if r.Action.Kind == rules.Proxy && r.Action.Exit == tag {
			out = append(out, i+1)
		}
	}
	return out
}

// UnknownExitRefs 检查规则里的分流出口都已配置：返回第一个不存在的出口的说明（指出是第几条），都存在时 nil。
func UnknownExitRefs(c *rules.Config, has func(string) bool) error {
	if c.Default.Kind == rules.Proxy && c.Default.Exit != guestabi.ExitMain && !has(c.Default.Exit) {
		return fmt.Errorf("default：出口 %s 不存在", c.Default.Exit)
	}
	for i, r := range c.Rules {
		if r.Action.Kind == rules.Proxy && r.Action.Exit != guestabi.ExitMain && !has(r.Action.Exit) {
			return fmt.Errorf("rules 第 %d 条（%s）：出口 %s 不存在，请先在“出口”里添加", i+1, r.App, r.Action.Exit)
		}
	}
	return nil
}
