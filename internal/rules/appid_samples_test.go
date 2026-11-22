package rules

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var updateAppIDSamples = flag.Bool("update-appid-samples", false, "重写 app/Tests/AbroadKitTests/appid-samples.json")

// M5-9 验收 c：应用把拖进来的应用包变成应用 ID 时，要和这里的归一规则一致。样本由这个测试生成、入库，Swift 的测试对照它。
func TestAppIDSamples(t *testing.T) {
	inputs := []string{
		"com.Google.Chrome", "com.apple.Safari", "COM.TINYSPECK.SLACKMACAPP", "org.mozilla.firefox", "com.anthropic.claudefordesktop",
		"com.west2online.ClashXPro", "io.github.clash-verge-rev.clash-verge-rev",
		"path:/Applications/Foo.app/Contents/MacOS/Foo", "path:/usr/local/bin/MyTool", "system:mDNSResponder", "unknown",
	}
	type sample struct {
		Raw        string `json:"raw"`
		Normalized string `json:"normalized"`
		Valid      bool   `json:"valid"`
		Proxy      bool   `json:"proxy_app"`
	}
	var out []sample
	for _, in := range inputs {
		n := NormalizeAppID(AppID(in))
		out = append(out, sample{Raw: in, Normalized: string(n), Valid: ValidAppID(n) == nil, Proxy: IsProxyApp(n)})
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	b = append(b, '\n')
	p := filepath.Join("..", "..", "app", "Tests", "AbroadKitTests", "appid-samples.json")
	if *updateAppIDSamples {
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old, err := os.ReadFile(p)
	if err != nil || string(old) != string(b) {
		t.Fatalf("%s 与归一规则不一致（go test ./internal/rules -run TestAppIDSamples -update-appid-samples 重新生成）", p)
	}
}
