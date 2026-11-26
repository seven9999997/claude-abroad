package pf

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
)

// Runner 执行一次 pfctl（args 不含程序名）。真实实现是 exec.go 的 ExecRunner（只调 /sbin/pfctl）；
// 测试用 FakePF。pfctl 有些信息（例如 -E 的 token）打在 stderr，所以两路都要。
type Runner interface {
	Run(ctx context.Context, args []string, stdin []byte) (stdout, stderr []byte, err error)
}

// 错误。
var (
	ErrBadAnchor           = errors.New("锚点名不合法")
	ErrBadToken            = errors.New("启用引用 token 不合法")
	ErrNoToken             = errors.New("pfctl -E 的输出里没有 token")
	ErrVerifyFailed        = errors.New("加载后锚点里的规则与期望不一致")
	ErrAnchorNotReferenced = errors.New("主规则集不再引用 com.apple/* 锚点（我们的规则不会被求值）")
	// ErrTokenInvalid 是 pfctl -X 报 token 无效（已经释放过，或别的软件 pfctl -d 让它作废了）。
	// 调用方按“已释放”处理。真机输出待 M2-9 采样核对。
	ErrTokenInvalid = errors.New("启用引用 token 已失效")
)

var (
	anchorRe = regexp.MustCompile(`^com\.apple/[0-9]{3}\.[a-z0-9]+$`)
	tokenRe  = regexp.MustCompile(`^[0-9]+$`)
	tokenOut = regexp.MustCompile(`Token\s*:\s*(\d+)`)
)

// DefaultAnchor 是默认锚点（设计 6.1）。R2 之后可能改成 100.abroad，所以 Controller 里是字段。
const DefaultAnchor = "com.apple/250.abroad"

// Status 是 pf 的当前状态（原始文本，比对经 Matcher）。
type Status struct {
	Enabled     bool   // pf 已启用
	Referenced  bool   // 主规则集仍引用 com.apple/* 锚点
	AnchorRules []byte // pfctl -a <锚点> -s rules 的原始输出
	Table       []byte // pfctl -a <锚点> -t abroad_lan -T show 的原始输出；表不存在或查询出错时为空
}

// Matcher 判断锚点里的规则是否与期望一致（不写死 pfctl 的打印格式；默认实现见 matcher.go）。
type Matcher interface {
	// Present 报告锚点里有没有规则。
	Present(st Status) bool
	// Same 报告锚点里的规则和表是否与 expected（Render 的输出）一致。
	Same(expected []byte, st Status) bool
}

// Controller 拼 pfctl 的参数并解析输出。它不判断“该不该”做，只负责做（顺序由调用方、Ensure 决定）。
type Controller struct {
	R       Runner
	Anchor  string // 为空时用 DefaultAnchor
	Matcher Matcher
	// BeforeEnable 在真正调用 -E 之前调用（可为 nil）。组装时接到状态文件的“将要启用”标记落盘：
	// 崩溃在 -E 成功与 token 落盘之间时，恢复路径能知道可能漏了一个启用引用。返回错误就不调用 -E。
	BeforeEnable func() error
}

func (c *Controller) anchor() (string, error) {
	a := c.Anchor
	if a == "" {
		a = DefaultAnchor
	}
	if !anchorRe.MatchString(a) {
		return "", ErrBadAnchor
	}
	return a, nil
}

func (c *Controller) run(ctx context.Context, stdin []byte, args ...string) ([]byte, []byte, error) {
	if c.R == nil {
		return nil, nil, errors.New("没有配置 pfctl 执行器")
	}
	return c.R.Run(ctx, args, stdin)
}

func cmdErr(op string, stderr []byte, err error) error {
	msg := strings.TrimSpace(string(stderr))
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return fmt.Errorf("pfctl %s 失败：%w（%s）", op, err, msg)
}

// LoadArgs 是加载锚点规则的参数：关闭规则集优化器（-o none，否则 pfctl 会合并、重排规则，加载的规则和
// Render 的文本不再逐行对应），规则经 stdin（M2-6b 实测 -n -f - 可用）。
func LoadArgs(anchor string) []string { return []string{"-o", "none", "-a", anchor, "-f", "-"} }

// Load 把 rules 加载到锚点（整体替换锚点内容）。
func (c *Controller) Load(ctx context.Context, rules []byte) error {
	a, err := c.anchor()
	if err != nil {
		return err
	}
	if _, se, err := c.run(ctx, rules, LoadArgs(a)...); err != nil {
		return cmdErr("加载锚点", se, err)
	}
	return nil
}

// Flush 清空锚点里的规则和表（卸载、恢复时用）。
func (c *Controller) Flush(ctx context.Context) error {
	a, err := c.anchor()
	if err != nil {
		return err
	}
	if _, se, err := c.run(ctx, nil, "-a", a, "-F", "all"); err != nil {
		return cmdErr("清空锚点", se, err)
	}
	return nil
}

// Enable 取得一个启用引用（pfctl -E），返回 token。token 可能打在 stdout 或 stderr。
func (c *Controller) Enable(ctx context.Context) (string, error) {
	if c.BeforeEnable != nil {
		if err := c.BeforeEnable(); err != nil {
			return "", err
		}
	}
	so, se, err := c.run(ctx, nil, "-E")
	if err != nil {
		return "", cmdErr("启用", se, err)
	}
	return parseToken(append(append([]byte{}, so...), se...))
}

func parseToken(out []byte) (string, error) {
	m := tokenOut.FindSubmatch(out)
	if m == nil {
		return "", ErrNoToken
	}
	return string(m[1]), nil
}

// Release 释放一个启用引用（pfctl -X <token>）。
func (c *Controller) Release(ctx context.Context, token string) error {
	if !tokenRe.MatchString(token) {
		return ErrBadToken
	}
	if _, se, err := c.run(ctx, nil, "-X", token); err != nil {
		if bytes.Contains(bytes.ToLower(se), []byte("invalid token")) {
			return fmt.Errorf("%w：%w", ErrTokenInvalid, cmdErr("释放启用引用", se, err))
		}
		return cmdErr("释放启用引用", se, err)
	}
	return nil
}

// KillStates 清掉所有状态（pfctl -F states）：加载规则之前就有的状态会让旧连接继续走（设计 6.1）。
func (c *Controller) KillStates(ctx context.Context) error {
	if _, se, err := c.run(ctx, nil, "-F", "states"); err != nil {
		return cmdErr("清状态", se, err)
	}
	return nil
}

// KillStatesToArgs 是“只清掉目的地在 dst 里的状态”的 pfctl 参数（M6-11b，architect 2026-11-27）：
// `-k <同族的任意地址> -k <dst>`。macOS 上的确切效果待真机验证（scripts/m6 的核对步骤），验证之前默认不用。
func KillStatesToArgs(dst netip.Prefix) []string {
	all := "0.0.0.0/0"
	if dst.Addr().Is6() {
		all = "::/0"
	}
	return []string{"-k", all, "-k", dst.Masked().String()}
}

// KillStatesTo 只清掉目的地在 dst 里的状态（不动别的连接）。
func (c *Controller) KillStatesTo(ctx context.Context, dst netip.Prefix) error {
	if _, se, err := c.run(ctx, nil, KillStatesToArgs(dst)...); err != nil {
		return cmdErr("清掉到回国拦截地址段的状态", se, err)
	}
	return nil
}

// referencesAnchor 报告主规则集是否对 com.apple/* 锚点做过滤求值：按行判断，某一行去掉首尾空白后以
// `anchor "com.apple/*"` 开头才算。macOS 默认的 `scrub-anchor "com.apple/*" …` 也包含这个子串，但它不求值
// 过滤规则；只留 scrub-anchor 时保护已经失效。
func referencesAnchor(main []byte) bool {
	for _, l := range bytes.Split(main, []byte("\n")) {
		if bytes.HasPrefix(bytes.TrimSpace(l), []byte(`anchor "com.apple/*"`)) {
			return true
		}
	}
	return false
}

// Status 读取 pf 的当前状态。表查询出错按“表不存在”处理。
func (c *Controller) Status(ctx context.Context) (Status, error) {
	a, err := c.anchor()
	if err != nil {
		return Status{}, err
	}
	var st Status
	info, se, err := c.run(ctx, nil, "-s", "info")
	if err != nil {
		return st, cmdErr("读状态", se, err)
	}
	first, _, _ := bytes.Cut(info, []byte("\n"))
	st.Enabled = bytes.Contains(first, []byte("Status: Enabled"))
	main, se, err := c.run(ctx, nil, "-s", "rules")
	if err != nil {
		return st, cmdErr("读主规则集", se, err)
	}
	st.Referenced = referencesAnchor(main)
	if st.AnchorRules, se, err = c.run(ctx, nil, "-a", a, "-s", "rules"); err != nil {
		return st, cmdErr("读锚点规则", se, err)
	}
	if tb, _, err := c.run(ctx, nil, "-a", a, "-t", "abroad_lan", "-T", "show"); err == nil {
		st.Table = tb
	}
	return st, nil
}
