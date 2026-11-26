package pf

import "net/netip"

// 回国拦截用哪一套地址段（M6-11b，architect 2026-11-27）：只有两套编译进程序的常量——真实段（AnthropicNets）和测试段
// （只在开发构建里，rbset_dev.go）。abroadd 在首次准备时把“用哪一套”写进状态目录的选择文件（state.ReturnBlockSetFile，
// 内容只是一个取值，改不出第三种地址）；规则生成、巡检、guard、应急程序都经 ChooseReturnBlock 从这同一个来源取。
//   - 发布构建：只认真实段；文件写什么、在不在都按真实段（管控不因为一个文件坏了就失效）；
//   - 开发构建：按文件选；文件不在、读不出、内容不认识时不加载任何一组（开发构建不能影响这台 Mac 上真实的 Claude）。

// ReturnBlockSet 是选择文件里的取值。
type ReturnBlockSet string

const (
	SetReal ReturnBlockSet = "real" // 真实的 Anthropic 地址段
	SetTest ReturnBlockSet = "test" // 文档保留地址里的测试段（只在开发构建里有）
)

// ReturnBlockChoice 是选择的结果。
type ReturnBlockChoice struct {
	Load bool           // 要不要加载
	Set  ReturnBlockSet // 用的是哪一套（Load 为真时）
	Nets []netip.Prefix // 地址段
	Note string         // 选择文件读不出等情况的说明（给日志和事件）
}

// Config 是这一选择对应的规则配置（Load 为假时为零值）。
func (c ReturnBlockChoice) Config() Config {
	if !c.Load {
		return Config{}
	}
	return Config{ReturnBlock: c.Nets}
}

func realChoice(note string) ReturnBlockChoice {
	return ReturnBlockChoice{Load: true, Set: SetReal, Nets: AnthropicNets, Note: note}
}

// ChooseReturnBlock 按选择文件的读取结果（取值、读取错误）选地址段；发布构建和开发构建的规则见本文件开头。
func ChooseReturnBlock(set ReturnBlockSet, readErr error) ReturnBlockChoice {
	return chooseReturnBlock(set, readErr)
}
