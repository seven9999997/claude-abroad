package envcheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-abroad/internal/exitcheck"
)

func runRep(at time.Time, hard exitcheck.Result, items ...ItemResult) Report {
	return Report{At: at, Hard: hard, Items: items}
}

// 补齐四验收 a、b：结果变化时新增、相同时只更新时间；上限淘汰最旧的；只有时间、总判定、各项的标识和结果（没有说明）；UTC；
// 文件 0600、重启后还在、损坏时从空开始。
func TestRunHistory(t *testing.T) {
	dir := t.TempDir()
	h := &RunHistory{Dir: dir, Max: 3}
	cst := time.FixedZone("CST", 8*3600)
	t0 := time.Date(2026, 11, 27, 20, 0, 0, 0, cst)
	pass := ItemResult{ID: "01", Name: "出口 IP", Level: Hard, Result: exitcheck.Pass, Detail: "SECRETDETAIL 203.0.113.9", Fix: "SECRETFIX"}
	fail := pass
	fail.Result = exitcheck.Fail
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(h.Record(runRep(t0, exitcheck.Pass, pass)))
	must(h.Record(runRep(t0.Add(time.Minute), exitcheck.Pass, pass)))
	l := h.List()
	if len(l) != 1 || !l[0].LastSeen.Equal(t0.Add(time.Minute)) || l[0].At.Location() != time.UTC {
		t.Fatalf("相同的应只更新时间、UTC：%+v", l)
	}
	must(h.Record(runRep(t0.Add(2*time.Minute), exitcheck.Fail, fail)))
	l = h.List()
	if len(l) != 2 || l[0].Hard != exitcheck.Fail || l[1].Hard != exitcheck.Pass {
		t.Fatalf("变化时新增、最新的在前：%+v", l)
	}
	must(h.Record(runRep(t0.Add(3*time.Minute), exitcheck.Pass, pass)))
	must(h.Record(runRep(t0.Add(4*time.Minute), exitcheck.Fail, fail)))
	if l = h.List(); len(l) != 3 || !l[2].At.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("超过上限淘汰最旧的：%+v", l)
	}
	b, _ := os.ReadFile(filepath.Join(dir, RunHistoryFileName))
	if strings.Contains(string(b), "SECRET") || strings.Contains(string(b), "203.0.113") || strings.Contains(string(b), "出口 IP") {
		t.Fatalf("历史里不应有说明、名字：%s", b)
	}
	fi, _ := os.Stat(filepath.Join(dir, RunHistoryFileName))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("权限：%v", fi.Mode())
	}
	if l2 := (&RunHistory{Dir: dir}).List(); len(l2) != 3 {
		t.Fatal("重启后还在")
	}
	must(os.WriteFile(filepath.Join(dir, RunHistoryFileName), []byte("{坏"), 0o600))
	if l3 := h.List(); len(l3) != 0 {
		t.Fatal("损坏时从空开始")
	}
	must(h.Record(runRep(t0, exitcheck.Pass, pass)))
	if len(h.List()) != 1 {
		t.Fatal("损坏后照常记录")
	}
	must(h.Clear())
	if len(h.List()) != 0 {
		t.Fatal("清空")
	}
	var nilH *RunHistory
	if nilH.Record(runRep(t0, exitcheck.Pass)) != nil || len(nilH.List()) != 0 || nilH.Clear() != nil {
		t.Fatal("没有环境检测时安全")
	}
	// 写不下：返回错误（调用方只记事件）
	bad := &RunHistory{Dir: filepath.Join(dir, "nope")}
	if bad.Record(runRep(t0, exitcheck.Pass, pass)) == nil {
		t.Fatal("目录不在时应返回错误")
	}
}
