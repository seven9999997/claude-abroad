package daemon

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var updateSamples = flag.Bool("update-api-samples", false, "重写 testdata/api-samples（M5-2 迁移前采样用）")

// normalize 去掉应答里随时间变化的字段（事件时间、放行期剩余），其余逐字段保留。
func normalize(t *testing.T, b []byte) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("应答不是 JSON：%s", b)
	}
	var walk func(x any) any
	walk = func(x any) any {
		switch m := x.(type) {
		case map[string]any:
			for k := range m {
				switch k {
				case "time", "lease_left_ms", "at", "confirm", "expires_in_s":
					m[k] = "<变化>"
				default:
					m[k] = walk(m[k])
				}
			}
		case []any:
			for i := range m {
				m[i] = walk(m[i])
			}
		}
		return x
	}
	out, _ := json.MarshalIndent(walk(v), "", "  ")
	return append(out, '\n')
}

func sample(t *testing.T, name string, code int, body []byte) {
	t.Helper()
	got := append([]byte{byte('0' + code/100), byte('0' + code/10%10), byte('0' + code%10), '\n'}, normalize(t, body)...)
	p := filepath.Join("testdata", "api-samples", name+".json")
	if *updateSamples {
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("没有样本 %s：%v", p, err)
	}
	if string(want) != string(got) {
		t.Errorf("%s 的应答与迁移前的样本不同：\n--- 样本\n%s\n--- 现在\n%s", name, want, got)
	}
}

// m5-plan v2.1 验收 e：同步的启用、停用、回国、改规则的应答与迁移前逐字段相同（样本在 M5-2 动手之前采）。
func TestAPISamplesUnchanged(t *testing.T) {
	e := newEnv(t)
	d := e.start(e.opts())
	code, b := call(t, d, "PUT", "/v1/rules?drop_affected=0", []byte("rules:\n  - {app: com.example.a, action: block}\n"))
	sample(t, "rules-put", code, b)
	code, b = call(t, d, "POST", "/v1/enable", nil)
	sample(t, "enable", code, b)
	code, b = call(t, d, "POST", "/v1/disable", nil)
	sample(t, "disable", code, b)
	code, b = call(t, d, "POST", "/v1/restore", nil)
	sample(t, "restore-step1", code, b)
	var cn ConfirmNeeded
	_ = json.Unmarshal(b, &cn)
	cb, _ := json.Marshal(ConfirmRequest{Confirm: cn.Confirm})
	code, b = call(t, d, "POST", "/v1/restore", cb)
	sample(t, "restore-step2", code, b)
}
