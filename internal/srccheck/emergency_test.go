package srccheck

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claude-abroad/internal/claudecfg"
	"claude-abroad/internal/pf"
	"claude-abroad/internal/resolverlock"
	"claude-abroad/internal/sysnet"
)

// 应急恢复文档（docs/emergency.md，M6-5a）和代码里的名字保持一致：锚点、DNS 键、锁的域名、托管设置的文件和端口；
// 第一步是“先退出 Claude”。代码改了名字而文档没跟着改时这里失败。
func TestEmergencyDocInSync(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "emergency.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	want := []string{
		pf.DefaultAnchor,
		sysnet.DNSKey,
		claudecfg.FileName,
		fmt.Sprintf("127.0.0.1:%d", claudecfg.DefaultPort),
		"managed-settings.d",
	}
	for _, d := range resolverlock.Domains() {
		want = append(want, "/etc/resolver/"+d)
	}
	for _, w := range want {
		if !strings.Contains(doc, w) {
			t.Errorf("应急文档里没有 %q（代码改了、文档没跟着改？）", w)
		}
	}
	// M6-5b：先应急程序、后手动步骤；应急程序的位置和开发安装一致。
	r := strings.Index(doc, "/Library/Application Support/claude-abroad/bin/abroad-rescue")
	if r < 0 || r > strings.Index(doc, "## 第一步") {
		t.Error("应急程序应写在手动步骤之前，位置是安装目录的 bin/abroad-rescue")
	}
	i := strings.Index(doc, "## 第一步")
	if i < 0 || !strings.Contains(doc[i:i+80], "退出 Claude") {
		t.Fatal("第一步应是“先退出 Claude”")
	}
}
