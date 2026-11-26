package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"claude-abroad/internal/claudecfg"
	"claude-abroad/internal/resolverlock"
	"claude-abroad/internal/state"
)

// 首次准备（M6-8a，设计 10.2、11.4）：abroadd 写入 Claude 启动管控里的两份静态文件——Claude 域名解析锁（第一层）和
// Claude Code 的托管设置（第四层）。写完之后回国、停用、启用、重启都不碰它们，只有一键卸载才删（M6-5a）。
// 建专用用户、建目录、注册后台服务这些要 root shell 的事由安装脚本做（M6-8b）。
//
//   - 只在 System.FirstRun 为真时做（abroadd 带 --launch-control；测试和假系统默认不做）；
//   - 已卸载（状态目录有 uninstalled 标记）时不做：卸载后重启 abroadd 不会把锁写回去；
//   - M6-8a 补审返工：**冲突不算完成**——别人的同名文件不覆盖，但不写 prepared 标记、done 为假，说明里写清是哪个
//     文件和下一步；没完成时冲突检查里有一条“必须处理”，阻止启用（管控是“回国后连不上”的前提）；
//   - 没完成时重试：启动时、每次冲突检查（启用前、用户点“重新检测”）时再试一次；两个 Write 都可重入，一部分写成功、
//     另一部分冲突时已写的留着，重试时不重复写；
//   - 完成之后才出现的冲突（别人后来写了文件）不回退完成标记：自检报告，解析锁那一项提示，托管设置那一项必须处理；
//   - 开发构建只管测试目标时（System.PrepareTestOnly），不写 Claude Code 的托管设置（不影响本机真实的 Claude Code）。

// PreparedFile 是“首次准备做完了”的标记（状态目录里）。
const PreparedFile = state.PreparedFile

// PrepareStatus 是首次准备的结果（status.prepare）。
type PrepareStatus struct {
	Done  bool     `json:"done"`            // 做完了（有标记）
	Notes []string `json:"notes,omitempty"` // 最近一次尝试的说明：冲突、失败的原因和下一步（不含路径）
}

// prepareNext 是没完成时的下一步。
const prepareNext = "处理掉说明里的那个文件后，点“重新检测”或再点启用：会自动再试一次（不用重启）"

func (d *Daemon) preparedMarker() string { return filepath.Join(d.opts.StateDir, PreparedFile) }

// prepare 在恢复状态之前、解析锁自检之前调用（启动时的第一次尝试）。
func (d *Daemon) prepare() {
	if !d.opts.Sys.FirstRun {
		return
	}
	d.tryPrepare()
}

// prepareProblem 给冲突检查用（启用前、“重新检测”时调用）：没完成时先再试一次；还没完成就返回说明（阻止启用）。
func (d *Daemon) prepareProblem() string {
	if !d.opts.Sys.FirstRun {
		return ""
	}
	ps := d.tryPrepare()
	if ps.Done {
		return ""
	}
	return "首次准备没有完成：" + strings.Join(ps.Notes, "；")
}

// tryPrepare 尝试一次（已完成时直接返回；并发安全）。
func (d *Daemon) tryPrepare() PrepareStatus {
	d.prepareMu.Lock()
	defer d.prepareMu.Unlock()
	if cur := d.prepareSt.Load(); cur != nil && cur.Done {
		return *cur
	}
	ps := &PrepareStatus{}
	defer func() { d.prepareSt.Store(ps) }()
	if _, err := os.Stat(d.preparedMarker()); err == nil {
		ps.Done = true
		d.writeReturnBlockSet(false) // M6-11b 之前做的首次准备没有选择文件：补写
		d.backfillLock(ps) // M6-12：新版本的内置名单比写入时多出来的，补写（冲突时不算完成）
		return *ps
	}
	if d.uninstalled() {
		ps.Notes = append(ps.Notes, "已卸载，不再写入启动管控")
		return *ps
	}
	ok := true
	if lk := d.opts.Sys.ResolverLock; lk != nil {
		res, err := lk.Write()
		switch {
		case errors.Is(err, resolverlock.ErrConflict):
			ok = false
			ps.Notes = append(ps.Notes, "Claude 域名解析锁：这些域名在解析锁的目录里已有别人的文件，没有覆盖："+strings.Join(res.Conflicts, "、")+"。"+prepareNext)
			d.events.Add("prepare", "resolver_lock_conflict", strings.Join(res.Conflicts, "、"))
		case err != nil:
			ok = false
			ps.Notes = append(ps.Notes, "写入 Claude 域名解析锁失败（会再试）")
			d.events.Add("prepare", "resolver_lock_failed", err.Error())
		}
	}
	if cs := d.opts.Sys.ClaudeSettings; cs != nil && !d.opts.Sys.PrepareTestOnly {
		conf, err := cs.Write()
		switch {
		case errors.Is(err, claudecfg.ErrConflict):
			ok = false
			var parts []string
			for _, c := range conf {
				if c.Var != "" {
					parts = append(parts, c.File+" 的 "+c.Var)
				} else {
					parts = append(parts, c.File+"（解析不了）")
				}
			}
			ps.Notes = append(ps.Notes, "Claude Code 的托管设置：别的文件也定义了代理变量，没有写入："+strings.Join(parts, "、")+"。"+prepareNext)
			d.events.Add("prepare", "claude_settings_conflict", strings.Join(parts, "、"))
		case err != nil:
			ok = false
			ps.Notes = append(ps.Notes, "写入 Claude Code 的托管设置失败（会再试）")
			d.events.Add("prepare", "claude_settings_failed", err.Error())
		}
	}
	if !ok {
		return *ps
	}
	if err := writeFileAtomic(d.preparedMarker(), []byte(time.Now().UTC().Format(time.RFC3339)+"\n")); err != nil {
		ps.Notes = append(ps.Notes, "记下首次准备完成失败（会再试）")
		return *ps
	}
	ps.Done = true
	d.writeReturnBlockSet(true)
	d.events.Add("prepare", "done", "")
	return *ps
}

// backfillLock 在首次准备已经完成时补写解析锁（M6-12，m6-plan v2.8 第 4 条）：新版本的内置名单比记录里多出来的那几个
// 自动补写并记事件；以前写过、后来被改或不见的不自动重写（自检照常报告未生效）；已卸载不写；别人的同名文件不覆盖——
// 这时按“冲突不算完成”处理：done 为假、说明写清下一步，冲突检查挡住启用，处理掉后检查时再试（完成标记不删）。
func (d *Daemon) backfillLock(ps *PrepareStatus) {
	lk := d.opts.Sys.ResolverLock
	if lk == nil || d.uninstalled() {
		return
	}
	res, err := lk.Backfill()
	if len(res.Written) > 0 {
		d.events.Add("prepare", "resolver_lock_backfilled", strings.Join(res.Written, "、"))
	}
	switch {
	case errors.Is(err, resolverlock.ErrConflict):
		ps.Done = false
		ps.Notes = append(ps.Notes, "Claude 域名解析锁（新版本追加的域名）：这些域名在解析锁的目录里已有别人的文件，没有覆盖："+strings.Join(res.Conflicts, "、")+"。"+prepareNext)
		d.events.Add("prepare", "resolver_lock_conflict", strings.Join(res.Conflicts, "、"))
	case err != nil:
		ps.Done = false
		ps.Notes = append(ps.Notes, "补写 Claude 域名解析锁失败（会再试）")
		d.events.Add("prepare", "resolver_lock_failed", err.Error())
	}
}
