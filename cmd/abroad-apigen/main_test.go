package main

import (
	"claude-abroad/internal/api"
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

// m5-plan v2.7 验收 a：生成的 Swift 里有同一组错误代码；错误应答带 code 字段。
func TestErrorCodesGenerated(t *testing.T) {
	sw, oa := generate()
	s := string(sw)
	if !strings.Contains(s, "public enum ApiErrorCode: String, Codable, Sendable, CaseIterable {") {
		t.Fatal("缺 ApiErrorCode")
	}
	i := strings.Index(s, "public enum ApiErrorCode")
	block := s[i : i+strings.Index(s[i:], "}\n")]
	if n := strings.Count(block, "    case "); n != len(api.ErrorCodes) {
		t.Fatalf("Swift 枚举 %d 个，代码表 %d 个", n, len(api.ErrorCodes))
	}
	seen := map[api.ErrorCode]bool{}
	for _, c := range api.ErrorCodes {
		if seen[c] {
			t.Errorf("代码重复：%s", c)
		}
		seen[c] = true
		if !strings.Contains(block, " = \""+string(c)+"\"") || !strings.Contains(string(oa), "\""+string(c)+"\"") {
			t.Errorf("生成结果里缺代码 %s", c)
		}
	}
	if !strings.Contains(s, "public struct ApiErrorBody") || !strings.Contains(s, "case code = \"code\"") {
		t.Fatal("错误应答应带 code 字段")
	}
}
