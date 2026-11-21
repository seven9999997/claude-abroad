package platform

import (
	"errors"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestUnsupported(t *testing.T) {
	err := Unsupported("传递文件描述符")
	if !errors.Is(err, ErrUnsupported) || !errors.Is(err, errors.ErrUnsupported) {
		t.Fatal(err)
	}
	if !strings.Contains(err.Error(), "传递文件描述符") || !strings.Contains(err.Error(), "本平台尚未支持") {
		t.Fatal(err)
	}
}

// 验收 e：Windows 上各程序的入口行为；macOS、Linux 不受影响。
func TestEntries(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		if m, c := MacOnlyEntry("abroad-vm", goos); m != "" || c != 0 {
			t.Fatalf("%s 上 abroad-vm 应照常运行", goos)
		}
		if m, c := RealModeEntry(goos); m != "" || c != 0 {
			t.Fatalf("%s 上真实模式应照常运行", goos)
		}
	}
	m, c := MacOnlyEntry("abroad-vm", "windows")
	if c == 0 || !strings.Contains(m, "abroad-vm") || !strings.Contains(m, "只用于 macOS") {
		t.Fatalf("%q %d", m, c)
	}
	m, c = RealModeEntry("windows")
	if c != ExitUnsupported || !strings.Contains(m, "尚未支持") || !strings.Contains(m, "--fake-system") {
		t.Fatalf("%q %d", m, c)
	}
}

// platform 只用标准库、不做网络访问：fakedns、state 等包的源码检查放行它的前提（W-1）。
func TestStdlibOnlyNoNetwork(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.Contains(strings.SplitN(p, "/", 2)[0], ".") || strings.HasPrefix(p, "claude-abroad/") || p == "net" || strings.HasPrefix(p, "net/") || p == "os/exec" {
				t.Errorf("%s import 了 %s", f, p)
			}
		}
	}
}
