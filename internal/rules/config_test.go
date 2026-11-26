package rules

import (
	"bytes"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"claude-abroad/internal/srccheck"
)

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name, y, want string // want：错误里必须出现的定位
	}{
		{"default 是分流出口", "default: proxy:wg-a\n", "default"},
		{"default 是直连", "default: direct\n", "default"},
		{"出口标签不合法", "rules: [{app: com.a.b, action: \"proxy:Bad_Tag\"}]\n", "rules 第 1 条"},
		{"动作不认识", "rules: [{app: com.a.b, action: allow}]\n", "rules 第 1 条"},
		{"应用 ID 不合法", "rules: [{app: \"not an id\", action: block}]\n", "rules 第 1 条"},
		{"path 不是绝对路径", "rules: [{app: \"path:bin/git\", action: block}]\n", "rules 第 1 条"},
		{"path 不规范", "rules: [{app: \"path:/a/../b\", action: block}]\n", "rules 第 1 条"},
		{"local 与置顶重叠（精确）", "local_domains: [claude.ai]\n", "local_domains 第 1 项"},
		{"local 与置顶重叠（通配覆盖）", "local_domains: [\"*.corp.example\", \"*.ai\"]\n", "local_domains 第 2 项"},
		{"local 与追加置顶重叠", "pinned_extra: [x.test]\nlocal_domains: [\"*.test\"]\n", "local_domains 第 1 项"},
		{"local 是单独的 *", "local_domains: [\"*\"]\n", "local_domains 第 1 项"},
		{"代理软件设成直连", "rules: [{app: com.west2online.ClashX, action: direct}]\n", "rules 第 1 条"},
		{"代理软件进 offline_allow", "offline_allow: [\"path:/opt/homebrew/bin/mihomo\"]\n", "offline_allow 第 1 项"},
		{"Claude 设成直连", "rules: [{app: com.anthropic.claudefordesktop, action: direct}]\n", "rules 第 1 条"},
		{"Claude Code 设成直连", "rules: [{app: \"path:/Users/u/.local/bin/claude\", action: direct}]\n", "rules 第 1 条"},
		{"Claude 进 offline_allow", "offline_allow: [com.anthropic.claudefordesktop]\n", "offline_allow 第 1 项"},
		{"unknown 写进规则", "rules: [{app: unknown, action: block}]\n", "rules 第 1 条"},
		{"unknown 进 offline_allow", "offline_allow: [unknown]\n", "offline_allow 第 1 项"},
		{"未知字段", "default: proxy:main\nfoo: 1\n", "不认识的字段"},
		{"规则里的未知字段", "rules: [{app: com.a.b, action: block, note: x}]\n", "不认识的字段"},
		{"重复的应用规则", "rules: [{app: com.a.b, action: block}, {app: com.a.b, action: direct}]\n", "rules 第 2 条"},
		{"offline 重复", "offline_allow: [com.a.b, com.a.b]\n", "offline_allow 第 2 项"},
		{"多个文档", "default: block\n---\ndefault: proxy:main\n", "一个 YAML 文档"},
		{"超过大小上限", "# " + strings.Repeat("x", MaxConfigSize) + "\n", "上限"},
		{"一个映射里键太多", "default: block\n" + strings.Repeat("rules: []\n", 20), "最多 16 个字段"},
		{"列表太长", "offline_allow: [" + strings.Repeat("a.b,", 5000) + "]\n", "最多 4096 项"},
		{"嵌套太深", "rules: " + strings.Repeat("[", 20) + strings.Repeat("]", 20) + "\n", "嵌套层数太多"},
		{"别名", "default: &d block\nrules: []\noffline_allow: []\nlocal_domains: []\npinned_extra: []\nblock_udp_443: false\n", "锚点和别名"},
		{"pinned_extra 不合法", "pinned_extra: [\"a*.b\"]\n", "pinned_extra 第 1 项"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.y))
			if err == nil {
				t.Fatal("应被拒绝")
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.want) || !strings.HasPrefix(msg, "读取分流规则失败：") || !strings.HasSuffix(msg, "请修改规则后重试。") {
				t.Fatalf("错误应是 usererror 并指出 %q：%s", tc.want, msg)
			}
		})
	}
}

func TestParseAcceptsAndRoundTrip(t *testing.T) {
	for _, y := range []string{
		"",
		"default: block\n",
		testConf,
		testConf + "block_udp_443: true\npinned_extra: [\"*.Staging.Example.\"]\n",
		"rules: [{app: \"system:mDNSResponder\", action: direct}, {app: com.example.x, action: \"proxy:main\"}]\n",
		"local_domains: [\"*.LOCAL\", \"例子.中国\"]\n",
	} {
		c, err := Parse([]byte(y))
		if err != nil {
			t.Fatalf("应接受：%v\n%s", err, y)
		}
		b, err := c.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		c2, err := Parse(b)
		if err != nil {
			t.Fatalf("Marshal 的结果读不回：%v\n%s", err, b)
		}
		if !reflect.DeepEqual(c, c2) {
			t.Fatalf("往返后不相等：\n%+v\n%+v", c, c2)
		}
	}
	if c := mustParse(t, ""); c.Default != ActMain {
		t.Error("空文件的 default 应是 proxy:main")
	}
	if !reflect.DeepEqual(mustParse(t, ""), DefaultConfig()) {
		t.Error("空文件应等于 DefaultConfig")
	}
}

func TestAppIDCaseInsensitive(t *testing.T) {
	c := mustParse(t, "rules: [{app: com.Google.Chrome, action: block}, {app: \"path:/Opt/X\", action: block}]\n")
	if c.Rules[0].App != "com.google.chrome" || c.Rules[1].App != "path:/Opt/X" {
		t.Fatalf("Bundle ID 应归一成小写，path 原样：%+v", c.Rules)
	}
	if c.Decide(Who{App: "com.google.chrome"}, plain(80), envOK()).Action != ActBlock || c.Decide(Who{App: "COM.GOOGLE.CHROME"}, plain(80), envOK()).Action != ActBlock {
		t.Fatal("Bundle ID 匹配应不区分大小写")
	}
	if c.Decide(Who{App: "path:/opt/x"}, plain(80), envOK()).Action == ActBlock {
		t.Fatal("path: 区分大小写")
	}
	if _, err := Parse([]byte("rules: [{app: com.a.B, action: block}, {app: com.a.b, action: direct}]\n")); err == nil {
		t.Fatal("只差大小写的两条规则应算重复")
	}
}

func TestIsLocal(t *testing.T) {
	c := mustParse(t, "local_domains: [\"*.corp.example\", printer.lan]\n")
	for n, want := range map[string]bool{"git.corp.example": true, "GIT.CORP.EXAMPLE.": true, "corp.example": false, "printer.lan": true, "claude.ai": false} {
		if c.IsLocal(n) != want {
			t.Errorf("IsLocal(%q) 应为 %v", n, want)
		}
	}
}

// 回归（M3-1 模糊测试发现）：同一映射里几万个重复键曾让解析卡住数分钟（平方级），现在要在 5 秒内拒绝
// （正常不到 0.1 秒；上限放宽是给 -race 加全量并行时的负载留余量，平方级时是几分钟，仍能抓到）。
func TestParseManyKeysFast(t *testing.T) {
	var b strings.Builder
	for b.Len() < MaxConfigSize-16 {
		b.WriteString("rules: []\n")
	}
	start := time.Now()
	if _, err := Parse([]byte(b.String())); err == nil {
		t.Fatal("应被拒绝")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("拒绝用了 %v，超过 5 秒", d)
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(testConf))
	f.Add([]byte("default: block\nlocal_domains: [\"*.x\"]\n"))
	f.Add([]byte("rules: [{app: \"path:/a\", action: \"proxy:a-1\"}]\noffline_allow: []\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		start := time.Now()
		c, err := Parse(b)
		if d := time.Since(start); d > 200*time.Millisecond {
			t.Fatalf("解析用了 %v（%d 字节）", d, len(b))
		}
		if err != nil {
			return
		}
		m1, err := c.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		c2, err := Parse(m1)
		if err != nil {
			t.Fatalf("再解析失败：%v\n%s", err, m1)
		}
		if !reflect.DeepEqual(c, c2) {
			t.Fatalf("往返不稳定")
		}
		m2, _ := c2.Marshal()
		if !bytes.Equal(m1, m2) {
			t.Fatalf("两次序列化不同")
		}
	})
}

// 验收 g：纯计算包，只依赖标准库、usererror、yaml、idna；不执行命令，不碰网络。
func TestPackageDeps(t *testing.T) {
	srccheck.NoExec(t, ".")
	allowed := map[string]bool{
		"claude-abroad/internal/usererror": true,
		"go.yaml.in/yaml/v3":               true,
		"golang.org/x/net/idna":            true,
	}
	banned := []string{"net", "os", "os/exec", "syscall", "golang.org/x/sys/unix"}
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af := srccheck.Parse(t, f)
		for _, im := range af.Imports {
			p := strings.Trim(im.Path.Value, `"`)
			for _, b := range banned {
				if p == b {
					t.Errorf("%s 导入了 %s", f, p)
				}
			}
			if strings.Contains(p, ".") && !allowed[p] {
				t.Errorf("%s 导入了不在白名单里的包 %s", f, p)
			}
			if strings.HasPrefix(p, "claude-abroad/") && !allowed[p] {
				t.Errorf("%s 导入了内部包 %s", f, p)
			}
		}
	}
}

// 补齐三验收 f、g：不分配假地址的用户名单——合法写法、不合法写法（只有顶级域的通配、空后缀、不像域名的）、Claude 域名及其
// 子域名加不进、和追加的置顶域名的冲突判断。
func TestBypassExtra(t *testing.T) {
	c, err := Parse([]byte("bypass_extra: [nas.home.example, \"*.corp.example\"]\n"))
	if err != nil || !slices.Equal(c.BypassExtra, []string{"nas.home.example", "*.corp.example"}) {
		t.Fatalf("%v %v", c, err)
	}
	b, _ := c.Marshal()
	if !strings.Contains(string(b), "bypass_extra") {
		t.Fatalf("应写回：%s", b)
	}
	if b, _ := DefaultConfig().Marshal(); strings.Contains(string(b), "bypass_extra") {
		t.Fatal("没有时不写（规则文件和标识保持原样）")
	}
	for name, doc := range map[string]string{
		"只有顶级域的通配":   "bypass_extra: [\"*.com\"]\n",
		"空后缀":        "bypass_extra: [\"*.\"]\n",
		"不像域名":       "bypass_extra: [\"a b\"]\n",
		"Claude 域名":  "bypass_extra: [claude.ai]\n",
		"Claude 子域名": "bypass_extra: [api.anthropic.com]\n",
		"盖住 Claude":  "bypass_extra: [\"*.ai\"]\n",
		"重复":         "bypass_extra: [a.example, A.example]\n",
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s 应被拒", name)
		}
	}
	c, _ = Parse([]byte("pinned_extra: [\"*.mine.example\"]\nbypass_extra: [x.mine.example]\n")) // 文档本身不拒（顺序由后台判）
	if q, bad := c.BypassPinnedConflict("x.mine.example"); !bad || q != "*.mine.example" {
		t.Fatalf("和追加的置顶域名重叠：%q %v", q, bad)
	}
	if _, bad := c.BypassPinnedConflict("other.example"); bad {
		t.Fatal("不重叠")
	}
}
