package guard

import "testing"

// 开机标识可读（只读的 sysctl）、两次相同。
func TestBootIDDarwin(t *testing.T) {
	a, err := BootID()
	if err != nil || len(a) < 16 {
		t.Fatalf("%q %v", a, err)
	}
	if b, _ := BootID(); b != a {
		t.Fatal("同一次开机两次读到的标识不同")
	}
}
