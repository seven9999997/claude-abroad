// Package rules 是按应用分流的规则引擎（design 5.2、m3-plan M3-1）：配置的解析与校验，以及纯函数的判定。
// 只做计算，不做 I/O，不依赖网络栈、daemon、系统层。
package rules

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"

	"claude-abroad/internal/usererror"
	"go.yaml.in/yaml/v3"
)

// ExitMain 是主代理的出口标签（与 guestabi.ExitMain 相同；这里不 import guestabi，保持纯计算包）。
const ExitMain = "main"

// MaxConfigSize 是规则文件的大小上限。
const MaxConfigSize = 256 << 10

// AppID 是应用 ID（design 5.1）：Bundle ID、"path:<绝对路径>"、"system:<名字>"，识别失败时是 "unknown"。
type AppID string

// Unknown 是识别失败时的应用 ID。只走 default，不能写进规则。
const Unknown AppID = "unknown"

type Kind int

const (
	Proxy Kind = iota + 1
	Direct
	Block
)

func (k Kind) String() string {
	switch k {
	case Proxy:
		return "proxy"
	case Direct:
		return "direct"
	case Block:
		return "block"
	}
	return "invalid"
}

// Action 是动作：proxy:<出口>、direct、block。
type Action struct {
	Kind Kind
	Exit string // Kind==Proxy 时的出口标签
}

var (
	ActDirect = Action{Kind: Direct}
	ActBlock  = Action{Kind: Block}
	ActMain   = Action{Kind: Proxy, Exit: ExitMain}
)

func (a Action) String() string {
	if a.Kind == Proxy {
		return "proxy:" + a.Exit
	}
	return a.Kind.String()
}

var exitTagRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ParseAction 解析 "proxy:<出口>"、"direct"、"block"。
func ParseAction(s string) (Action, error) {
	switch s {
	case "direct":
		return ActDirect, nil
	case "block":
		return ActBlock, nil
	}
	if e, ok := strings.CutPrefix(s, "proxy:"); ok {
		if !exitTagRe.MatchString(e) {
			return Action{}, fmt.Errorf("出口标签 %q 不合法（小写字母开头，只含小写字母、数字和连字符，最长 32 个字符）", e)
		}
		return Action{Kind: Proxy, Exit: e}, nil
	}
	return Action{}, fmt.Errorf("动作 %q 不认识（只能是 proxy:<出口>、direct、block）", s)
}

// Rule 是一条应用规则。
type Rule struct {
	App    AppID
	Action Action
}

// Config 是规则文件（design 5.2）。只能经 Parse 得到，或由 Default() 给出。
type Config struct {
	Default      Action
	Rules        []Rule
	OfflineAllow []AppID
	LocalDomains []string // 归一后的模式
	PinnedExtra  []string // 用户追加的置顶域名（归一后的模式）；内置的永远在
	BlockUDP443  bool
	// BypassExtra 是用户追加的“不分配假地址”的域名（补齐三，设计 4.3；归一后的模式）。内置的几项在 fakedns 里、
	// 永远在。这些域名的 A 查询交给上游（经主代理的 DoH）解析，之后的连接仍按规则判定。
	BypassExtra []string

	local  []pattern
	pinned []pattern // 用户追加的置顶域名（已解析，Parse 时算好）
}

// DefaultConfig 是最严格的默认配置：全部走主代理，没有任何直连。
func DefaultConfig() *Config { return &Config{Default: ActMain} }

type fileRule struct {
	App    string `yaml:"app" json:"app"`
	Action string `yaml:"action" json:"action"`
}

type file struct {
	Default      string     `yaml:"default" json:"default"`
	Rules        []fileRule `yaml:"rules" json:"rules"`
	OfflineAllow []string   `yaml:"offline_allow" json:"offline_allow"`
	LocalDomains []string   `yaml:"local_domains" json:"local_domains"`
	PinnedExtra  []string   `yaml:"pinned_extra" json:"pinned_extra"`
	BlockUDP443  bool       `yaml:"block_udp_443" json:"block_udp_443"`
	BypassExtra  []string   `yaml:"bypass_extra,omitempty" json:"bypass_extra,omitempty"`
}

// FileRule、File 是规则文件的形式（GET /v1/rules?format=json 用；字段与 YAML 一一对应，JSON 也能直接 PUT 回去）。
type (
	FileRule = fileRule
	File     = file
)

// ToFile 是 Marshal 之前的那一步（规则文件的结构）。
func (c *Config) ToFile() File {
	f := file{
		Default:      c.Default.String(),
		OfflineAllow: []string{},
		LocalDomains: append([]string{}, c.LocalDomains...),
		PinnedExtra:  append([]string{}, c.PinnedExtra...),
		BlockUDP443:  c.BlockUDP443,
		BypassExtra:  append([]string(nil), c.BypassExtra...),
		Rules:        []fileRule{},
	}
	for _, r := range c.Rules {
		f.Rules = append(f.Rules, fileRule{App: string(r.App), Action: r.Action.String()})
	}
	for _, a := range c.OfflineAllow {
		f.OfflineAllow = append(f.OfflineAllow, string(a))
	}
	return f
}

func invalid(item, reason string) error {
	return usererror.New("读取分流规则", item+"："+reason, "请修改规则后重试。", nil)
}

var (
	bundleRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*(\.[A-Za-z0-9_-]+)+$`)
	systemRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// NormalizeAppID 归一应用 ID：Bundle ID 一律转小写（procinfo 产出的也是小写），path: 和 system: 原样。
func NormalizeAppID(id AppID) AppID {
	s := string(id)
	if id == Unknown || strings.HasPrefix(s, "path:") || strings.HasPrefix(s, "system:") {
		return id
	}
	return AppID(strings.ToLower(s))
}

// ValidAppID 校验应用 ID 的形式。unknown 是合法的应用 ID，但不能写进规则（由 Parse 另判）。
func ValidAppID(id AppID) error {
	s := string(id)
	switch {
	case id == Unknown:
		return nil
	case strings.HasPrefix(s, "path:"):
		p := s[5:]
		if !strings.HasPrefix(p, "/") || len(p) > 1024 || path.Clean(p) != p || strings.ContainsAny(p, "\x00\n\r") {
			return errors.New("path: 后面要是规范的绝对路径")
		}
		return nil
	case strings.HasPrefix(s, "system:"):
		if !systemRe.MatchString(s[7:]) {
			return errors.New("system: 后面的名字不合法")
		}
		return nil
	case len(s) <= 255 && bundleRe.MatchString(s):
		return nil
	}
	return errors.New("应用 ID 只能是 Bundle ID、path:<绝对路径> 或 system:<名字>")
}

// Parse 解析并校验规则文件。任何一项不合法都整体拒绝，错误里指出是哪一项。
func Parse(b []byte) (*Config, error) {
	if len(b) > MaxConfigSize {
		return nil, invalid("规则文件", fmt.Sprintf("超过 %d KiB 的上限", MaxConfigSize>>10))
	}
	// 先解析成节点树（线性）并检查形状，再正式解码。yaml 库解码映射时查重复键是平方级的，
	// 一个映射里塞几万个键就能卡住几分钟（M3-1 模糊测试发现），所以先限住键数、条数和深度。
	var root yaml.Node
	pre := yaml.NewDecoder(bytes.NewReader(b))
	if err := pre.Decode(&root); err != nil && !errors.Is(err, io.EOF) {
		return nil, invalid("规则文件", "格式不对（"+yamlReason(err)+"）")
	}
	var extra yaml.Node
	if err := pre.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, invalid("规则文件", "只能有一个 YAML 文档")
	}
	if err := checkShape(&root, 0); err != nil {
		return nil, invalid("规则文件", err.Error())
	}
	var f file
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, invalid("规则文件", "格式不对或有不认识的字段（"+yamlReason(err)+"）")
	}
	return fromFile(f)
}

// 规则文件的形状上限：顶层 6 个字段，规则 2 个字段，深度最多 4 层（文档、顶层映射、列表、规则映射）。
const (
	maxDepth    = 6
	maxMapKeys  = 16
	maxSeqItems = 4096
)

func checkShape(n *yaml.Node, depth int) error {
	if depth > maxDepth {
		return errors.New("嵌套层数太多")
	}
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return errors.New("不支持 YAML 的锚点和别名")
	}
	switch n.Kind {
	case yaml.MappingNode:
		if len(n.Content) > 2*maxMapKeys {
			return fmt.Errorf("一个映射里最多 %d 个字段", maxMapKeys)
		}
	case yaml.SequenceNode:
		if len(n.Content) > maxSeqItems {
			return fmt.Errorf("一个列表最多 %d 项", maxSeqItems)
		}
	}
	for _, c := range n.Content {
		if err := checkShape(c, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// yamlReason 只取 yaml 报错里的行号和种类，不回显内容。
func yamlReason(err error) string {
	s := err.Error()
	if i := strings.Index(s, "line "); i >= 0 {
		j := i + 5
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if strings.Contains(s, "not found in type") {
			return "第 " + s[i+5:j] + " 行有不认识的字段"
		}
		return "第 " + s[i+5:j] + " 行"
	}
	return "无法解析"
}

func fromFile(f file) (*Config, error) {
	c := &Config{BlockUDP443: f.BlockUDP443}
	if f.Default == "" {
		c.Default = ActMain
	} else {
		a, err := ParseAction(f.Default)
		if err != nil {
			return nil, invalid("default", err.Error())
		}
		if a.Kind == Direct || (a.Kind == Proxy && a.Exit != ExitMain) {
			return nil, invalid("default", "只能是 proxy:main 或 block，不能设成分流出口或本机网络")
		}
		c.Default = a
	}
	seen := map[AppID]bool{}
	for i, r := range f.Rules {
		item := fmt.Sprintf("rules 第 %d 条", i+1)
		id := NormalizeAppID(AppID(r.App))
		if err := checkRuleApp(id); err != nil {
			return nil, invalid(item, err.Error())
		}
		if seen[id] {
			return nil, invalid(item, "同一个应用（"+r.App+"）只能有一条规则")
		}
		seen[id] = true
		a, err := ParseAction(r.Action)
		if err != nil {
			return nil, invalid(item, err.Error())
		}
		if a.Kind == Direct {
			if err := checkDirectApp(id); err != nil {
				return nil, invalid(item, err.Error())
			}
		}
		c.Rules = append(c.Rules, Rule{App: id, Action: a})
	}
	seenOff := map[AppID]bool{}
	for i, s := range f.OfflineAllow {
		item := fmt.Sprintf("offline_allow 第 %d 项", i+1)
		id := NormalizeAppID(AppID(s))
		if err := checkRuleApp(id); err != nil {
			return nil, invalid(item, err.Error())
		}
		if err := checkDirectApp(id); err != nil {
			return nil, invalid(item, err.Error())
		}
		if seenOff[id] {
			return nil, invalid(item, "重复")
		}
		seenOff[id] = true
		c.OfflineAllow = append(c.OfflineAllow, id)
	}
	var pinned []pattern
	pinned = append(pinned, builtinPinned...)
	for i, s := range f.PinnedExtra {
		p, err := parsePattern(s)
		if err != nil {
			return nil, invalid(fmt.Sprintf("pinned_extra 第 %d 项", i+1), "不是合法的域名或 *.域名")
		}
		pinned = append(pinned, p)
		c.pinned = append(c.pinned, p)
		c.PinnedExtra = append(c.PinnedExtra, p.String())
	}
	for i, s := range f.LocalDomains {
		item := fmt.Sprintf("local_domains 第 %d 项", i+1)
		p, err := parsePattern(s)
		if err != nil {
			return nil, invalid(item, "不是合法的域名或 *.域名")
		}
		for _, q := range pinned {
			if overlap(p, q) {
				return nil, invalid(item, "与置顶域名 "+q.String()+" 有重叠，Claude 相关的域名不能走本机网络")
			}
		}
		c.local = append(c.local, p)
		c.LocalDomains = append(c.LocalDomains, p.String())
	}
	seenBy := map[string]bool{}
	for i, s := range f.BypassExtra {
		item := fmt.Sprintf("bypass_extra 第 %d 项", i+1)
		p, err := parsePattern(s)
		if err != nil {
			return nil, invalid(item, "不是合法的域名或 *.域名")
		}
		if p.Wild && !strings.Contains(p.Base, ".") {
			return nil, invalid(item, "通配不能只写顶级域（如 *.com），要写到某个域名之下")
		}
		for _, q := range builtinPinned {
			if overlap(p, q) {
				return nil, invalid(item, "与置顶域名 "+q.String()+" 有重叠，Claude 相关的域名必须分配假地址、走主代理")
			}
		}
		if seenBy[p.String()] {
			return nil, invalid(item, "重复")
		}
		seenBy[p.String()] = true
		c.BypassExtra = append(c.BypassExtra, p.String())
	}
	return c, nil
}

// BypassPinnedConflict 报告不分配假地址的一个模式是否和置顶域名（内置的或这份规则里追加的）重叠，返回重叠的那个置顶模式
// （补齐三：新加进名单的模式和追加的置顶域名重叠时拒绝；已经在名单里、之后被加进置顶的，按置顶处理，fakedns 匹配时排除）。
func (c *Config) BypassPinnedConflict(s string) (string, bool) {
	p, err := parsePattern(s)
	if err != nil {
		return "", false
	}
	all := append(append([]pattern(nil), builtinPinned...), c.pinned...)
	for _, q := range all {
		if overlap(p, q) {
			return q.String(), true
		}
	}
	return "", false
}

func checkRuleApp(id AppID) error {
	if id == Unknown {
		return errors.New("unknown 表示识别不到的应用，只走 default，不能写进规则")
	}
	return ValidAppID(id)
}

func checkDirectApp(id AppID) error {
	if IsClaudeApp(id) {
		return errors.New("Claude 的应用不能走本机网络")
	}
	if IsProxyApp(id) {
		return errors.New("代理软件不能走本机网络（它转发的别的应用的流量会从本机 IP 出去）")
	}
	return nil
}

// Marshal 序列化成规则文件。Parse(Marshal(c)) 与 c 相等。
func (c *Config) Marshal() ([]byte, error) {
	f := c.ToFile()
	return yaml.Marshal(&f)
}

// IsLocal 判断归一后的名字是否命中 local_domains（fakedns 分配假地址时打“本地域名”标记用）。
func (c *Config) IsLocal(name string) bool {
	n, err := NormalizeName(name)
	if err != nil || c.pinnedNorm(n) {
		return false
	}
	for _, p := range c.local {
		if p.match(n) {
			return true
		}
	}
	return false
}

// pinnedNorm 判断归一后的名字是否命中置顶（内置加已解析的用户追加名单）。
func (c *Config) pinnedNorm(n string) bool {
	for _, p := range builtinPinned {
		if p.match(n) {
			return true
		}
	}
	for _, p := range c.pinned {
		if p.match(n) {
			return true
		}
	}
	return false
}

// isPinned 同 IsPinned，但用 Parse 时解析好的名单。
func (c *Config) isPinned(name string) (pinned, ok bool) {
	n, err := NormalizeName(name)
	if err != nil {
		return false, false
	}
	return c.pinnedNorm(n), true
}
