package main

import "claude-abroad/internal/tun"

// openUtun 让系统分配一个新的 utun（unit -1 表示由系统分配编号），MTU 1500。需要 root。
func openUtun() (tun.Device, error) { return tun.OpenUtun(-1, 1500) }
