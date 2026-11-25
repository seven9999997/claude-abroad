package envcheck

import (
	"testing"
	"testing/fstest"
)

// 设备体检：认出中文办公字体（系统和用户两处），不认其他字体和非字体文件；目录不存在时不报错；不含完整路径。
func TestRunDevice(t *testing.T) {
	sys := fstest.MapFS{
		"Library/Fonts/SimSun.ttc":           {},
		"Library/Fonts/msyh.ttc":             {},
		"Library/Fonts/Arial.ttf":            {},
		"Library/Fonts/FZLTXHK.TTF":          {},
		"Library/Fonts/Microsoft/simhei.ttf": {},
		"Library/Fonts/simsun.txt":           {},
	}
	home := fstest.MapFS{"Library/Fonts/Deng.ttf": {}, "Library/Fonts/Helvetica.dfont": {}}
	rep := RunDevice(sys, home)
	got := map[string]string{}
	for _, f := range rep.Findings {
		got[f.File] = f.Location + "/" + f.Font
	}
	want := map[string]string{
		"SimSun.ttc":  "系统字体/宋体（SimSun）",
		"msyh.ttc":    "系统字体/微软雅黑（Microsoft YaHei）",
		"FZLTXHK.TTF": "系统字体/方正字体",
		"simhei.ttf":  "系统字体/黑体（SimHei）",
		"Deng.ttf":    "用户字体/等线（DengXian）",
	}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s：%q，应为 %q", k, got[k], v)
		}
	}
	if rep.Note == "" {
		t.Fatal("应有说明")
	}
	// 目录不存在、没给：空结果、不报错
	if r := RunDevice(fstest.MapFS{}, nil); len(r.Findings) != 0 || r.Findings == nil {
		t.Fatalf("%+v", r)
	}
}
