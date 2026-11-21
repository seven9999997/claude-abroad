package tunsim

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// tunsim 不建真实 utun、不改宿主网络、不碰本机解析器，只连 unix 的 data.sock。
func TestSourceNoHostNetwork(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	dial := regexp.MustCompile(`(net\.Dial|\.DialContext)\(([^)]*)\)`)
	for _, p := range files {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, bad := range []string{"OpenUtun", "exec.Command", "pfctl", "networksetup", "ifconfig", "\"route\"", "/sbin/route",
			"net.Listen", "net.Resolver", "net.Lookup", "DefaultResolver"} {
			if strings.Contains(src, bad) {
				t.Errorf("%s 里出现了 %s", p, bad)
			}
		}
		for _, m := range dial.FindAllStringSubmatch(src, -1) {
			if !strings.Contains(m[2], `"unix"`) {
				t.Errorf("%s 里的拨号只允许 unix：%s", p, m[0])
			}
		}
	}
}
