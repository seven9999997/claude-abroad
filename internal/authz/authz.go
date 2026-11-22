// Package authz 是敏感操作的本人授权（设计 11.1，m5-plan v2 第 5 节表后“授权凭证的语义”）。
//
// 流程：应用经系统授权服务申请权限项 RightSensitive（Touch ID 或登录密码），把系统授权的外部形式（32 字节）
// 随请求交给 abroadd；abroadd 用 SystemVerifier 核验它确实拥有这个权限项之后，**签发自己的短期凭证**
// （Issuer：随机 128 位、一次性、5 分钟内有效、绑定操作类型和参数摘要、只在内存里），之后的敏感请求带的是
// 这个凭证。系统凭证本身不绑定操作，所以不直接当作授权使用：否则一次授权可以拿去做任何敏感操作。
//
// 本文件是平台无关的部分（Mac、Windows 共用）；SystemVerifier 的 darwin 实现按 M5-1 探查的结论在 M5-3 做。
package authz

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"
)

// RightSensitive 是我们登记的自定义权限项（规则不共享：每个授权会话各自认证）。
const RightSensitive = "com.claude-abroad.sensitive"

// TokenTTL 是我们签发的短期凭证的有效期。
const TokenTTL = 5 * time.Minute

// ExternalFormSize 是系统授权外部形式（AuthorizationExternalForm）的长度。
const ExternalFormSize = 32

// MaxPending 是同时有效的凭证上限：超过时先清掉最早的（防止被刷满内存）。
const MaxPending = 64

// 错误。
var (
	ErrToken       = errors.New("授权凭证无效、已过期、已经用过，或不是给这个操作的")
	ErrExternal    = errors.New("系统授权的格式不对")
	ErrNotVerified = errors.New("系统授权没有通过核验")
	ErrUnsupported = errors.New("这个平台还不支持本人授权")
)

// SystemVerifier 核验系统授权的外部形式确实拥有 right（不弹任何交互）。
type SystemVerifier interface {
	Verify(ctx context.Context, external []byte, right string) error
}

// Unsupported 是还没有实现的平台用的核验器：一律拒绝（不能假装通过）。
type Unsupported struct{}

// Verify 一律返回 ErrUnsupported。
func (Unsupported) Verify(context.Context, []byte, string) error { return ErrUnsupported }

// ParseExternal 解析请求里系统授权外部形式的十六进制文本：必须正好 32 字节、不能全是 0。
func ParseExternal(s string) ([]byte, error) {
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ExternalFormSize {
		return nil, ErrExternal
	}
	zero := true
	for _, x := range b {
		if x != 0 {
			zero = false
			break
		}
	}
	if zero {
		return nil, ErrExternal
	}
	return b, nil
}

// Digest 是参数摘要（顺序有关；与确认码用同样的写法：每项后面跟一个 0 字节）。
func Digest(params ...string) string {
	h := sha256.New()
	for _, p := range params {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

type pending struct {
	op, digest string
	exp        time.Time
	seq        uint64
}

// Issuer 签发和核对我们自己的短期凭证。零值不可用，用 NewIssuer。
type Issuer struct {
	now func() time.Time
	mu  sync.Mutex
	m   map[string]pending
	seq uint64
}

// NewIssuer 新建签发器（now 为 nil 时用 time.Now）。
func NewIssuer(now func() time.Time) *Issuer {
	if now == nil {
		now = time.Now
	}
	return &Issuer{now: now, m: map[string]pending{}}
}

// Exchange 用系统授权换一个绑定 op 和 digest 的短期凭证：先核验，通过才签发。
func (i *Issuer) Exchange(ctx context.Context, v SystemVerifier, external []byte, op, digest string) (string, error) {
	if len(external) != ExternalFormSize {
		return "", ErrExternal
	}
	if op == "" || digest == "" {
		return "", errors.New("签发授权凭证：缺少操作类型或参数摘要")
	}
	if err := v.Verify(ctx, external, RightSensitive); err != nil {
		if errors.Is(err, ErrUnsupported) {
			return "", err
		}
		return "", ErrNotVerified
	}
	return i.issue(op, digest), nil
}

func (i *Issuer) issue(op, digest string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("读随机数失败：" + err.Error()) // crypto/rand 在受支持的系统上不会失败
	}
	tok := hex.EncodeToString(b[:])
	now := i.now()
	i.mu.Lock()
	defer i.mu.Unlock()
	for k, p := range i.m {
		if !now.Before(p.exp) {
			delete(i.m, k)
		}
	}
	for len(i.m) >= MaxPending { // 清掉最早签发的
		var oldK string
		var oldS uint64
		for k, p := range i.m {
			if oldK == "" || p.seq < oldS {
				oldK, oldS = k, p.seq
			}
		}
		delete(i.m, oldK)
	}
	i.seq++
	i.m[tok] = pending{op: op, digest: digest, exp: now.Add(TokenTTL), seq: i.seq}
	return tok
}

// Take 核对并作废凭证：存在、没过期、操作和参数摘要都相符才通过。只要凭证存在，不论是否通过都作废（一次性）。
func (i *Issuer) Take(token, op, digest string) error {
	now := i.now()
	i.mu.Lock()
	defer i.mu.Unlock()
	p, ok := i.m[token]
	if !ok {
		return ErrToken
	}
	delete(i.m, token)
	if !now.Before(p.exp) || subtle.ConstantTimeCompare([]byte(p.op), []byte(op)) != 1 ||
		subtle.ConstantTimeCompare([]byte(p.digest), []byte(digest)) != 1 {
		return ErrToken
	}
	return nil
}

// Pending 是当前有效的凭证数（测试和诊断用；不暴露凭证本身）。
func (i *Issuer) Pending() int {
	now := i.now()
	i.mu.Lock()
	defer i.mu.Unlock()
	n := 0
	for _, p := range i.m {
		if now.Before(p.exp) {
			n++
		}
	}
	return n
}
