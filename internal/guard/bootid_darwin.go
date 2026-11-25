package guard

import "golang.org/x/sys/unix"

// BootID 是本次开机的标识（kern.bootsessionuuid，每次开机都不同）。
func BootID() (string, error) { return unix.Sysctl("kern.bootsessionuuid") }
