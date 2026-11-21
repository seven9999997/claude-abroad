package main

import "os"

// exitProcess 只在不是 PID 1 时调用（serve、version 等）。PID 1 的路径一律关机，
// 所以 os.Exit 只允许出现在这个文件里（main_test.go 扫描源码检查）。
func exitProcess(code int) { os.Exit(code) }
