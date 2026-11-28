// Package callersig 是调用方签名校验的本机证书实现（设计 11.1，v0.73）：没有付费开发者账号时，
// 应用和命令行用本机生成的自签名代码签名证书签名（scripts/dev/local-sign.sh），证书的 SHA-1 指纹在构建时经
// -ldflags 写进 abroadd（LeafSHA1）。abroadd 对控制台用户的连接按对端进程号校验运行中的代码签名：
// 签名有效、并且叶证书就是这一张，才算本项目的应用或命令行。证书不需要被系统信任（只认指纹，不看信任链）。
package callersig

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LeafSHA1 是本机签名证书的 SHA-1 指纹（40 位十六进制，构建时写入；空表示这次构建没有签名，校验不启用）。
var LeafSHA1 string

var hexRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ValidLeaf 报告指纹格式对不对（大小写不限）。
func ValidLeaf(s string) bool { return hexRe.MatchString(strings.ToLower(s)) }

// Requirement 是按指纹认证书的代码签名要求（codesign -R 的写法）。
func Requirement(leaf string) string { return `certificate leaf = H"` + strings.ToLower(leaf) + `"` }

// Key 标识一个进程（进程号 + 启动时间，防止进程号被复用后沿用旧结果）。
type Key struct {
	PID   int
	Start int64 // 启动时间（微秒）；取不到时 0，此时不缓存
}

// Verifier 实现 api.CallerVerifier，并按进程校验签名。
type Verifier struct {
	Leaf string
	// Check 校验运行中的进程 pid 满足 req（nil 用 /usr/bin/codesign --verify -R <req> <pid>；测试注入）。
	Check func(ctx context.Context, pid int, req string) error
	// Start 取进程的启动时间（nil 时不缓存）。
	Start func(pid int) (int64, error)
	TTL   time.Duration // 结果缓存多久，默认 5 分钟
	Now   func() time.Time

	mu    sync.Mutex
	cache map[Key]entry
}

type entry struct {
	ok bool
	at time.Time
}

// New 按指纹建一个校验器；指纹为空或格式不对时返回 nil（校验不启用）。
func New(leaf string) *Verifier {
	if !ValidLeaf(leaf) {
		return nil
	}
	return &Verifier{Leaf: strings.ToLower(leaf)}
}

func (v *Verifier) Name() string  { return "local_certificate" }
func (v *Verifier) Enabled() bool { return v != nil && ValidLeaf(v.Leaf) }

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// Verify 报告进程 pid 是不是本项目签名的程序（签名有效、叶证书指纹一致）。校验失败、出错都算不是。
func (v *Verifier) Verify(ctx context.Context, pid int) bool {
	if !v.Enabled() || pid <= 0 {
		return false
	}
	k := Key{PID: pid}
	if v.Start != nil {
		if s, err := v.Start(pid); err == nil {
			k.Start = s
		}
	}
	ttl := v.TTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if k.Start != 0 {
		v.mu.Lock()
		e, ok := v.cache[k]
		v.mu.Unlock()
		if ok && v.now().Sub(e.at) < ttl {
			return e.ok
		}
	}
	check := v.Check
	if check == nil {
		check = codesignCheck
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ok := check(ctx, pid, Requirement(v.Leaf)) == nil
	if k.Start != 0 {
		v.mu.Lock()
		if v.cache == nil {
			v.cache = map[Key]entry{}
		}
		if len(v.cache) > 256 { // 进程来来去去：超过上限就整个清掉
			v.cache = map[Key]entry{}
		}
		v.cache[k] = entry{ok: ok, at: v.now()}
		v.mu.Unlock()
	}
	return ok
}

func codesignCheck(ctx context.Context, pid int, req string) error {
	return exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "-R="+req, strconv.Itoa(pid)).Run()
}
