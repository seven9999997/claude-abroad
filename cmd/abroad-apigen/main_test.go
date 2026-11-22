package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// m5-plan v2.1 验收 i：生成结果入库；重新生成的结果与入库的一致（避免两边悄悄漂移）。
func TestGeneratedUpToDate(t *testing.T) {
	sw, oa := generate()
	for _, f := range []struct {
		path string
		b    []byte
	}{{SwiftOut, sw}, {OpenAPIOut, oa}} {
		old, err := os.ReadFile(filepath.Join("..", "..", f.path))
		if err != nil {
			t.Fatalf("没有入库的 %s：%v", f.path, err)
		}
		if string(old) != string(f.b) {
			t.Errorf("%s 与重新生成的结果不一致：执行 go run ./cmd/abroad-apigen 重新生成", f.path)
		}
	}
	s := string(sw)
	for _, want := range []string{"public struct DaemonStatus", "public struct DaemonJob", "public struct DaemonStreamEvent", "public struct ApiSecurity", "public struct DaemonConfirmNeeded"} {
		if !strings.Contains(s, want) {
			t.Errorf("Swift 类型里缺 %s", want)
		}
	}
	for _, want := range []string{`"/v1/status"`, `"/v1/jobs/{id}"`, `"x-level": "sensitive"`, `"x-async": true`, `"text/event-stream"`} {
		if !strings.Contains(string(oa), want) {
			t.Errorf("接口描述里缺 %s", want)
		}
	}
}
