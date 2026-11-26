package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"claude-abroad/internal/integrity"
)

// integrityManifest 是 abroad-dev integrity-manifest：给发布构建生成完整性清单（M6-7）。
//
//	abroad-dev integrity-manifest -base <根目录> -out <清单> 文件1 文件2 …
//
// 文件是相对 -base 的路径。写出清单后在标准输出打印它的摘要，发布脚本再用
// -ldflags "-X claude-abroad/internal/integrity.ManifestDigest=<摘要>" 编译 abroadd 和 abroad-guard。
// 清单不能包含 abroadd、abroad-guard 自己（它们的内容取决于这个摘要）。退出码：0 成功；1 失败；2 参数错。
func integrityManifest(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("integrity-manifest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("base", "", "清单里相对路径的根（必填）")
	out := fs.String("out", "", "清单写到哪里（必填）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *base == "" || *out == "" || fs.NArg() == 0 {
		fmt.Fprintln(stderr, "用法：abroad-dev integrity-manifest -base <根目录> -out <清单> 文件…")
		return 2
	}
	for _, n := range fs.Args() {
		if n == "abroadd" || n == "abroad-guard" || hasBase(n, "abroadd") || hasBase(n, "abroad-guard") {
			fmt.Fprintf(stderr, "清单不能包含做校验的程序自己：%s\n", n)
			return 2
		}
	}
	b, err := integrity.Generate(*base, fs.Args())
	if err != nil {
		fmt.Fprintln(stderr, "生成完整性清单失败：", err)
		return 1
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		fmt.Fprintln(stderr, "写完整性清单失败：", err)
		return 1
	}
	fmt.Fprintln(stdout, integrity.Digest(b))
	return 0
}

func hasBase(p, name string) bool {
	return len(p) > len(name) && p[len(p)-len(name)-1] == '/' && p[len(p)-len(name):] == name
}

// integrityCheck 是 abroad-dev integrity-check：用和 abroadd 同一份校验逻辑检查产物（M6-7 补审返工：构建脚本末尾的
// 自检，签名和生成清单的顺序弄反时这里会立刻报出来）。
//
//	abroad-dev integrity-check -manifest <清单> -base <根目录> -digest <摘要>
//
// 退出码：0 通过；1 不通过；2 参数错。
func integrityCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("integrity-check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	manifest := fs.String("manifest", "", "清单（必填）")
	base := fs.String("base", "", "清单里相对路径的根（必填）")
	digest := fs.String("digest", "", "清单应有的摘要（必填，就是写进 abroadd 的那个）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *manifest == "" || *base == "" || *digest == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "用法：abroad-dev integrity-check -manifest <清单> -base <根目录> -digest <摘要>")
		return 2
	}
	r := (&integrity.Checker{Manifest: *manifest, Base: *base, Digest: *digest}).Check()
	fmt.Fprintln(stdout, r.Summary())
	if !r.Checked || !r.OK {
		return 1
	}
	return 0
}
