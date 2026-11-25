package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claude-abroad/internal/integrity"
)

func TestIntegrityManifestCmd(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "abroad"), []byte("cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "manifest.json")
	var so, se bytes.Buffer
	if code := integrityManifest([]string{"-base", dir, "-out", out, "bin/abroad"}, &so, &se); code != 0 {
		t.Fatalf("code %d: %s", code, se.String())
	}
	digest := strings.TrimSpace(so.String())
	c := &integrity.Checker{Manifest: out, Base: dir, Digest: digest}
	if r := c.Check(); !r.Checked || !r.OK {
		t.Fatalf("生成的清单校验不过：%+v", r)
	}
	for _, bad := range [][]string{{"-base", dir, "-out", out, "bin/abroadd"}, {"-base", dir, "-out", out, "abroad-guard"}, {"-base", dir, "-out", out}} {
		if code := integrityManifest(bad, &so, &se); code != 2 {
			t.Errorf("%v：退出码 %d，应为 2", bad, code)
		}
	}
	if code := integrityManifest([]string{"-base", dir, "-out", out, "bin/missing"}, &so, &se); code != 1 {
		t.Errorf("缺文件：退出码 %d，应为 1", code)
	}
}
