package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
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
	// EnvHistory 是环境检测的历史（补齐四：只有时间、总判定、各项的标识和结果）。
	EnvHistory []envcheck.RunEntry `json:"envcheck_history,omitempty"`
	// EnableFailure 是最近一次启用失败（出口检查没通过、回滚之前）的现场快照（只在内存里；只有接口名和计数）。
	EnableFailure *EnableSnapshot `json:"enable_failure_snapshot,omitempty"`
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
	dg.EnvHistory = d.envHist.List()
	dg.EnableFailure = d.enableSnap.Load()
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
	out, err := renderDiagnostics(d.diagnostics(r.URL.Query().Get("include_domains") == "1"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody{"生成诊断包失败"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, out)
}

// renderDiagnostics 把诊断包写成文本并做诊断包专用的处理（M6-6a 补审返工）：
//   - 网址整段替换成 DiagURLPlaceholder（连主机名一起；通用的 redact 只去口令和参数，这里更严）；
//   - 时间一律换成 UTC（不带本机时区）；
//   - 最后再经 redact 统一脱敏一遍。
func renderDiagnostics(dg Diagnostics) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // & 不转义成 \u0026，网址才能整段认出来
	enc.SetIndent("", "  ")
	if err := enc.Encode(dg); err != nil {
		return "", err
	}
	s := diagURLRe.ReplaceAllString(buf.String(), DiagURLPlaceholder)
	s = diagTimeRe.ReplaceAllStringFunc(s, func(m string) string {
		t, err := time.Parse(time.RFC3339Nano, m)
		if err != nil {
			return m
		}
		return t.UTC().Format(time.RFC3339Nano)
	})
	return redact.String(s), nil
}

// DiagURLPlaceholder 是诊断包里网址的替换文字。
const DiagURLPlaceholder = "[网址已去掉]"

var (
	// 任何“协议://”开头的一段，到空白、引号或反斜杠（JSON 转义）为止。
	diagURLRe = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.\-]*://[^\s"\\]*`)
	// RFC 3339 时间（带时区偏移）。
	diagTimeRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?([+-]\d{2}:\d{2}|Z)`)
)
