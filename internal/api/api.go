// Package api 是 abroadd 本地接口的平台无关部分（m5-plan v2.1 的 M5-2）：三级权限、调用方身份、路由描述、
// “不能注册”的清单、调用方签名校验（推迟期间是“未启用”实现）、改规则的按内容分级。Mac、Windows 共用。
package api

import (
	"fmt"
	"strings"
)

// Level 是接口的权限级别（设计 11.1）。
type Level int

const (
	Read      Level = iota + 1 // 只读：状态、检测结果、规则和出口列表（不含凭据）
	Normal                     // 普通：启用、停用、重新检测、检查出口
	Sensitive                  // 敏感：回国、卸载、更换主代理、增删出口和来源、放松保护的改规则……要本人授权
	// PerCall 是“按本次调用决定”：处理函数按本次调用算出级别（启用、切换地区看地区会不会变；改规则看内容），
	// 路由层只要求至少能做普通操作。
	PerCall
)

func (l Level) String() string {
	switch l {
	case Read:
		return "read"
	case Normal:
		return "normal"
	case Sensitive:
		return "sensitive"
	case PerCall:
		return "per_call"
	}
	return fmt.Sprintf("level(%d)", int(l))
}

// Caller 是调用方的身份（m5-plan v2.1 决定 1）。
type Caller int

const (
	// Other 是既不是控制台用户、也不是 root（开发模式下也不是属主）的对端：连接在解析任何请求之前断开。
	Other Caller = iota
	// Console 是当前控制台用户：只读和普通能做，敏感操作要本人授权（M5-3 接上带凭证的路径；之前一律 403）。
	Console
	// Trusted 是完全信任：对端是 root；开发模式下对端是状态目录的属主。三级都能做（命令行经 sudo 就是这种）。
	Trusted
)

func (c Caller) String() string {
	switch c {
	case Trusted:
		return "trusted"
	case Console:
		return "console"
	}
	return "other"
}

// Classify 按对端 uid 归类：等于属主或为 0 是完全信任；等于控制台用户是控制台用户；其余是其他。
// consoleUID 取不到（没人登录）时传 -1。
func Classify(peerUID, ownerUID, consoleUID int) Caller {
	switch {
	case peerUID < 0:
		return Other
	case peerUID == 0 || peerUID == ownerUID:
		return Trusted
	case consoleUID > 0 && peerUID == consoleUID:
		return Console
	}
	return Other
}

// Allowed 判断这种调用方能不能做这一级的操作（PerCall 按普通算：具体级别由处理函数再判）。
func Allowed(c Caller, l Level) bool {
	switch c {
	case Trusted:
		return true
	case Console:
		return l == Read || l == Normal || l == PerCall
	}
	return false
}

// AllowedSensitive 是“按本次调用决定”的接口在算出级别之后再判一次：敏感时只有完全信任的调用方能做
// （M5-3 起，控制台用户带了核验过的本人授权凭证也能做）。
func AllowedSensitive(c Caller) bool { return c == Trusted }

// NeedAuthText 是控制台用户做敏感操作时 403 的说明。
const NeedAuthText = "需要本人授权：这是敏感操作，只能在应用里经系统授权（密码或指纹）后执行，或在终端里用 sudo abroad 执行"

// Route 描述一个接口（路由表的一行；处理函数在 daemon 里按 ID 对上）。
type Route struct {
	ID     string // 稳定的名字，处理函数按它对上
	Method string
	Path   string // net/http 的模式写法，例如 /v1/exits/{tag}
	Level  Level
	Async  bool   // 支持 ?async=1 以任务执行
	Req    any    // 请求体的类型（零值）；nil 表示没有请求体或不是 JSON
	Resp   any    // 成功应答的类型（零值）；nil 表示没有固定类型
	Note   string // 给人看的说明（进接口描述文件）
}

// Pattern 是给 ServeMux 用的“方法 路径”。
func (r Route) Pattern() string { return r.Method + " " + r.Path }

// Forbidden 是永远不能注册的接口（设计 11.1 的敏感清单里，M5 不提供、也不允许悄悄加上的）：
// 读取凭据、打开调试终端、改 Claude 进程清单；以及已移除的旧别名 /v1/baseline（M6-1，不允许再加回来）。按路径片段匹配。
var Forbidden = []string{"credential", "secret", "debug", "shell", "terminal", "claude-procs", "process-list", "/v1/baseline"}

// CheckRoutes 检查路由表：ID 和“方法 路径”不重复；路径以 /v1/ 开头；级别有效；没有命中“不能注册”的清单。
func CheckRoutes(rs []Route) error {
	ids := map[string]bool{}
	pats := map[string]bool{}
	for _, r := range rs {
		if r.ID == "" || ids[r.ID] {
			return fmt.Errorf("路由 ID 为空或重复：%q", r.ID)
		}
		ids[r.ID] = true
		if pats[r.Pattern()] {
			return fmt.Errorf("路由重复：%s", r.Pattern())
		}
		pats[r.Pattern()] = true
		if !strings.HasPrefix(r.Path, "/v1/") {
			return fmt.Errorf("路径要以 /v1/ 开头：%s", r.Path)
		}
		switch r.Method {
		case "GET", "POST", "PUT", "DELETE":
		default:
			return fmt.Errorf("不认识的方法：%s", r.Pattern())
		}
		if r.Level < Read || r.Level > PerCall {
			return fmt.Errorf("级别无效：%s", r.Pattern())
		}
		low := strings.ToLower(r.Path)
		for _, f := range Forbidden {
			if strings.Contains(low, f) {
				return fmt.Errorf("不能注册的接口：%s（命中 %q）", r.Pattern(), f)
			}
		}
	}
	return nil
}

// CallerVerifier 校验调用方的代码签名（设计 11.1：Team ID 一致才算本项目的应用或命令行）。
// 付费开发者账号推迟期间接 Disabled。
type CallerVerifier interface {
	// Name 是实现的名字，写进 status 的 security 字段。
	Name() string
	// Enabled 为假时，“普通”一级等同“只读”一级的调用方也能做。
	Enabled() bool
}

// Disabled 是“调用方签名校验未启用”的实现（名字和说明都写明，不悄悄降级）。
type Disabled struct{}

func (Disabled) Name() string  { return "disabled" }
func (Disabled) Enabled() bool { return false }

// Security 是 status 里的安全说明（m5-plan 总原则 2：推迟期间的降级要让用户看得到）。
type Security struct {
	CallerVerification string `json:"caller_verification"` // disabled：调用方签名校验未启用
	Note               string `json:"note"`
	Caller             string `json:"caller"` // 这次调用方的身份：trusted、console
}

// SecurityNote 是签名校验未启用时给用户看的说明。
const SecurityNote = "调用方签名校验要等正式签名的版本：现在本机任何以你的身份运行的程序都能启用、停用、重新检测；回国、换主代理、增删出口等敏感操作仍要本人授权或 sudo"
