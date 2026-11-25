package platform

// 各程序在没有实现的平台上的入口行为（W-1，验收 e）：纯函数，不带构建约束，在任何平台上都能单测。
// 只对 Windows 拒绝；macOS、Linux 的行为不变。

// ExitUnsupported 是“本平台尚未支持”时程序的退出码。
const ExitUnsupported = 3

// MacOnlyEntry 判断只用于 macOS 的程序（abroad-vm、m0-r2-probe）能不能在 goos 上运行：
// 不能时返回要打印的说明和退出码（非零）；能时返回 ("", 0)。
func MacOnlyEntry(prog, goos string) (msg string, code int) {
	if goos == "windows" {
		return prog + "：本程序只用于 macOS（Windows 版的对应部分见 docs/design-windows.md）", ExitUnsupported
	}
	return "", 0
}

// RealModeEntry 判断 abroadd 的真实模式能不能在 goos 上运行（假系统模式各平台都能跑）：
// 不能时返回要打印的说明和退出码；能时返回 ("", 0)。
func RealModeEntry(goos string) (msg string, code int) {
	if goos == "windows" {
		return "abroadd：真实模式在 Windows 上" + ErrUnsupported.Error() + "（开发构建里有假系统模式可以试运行）", ExitUnsupported
	}
	return "", 0
}
