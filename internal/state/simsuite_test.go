package state

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestSimBackendSuite 用 STATE_BACKEND=sim 把整个包（除了自己）再跑一遍：路由、DNS、IPv6 换成
// “真实实现 + 命令模拟器”（M2-9b-1 b）。这样标准的 go test ./internal/... 两种后端都覆盖到。
func TestSimBackendSuite(t *testing.T) {
	if simBackend {
		t.Skip("已经在模拟器后端里")
	}
	if testing.Short() {
		t.Skip("-short")
	}
	// 子进程自成一体（自己的临时目录、假系统），和本进程里的测试没有共享状态：并行跑，不再排在所有并行测试前面独占一段时间。
	t.Parallel()
	cmd := exec.Command(os.Args[0], "-test.run=^Test", "-test.skip=^TestSimBackendSuite$", "-test.count=1")
	cmd.Env = append(os.Environ(), "STATE_BACKEND=sim")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("模拟器后端没有通过：%v\n%s", err, tail(string(out), 80))
	}
	t.Logf("模拟器后端：%s", strings.TrimSpace(tail(string(out), 3)))
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
