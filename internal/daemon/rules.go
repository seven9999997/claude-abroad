package daemon

import (
	"claude-abroad/internal/api"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"claude-abroad/internal/rules"
	"claude-abroad/internal/state"
	"claude-abroad/internal/usererror"
)

// RulesFile 是分流规则文件（状态目录里，0600，原子写；m3-plan v2.8 决定 3）。
const RulesFile = "rules.yaml"

// RulesSummary 是 status 里的规则摘要（不含应用 ID、域名）。
type RulesSummary struct {
	Default      string `json:"default"`
	Direct       int    `json:"direct"`
	Proxy        int    `json:"proxy"`
	Block        int    `json:"block"`
	OfflineAllow int    `json:"offline_allow"`
	LocalDomains int    `json:"local_domains"`
	PinnedExtra  int    `json:"pinned_extra"`
	BlockUDP443  bool   `json:"block_udp_443"`
}

func summarize(c *rules.Config) *RulesSummary {
	s := &RulesSummary{Default: c.Default.String(), OfflineAllow: len(c.OfflineAllow), LocalDomains: len(c.LocalDomains),
		PinnedExtra: len(c.PinnedExtra), BlockUDP443: c.BlockUDP443}
	for _, r := range c.Rules {
		switch r.Action.Kind {
		case rules.Direct:
			s.Direct++
		case rules.Proxy:
			s.Proxy++
		case rules.Block:
			s.Block++
		}
	}
	return s
}

// DirectStatus 是 status 里直连拨号模块的情况。
type DirectStatus struct {
	Attached   bool `json:"attached"`   // 接上了直连拨号模块
	Configured bool `json:"configured"` // 最近一次下发配置成功
}

func (d *Daemon) rulesPath() string { return filepath.Join(d.opts.StateDir, RulesFile) }

// currentRules 返回当前生效的规则。
func (d *Daemon) currentRules() *rules.Config {
	d.rulesMu.Lock()
	defer d.rulesMu.Unlock()
	if d.rules == nil {
		return rules.DefaultConfig()
	}
	return d.rules
}

// loadRules 启动时读规则文件：不存在用默认配置；读不出来或校验不过用默认配置、发事件，不退出。
func (d *Daemon) loadRules() *rules.Config {
	b, err := os.ReadFile(d.rulesPath())
	if errors.Is(err, fs.ErrNotExist) {
		return rules.DefaultConfig()
	}
	if err == nil {
		var c *rules.Config
		if c, err = rules.Parse(b); err == nil {
			return c
		}
	}
	d.events.Add("rules", "rules_invalid", "规则文件读不出来或不合法，暂用默认规则（全部走主代理）："+usererror.Reason(err))
	return rules.DefaultConfig()
}

// writeFileAtomic 原子地写文件：同目录临时文件 → 0600 → fsync → rename → fsync 目录。
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".rules-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if df, err := os.Open(dir); err == nil {
		_ = df.Sync()
		df.Close()
	}
	return nil
}

// applyRules 换规则，顺序固定：写文件 → 替换网络栈的规则（含同步 fakedns、更新直连配置的代数）。
// 替换失败时规则文件写回旧内容（m3-plan v2.8 决定 3、v2.9：不再同步下发直连配置，直连拨号模块
// 下一次回 stale_config 时由客户端补发）。调用方持有状态机的忙标记。
func (d *Daemon) applyRules(next *rules.Config, drop bool) error {
	old := d.currentRules()
	nb, err := next.Marshal()
	if err != nil {
		return err
	}
	ob, _ := old.Marshal()
	write := d.writeRules
	if write == nil {
		write = writeFileAtomic
	}
	fail := func(step string, err error) error {
		return usererror.Wrap("更新分流规则（"+step+"）", err, "规则保持原样，请再试一次。")
	}
	if err := write(d.rulesPath(), nb); err != nil {
		return fail("写规则文件", err)
	}
	if dp := d.opts.Sys.Data; dp != nil {
		if err := dp.SetRules(next, drop); err != nil {
			if rerr := write(d.rulesPath(), ob); rerr != nil {
				d.events.Add("rules", "rules_file_restore_failed", "规则没有换成，但规则文件没能写回旧内容："+usererror.Reason(rerr))
				return usererror.Wrap("更新分流规则（替换网络栈的规则）", err,
					"当前仍按旧规则分流，但规则文件可能已是新内容（重启后会按新规则）。请重新提交一次规则。")
			}
			return fail("替换网络栈的规则", err)
		}
	}
	d.rulesMu.Lock()
	prev := d.rules
	d.rules = next
	d.rulesMu.Unlock()
	if d.claudeRouteKey(prev) != d.claudeRouteKey(next) {
		d.envKick() // Claude 的去向变了：立即补一轮环境检测（m4-plan v2.6 决定 4）
	}
	return nil
}

// handleGetRules 返回当前规则的 YAML。
func (d *Daemon) handleGetRules(w http.ResponseWriter, r *http.Request) {
	if _, err := io.ReadAll(r.Body); err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, errBody{"请求体超过 64 KiB"})
		return
	}
	cur := d.currentRules()
	w.Header().Set("ETag", `"`+RulesTag(cur)+`"`) // 条件写入的标识（补齐一）：两种格式同一个值
	switch r.URL.Query().Get("format") {
	case "", "yaml":
	case "json": // M5-9：应用用（字段与 YAML 一一对应；JSON 是 YAML 的子集，可以原样 PUT 回来）
		writeJSON(w, http.StatusOK, cur.ToFile())
		return
	default:
		writeErr(w, http.StatusBadRequest, api.ErrorBody{Error: "format 只能是 yaml 或 json", Field: "format"})
		return
	}
	b, err := cur.Marshal()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody{err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// handlePutRules 换规则：?drop_affected=1 时断开判定结果变了的在途连接。校验失败回 422；
// 与启用、停用、恢复互斥，冲突回 409。敏感操作：M5 之前只有状态目录的属主能调用（本地接口已经校验对端 uid）。
func (d *Daemon) handlePutRules(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, errBody{"请求体超过 64 KiB"})
		return
	}
	// 条件写入（补齐一，architect 2026-11-27）：带了 If-Match 且和当前规则的标识不一致 → 412 rules_changed，什么都不改；
	// 在解析、分级、授权之前判断（旧文档里的放松不能因为带了 sudo 就把别人刚做的改动冲掉）。不带时行为照旧。
	ifMatch, conditional := rulesIfMatch(r)
	if conditional && ifMatch != RulesTag(d.currentRules()) {
		writeRulesChanged(w)
		return
	}
	drop := false
	switch r.URL.Query().Get("drop_affected") {
	case "", "0":
	case "1":
		drop = true
	default:
		writeJSON(w, http.StatusBadRequest, errBody{"drop_affected 只能是 0 或 1"})
		return
	}
	next, err := rules.Parse(body)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, errBody{err.Error()})
		return
	}
	// 补齐三：新加进“不分配假地址”名单的模式不能和置顶域名（内置的、这份规则里追加的）重叠。已经在名单里、之后被加进
	// 置顶的不拒绝，按置顶处理（fakedns 匹配时排除）。
	cur := d.currentRules()
	for i, p := range next.BypassExtra {
		if slices.Contains(cur.BypassExtra, p) {
			continue
		}
		if q, bad := next.BypassPinnedConflict(p); bad {
			writeErr(w, http.StatusUnprocessableEntity, api.ErrorBody{Field: fmt.Sprintf("bypass_extra[%d]", i),
				Error: usererror.New("改规则", "bypass_extra 第 "+strconv.Itoa(i+1)+" 项与置顶域名 "+q+" 有重叠，置顶的域名必须分配假地址、走主代理", "请去掉这一项后重试。", nil).Error()})
			return
		}
	}
	// 按内容分级（m5-plan v2.1 决定 3）：放松保护的改动是敏感。
	if lv, why := api.RulesLevel(d.currentRules(), next); lv == api.Sensitive && !requireSensitive(w, r, strings.Join(why, "；")) {
		return
	}
	if err := d.m.Exclusive(func() error {
		// 规则引用的分流出口都要已配置（m4-plan v2.11 决定 2）：在忙标记里查，不会和出口的删除交叉。
		if conditional && ifMatch != RulesTag(d.currentRules()) { // 检查和写入之间被别的写入抢先
			return errRulesChanged
		}
		if err := UnknownExitRefs(next, d.exitKnown); err != nil {
			return errUnknownExit{err}
		}
		return d.applyRules(next, drop)
	}); err != nil {
		if errors.Is(err, errRulesChanged) {
			writeRulesChanged(w)
			return
		}
		code := http.StatusInternalServerError
		var ue errUnknownExit
		switch {
		case errors.Is(err, state.ErrBusy):
			code = http.StatusConflict
		case errors.As(err, &ue):
			code = http.StatusUnprocessableEntity
		}
		b := api.ErrorBody{Error: err.Error()}
		var fe FieldError
		if errors.As(err, &fe) {
			b.Field = fe.Field
		}
		if errors.Is(err, state.ErrBusy) {
			b.Code = api.CodeBusy
		}
		writeErr(w, code, b)
		return
	}
	writeJSON(w, http.StatusOK, d.Status())
}

// rulesStatus 填 status 里和 M3 有关的部分。
func (d *Daemon) rulesStatus(st *Status) {
	st.Rules = summarize(d.currentRules())
	if dp := d.opts.Sys.Data; dp != nil {
		s := dp.Stats()
		st.Stack = &s
		a, c := dp.DirectStatus()
		st.Direct = &DirectStatus{Attached: a, Configured: c}
		st.Opens = dp.OpenCounts()
	}
	st.Exits = d.exitStatuses()
}

// wireRules 在启动时读规则、接到数据面，并接上直连拨号模块配置里的本机 DNS（从状态文件读）。
func (d *Daemon) wireRules() {
	c := d.loadRules()
	d.rulesMu.Lock()
	d.rules = c
	d.rulesMu.Unlock()
	dp := d.opts.Sys.Data
	if dp == nil {
		return
	}
	dp.origDNS = func() []netip.Addr {
		f, err := d.store.Load()
		if err != nil {
			return nil
		}
		return f.OrigDNS
	}
	_ = dp.SetRules(c, false)
}

// RulesTag 是规则内容的标识（条件写入用，补齐一）：规范化之后（Marshal 的 YAML）的 SHA-256 前 16 位十六进制。不另存状态，
// abroadd 重启后不变；YAML、JSON 两种读法同一个值；同样的内容提交两次不变。
func RulesTag(c *rules.Config) string {
	b, err := c.Marshal()
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// rulesIfMatch 取 If-Match（去掉引号和弱标记）；没有这个头时 ok 为假。
func rulesIfMatch(r *http.Request) (tag string, ok bool) {
	v := strings.TrimSpace(r.Header.Get("If-Match"))
	if v == "" {
		return "", false
	}
	v = strings.TrimPrefix(v, "W/")
	return strings.Trim(v, `"`), true
}

var errRulesChanged = errors.New("规则在读取之后被改过")

// RulesChangedText 是条件写入冲突时的说明。
const RulesChangedText = "规则在你读取之后被别处改过，这次没有改动。请重新读取后再改。"

func writeRulesChanged(w http.ResponseWriter) {
	writeErr(w, http.StatusPreconditionFailed, api.ErrorBody{Code: api.CodeRulesChanged, Error: RulesChangedText})
}
