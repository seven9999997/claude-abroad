// Package buildinfo 是构建时写进程序的版本号（提交号、构建时间、工作区是否有未提交改动）和状态目录的格式版本号
// （设计 11.4 的“原地升级”，v0.71）。
//
// 版本号由 scripts/dev/integrity-build.sh 经 -ldflags -X 写入：
//
//	-X claude-abroad/internal/buildinfo.Commit=<提交号>
//	-X claude-abroad/internal/buildinfo.BuildTime=<UTC，20261127T101500Z>
//	-X claude-abroad/internal/buildinfo.Dirty=true|false
//
// 没写入时（go build、go test）是 "unknown"：原地升级按“版本不明”处理。
package buildinfo

import "regexp"

// 由 -ldflags -X 写入（只能是字符串变量）。
var (
	Commit    = "unknown"
	BuildTime = "unknown"
	Dirty     = "unknown"
)

// StateFormat 是整个状态目录的落盘格式版本号：状态文件、主代理来源和选择、规则、分流出口、地区、各开关、首次准备标记、
// 托管设置和解析锁的记录……任何一种落盘结构的字段有增删、改名、改类型，都要把它加一（internal/daemon 的
// TestStateFormatPinned 守着）。新旧不一样时原地升级拒绝、改用卸载再重装：不做迁移，保证升级失败退回旧版本时
// 旧版本还能读懂状态。
const StateFormat = 1

// Info 是 /v1/status 的 build、abroadd --version 和 payload 里 build.json 的内容（字段名三处一致）。
type Info struct {
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	Dirty     bool   `json:"dirty"`
}

// Version 是 abroadd --version 打印的内容（和 build.json 相同）。
type Version struct {
	Info
	StateFormat int `json:"state_format"`
}

var (
	commitRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	timeRe   = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z$`)
)

// Get 返回写进程序的版本号。
func Get() Info { return Info{Commit: Commit, BuildTime: BuildTime, Dirty: Dirty == "true"} }

// GetVersion 返回 abroadd --version 的内容。
func GetVersion() Version { return Version{Info: Get(), StateFormat: StateFormat} }

// Known 报告版本号是不是构建时写进来的（提交号和构建时间的格式都对）。
func (i Info) Known() bool { return commitRe.MatchString(i.Commit) && timeRe.MatchString(i.BuildTime) }
