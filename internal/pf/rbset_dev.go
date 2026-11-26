//go:build abroaddev

package pf

import "net/netip"

// DevBuild 报告是不是开发构建。
const DevBuild = true

// TestNets 是开发构建只管测试目标时回国拦截用的测试段：文档保留地址（RFC 5737 的 TEST-NET-1、RFC 3849 的
// 2001:db8::/32 里的一小段），不会有真实服务。只在开发构建里有（源码检查守着）。
var TestNets = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("2001:db8:abd::/48")}

// 开发构建：按选择文件选；读不出、内容不认识时不加载任何一组。
func chooseReturnBlock(set ReturnBlockSet, readErr error) ReturnBlockChoice {
	switch {
	case readErr != nil:
		return ReturnBlockChoice{Note: "回国拦截的选择文件读不出，开发构建不加载任何一组"}
	case set == SetTest:
		return ReturnBlockChoice{Load: true, Set: SetTest, Nets: TestNets}
	case set == SetReal:
		return realChoice("开发构建在用真实的 Anthropic 地址段（没有只管测试目标）")
	}
	return ReturnBlockChoice{Note: "回国拦截的选择文件内容不认识，开发构建不加载任何一组"}
}
