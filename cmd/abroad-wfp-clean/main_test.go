package main

import (
	"bytes"
	"errors"
	"runtime"
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	for _, c := range []struct {
		args []string
		del  bool
		help bool
		bad  bool
	}{
		{nil, false, false, false},
		{[]string{"--delete"}, true, false, false},
		{[]string{"-h"}, false, true, false},
		{[]string{"/?"}, false, true, false},
		{[]string{"--delet"}, false, false, true},
		{[]string{"-delete"}, false, false, true},
		{[]string{"delete"}, false, false, true},
		{[]string{"--delete", "--force"}, true, false, true},
		{[]string{"--dry-run"}, false, false, true},
	} {
		o, err := parseArgs(c.args)
		if (err != nil) != c.bad || (!c.bad && (o.Delete != c.del || o.Help != c.help)) {
			t.Errorf("%v：%+v %v", c.args, o, err)
		}
	}
}

func TestReport(t *testing.T) {
	plan := []string{"过滤器 A x", "子层 B y", "提供者 C z"}
	cases := []struct {
		name   string
		plan   []string
		del    bool
		err    error
		after  []string
		code   int
		substr string
	}{
		{"没有对象", nil, false, nil, nil, 0, "不需要还原"},
		{"没有对象真删", nil, true, nil, nil, 0, "不需要还原"},
		{"空跑", plan, false, nil, nil, 0, "这是空跑"},
		{"真删干净", plan, true, nil, nil, 0, "确认没有残留"},
		{"真删有残留", plan, true, nil, []string{"提供者 C z"}, 1, "仍有 1 个"},
		{"删除报错但已干净", plan, true, errors.New("x"), nil, 1, "删除失败"},
	}
	for _, c := range cases {
		text, code := report(c.plan, c.del, c.err, c.after)
		if code != c.code || !strings.Contains(text, c.substr) {
			t.Errorf("%s：%d\n%s", c.name, code, text)
		}
		if !c.del && strings.Contains(text, "已全部删除") {
			t.Errorf("%s：空跑不能说已删除", c.name)
		}
	}
}

func TestRealMain(t *testing.T) {
	var out, errb bytes.Buffer
	if code := realMain([]string{"--bogus"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "用法") {
		t.Fatalf("坏参数：%d %s", code, errb.String())
	}
	out.Reset()
	if code := realMain([]string{"-h"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "--delete") {
		t.Fatalf("帮助：%d", code)
	}
	if runtime.GOOS != "windows" {
		errb.Reset()
		if code := realMain(nil, &out, &errb); code != 3 || !strings.Contains(errb.String(), "只在 Windows 上使用") {
			t.Fatalf("非 Windows：%d %s", code, errb.String())
		}
	}
}
