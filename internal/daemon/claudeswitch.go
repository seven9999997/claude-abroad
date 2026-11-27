package daemon

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"claude-abroad/internal/api"
	"claude-abroad/internal/claudecfg"
	"claude-abroad/internal/usererror"
)

// 第四道的开关（设计 v0.68，用户 2026-11-27：“caibao这个兜底做个开关吧，默认不开启”）：
//   - 持久化的设置（不是启动参数），存在状态目录里只有 root 可读写的文件里，原子写入。文件不在、读不出、不是普通文件、
//     权限对组或其他人开放，一律按“关”处理（取“不改动这台机器上别的软件”的一边），并记事件；
//   - 只有打开了启动管控的安装才有这个开关（Options.ClaudeProxyAddr 或 System.ClaudeSettings 接上了）；
//   - 两个方向都要本人授权（打开会写对整台机器所有 Claude Code 生效的设置；关掉是少一道管控）；
//   - 关着（默认）：首次准备不写托管设置；本机代理端口不监听；冲突检查里和它有关的两项不出现、不挡启用；
//   - 打开：先查冲突（别的托管设置定义了那几个变量就拒绝，开关保持关）；写托管设置的分片 → 开始监听 → 记下开关，
//     任何一步失败都回到关（已写的撤掉）。打开之后的行为和以前相同；
//   - 关掉：删我们写的分片（只删内容和记录一致的）→ 停止监听 → 记下开关。被改过删不掉时如实报告，开关仍记为关，
//     状态里提示那份文件要用户自己处理；
//   - 开发构建只管测试目标时（System.PrepareTestOnly）：打开只监听，不写托管设置；
//   - 回国不碰它；卸载不管开没开都按记录删分片，并把开关记为关。

// ClaudeProxySwitchFile 是开关的文件（状态目录里，0600）。
const ClaudeProxySwitchFile = "claude-proxy-switch.json"

// ClaudeProxySetting 是 PUT /v1/settings/claude-proxy 的请求体。
type ClaudeProxySetting struct {
	Enabled bool `json:"enabled"`
}

type claudeSwitchFile struct {
	Version int  `json:"version"`
	On      bool `json:"on"`
}

// ClaudeProxyView 是 GET/PUT /v1/settings/claude-proxy 的应答。
type ClaudeProxyView struct {
	Available bool               `json:"available"`           // 这次安装打开了启动管控（才有这个开关）
	Enabled   bool               `json:"enabled"`             // 第四道打开着
	TestMode  bool               `json:"test_mode,omitempty"` // 开发构建只管测试目标：打开只监听，不写托管设置
	Leftover  bool               `json:"leftover,omitempty"`  // 关着，但托管设置目录里还有我们这个名字的文件（要用户自己处理）
	Note      string             `json:"note,omitempty"`      // 没打开管控时的说明
	Notes     []string           `json:"notes,omitempty"`     // 这一次操作的说明（关掉时托管设置被改过、没有删等）
	Status    *ClaudeProxyStatus `json:"status,omitempty"`    // 第四道的状态（同 status.claude_code）
}

// ClaudeProxyConsequences 是打开第四道的后果（命令行打开前打印并要求确认；应用的文字在它自己的文字表里，内容一致）。
var ClaudeProxyConsequences = []string{
	"这台 Mac 上所有 Claude Code 都会被指向 Claude 出国的本机代理（127.0.0.1:47821）。",
	"没有处于“已启用”时，Claude Code 一律连不上，包括经本机别的代理线路启动的。",
	"关掉开关（sudo abroad claude-proxy off）或卸载才恢复。",
}

// ClaudeProxyOffNote 是第四道关着时各处的说明（和“未生效”区分开）。
const ClaudeProxyOffNote = "未开启（默认关）"

// ClaudeProxyTestNote 是开发构建只管测试目标时的标注。
const ClaudeProxyTestNote = "测试模式：只监听，不写托管设置"

// ClaudeProxyLeftoverNote 是关着、但托管设置目录里还有我们这个名字的文件时的提示。
const ClaudeProxyLeftoverNote = "Claude Code 的托管设置目录里还有 claude-abroad.json（被改过所以没有删，或是上一版写下的）：它会把 Claude Code 指向一个没在听的端口，请自己检查后删掉，或再执行一次 sudo abroad claude-proxy off"

// cpAvailable 报告这次安装有没有第四道的开关（打开了启动管控：端口或托管设置接上了）。
func (d *Daemon) cpAvailable() bool {
	return d.opts.ClaudeProxyAddr != "" || d.opts.Sys.ClaudeSettings != nil
}

// cpTestMode：开发构建只管测试目标（打开只监听，不写托管设置）。
func (d *Daemon) cpTestMode() bool { return d.opts.Sys.PrepareTestOnly }

func (d *Daemon) switchPath() string { return filepath.Join(d.opts.StateDir, ClaudeProxySwitchFile) }

// loadClaudeSwitch 在启动时读开关（没有开关的安装什么都不做）。不在、读不出、权限过宽都按关，并记事件。
func (d *Daemon) loadClaudeSwitch() {
	if !d.cpAvailable() {
		return
	}
	on, why := readClaudeSwitch(d.switchPath())
	d.cpOn.Store(on)
	switch {
	case why != "":
		d.events.Add("claude_proxy", "switch_invalid", why)
	case !on:
		d.events.Add("claude_proxy", "switch_off", ClaudeProxyOffNote)
	default:
		d.events.Add("claude_proxy", "switch_on", "")
	}
}

// readClaudeSwitch 读开关文件：返回是否打开；按关处理的异常情况给出原因（文件不在不算异常）。
func readClaudeSwitch(path string) (bool, string) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, ""
	}
	if err != nil {
		return false, "第四道的开关读不出来，按关处理"
	}
	if !info.Mode().IsRegular() {
		return false, "第四道的开关文件不是普通文件，按关处理"
	}
	if info.Mode().Perm()&0o077 != 0 {
		return false, "第四道的开关文件权限过宽（组或其他人可以读写），按关处理"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false, "第四道的开关读不出来，按关处理"
	}
	var f claudeSwitchFile
	if json.Unmarshal(b, &f) != nil || f.Version != 1 {
		return false, "第四道的开关文件损坏，按关处理"
	}
	return f.On, ""
}

func (d *Daemon) saveClaudeSwitch(on bool) error {
	b, _ := json.Marshal(claudeSwitchFile{Version: 1, On: on})
	w := d.writeSwitch
	if w == nil {
		w = writeFileAtomic
	}
	return w(d.switchPath(), append(b, '\n'))
}

// curCP 取当前的本机代理端口（第四道关着时为 nil）。
func (d *Daemon) curCP() *claudeProxy {
	d.cpMu.Lock()
	defer d.cpMu.Unlock()
	return d.cproxy
}

// errCPConflict 是打开时别的托管设置定义了代理变量。
type errCPConflict struct{ parts []string }

func (e errCPConflict) Error() string {
	return usererror.New("打开第四道", "Claude Code 的托管设置里别的文件也定义了代理变量："+strings.Join(e.parts, "、"),
		"开关仍是关。请确认这些文件是谁写的，去掉其中的代理变量后再试；不会替你改别人的文件。", claudecfg.ErrConflict).Error()
}

func conflictParts(cs []claudecfg.Conflict) []string {
	var parts []string
	for _, c := range cs {
		if c.File == claudecfg.FileName && c.Var == "" { // 我们这个名字的文件，内容既不是要写的、也不是记录里的
			parts = append(parts, c.File+"（同名文件不是我们写的或被改过，没有覆盖）")
			continue
		}
		if c.Var != "" {
			parts = append(parts, c.File+" 的 "+c.Var)
		} else {
			parts = append(parts, c.File+"（解析不了）")
		}
	}
	return parts
}

// claudeProxyOn 打开第四道：查冲突 → 写分片 → 监听 → 记开关；任何一步失败回到关、撤掉已写的。已经打开时什么都不做。
func (d *Daemon) claudeProxyOn() ([]claudecfg.Conflict, error) {
	d.cpSwitchMu.Lock()
	defer d.cpSwitchMu.Unlock()
	if d.cpOn.Load() {
		return nil, nil
	}
	if d.uninstalling.Load() || d.uninstalled() { // 入口检查之后才开始的卸载（同状态机的 Admit）
		return nil, ErrUninstalledOp
	}
	cs := d.opts.Sys.ClaudeSettings
	write := cs != nil && !d.cpTestMode()
	if write {
		if conf := cs.Conflicts(); len(conf) > 0 {
			d.events.Add("claude_proxy", "switch_on_conflict", strings.Join(conflictParts(conf), "、"))
			return conf, errCPConflict{conflictParts(conf)}
		}
		if conf, err := cs.Write(); err != nil {
			_, _ = cs.Remove() // 写到一半（记录写了、文件没写成）：按记录收拾（只删内容一致的）
			if errors.Is(err, claudecfg.ErrConflict) {
				d.events.Add("claude_proxy", "switch_on_conflict", strings.Join(conflictParts(conf), "、"))
				return conf, errCPConflict{conflictParts(conf)}
			}
			d.events.Add("claude_proxy", "switch_on_failed", "写托管设置："+err.Error())
			return nil, usererror.Wrap("打开第四道（写 Claude Code 的托管设置）", err, "开关仍是关，什么都没有留下。请重试。")
		}
	}
	undoWrite := func() {
		if write {
			if left, err := cs.Remove(); left || err != nil {
				d.events.Add("claude_proxy", "switch_on_undo_incomplete", "撤掉托管设置没有完成")
			}
		}
	}
	if d.opts.ClaudeProxyAddr != "" {
		cp, err := d.listenClaudeProxyOnce()
		if err != nil {
			undoWrite()
			d.events.Add("claude_proxy", "switch_on_failed", "监听："+err.Error())
			return nil, usererror.New("打开第四道（监听本机代理端口）", err.Error(), "开关仍是关，已写的托管设置已撤掉。请退出占用这个端口的程序后重试。", nil)
		}
		d.cpMu.Lock()
		d.cproxy = cp
		d.cpMu.Unlock()
	}
	if err := d.saveClaudeSwitch(true); err != nil {
		d.stopCP()
		undoWrite()
		d.events.Add("claude_proxy", "switch_on_failed", "记下开关："+err.Error())
		return nil, usererror.Wrap("打开第四道（记下开关）", err, "开关仍是关，已写的托管设置已撤掉、端口已停止监听。请重试。")
	}
	d.cpOn.Store(true)
	d.events.Add("claude_proxy", "switch_on", "")
	return nil, nil
}

// claudeProxyOff 关掉第四道：删分片（只删内容和记录一致的）→ 停止监听 → 记开关。已经关着时也按记录收拾一遍（上一版写下的）。
// 返回这一次的说明（被改过没有删等）。
func (d *Daemon) claudeProxyOff() ([]string, error) {
	d.cpSwitchMu.Lock()
	defer d.cpSwitchMu.Unlock()
	var notes []string
	if cs := d.opts.Sys.ClaudeSettings; cs != nil {
		left, err := cs.Remove()
		switch {
		case err != nil:
			notes = append(notes, "删除 Claude Code 的托管设置没有完成（"+err.Error()+"）；再执行一次关闭会重试")
			d.events.Add("claude_proxy", "switch_off_left", err.Error())
		case left:
			notes = append(notes, ClaudeProxyLeftoverNote)
			d.events.Add("claude_proxy", "switch_off_left", "托管设置被改过，没有删")
		}
	}
	d.stopCP()
	d.cpOn.Store(false)
	if err := d.saveClaudeSwitch(false); err != nil {
		d.events.Add("claude_proxy", "switch_off_failed", "记下开关："+err.Error())
		return notes, usererror.Wrap("关闭第四道（记下开关）", err, "托管设置已按记录删除、端口已停止监听；请重试，否则后台重启后会按旧的开关重新监听。")
	}
	d.events.Add("claude_proxy", "switch_off", strings.Join(notes, "；"))
	return notes, nil
}

// stopCP 停掉本机代理端口（没在听时什么都不做）。
func (d *Daemon) stopCP() {
	d.cpMu.Lock()
	cp := d.cproxy
	d.cproxy = nil
	d.cpMu.Unlock()
	cp.stop()
}

func (d *Daemon) claudeProxyView() ClaudeProxyView {
	if !d.cpAvailable() {
		return ClaudeProxyView{Note: LaunchControlOffNote}
	}
	st := d.claudeCodeStatus()
	v := ClaudeProxyView{Available: true, Enabled: st.Enabled, TestMode: st.TestMode, Leftover: st.Leftover, Status: st}
	return v
}

func (d *Daemon) handleClaudeProxyGet(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
	writeJSON(w, http.StatusOK, d.claudeProxyView())
}

func (d *Daemon) handleClaudeProxyPut(w http.ResponseWriter, r *http.Request) {
	var req ClaudeProxySetting
	b, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if json.Unmarshal(b, &req) != nil {
		writeErr(w, http.StatusBadRequest, api.ErrorBody{Error: "请求体不合法：只认 {\"enabled\": true|false}", Field: "enabled"})
		return
	}
	if !d.cpAvailable() {
		writeErr(w, http.StatusNotImplemented, api.ErrorBody{Error: usererror.New("切换第四道", "这次安装没有打开启动管控", "第四道只在打开了启动管控的安装里有。", nil).Error()})
		return
	}
	if req.Enabled {
		conf, err := d.claudeProxyOn()
		if err != nil {
			if len(conf) > 0 {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"code": api.CodeInvalid, "error": err.Error(), "conflicts": conf})
				return
			}
			if errors.Is(err, ErrUninstalledOp) {
				writeErr(w, http.StatusConflict, api.ErrorBody{Code: api.CodeUninstalled, Error: UninstalledText})
				return
			}
			writeErr(w, http.StatusInternalServerError, api.ErrorBody{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, d.claudeProxyView())
		return
	}
	notes, err := d.claudeProxyOff()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, api.ErrorBody{Error: err.Error()})
		return
	}
	v := d.claudeProxyView()
	v.Notes = notes
	writeJSON(w, http.StatusOK, v)
}
