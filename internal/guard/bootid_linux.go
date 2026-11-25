package guard

import (
	"os"
	"strings"
)

// BootID 是本次开机的标识。
func BootID() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b)), err
}
