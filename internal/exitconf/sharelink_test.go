package exitconf

import (
	"encoding/base64"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func oneProxy(t *testing.T, link string) map[string]any {
	t.Helper()
	b, err := ShareLinksToClash(link)
	if err != nil {
		t.Fatalf("%s：%v", link[:10], err)
	}
	var doc struct {
		Proxies []map[string]any `yaml:"proxies"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil || len(doc.Proxies) != 1 {
		t.Fatalf("%s：%v %s", link[:10], err, b)
	}
	return doc.Proxies[0]
}

// 六种分享链接各一条：转成 Clash 节点，并经 ParseClash 校验通过。
func TestShareLinks(t *testing.T) {
	ssUser := base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:S3cretPass"))
	ssOld := base64.StdEncoding.EncodeToString([]byte("chacha20-ietf-poly1305:Pw0ld@ss.example.com:8389"))
	vm := base64.StdEncoding.EncodeToString([]byte(`{"v":"2","ps":"vm1","add":"vm.example.com","port":"443","id":"b831381d-6324-4d53-ad4f-8cda48b30811","aid":"0","net":"ws","path":"/ray","host":"cdn.example.com","tls":"tls"}`))
	cases := []struct {
		link string
		typ  string
		chk  func(p map[string]any) bool
	}{
		{"ss://" + ssUser + "@ss.example.com:8388#my%20ss", "ss", func(p map[string]any) bool { return p["cipher"] == "aes-256-gcm" && p["password"] == "S3cretPass" && p["name"] == "my ss" }},
		{"ss://" + ssOld + "#old", "ss", func(p map[string]any) bool { return p["server"] == "ss.example.com" && p["port"] == 8389 }},
		{"vmess://" + vm, "vmess", func(p map[string]any) bool {
			return p["network"] == "ws" && p["tls"] == true && p["servername"] == "cdn.example.com"
		}},
		{"vless://b831381d-6324-4d53-ad4f-8cda48b30811@vl.example.com:443?encryption=none&security=reality&sni=www.example.com&fp=chrome&pbk=PUBKEY&sid=ab&type=grpc&serviceName=svc&flow=xtls-rprx-vision#vl", "vless", func(p map[string]any) bool {
			ro, _ := p["reality-opts"].(map[string]any)
			return p["flow"] == "xtls-rprx-vision" && ro["public-key"] == "PUBKEY" && p["network"] == "grpc"
		}},
		{"trojan://TrPass@tr.example.com:443?sni=tr.example.com&allowInsecure=1#tr", "trojan", func(p map[string]any) bool { return p["password"] == "TrPass" && p["skip-cert-verify"] == true }},
		{"hy2://HyPass@hy.example.com:8443?sni=hy.example.com&obfs=salamander&obfs-password=ob#hy", "hysteria2", func(p map[string]any) bool { return p["password"] == "HyPass" && p["obfs"] == "salamander" }},
		{"tuic://b831381d-6324-4d53-ad4f-8cda48b30811:TuPass@tu.example.com:443?sni=tu.example.com&congestion_control=bbr&alpn=h3#tu", "tuic", func(p map[string]any) bool {
			return p["password"] == "TuPass" && p["congestion-controller"] == "bbr"
		}},
	}
	for _, c := range cases {
		p := oneProxy(t, c.link)
		if p["type"] != c.typ || !c.chk(p) {
			t.Errorf("%s：%v", c.typ, p)
		}
	}
	// 多行一起导入：空行和注释跳过
	b, err := ShareLinksToClash("# 我的节点\n" + cases[4].link + "\n\n" + cases[5].link + "\n")
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseClash(b)
	if err != nil || len(c.Endpoints()) != 2 {
		t.Fatalf("%v %v", c, err)
	}
}

// 不合法的：都拒绝，错误里不回显链接内容（密码）。
func TestShareLinksInvalid(t *testing.T) {
	bad := map[string]string{
		"不认识的种类":   "socks5://u:SecretX1@h.example.com:1080",
		"没有端口":     "trojan://SecretX2@tr.example.com#x",
		"base64 不对": "vmess://@@@@SecretX3",
		"带插件的 SS":   "ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:SecretX4")) + "@h.example.com:8388?plugin=obfs-local#x",
		"缺密码":      "trojan://@tr.example.com:443",
		"不是链接":     "SecretX5",
		"空":        "\n\n# 只有注释\n",
		"端口太大":     "trojan://SecretX6@tr.example.com:70000",
	}
	for name, link := range bad {
		_, err := ShareLinksToClash(link)
		if err == nil {
			t.Errorf("%s：应拒绝", name)
			continue
		}
		for _, s := range []string{"SecretX1", "SecretX2", "SecretX3", "SecretX4", "SecretX5", "SecretX6"} {
			if strings.Contains(err.Error(), s) {
				t.Errorf("%s：错误里回显了链接内容：%v", name, err)
			}
		}
	}
}
