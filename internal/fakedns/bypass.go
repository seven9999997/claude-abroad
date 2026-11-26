package fakedns

import (
	"claude-abroad/internal/rules"
	"errors"
	"strings"
	"sync"
)

// BuiltinBypass 是内置的“不使用假地址”名单（设计 4.3）：时间同步、系统联网检测、局域网域名。
var BuiltinBypass = []string{
	"time.apple.com", "time-ios.apple.com", "time-macos.apple.com", "captive.apple.com",
	"*.lan", "*.local",
}

// protected 是永远不能进名单的域名（及其子域）：它们必须分配假地址、走主代理（设计 4.3）。
// 从规则引擎的内置置顶名单推导，不另外维护一份（m4-plan v2.7 第 0 步）。
var protected = rules.BuiltinPinnedBases()

// ErrProtected 是试图把 Claude 相关域名加入名单。
var ErrProtected = errors.New("Claude 相关域名不能加入不使用假地址的名单")

// isProtected 报告规范化后的 name 是否是受保护的域名或其子域。
func isProtected(name string) bool {
	for _, p := range protected {
		if name == p || strings.HasSuffix(name, "."+p) {
			return true
		}
	}
	return false
}

// Bypass 是“不使用假地址”的域名名单，并发安全。规则有两种：
//   - 精确：example.com 只匹配 example.com；
//   - 后缀：*.lan 匹配 lan 的任意子域（不含 lan 本身）。
//
// 域名都先经 Normalize 规范化。Claude 相关域名添加时拒绝，匹配时也再判一次。
type Bypass struct {
	mu     sync.RWMutex
	exact  map[string]bool
	suffix map[string]bool
	// 用户追加的（补齐三：规则文档的 bypass_extra，换规则时整体替换）和用户追加的置顶域名（匹配时排除：已经在名单里、
	// 之后被加进置顶的，按置顶处理，照常分配假地址）。
	userExact, userSuffix map[string]bool
	pinnedExtra           []string
}

// SetUser 整体替换用户追加的名单和用户追加的置顶域名（补齐三）。任何一项会覆盖 Claude 相关域名时拒绝、什么都不改。
func (b *Bypass) SetUser(patterns, pinnedExtra []string) error {
	ex, sf := map[string]bool{}, map[string]bool{}
	for _, p := range patterns {
		n, wild, err := parsePattern(p)
		if err != nil {
			return err
		}
		if isProtected(n) {
			return ErrProtected
		}
		if wild {
			for _, q := range protected {
				if strings.HasSuffix(q, "."+n) {
					return ErrProtected
				}
			}
			sf[n] = true
		} else {
			ex[n] = true
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.userExact, b.userSuffix, b.pinnedExtra = ex, sf, append([]string(nil), pinnedExtra...)
	return nil
}

// NewBypass 返回带内置名单的 Bypass。
func NewBypass() *Bypass {
	b := &Bypass{exact: map[string]bool{}, suffix: map[string]bool{}}
	for _, p := range BuiltinBypass {
		if err := b.Add(p); err != nil {
			panic(err) // 内置项写错是编程错误
		}
	}
	return b
}

// parse 把规则拆成（域名，是否后缀）。
func parsePattern(pattern string) (string, bool, error) {
	wild := strings.HasPrefix(pattern, "*.")
	n, err := Normalize(strings.TrimPrefix(pattern, "*."))
	if err != nil {
		return "", false, err
	}
	return n, wild, nil
}

// Add 加一条规则。会覆盖 Claude 相关域名的规则一律拒绝：精确规则是受保护域名或其子域，
// 或后缀规则的范围包含受保护域名（例如 *.com、*.ai），或在受保护域名之下（例如 *.claude.ai）。
func (b *Bypass) Add(pattern string) error {
	n, wild, err := parsePattern(pattern)
	if err != nil {
		return err
	}
	if isProtected(n) {
		return ErrProtected
	}
	if wild {
		for _, p := range protected {
			if strings.HasSuffix(p, "."+n) {
				return ErrProtected
			}
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if wild {
		b.suffix[n] = true
	} else {
		b.exact[n] = true
	}
	return nil
}

// Remove 删掉一条规则（不存在时什么都不做）。
func (b *Bypass) Remove(pattern string) {
	n, wild, err := parsePattern(pattern)
	if err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if wild {
		delete(b.suffix, n)
	} else {
		delete(b.exact, n)
	}
}

// Match 报告规范化后的 name 是否在名单里。受保护的域名永远返回 false。
func (b *Bypass) Match(name string) bool {
	if b == nil || isProtected(name) {
		return false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.pinnedExtra) > 0 { // 用户追加的置顶域名同样永远分配假地址（匹配时再判一次）
		if pinned, ok := rules.IsPinned(name, b.pinnedExtra); !ok || pinned {
			return false
		}
	}
	if b.exact[name] || b.userExact[name] {
		return true
	}
	for s := name; ; {
		i := strings.IndexByte(s, '.')
		if i < 0 {
			return false
		}
		s = s[i+1:]
		if b.suffix[s] || b.userSuffix[s] {
			return true
		}
	}
}
