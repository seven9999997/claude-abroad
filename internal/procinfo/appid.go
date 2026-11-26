package procinfo

import (
	"path"
	"regexp"
	"strings"

	"claude-abroad/internal/rules"
)

// SharedSystem 是“被很多应用共用”的系统进程和一句说明（补齐四，architect 2026-11-27；设计 5.1）：界面把它们单列、旁边显示说明，
// 给它们设规则之前提醒“改它会同时影响那些应用”。只放在开发用的 Mac 上实际看得到、用途明确的（2026-11-27 只读查看进程表）：
//   - nsurlsessiond   /usr/libexec/nsurlsessiond
//   - apsd            /System/Library/PrivateFrameworks/ApplePushService.framework/apsd
//   - cloudd          /System/Library/PrivateFrameworks/CloudKitDaemon.framework/Support/cloudd
//   - mDNSResponder   /usr/sbin/mDNSResponder
//   - trustd          /usr/libexec/trustd
//   - timed           /usr/libexec/timed
//
// 拿不准的不放（akd、identityservicesd、softwareupdated 等）。这不是“系统服务直连”的预设（那个等 R7）。
var SharedSystem = map[string]string{
	"system:nsurlsessiond": "替很多应用做后台下载和上传（App Store、iCloud、各种应用的后台传输）",
	"system:apsd":          "推送服务：所有应用的推送通知都经它",
	"system:cloudd":        "iCloud 同步：替很多应用同步 iCloud 里的数据",
	"system:mDNSResponder": "系统的域名解析和局域网设备发现：所有应用的域名查询都经它",
	"system:trustd":        "证书检查：所有应用的 HTTPS 证书吊销检查都经它",
	"system:timed":         "时间同步：系统时钟靠它校准",
}

// 应用的种类（补齐四：界面单列系统进程）。
const (
	KindApp     = "app"     // 普通应用（Bundle ID）
	KindSystem  = "system"  // 系统进程（system:）
	KindPath    = "path"    // 按路径识别的程序（path:）
	KindUnknown = "unknown" // 识别不出的
)

// AppKind 是应用 ID 的种类（由应用 ID 的形式决定，和识别规则同一处）。
func AppKind(id rules.AppID) string {
	s := string(id)
	switch {
	case id == rules.Unknown || s == "":
		return KindUnknown
	case strings.HasPrefix(s, "system:"):
		return KindSystem
	case strings.HasPrefix(s, "path:"):
		return KindPath
	}
	return KindApp
}

// systemNames 是 design 5.1 列出的共享或系统进程（按可执行文件名）。
var systemNames = map[string]bool{
	"mDNSResponder":               true,
	"nsurlsessiond":               true,
	"apsd":                        true,
	"com.apple.WebKit.Networking": true,
	"trustd":                      true,
	"timed":                       true,
	"softwareupdated":             true,
}

// systemPrefixes 下、又不在 .app 包里的可执行文件归为系统进程。
var systemPrefixes = []string{"/System/", "/usr/libexec/", "/usr/sbin/", "/sbin/"}

// TerminalApps 是“继承父进程”时停下来的终端类应用（Bundle ID，小写）。
var TerminalApps = map[rules.AppID]bool{
	"com.apple.terminal":            true,
	"com.googlecode.iterm2":         true,
	"dev.warp.warp-stable":          true,
	"dev.warp.warp":                 true,
	"net.kovidgoyal.kitty":          true,
	"org.alacritty":                 true,
	"io.alacritty":                  true,
	"com.github.wez.wezterm":        true,
	"com.microsoft.vscode":          true, // VS Code 的集成终端
	"com.microsoft.vscodeinsiders":  true,
	"com.vscodium":                  true,
	"com.todesktop.230313mzl4w4u92": true, // Cursor
	"com.exafunction.windsurf":      true,
}

var bundleIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*(\.[a-z0-9_-]+)+$`)

// BundleReader 读应用包的 Bundle ID（appPath 是 .app 目录的绝对路径）。
type BundleReader func(appPath string) (string, error)

// outerApp 返回路径里最外层的 .app 目录；不在应用包里返回空。
func outerApp(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		if i > 0 && strings.HasSuffix(s, ".app") && len(s) > len(".app") {
			return strings.Join(parts[:i+1], "/")
		}
	}
	return ""
}

// AppIDForPath 把可执行文件路径映射成应用 ID（design 5.1）。纯函数（Bundle ID 经 bundle 读取）。
//   - 在 .app 包里：最外层 .app 的 Bundle ID（小写、校验字符集）；读不出来或不合法 → unknown，不退回成 path:；
//   - /System/、/usr/libexec/、/usr/sbin/、/sbin/ 下，或在系统进程名单里 → system:<文件名>；
//   - 其余 → path:<清理后的绝对路径>。
func AppIDForPath(p string, bundle BundleReader) rules.AppID {
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\x00\n\r") {
		return rules.Unknown
	}
	p = path.Clean(p)
	if app := outerApp(p); app != "" {
		if bundle == nil {
			return rules.Unknown
		}
		id, err := bundle(app)
		if err != nil {
			return rules.Unknown
		}
		id = strings.ToLower(strings.TrimSpace(id))
		if len(id) > 255 || !bundleIDRe.MatchString(id) {
			return rules.Unknown
		}
		return rules.AppID(id)
	}
	base := path.Base(p)
	if systemNames[base] {
		return rules.AppID("system:" + base)
	}
	for _, pre := range systemPrefixes {
		if strings.HasPrefix(p, pre) {
			if rules.ValidAppID(rules.AppID("system:"+base)) != nil {
				return rules.Unknown
			}
			return rules.AppID("system:" + base)
		}
	}
	id := rules.AppID("path:" + p)
	if rules.ValidAppID(id) != nil {
		return rules.Unknown
	}
	return id
}
