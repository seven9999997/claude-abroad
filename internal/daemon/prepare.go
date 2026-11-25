package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"claude-abroad/internal/claudecfg"
	"claude-abroad/internal/resolverlock"
)

// 首次准备（M6-8a，设计 10.2、11.4）：abroadd 第一次启动时写入 Claude 启动管控里的两份静态文件——
// Claude 域名解析锁（第一层）和 Claude Code 的托管设置（第四层）。之后回国、停用、启用、重启都不碰它们，
// 只有一键卸载才删（M6-5a）。建专用用户、建目录、注册后台服务这些要 root shell 的事由安装脚本做（M6-8b）。
//
//   - 只在 System.FirstRun 为真时做（真实模式由组装方打开；测试和假系统默认不做）；
//   - 已卸载（状态目录有 uninstalled 标记）时不做：卸载后重启 abroadd 不会把锁写回去；
//   - 两项都写成功（冲突不算失败：别人的文件不覆盖，状态和冲突检查里会写明）才写 prepared 标记，
//     否则下次启动再试；写过的再写一遍结果相同（两个 Write 都可重入）。
//   - 开发构建只管测试目标时（System.PrepareTestOnly），不写 Claude Code 的托管设置（不影响本机真实的 Claude Code）。

// PreparedFile 是“首次准备做完了”的标记（状态目录里）。
const PreparedFile = "prepared"

// PrepareStatus 是首次准备的结果（status.prepare）。
type PrepareStatus struct {
	Done  bool     `json:"done"`            // 做完了（有标记）
	Notes []string `json:"notes,omitempty"` // 这次启动时的说明：冲突、失败的原因（不含路径）
}

func (d *Daemon) preparedMarker() string { return filepath.Join(d.opts.StateDir, PreparedFile) }

// prepare 在恢复状态之前、解析锁自检之前调用。
func (d *Daemon) prepare() {
	if !d.opts.Sys.FirstRun {
		return
	}
	ps := &PrepareStatus{}
	d.prepareSt = ps
	if _, err := os.Stat(d.preparedMarker()); err == nil {
		ps.Done = true
		return
	}
	if d.uninstalled() {
		ps.Notes = append(ps.Notes, "已卸载，不再写入启动管控")
		return
	}
	ok := true
	if lk := d.opts.Sys.ResolverLock; lk != nil {
		res, err := lk.Write()
		switch {
		case errors.Is(err, resolverlock.ErrConflict):
			ps.Notes = append(ps.Notes, "Claude 域名解析锁：这些域名已有别人的设置，没有覆盖："+strings.Join(res.Conflicts, "、"))
		case err != nil:
			ok = false
			ps.Notes = append(ps.Notes, "写入 Claude 域名解析锁失败（下次启动重试）")
			d.events.Add("prepare", "resolver_lock_failed", err.Error())
		}
	}
	if cs := d.opts.Sys.ClaudeSettings; cs != nil && !d.opts.Sys.PrepareTestOnly {
		conf, err := cs.Write()
		switch {
		case errors.Is(err, claudecfg.ErrConflict):
			var parts []string
			for _, c := range conf {
				if c.Var != "" {
					parts = append(parts, c.File+" 的 "+c.Var)
				} else {
					parts = append(parts, c.File)
				}
			}
			ps.Notes = append(ps.Notes, "Claude Code 的托管设置：别的文件也定义了代理变量，没有写入："+strings.Join(parts, "、"))
		case err != nil:
			ok = false
			ps.Notes = append(ps.Notes, "写入 Claude Code 的托管设置失败（下次启动重试）")
			d.events.Add("prepare", "claude_settings_failed", err.Error())
		}
	}
	if !ok {
		return
	}
	if err := writeFileAtomic(d.preparedMarker(), []byte(time.Now().UTC().Format(time.RFC3339)+"\n")); err != nil {
		ps.Notes = append(ps.Notes, "记下首次准备完成失败（下次启动重试）")
		return
	}
	ps.Done = true
	d.events.Add("prepare", "done", strings.Join(ps.Notes, "；"))
}
