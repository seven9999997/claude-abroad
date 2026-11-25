package integrity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setup(t *testing.T) (string, string, string) {
	t.Helper()
	base := t.TempDir()
	_ = os.MkdirAll(filepath.Join(base, "guest"), 0o755)
	_ = os.WriteFile(filepath.Join(base, "abroad-vm"), []byte("vm binary"), 0o755)
	_ = os.WriteFile(filepath.Join(base, "guest", "rootfs.img"), []byte("rootfs"), 0o644)
	b, err := Generate(base, []string{"abroad-vm", "guest/rootfs.img"})
	if err != nil {
		t.Fatal(err)
	}
	mf := filepath.Join(base, "manifest.json")
	_ = os.WriteFile(mf, b, 0o644)
	return base, mf, Digest(b)
}

func TestCheckOK(t *testing.T) {
	base, mf, dg := setup(t)
	if r := (&Checker{Manifest: mf, Base: base, Digest: dg}).Check(); !r.OK || !r.Checked {
		t.Fatalf("%+v", r)
	}
	// 同样的输入逐字节相同
	b1, _ := Generate(base, []string{"guest/rootfs.img", "abroad-vm"})
	b2, _ := os.ReadFile(mf)
	if string(b1) != string(b2) {
		t.Fatal("清单应逐字节相同（与输入顺序无关）")
	}
}

// 缺文件、内容不对、清单被改、开发构建、没配置。
func TestCheckProblems(t *testing.T) {
	base, mf, dg := setup(t)
	c := &Checker{Manifest: mf, Base: base, Digest: dg}
	_ = os.WriteFile(filepath.Join(base, "abroad-vm"), []byte("tampered"), 0o755)
	if r := c.Check(); r.OK || len(r.Problems) != 1 || r.Problems[0].Kind != "mismatch" || !strings.Contains(r.Summary(), "abroad-vm 内容不对") {
		t.Fatalf("内容不对：%+v", r)
	}
	_ = os.Remove(filepath.Join(base, "guest", "rootfs.img"))
	r := c.Check()
	kinds := map[string]string{}
	for _, p := range r.Problems {
		kinds[p.File] = p.Kind
	}
	if kinds["guest/rootfs.img"] != "missing" {
		t.Fatalf("缺文件：%+v", r)
	}
	// 清单被改（连同哈希一起改也没用：摘要对不上）
	b, _ := os.ReadFile(mf)
	_ = os.WriteFile(mf, []byte(strings.Replace(string(b), "\"version\": 1", "\"version\": 1 ", 1)), 0o644)
	if r := c.Check(); r.OK || r.Problems[0].Kind != "manifest" {
		t.Fatalf("清单被改：%+v", r)
	}
	// 开发构建、没配置：不校验、不拦
	if r := (&Checker{Manifest: mf, Base: base}).Check(); !r.OK || r.Checked || r.Note == "" {
		t.Fatalf("开发构建：%+v", r)
	}
	var nilc *Checker
	if r := nilc.Check(); !r.OK || r.Checked {
		t.Fatal("没配置")
	}
	if _, err := Generate(base, []string{"../x"}); err == nil {
		t.Fatal("不合法的路径应拒绝")
	}
}
