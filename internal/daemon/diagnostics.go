package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"runtime"
	"runtime/debug"
	"time"

	"claude-abroad/internal/claudedomains"
	"claude-abroad/internal/envcheck"
	"claude-abroad/internal/redact"
)

// 设备体检和诊断包（M6-6a）。

// handleDeviceCheck 是 POST /v1/envcheck/device：扫描字体目录，只返回结果（不参与放行、不关闸门）。
func (d *Daemon) handleDeviceCheck(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
	writeJSON(w, http.StatusOK, envcheck.RunDevice(d.opts.Sys.DeviceRoot, d.opts.Sys.DeviceHome))
}

// Diagnostics 是诊断包（脱敏之后）：版本、状态、最近的环境检测、启动管控、引擎状态、事件。
// 整段 JSON 生成后再经 redact 统一脱敏一遍（出口 IP、订阅地址、节点地址、口令一类不会出现在里面）。
type Diagnostics struct {
	GeneratedAt time.Time         `json:"generated_at"`
	Version     DiagnosticVersion `json:"version"`
	Status      Status            `json:"status"`
	EnvCheck    *envcheck.Report  `json:"envcheck,omitempty"`
	LaunchGuard LaunchGuardView   `json:"launchguard"`
	// ClaudeDomains 是 Claude 访问过的域名（M6-12）：默认只放条数；用户显式选择（?include_domains=1）才放清单——
	// 里面会有用户让 Claude Code 访问过的网站。
	ClaudeDomains DiagnosticDomains `json:"claude_domains"`
}

// DiagnosticDomains 是诊断包里的 Claude 访问域名。
type DiagnosticDomains struct {
	Count   int                   `json:"count"`
	Domains []claudedomains.Entry `json:"domains,omitempty"`
}

// DiagnosticVersion 是程序的版本信息。
type DiagnosticVersion struct {
	Module   string `json:"module"`
	Version  string `json:"version"`
	Revision string `json:"revision,omitempty"`
	Go       string `json:"go"`
	OS       string `json:"os"`
}

func buildVersion() DiagnosticVersion {
	v := DiagnosticVersion{Go: runtime.Version(), OS: runtime.GOOS + "/" + runtime.GOARCH}
	if bi, ok := debug.ReadBuildInfo(); ok {
		v.Module, v.Version = bi.Main.Path, bi.Main.Version
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				v.Revision = s.Value
			}
		}
	}
	return v
}

func (d *Daemon) diagnostics(includeDomains bool) Diagnostics {
	dg := Diagnostics{GeneratedAt: time.Now().UTC(), Version: buildVersion(), Status: d.Status(), LaunchGuard: d.launchGuardView()}
	if d.env != nil {
		if rep, ok := d.env.Last(); ok {
			dg.EnvCheck = &rep
		}
	}
	cd := d.claudeDomainsView()
	dg.ClaudeDomains.Count = cd.Count
	if includeDomains {
		dg.ClaudeDomains.Domains = cd.Domains
	} else if cc := dg.Status.ClaudeCode; cc != nil && len(cc.Recent) > 0 {
		// 本机代理端口的最近连接里也有目的域名：默认去掉（程序名和去向留着），和清单同样的规则（M6-12）
		c := *cc
		c.Recent = append([]ClaudeProxyConn(nil), cc.Recent...)
		for i := range c.Recent {
			c.Recent[i].Host = ""
		}
		dg.Status.ClaudeCode = &c
	}
	return dg
}

// handleDiagnostics 是 GET /v1/diagnostics：整段脱敏后的诊断包（JSON）。命令行和应用把它存成文件交给用户。
func (d *Daemon) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	raw, err := json.MarshalIndent(d.diagnostics(r.URL.Query().Get("include_domains") == "1"), "", "  ")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody{"生成诊断包失败"})
		return
	}
	out := redact.String(string(raw))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, out)
}
