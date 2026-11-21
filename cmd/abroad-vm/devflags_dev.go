//go:build abroaddev

package main

import "flag"

// 开发构建（go build -tags abroaddev）才有 --dev-agent：给内核命令行加 abroad.dev=1，
// agent 据此注册开发用的多出口接口（m3-plan v2.8 决定 7）。发布构建没有这个文件。
func init() {
	addDevFlags = func(fs *flag.FlagSet, o *options) {
		fs.BoolVar(&o.devAgent, "dev-agent", false, "开发构建：内核命令行加 abroad.dev=1（agent 注册 /v1/dev-exits）")
	}
}
