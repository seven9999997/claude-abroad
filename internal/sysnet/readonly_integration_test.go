//go:build integration

package sysnet

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestReadOnlyCollectSamples 只在设置 SYSNET_SAMPLE_DIR 时运行：经 ReadOnlyRunner 采集原始输出（含本机地址，
// 只能写到会话的临时目录，脱敏后才能进 testdata）。
func TestReadOnlyCollectSamples(t *testing.T) {
	dir := os.Getenv("SYSNET_SAMPLE_DIR")
	if dir == "" {
		t.Skip("没有设置 SYSNET_SAMPLE_DIR")
	}
	r := ReadOnlyRunner{R: ExecRunner{}}
	ctx := context.Background()
	save := func(name, path string, args []string, stdin []byte) []byte {
		so, se, err := r.Run(ctx, path, args, stdin)
		out := append(append([]byte{}, so...), []byte("\n--- stderr ---\n")...)
		out = append(out, se...)
		if err != nil {
			out = append(out, []byte("\n--- err ---\n"+err.Error())...)
		}
		if werr := os.WriteFile(filepath.Join(dir, name), out, 0o600); werr != nil {
			t.Fatal(werr)
		}
		return so
	}
	list := save("listallnetworkservices.txt", PathNetworksetup, []string{"-listallnetworkservices"}, nil)
	for i, l := range strings.Split(string(list), "\n")[1:] {
		l = strings.TrimPrefix(strings.TrimSpace(l), "*")
		if l == "" || ValidService(l) != nil {
			continue
		}
		save(fmt.Sprintf("getinfo-%d.txt", i), PathNetworksetup, []string{"-getinfo", l}, nil)
	}
	save("route-get-1.1.1.1.txt", PathRoute, []string{"-n", "get", "1.1.1.1"}, nil)
	save("route-get-198.18.0.2.txt", PathRoute, []string{"-n", "get", "198.18.0.2"}, nil)
	save("route-get-127.0.0.1.txt", PathRoute, []string{"-n", "get", "127.0.0.1"}, nil)
	save("scutil-show-missing.txt", PathScutil, nil, []byte("show State:/Network/Service/claude-abroad/DNS\n"))
	save("scutil-show-global-dns.txt", PathScutil, nil, []byte("show State:/Network/Global/DNS\n"))
	save("scutil-list-dns.txt", PathScutil, nil, []byte("list State:/Network/Service/.*/DNS\n"))
	// M2-8 冲突检查用。
	save("scutil-proxy.txt", PathScutil, []string{"--proxy"}, nil)
	save("scutil-nwi.txt", PathScutil, []string{"--nwi"}, nil)
	save("route-get-default.txt", PathRoute, []string{"-n", "get", "default"}, nil)
	save("netstat-rn.txt", PathNetstat, []string{"-rn", "-f", "inet"}, nil)
	save("ps-axo-comm.txt", PathPs, []string{"-axo", "comm"}, nil)
}

// logRunner 记录经 ReadOnlyRunner 执行的命令（只记程序和动作，不记服务名和地址）。
type logRunner struct {
	R    Runner
	mu   sync.Mutex
	cmds []string
}

func (l *logRunner) Run(ctx context.Context, path string, args []string, stdin []byte) ([]byte, []byte, error) {
	so, se, err := l.R.Run(ctx, path, args, stdin)
	desc := filepath.Base(path)
	switch {
	case len(args) > 0 && args[0] == "-n":
		desc += " -n " + args[1] + " <地址>"
	case len(args) > 0:
		desc += " " + args[0]
		if len(args) > 1 {
			desc += " <服务>"
		}
	case stdin != nil:
		desc += " <stdin: " + strings.Fields(string(stdin))[0] + ">"
	}
	if err != nil {
		desc += "（失败）"
	}
	l.mu.Lock()
	l.cmds = append(l.cmds, desc)
	l.mu.Unlock()
	return so, se, err
}

// TestReadOnlyLive：M2-7d 验收 f。本机只读实跑，全程只经 ReadOnlyRunner。
func TestReadOnlyLive(t *testing.T) {
	lr := &logRunner{R: ReadOnlyRunner{R: ExecRunner{}}}
	ctx := context.Background()
	var summary []string

	snap, err := (MacIPv6{R: lr}).Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot：%v", err)
	}
	if len(snap) == 0 {
		t.Fatal("没有列出网络服务")
	}
	modes := map[IPv6Mode]int{}
	for _, s := range snap {
		if !s.Valid() {
			t.Fatal("有不合法的 IPv6 设置")
		}
		modes[s.Mode]++
	}
	summary = append(summary, fmt.Sprintf("IPv6 快照：%d 个网络服务，模式分布 %v", len(snap), modes))

	via, err := (MacRoutes{R: lr}).Via(ctx, netip.MustParseAddr("1.1.1.1"))
	if err != nil || via == "" {
		t.Fatalf("Via：%q %v", via, err)
	}
	kind := "其他"
	switch {
	case strings.HasPrefix(via, "utun"):
		kind = "utun（本机已有别的 TUN）"
	case strings.HasPrefix(via, "en"):
		kind = "物理网卡"
	}
	summary = append(summary, "Via(1.1.1.1)：非空，接口种类 "+kind)

	ok, err := (MacDNS{R: lr, Server: netip.MustParseAddr("198.18.0.2")}).Present(ctx)
	if err != nil || ok {
		t.Fatalf("DNS.Present：%v %v", ok, err)
	}
	summary = append(summary, "DNS.Present：false，无错误（我们的键不存在）")

	ni, err := (MacInterfaces{}).Interfaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ifs, _ := StdInterfaces()
	if len(ifs) == 0 {
		t.Fatal("没有网卡")
	}
	summary = append(summary, fmt.Sprintf("Interfaces：系统网卡 %d 个，直连网段 %d 个，网桥 %d 个", len(ifs), len(ni.Direct), len(ni.VMIfs)))

	// 证据文件：命令清单和结果摘要（不含地址和服务名）。
	out := filepath.Join("..", "..", "scripts", "dev", "out", "sysnet-readonly-"+time.Now().Format("20060102-150405")+".txt")
	var b strings.Builder
	b.WriteString("M2-7d 只读实跑（go test -tags integration -run ReadOnly ./internal/sysnet/），全程经 ReadOnlyRunner\n\n命令：\n")
	counts := map[string]int{}
	var order []string
	for _, c := range lr.cmds {
		if counts[c] == 0 {
			order = append(order, c)
		}
		counts[c]++
	}
	for _, c := range order {
		fmt.Fprintf(&b, "  %s × %d\n", c, counts[c])
	}
	b.WriteString("\n结果：\n")
	for _, s := range summary {
		b.WriteString("  " + s + "\n")
	}
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Log(b.String())
}
