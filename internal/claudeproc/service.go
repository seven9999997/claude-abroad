package claudeproc

import (
	"context"
	"errors"

	"claude-abroad/internal/usererror"
)

// Preview 是给确认框看的内容（m4-plan v2.3 决定 7）：应用名、进程号、启动命令。
type Preview struct {
	Processes  []Target `json:"processes"`
	Unverified []Target `json:"unverified,omitempty"` // 看起来像 Claude 但没能确认、不会结束的
	WillKill   bool     `json:"will_kill"`
	Notice     string   `json:"notice,omitempty"`
	// M6-4b-3：正在运行的浏览器（每个浏览器一行；确认不了的带 Note）。默认不结束，用户显式选择时才结束确认过的。
	Browsers    []Target `json:"browsers,omitempty"`
	BrowserNote string   `json:"browser_note,omitempty"`
}

// Service 是 daemon 用的“预览 + 结束”（实现 state.Killer）。三种实现（决定 6）：
//   - 真实：Killer 带真实的信号发送器（NewReal，只在 cmd/abroadd 真实模式入口调用）；
//   - 不结束任何进程：Skip 为 true（--dev-skip-claude-kill），预览照样如实列出，每次结束时打醒目日志；
//   - 假：Killer 带假的信号发送器（假系统和测试）。
//
// 日志只记数量，不记命令行。
type Service struct {
	Scanner  *Scanner
	Killer   *Killer   // Skip 时可为 nil
	Browsers *Browsers // 回国时的浏览器（可为 nil）
	Skip     bool
	Logf     func(format string, args ...any)
}

// SkipNotice 是不结束进程时预览里的说明。
const SkipNotice = "开发模式：本次不会结束这些进程"

func (s *Service) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Preview 扫描一次，给确认框看。
func (s *Service) Preview(context.Context) (Preview, error) {
	rep, err := s.Scanner.Scan()
	if err != nil {
		return Preview{}, err
	}
	p := Preview{Processes: rep.Targets, Unverified: rep.Unverified, WillKill: !s.Skip}
	if p.Processes == nil {
		p.Processes = []Target{}
	}
	if s.Skip {
		p.Notice = SkipNotice
	}
	if bs, err := s.Browsers.Preview(); err == nil && len(bs) > 0 {
		p.Browsers, p.BrowserNote = bs, BrowserNote
	}
	return p, nil
}

// KillClaude 实现 state.Killer：结束全部 Claude 进程并确认已退出；结束不了时返回 *LeftError（带数量）。
func (s *Service) KillClaude(ctx context.Context) error {
	if s.Skip {
		s.logf("!!!!!!!! 开发模式：没有结束 Claude 进程（只有开发构建能这样运行）!!!!!!!!")
		return nil
	}
	if s.Killer == nil {
		return errors.New("没有配置结束进程的实现")
	}
	left, err := s.Killer.Kill(ctx)
	if err != nil {
		s.logf("结束 Claude 进程失败：残留 %d 个", len(left))
		var le *LeftError
		if errors.As(err, &le) {
			return usererror.New("结束 Claude 进程", le.Error(), "本机网络没有改动。请手动退出这些进程后再试。", err)
		}
		return err
	}
	s.logf("Claude 进程已全部结束")
	if KillBrowsersRequested(ctx) && s.Browsers != nil {
		// 用户显式选择：结束确认过的浏览器。结束不了的只记日志，不中止回国（浏览器不是不泄漏的依据，网络恢复靠的是 Claude 进程已结束）。
		left, err := s.Browsers.Kill(ctx, s.Killer)
		if err != nil || len(left) > 0 {
			s.logf("结束浏览器：残留 %d 个", len(left))
		} else {
			s.logf("确认过的浏览器已结束")
		}
	}
	return nil
}
