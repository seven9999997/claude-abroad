//go:build integration

package preflight

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"claude-abroad/internal/sysnet"
)

// TestReadOnlyLive：M2-8 验收 f。本机只读实跑（检查器内部一律包 ReadOnlyRunner；文件经只读的 os.DirFS）。
// 证据只写检查项的种类和数量，不写内容。
func TestReadOnlyLive(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	c := &Checker{R: sysnet.ExecRunner{}, Home: os.DirFS(home), Root: os.DirFS("/")}
	r := c.Run(context.Background())
	count := map[string]int{}
	for _, f := range r.Findings {
		count[string(f.Severity)+" "+f.ID]++
	}
	var keys []string
	for k := range count {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("M2-8 冲突检查只读实跑（go test -tags integration -run ReadOnly ./internal/preflight/）\n")
	b.WriteString("命令全部经 ReadOnlyRunner：netstat -rn -f inet、scutil --proxy、route -n get（default 和 3 个探测地址）、scutil --nwi、ps -axo comm；文件经只读的 os.DirFS。\n\n")
	fmt.Fprintf(&b, "结果：共 %d 条（阻断 %d、提示 %d）\n", len(r.Findings), len(r.Blocking()), len(r.Warnings()))
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s × %d\n", k, count[k])
	}
	out := filepath.Join("..", "..", "scripts", "dev", "out", "preflight-readonly-"+time.Now().Format("20060102-150405")+".txt")
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Log(b.String())
}
