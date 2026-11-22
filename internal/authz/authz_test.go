package authz

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeVerifier struct {
	ok    map[string]bool // 外部形式的十六进制 → 是否拥有权限
	right string
	calls int
}

func (f *fakeVerifier) Verify(_ context.Context, ext []byte, right string) error {
	f.calls++
	f.right = right
	if f.ok[string(ext)] {
		return nil
	}
	return errors.New("没有这个权限")
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var good = []byte(strings.Repeat("\x01", ExternalFormSize))
var bad = []byte(strings.Repeat("\x02", ExternalFormSize))

// m5-plan v2 验收 d：我们自己签发的短期凭证——核验系统授权后才签发；一次性；过期；绑定操作和参数摘要。
func TestExchangeAndTake(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	is := NewIssuer(c.now)
	v := &fakeVerifier{ok: map[string]bool{string(good): true}}
	d := Digest("restore")
	tok, err := is.Exchange(context.Background(), v, good, "restore", d)
	if err != nil || len(tok) != 32 || v.right != RightSensitive {
		t.Fatalf("应签发 128 位凭证并按我们的权限项核验：%q %v %q", tok, err, v.right)
	}
	if err := is.Take(tok, "restore", d); err != nil {
		t.Fatalf("第一次应通过：%v", err)
	}
	if err := is.Take(tok, "restore", d); !errors.Is(err, ErrToken) {
		t.Fatal("一次性：第二次应被拒")
	}
	// 系统授权没通过：不签发。
	if _, err := is.Exchange(context.Background(), v, bad, "restore", d); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("系统授权不对时不能签发：%v", err)
	}
	if is.Pending() != 0 {
		t.Fatal("没通过时不应留下凭证")
	}
	// 用于别的操作、参数摘要不符：被拒且作废。
	tok, _ = is.Exchange(context.Background(), v, good, "region", Digest("region", "Asia/Tokyo"))
	if err := is.Take(tok, "restore", Digest("restore")); !errors.Is(err, ErrToken) {
		t.Fatal("用于别的操作应被拒")
	}
	if err := is.Take(tok, "region", Digest("region", "Asia/Tokyo")); !errors.Is(err, ErrToken) {
		t.Fatal("核对失败后也作废（一次性）")
	}
	tok, _ = is.Exchange(context.Background(), v, good, "region", Digest("region", "Asia/Tokyo"))
	if err := is.Take(tok, "region", Digest("region", "Europe/Paris")); !errors.Is(err, ErrToken) {
		t.Fatal("参数摘要不符应被拒")
	}
	// 过期。
	tok, _ = is.Exchange(context.Background(), v, good, "restore", d)
	c.add(TokenTTL)
	if err := is.Take(tok, "restore", d); !errors.Is(err, ErrToken) {
		t.Fatal("5 分钟后应过期")
	}
	tok, _ = is.Exchange(context.Background(), v, good, "restore", d)
	c.add(TokenTTL - time.Second)
	if err := is.Take(tok, "restore", d); err != nil {
		t.Fatalf("有效期内应通过：%v", err)
	}
	// 不存在的凭证、空凭证。
	for _, x := range []string{"", "00000000000000000000000000000000", "zz"} {
		if err := is.Take(x, "restore", d); !errors.Is(err, ErrToken) {
			t.Errorf("%q 应被拒", x)
		}
	}
	// 缺操作或摘要：不签发。
	if _, err := is.Exchange(context.Background(), v, good, "", d); err == nil {
		t.Fatal("缺操作类型不能签发")
	}
	if _, err := is.Exchange(context.Background(), v, good[:31], "restore", d); !errors.Is(err, ErrExternal) {
		t.Fatal("外部形式长度不对应拒绝")
	}
}

func TestUnsupportedNeverPasses(t *testing.T) {
	is := NewIssuer(nil)
	if _, err := is.Exchange(context.Background(), Unsupported{}, good, "restore", Digest("restore")); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("未实现的平台一律拒绝：%v", err)
	}
}

func TestParseExternal(t *testing.T) {
	if b, err := ParseExternal(strings.Repeat("0a", 32)); err != nil || len(b) != 32 {
		t.Fatalf("%v", err)
	}
	for _, s := range []string{"", strings.Repeat("0a", 31), strings.Repeat("0a", 33), strings.Repeat("00", 32), strings.Repeat("zz", 32)} {
		if _, err := ParseExternal(s); !errors.Is(err, ErrExternal) {
			t.Errorf("%q 应被拒", s)
		}
	}
}

func TestDigestOrderAndSeparation(t *testing.T) {
	if Digest("a", "bc") == Digest("ab", "c") || Digest("a", "b") == Digest("b", "a") {
		t.Fatal("摘要应区分顺序和分隔")
	}
}

// 上限：被刷满时清掉最早签发的，不无限增长。
func TestMaxPending(t *testing.T) {
	is := NewIssuer(nil)
	v := &fakeVerifier{ok: map[string]bool{string(good): true}}
	first, _ := is.Exchange(context.Background(), v, good, "restore", Digest("1"))
	for i := 0; i < MaxPending+10; i++ {
		_, _ = is.Exchange(context.Background(), v, good, "restore", Digest("x"))
	}
	if is.Pending() > MaxPending {
		t.Fatalf("超过上限：%d", is.Pending())
	}
	if err := is.Take(first, "restore", Digest("1")); !errors.Is(err, ErrToken) {
		t.Fatal("最早的应被清掉")
	}
}

func TestConcurrent(t *testing.T) {
	is := NewIssuer(nil)
	v := &fakeVerifier{ok: map[string]bool{string(good): true}}
	tok, _ := is.Exchange(context.Background(), v, good, "restore", Digest("r"))
	var wg sync.WaitGroup
	var mu sync.Mutex
	okN := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if is.Take(tok, "restore", Digest("r")) == nil {
				mu.Lock()
				okN++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if okN != 1 {
		t.Fatalf("同一凭证并发核对只能通过一次：%d", okN)
	}
}
