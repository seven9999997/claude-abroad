package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// 源码检查（设计 4.5、底线 6）：cmd/ 和 internal/ 的非测试 Go 文件里
//   - 不调用 gvisor-tap-vsock 的 ServicesMux / Mux（含 /forwarder/expose，虚拟机不能让宿主开端口）；
//   - 不出现 NAT 网卡和桥接网卡（VZNATNetworkDeviceAttachment、NewNATNetworkDeviceAttachment、NewBridgedNetwork）；
//   - virtualnetwork 包只用 New 和 VirtualNetwork 类型，接入方式只用 AcceptVfkit；
//   - types.Configuration{…} 只在 internal/usernet/config.go 里构造。
func TestSourceNetworkInvariants(t *testing.T) {
	bannedSel := map[string]bool{"ServicesMux": true, "Mux": true, "NewNATNetworkDeviceAttachment": true,
		"NewBridgedNetwork": true, "AcceptQemu": true, "AcceptBess": true, "AcceptStdio": true, "AcceptVpnKit": true}
	bannedIdent := map[string]bool{"VZNATNetworkDeviceAttachment": true, "NATNetworkDeviceAttachment": true}
	files := 0
	for _, root := range []string{"../../cmd", "../../internal"} {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
			if err != nil {
				return err
			}
			files++
			isConfig := filepath.ToSlash(p) == "../../internal/usernet/config.go"
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					if bannedSel[x.Sel.Name] {
						t.Errorf("%s 用了 .%s", p, x.Sel.Name)
					}
					if id, ok := x.X.(*ast.Ident); ok && id.Name == "virtualnetwork" &&
						x.Sel.Name != "New" && x.Sel.Name != "VirtualNetwork" {
						t.Errorf("%s 用了 virtualnetwork.%s（只允许 New、VirtualNetwork）", p, x.Sel.Name)
					}
				case *ast.Ident:
					if bannedIdent[x.Name] {
						t.Errorf("%s 出现了 %s", p, x.Name)
					}
				case *ast.CompositeLit:
					if sel, ok := x.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Configuration" {
						if id, ok := sel.X.(*ast.Ident); ok && id.Name == "types" && !isConfig {
							t.Errorf("%s 构造了 types.Configuration（只允许在 internal/usernet/config.go）", p)
						}
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files < 30 {
		t.Fatalf("只扫到 %d 个文件，路径不对？", files)
	}
}

// abroad-vm 的非测试文件不 import managedproxy（M2-2：网卡查询在 internal/physif，abroad-vm 不需要 Cage 的代理管理）。
func TestAbroadVMDoesNotImportManagedproxy(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	n := 0
	for _, p := range files {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		n++
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == "claude-abroad/internal/managedproxy" {
				t.Errorf("%s import 了 managedproxy", p)
			}
		}
	}
	if n == 0 {
		t.Fatal("没扫到文件")
	}
}
