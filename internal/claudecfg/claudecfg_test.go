package claudecfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newSettings(t *testing.T) *Settings {
	t.Helper()
	base := t.TempDir()
	rec := filepath.Join(base, "st")
	_ = os.Mkdir(rec, 0o700)
	cc := filepath.Join(base, "ClaudeCode")
	_ = os.Mkdir(cc, 0o755)
	return &Settings{Dir: filepath.Join(cc, "managed-settings.d"), RecordDir: rec}
}

const wantContent = `{
  "env": {
    "https_proxy": "http://127.0.0.1:47821",
    "HTTPS_PROXY": "http://127.0.0.1:47821",
    "http_proxy": "http://127.0.0.1:47821",
    "HTTP_PROXY": "http://127.0.0.1:47821",
    "no_proxy": "localhost,127.0.0.1,::1",
    "NO_PROXY": "localhost,127.0.0.1,::1"
  }
}
`

// a. 内容逐字；六个变量齐全，四个代理变量同一地址，两个绕过变量只有回环；合法 JSON。
func TestContent(t *testing.T) {
	b, err := Content(DefaultPort)
	if err != nil || string(b) != wantContent {
		t.Fatalf("内容：\n%s\n%v", b, err)
	}
	var doc struct{ Env map[string]string }
	if err := json.Unmarshal(b, &doc); err != nil || len(doc.Env) != 6 {
		t.Fatalf("%v %v", doc, err)
	}
	for _, v := range []string{"https_proxy", "HTTPS_PROXY", "http_proxy", "HTTP_PROXY"} {
		if doc.Env[v] != "http://127.0.0.1:47821" {
			t.Fatalf("%s = %q", v, doc.Env[v])
		}
	}
	for _, v := range []string{"no_proxy", "NO_PROXY"} {
		if doc.Env[v] != "localhost,127.0.0.1,::1" {
			t.Fatalf("%s = %q", v, doc.Env[v])
		}
	}
	if _, err := Content(0); err == nil {
		t.Fatal("端口 0 应拒绝")
	}
}

// b. 写入：原子、普通用户可读、重复相同；主文件和别人的分片逐字节不变；删除只删自己的。
func TestWriteRemove(t *testing.T) {
	s := newSettings(t)
	_ = os.MkdirAll(s.Dir, 0o755)
	mainBody := []byte(`{"permissions":{"deny":["Bash(rm:*)"]}}`)
	otherBody := []byte(`{"env":{"FOO":"1"}}`)
	_ = os.WriteFile(s.mainPath(), mainBody, 0o644)
	_ = os.WriteFile(filepath.Join(s.Dir, "corp.json"), otherBody, 0o644)
	if cs, err := s.Write(); err != nil || cs != nil {
		t.Fatalf("%v %v", cs, err)
	}
	info, err := os.Stat(s.path())
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("%v %v", info, err)
	}
	b, _ := os.ReadFile(s.path())
	if string(b) != wantContent {
		t.Fatal("内容不对")
	}
	mt := info.ModTime()
	if _, err := s.Write(); err != nil {
		t.Fatal(err)
	}
	if info2, _ := os.Stat(s.path()); !info2.ModTime().Equal(mt) {
		t.Fatal("重复写入不应改动文件")
	}
	if st := s.Check(); !st.Effective || !st.Written {
		t.Fatalf("自检 %+v", st)
	}
	if m, _ := os.ReadFile(s.mainPath()); !bytes.Equal(m, mainBody) {
		t.Fatal("主文件被改了")
	}
	if o, _ := os.ReadFile(filepath.Join(s.Dir, "corp.json")); !bytes.Equal(o, otherBody) {
		t.Fatal("别人的分片被改了")
	}
	left, err := s.Remove()
	if err != nil || left {
		t.Fatalf("%v %v", left, err)
	}
	if _, err := os.Stat(s.path()); !os.IsNotExist(err) {
		t.Fatal("应删掉")
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "corp.json")); err != nil {
		t.Fatal("别人的分片不应被删")
	}
	if _, err := os.Stat(s.mainPath()); err != nil {
		t.Fatal("主文件不应被删")
	}
	// 被改过：留下
	_, _ = s.Write()
	_ = os.WriteFile(s.path(), []byte(`{"env":{}}`), 0o644)
	if left, err := s.Remove(); err != nil || !left {
		t.Fatalf("被改过应留下：%v %v", left, err)
	}
}

// b. 冲突：主文件或别人的分片定义了任一变量，不写入，写明文件和变量；解析不了的文件也算。
func TestConflicts(t *testing.T) {
	s := newSettings(t)
	_ = os.MkdirAll(s.Dir, 0o755)
	_ = os.WriteFile(s.mainPath(), []byte(`{"env":{"HTTPS_PROXY":"http://corp:8080","OTHER":"x"}}`), 0o644)
	_ = os.WriteFile(filepath.Join(s.Dir, "a.json"), []byte(`{"env":{"no_proxy":"*"}}`), 0o644)
	_ = os.WriteFile(filepath.Join(s.Dir, "bad.json"), []byte(`{`), 0o644)
	cs, err := s.Write()
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("%v", err)
	}
	want := map[Conflict]bool{{File: "managed-settings.json", Var: "HTTPS_PROXY"}: true, {File: "a.json", Var: "no_proxy"}: true, {File: "bad.json"}: true}
	if len(cs) != 3 {
		t.Fatalf("%v", cs)
	}
	for _, c := range cs {
		if !want[c] {
			t.Fatalf("多出冲突 %+v（%v）", c, cs)
		}
	}
	if _, err := os.Stat(s.path()); !os.IsNotExist(err) {
		t.Fatal("有冲突时不应写入")
	}
	// 同名文件不是我们写的：不覆盖
	s2 := newSettings(t)
	_ = os.MkdirAll(s2.Dir, 0o755)
	_ = os.WriteFile(s2.path(), []byte(`{"env":{}}`), 0o644)
	if _, err := s2.Write(); !errors.Is(err, ErrConflict) {
		t.Fatalf("同名别人的文件：%v", err)
	}
	if b, _ := os.ReadFile(s2.path()); string(b) != `{"env":{}}` {
		t.Fatal("别人的同名文件被覆盖了")
	}
}

// b. 自检：缺失、被改、普通用户读不到、有冲突；自检不写任何文件。
func TestCheck(t *testing.T) {
	s := newSettings(t)
	if st := s.Check(); st.Written || st.Effective {
		t.Fatalf("没写过：%+v", st)
	}
	if _, err := s.Write(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		mut  func()
		want string
	}{
		{"缺失", func() { _ = os.Remove(s.path()) }, "missing"},
		{"被改", func() { _ = os.WriteFile(s.path(), []byte(`{"env":{}}`), 0o644) }, "modified"},
		{"读不到", func() { _ = os.Chmod(s.path(), 0o600) }, "unreadable"},
		{"别人可写", func() { _ = os.Chmod(s.path(), 0o666) }, "permission"},
	}
	for _, c := range cases {
		_ = os.Remove(s.path())
		want, _ := Content(DefaultPort)
		_ = os.WriteFile(s.path(), want, 0o644)
		_ = os.Chmod(s.path(), 0o644)
		c.mut()
		before := snapshot(t, s.Dir)
		st := s.Check()
		if st.Problem != c.want || st.Effective {
			t.Fatalf("%s：%+v", c.name, st)
		}
		if !mapsEqual(before, snapshot(t, s.Dir)) {
			t.Fatalf("%s：自检改动了文件", c.name)
		}
	}
	// 有冲突
	_ = os.Remove(s.path())
	_, _ = s.Write()
	_ = os.WriteFile(filepath.Join(s.Dir, "z.json"), []byte(`{"env":{"http_proxy":"x"}}`), 0o644)
	if st := s.Check(); st.Effective || len(st.Conflicts) != 1 || st.Conflicts[0].Var != "http_proxy" {
		t.Fatalf("冲突：%+v", st)
	}
}

func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	m := map[string]string{}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		info, _ := e.Info()
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		m[e.Name()] = info.Mode().String() + "|" + info.ModTime().String() + "|" + string(b)
	}
	return m
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// 卸载后不留空目录：分片目录是我们建的、删完空了就一并删掉；原来就有的目录、还有别人文件的目录留下。
func TestRemoveCleansCreatedDir(t *testing.T) {
	s := newSettings(t)
	if _, err := s.Write(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Dir); !os.IsNotExist(err) {
		t.Fatal("我们建的空目录应删掉")
	}
	s2 := newSettings(t)
	_ = os.MkdirAll(s2.Dir, 0o755) // 原来就有
	_, _ = s2.Write()
	_, _ = s2.Remove()
	if _, err := os.Stat(s2.Dir); err != nil {
		t.Fatal("原来就有的目录不应删")
	}
	s3 := newSettings(t)
	_, _ = s3.Write()
	_ = os.WriteFile(filepath.Join(s3.Dir, "other.txt"), []byte("x"), 0o644)
	_, _ = s3.Remove()
	if _, err := os.Stat(filepath.Join(s3.Dir, "other.txt")); err != nil {
		t.Fatal("目录里还有别人的文件时应留下")
	}
}
