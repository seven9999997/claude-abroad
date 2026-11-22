package console

import (
	"errors"
	"os"
	"runtime"
	"testing"
)

func TestFromOwner(t *testing.T) {
	if _, err := FromOwner(0); !errors.Is(err, ErrNoConsoleUser) {
		t.Fatal("属于 root（登录窗口）时应是没有人登录")
	}
	if uid, err := FromOwner(501); err != nil || uid != 501 {
		t.Fatalf("%d %v", uid, err)
	}
}

// 只读的本机核对（m5-plan v2 验收 b）：在 Mac 上、有人登录时取到的应是一个非 root 的 uid；
// 以当前图形会话里的用户运行测试时，它应等于自己的 uid。输出里不写用户名。
func TestUIDReadOnly(t *testing.T) {
	uid, err := UID()
	if runtime.GOOS != "darwin" {
		if err == nil {
			t.Fatal("非 Mac 平台应返回“尚未支持”")
		}
		return
	}
	if errors.Is(err, ErrNoConsoleUser) {
		t.Skip("没有人在图形界面登录（例如经 SSH 运行）")
	}
	if err != nil || uid <= 0 {
		t.Fatalf("%d %v", uid, err)
	}
	t.Logf("控制台用户：已取到；等于运行测试的用户：%v", uid == os.Getuid())
}
