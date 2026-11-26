//go:build windows

package main

// socketAnswers 在 Windows 上没有实现（应急程序只用于 macOS，main 一开始就按“本平台尚未支持”退出）。
func socketAnswers(string) bool { return false }
