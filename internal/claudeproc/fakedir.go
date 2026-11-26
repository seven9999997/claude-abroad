package claudeproc

import (
	"path/filepath"
	"strings"

	"claude-abroad/internal/procinfo"
)

// 开发构建“只针对指定目录下的假进程”（M6-4b-1 返工，m6-plan 第 6.1 节）：真机脚本用开发构建验证第三层和回国结束
// Claude 时，判定改成“可执行文件在这个目录之下”，并且只针对这些进程；真实的判定（Bundle ID、签名）整个不用，本机真实的
// Claude 不受影响。只有开发构建的参数（cmd/abroadd 的 --dev-fake-claude-dir）会设它。

const fakeReason = "开发构建：指定目录下的测试进程"

// underDir 报告 p 在 dir 之下（都是绝对路径，按清理后的路径比较；dir 本身不算）。
func underDir(p, dir string) bool {
	if p == "" || dir == "" || !filepath.IsAbs(p) || !filepath.IsAbs(dir) {
		return false
	}
	d := filepath.Clean(dir) + string(filepath.Separator)
	return strings.HasPrefix(filepath.Clean(p), d)
}

type noProcs struct{}

func (noProcs) List() ([]procinfo.Proc, error) { return nil, nil }

// RestrictToDir 让回国结束 Claude（s）和第三层（g）都只针对 dir 下的进程；浏览器一律不列、不结束。s、g 可为 nil。
func RestrictToDir(s *Service, g *LaunchGuard, dir string) {
	if s != nil {
		s.Scanner.FakeDir = dir
		s.Browsers = &Browsers{Procs: noProcs{}}
	}
	if g != nil && g.Judge != nil {
		g.Judge.FakeDir = dir
	}
}
