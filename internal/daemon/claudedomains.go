package daemon

import (
	"context"
	"io"
	"net/http"
	"time"

	"claude-abroad/internal/claudedomains"
	"claude-abroad/internal/usererror"
)

// Claude 访问的域名（M6-12，设计 10.2 的 M6-12 段）：已启用时网络栈对识别为 Claude 的连接，把不在内置置顶后缀之下的
// 域名交过来；这里记到状态目录的 claude-domains.json（0600）。只观察，不自动改名单——是不是 Anthropic 自有由人看过
// 证据后改源码追加。新域名第一次出现时落盘，其余每分钟和退出时落盘。

// ClaudeDomainsFlushInterval 是定时落盘的间隔。
const ClaudeDomainsFlushInterval = time.Minute

// ClaudeDomainsNote 是给用户的说明（接口、命令行、应用共用）。
const ClaudeDomainsNote = "启用时 Claude（桌面版、Claude Code）访问过的、不在内置 Claude 域名之下的域名，只是线索：是不是 Anthropic 的由维护者看过证据后追加进内置名单。Claude Code 会替你抓取网页，所以这里也有你让它访问过的网站；只存在本机，诊断包默认不带清单。"

// ClaudeDomainsView 是 GET /v1/claude-domains 的应答。
type ClaudeDomainsView struct {
	Count   int                   `json:"count"`
	Domains []claudedomains.Entry `json:"domains"`
	Note    string                `json:"note"`
}

func (d *Daemon) startClaudeDomains() {
	dp := d.opts.Sys.Data
	if dp == nil || dp.proxy == nil {
		return
	}
	st := &claudedomains.Store{Dir: d.opts.StateDir}
	d.domains = st
	dp.proxy.SetClaudeDomainObserver(func(name string) {
		if st.Record(name) {
			_ = st.Flush() // 新域名：马上落盘
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	d.cdStop, d.cdDone = cancel, done
	go func() {
		defer close(done)
		t := time.NewTicker(ClaudeDomainsFlushInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = st.Flush()
				return
			case <-t.C:
				_ = st.Flush()
			}
		}
	}()
}

func (d *Daemon) stopClaudeDomains() {
	if d.cdStop == nil {
		return
	}
	if dp := d.opts.Sys.Data; dp != nil && dp.proxy != nil {
		dp.proxy.SetClaudeDomainObserver(nil)
	}
	d.cdStop()
	<-d.cdDone
	d.cdStop = nil
}

func (d *Daemon) claudeDomainsView() ClaudeDomainsView {
	v := ClaudeDomainsView{Domains: []claudedomains.Entry{}, Note: ClaudeDomainsNote}
	if d.domains != nil {
		v.Domains = d.domains.List()
		v.Count = len(v.Domains)
	}
	return v
}

// handleClaudeDomains 是 GET /v1/claude-domains（本机、本人可看）。
func (d *Daemon) handleClaudeDomains(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
	writeJSON(w, http.StatusOK, d.claudeDomainsView())
}

// handleClaudeDomainsClear 是 DELETE /v1/claude-domains：清空（内存和文件）。
func (d *Daemon) handleClaudeDomainsClear(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
	if d.domains != nil {
		if err := d.domains.Clear(); err != nil {
			writeJSON(w, http.StatusInternalServerError, errBody{usererror.Wrap("清空 Claude 访问过的域名", err, "请再试一次。").Error()})
			return
		}
	}
	d.events.Add("claude_domains", "cleared", "")
	writeJSON(w, http.StatusOK, d.claudeDomainsView())
}
