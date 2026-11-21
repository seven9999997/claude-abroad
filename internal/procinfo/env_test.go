package procinfo

import (
	"encoding/binary"
	"reflect"
	"strings"
	"testing"
)

func procargs(path string, args []string, env []string) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(len(args)))
	b = append(b, path...)
	b = append(b, 0, 0, 0)
	for _, a := range args {
		b = append(append(b, a...), 0)
	}
	for _, e := range env {
		b = append(append(b, e...), 0)
	}
	return append(b, 0)
}

// h. 只返回白名单变量名是否存在和语言变量的值；代理变量的值不出现在返回的结构里。
func TestParseProcEnv(t *testing.T) {
	const secret = "http://user:pw@proxy.example:8080"
	b := procargs("/usr/local/bin/claude", []string{"claude", "--resume"}, []string{
		"PATH=/usr/bin", "HTTPS_PROXY=" + secret, "all_proxy=" + secret, "LANG=ja_JP.UTF-8", "LC_ALL=", "TOKEN=abc", "NO_PROXY=localhost", "http_proxy=",
	})
	got, err := ParseProcEnv(b)
	if err != nil {
		t.Fatal(err)
	}
	want := EnvInfo{ProxyVars: []string{"HTTPS_PROXY", "all_proxy"}, Lang: "ja_JP.UTF-8"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	if strings.Contains(strings.Join(append(got.ProxyVars, got.Lang, got.LcAll), "|"), "proxy.example") {
		t.Fatal("代理变量的值不应离开读取函数")
	}
	// 参数里像环境变量的不算；没有环境变量也行。
	got, _ = ParseProcEnv(procargs("/bin/sh", []string{"sh", "HTTP_PROXY=x"}, nil))
	if len(got.ProxyVars) != 0 {
		t.Fatalf("参数不应当成环境变量：%+v", got)
	}
	for _, bad := range [][]byte{nil, {1, 0, 0, 0}, {0xff, 0xff, 0xff, 0x7f, 'a', 0}} {
		if _, err := ParseProcEnv(bad); err == nil {
			t.Fatalf("应报错：%v", bad)
		}
	}
}
