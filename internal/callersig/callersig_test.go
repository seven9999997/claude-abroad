package callersig

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

const leaf = "6C8EF4E3F9997FD632C9E3045F7D4720401ADBC8"

func TestNewAndRequirement(t *testing.T) {
	if New("") != nil || New("xyz") != nil || New(leaf[:39]) != nil {
		t.Fatal("指纹为空或格式不对时不启用")
	}
	v := New(leaf)
	if !v.Enabled() || v.Name() != "local_certificate" {
		t.Fatalf("%+v", v)
	}
	if got := Requirement(leaf); got != `certificate leaf = H"6c8ef4e3f9997fd632c9e3045f7d4720401adbc8"` {
		t.Fatal(got)
	}
	var nilV *Verifier
	if nilV.Enabled() || nilV.Verify(context.Background(), 1) {
		t.Fatal("nil 校验器不启用、一律不通过")
	}
}

// 按进程号 + 启动时间缓存；启动时间变了（进程号被复用）要重新校验；取不到启动时间不缓存。
func TestVerifyCache(t *testing.T) {
	calls := 0
	signed := map[int]bool{100: true}
	start := map[int]int64{100: 1, 200: 5}
	now := time.Unix(1000, 0)
	v := New(leaf)
	v.Now = func() time.Time { return now }
	v.Check = func(_ context.Context, pid int, req string) error {
		calls++
		if !strings.Contains(req, strings.ToLower(leaf)) {
			t.Errorf("要求：%s", req)
		}
		if signed[pid] {
			return nil
		}
		return errors.New("不满足")
	}
	v.Start = func(pid int) (int64, error) {
		if s, ok := start[pid]; ok {
			return s, nil
		}
		return 0, errors.New("取不到")
	}
	ctx := context.Background()
	if !v.Verify(ctx, 100) || !v.Verify(ctx, 100) || calls != 1 {
		t.Fatalf("应缓存：%d", calls)
	}
	if v.Verify(ctx, 200) || v.Verify(ctx, 200) || calls != 2 {
		t.Fatalf("不通过也缓存：%d", calls)
	}
	start[100] = 2 // 进程号 100 换成了别的进程
	signed[100] = false
	if v.Verify(ctx, 100) || calls != 3 {
		t.Fatalf("启动时间变了要重新校验：%d", calls)
	}
	now = now.Add(6 * time.Minute)
	if v.Verify(ctx, 200) || calls != 4 {
		t.Fatalf("过期要重新校验：%d", calls)
	}
	if v.Verify(ctx, 300) || v.Verify(ctx, 300) || calls != 6 {
		t.Fatalf("取不到启动时间不缓存：%d", calls)
	}
	if v.Verify(ctx, 0) || v.Verify(ctx, -1) || calls != 6 {
		t.Fatal("无效进程号直接不通过")
	}
}

// 真的调 codesign：测试进程没有这张证书的签名，校验不通过；启动时间取得到。
func TestCodesignRealProcess(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("只在 macOS 上")
	}
	v := New(leaf)
	v.Start = ProcStart
	if v.Verify(context.Background(), os.Getpid()) {
		t.Fatal("没有这张证书的签名，不应通过")
	}
	if s, err := ProcStart(os.Getpid()); err != nil || s <= 0 {
		t.Fatalf("启动时间：%d %v", s, err)
	}
}
