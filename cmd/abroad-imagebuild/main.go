// abroad-imagebuild 构建 Claude 出国的虚拟机镜像（设计文档 7.1），纯 Go 实现，
// 不依赖 Docker/Lima，不需要 root，只写输出目录（默认 guest/out/）。
//
//	abroad-imagebuild build  [-manifest guest/manifest.json] [-out guest/out] [-agent 路径]
//	abroad-imagebuild verify [-out guest/out]
//	abroad-imagebuild pin-packages [-manifest guest/manifest.json] [-out guest/out]
//
// build 会按清单下载输入文件（约 360 MB，主要是 Alpine netboot 包），校验 SHA-256 后组装。
// verify 离线检查输出目录，只读不写。
// pin-packages 按清单的 packages.want 重新固定 Alpine 软件包（校验索引签名、解依赖、核对每个包），
// 写回清单并打印新旧差异（m4-plan M4-1）。
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"time"

	"claude-abroad/internal/imagebuild"
	"claude-abroad/internal/usererror"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "build":
		err = build(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	case "pin-packages":
		err = pinPackages(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `用法：
  abroad-imagebuild build  [-manifest guest/manifest.json] [-out guest/out] [-agent 路径]
  abroad-imagebuild verify [-out guest/out]
  abroad-imagebuild pin-packages [-manifest guest/manifest.json] [-out guest/out]
`)
	os.Exit(2)
}

func build(args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	manifest := fs.String("manifest", "guest/manifest.json", "镜像清单")
	out := fs.String("out", "guest/out", "输出目录")
	agentPath := fs.String("agent", "", "abroad-agent（linux/arm64）；不给时放占位程序")
	_ = fs.Parse(args)

	var agent []byte
	if *agentPath != "" {
		b, err := os.ReadFile(*agentPath)
		if err != nil {
			return usererror.Wrap("读取 abroad-agent", err, "请先执行 GOOS=linux GOARCH=arm64 go build ./cmd/abroad-agent。")
		}
		agent = b
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	start := time.Now()
	info, err := imagebuild.Build(ctx, imagebuild.BuildOptions{
		ManifestPath: *manifest,
		OutDir:       *out,
		Agent:        agent,
		// 不设总超时（慢速网络下大文件要很久）；连续 60 秒收不到数据由下载器自己中止。
		Client: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 60 * time.Second,
			TLSHandshakeTimeout:   30 * time.Second,
		}},
		RootFS: imagebuild.Squashfs{},
		Log:    func(f string, a ...any) { fmt.Fprintf(os.Stderr, "• "+f+"\n", a...) },
	})
	if err != nil {
		return err
	}
	fmt.Printf("构建完成（%s）：内核 %s，启动模块 %d 个，输出在 %s\n",
		time.Since(start).Round(time.Second), info.KernelVersion, len(info.BootModules), *out)
	if info.AgentIsStub {
		fmt.Println("注意：abroad-agent 是占位程序，这个镜像还不能启动。")
	}
	return nil
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	out := fs.String("out", "guest/out", "输出目录")
	_ = fs.Parse(args)
	probs := imagebuild.Verify(*out, imagebuild.Squashfs{})
	if len(probs) == 0 {
		fmt.Println("镜像检查通过")
		return nil
	}
	for _, p := range probs {
		fmt.Fprintln(os.Stderr, "✗", p)
	}
	return fmt.Errorf("镜像检查发现 %d 个问题", len(probs))
}

func pinPackages(args []string) error {
	fs := flag.NewFlagSet("pin-packages", flag.ExitOnError)
	manifest := fs.String("manifest", "guest/manifest.json", "镜像清单")
	out := fs.String("out", "guest/out", "输出目录（缓存在 <out>/cache）")
	_ = fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	diff, err := imagebuild.PinPackages(ctx, imagebuild.PinOptions{
		ManifestPath: *manifest, RepoRoot: ".", OutDir: *out,
		Client: &http.Client{Timeout: 10 * time.Minute},
		Log:    func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
	})
	if err != nil {
		return err
	}
	if len(diff) == 0 {
		fmt.Println("软件包清单没有变化")
		return nil
	}
	fmt.Println("软件包清单的变化（请人工复核）：")
	for _, d := range diff {
		fmt.Println("  " + d)
	}
	return nil
}
