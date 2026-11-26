//go:build !abroaddev

package pf

// DevBuild 报告是不是开发构建。
const DevBuild = false

// 发布构建：只认真实段（选择文件写着“测试”也不认）。
func chooseReturnBlock(set ReturnBlockSet, readErr error) ReturnBlockChoice {
	switch {
	case readErr != nil:
		return realChoice("回国拦截的选择文件读不出，按真实段处理")
	case set != SetReal:
		return realChoice("发布构建只认真实段")
	}
	return realChoice("")
}
