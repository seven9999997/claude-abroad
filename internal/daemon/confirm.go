package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"claude-abroad/internal/claudeproc"
	"claude-abroad/internal/state"
	"claude-abroad/internal/usererror"
)

// 两步确认（m4-plan v2.3 决定 4）：恢复、启用、应用主代理、切换地区共用。M4-4 只接恢复，其余在 M4-5 接入。
//   - 确认码：随机 128 位，一次性，60 秒有效，绑定操作类型和参数摘要；
//   - 不绑定进程清单：预览到执行之间新出现的进程，以执行时的扫描为准；
//   - 只在内存里：进程重启后全部失效；不出现在日志和 status 里。

// ConfirmTTL 是确认码的有效期。
const ConfirmTTL = 60 * time.Second

// 需要确认的操作类型。
const (
	OpRestore = "restore"
)

// ErrConfirm 是确认码无效：不存在、过期、已经用过、用于别的操作或参数不符。
var ErrConfirm = errors.New("确认码无效")

type pendingConfirm struct {
	op, digest string
	exp        time.Time
}

type confirms struct {
	now func() time.Time
	mu  sync.Mutex
	m   map[string]pendingConfirm
}

func newConfirms(now func() time.Time) *confirms {
	if now == nil {
		now = time.Now
	}
	return &confirms{now: now, m: map[string]pendingConfirm{}}
}

// digestOf 是参数摘要（顺序有关）。
func digestOf(params ...string) string {
	h := sha256.New()
	for _, p := range params {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// issue 发一个确认码。
func (c *confirms) issue(op, digest string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("读随机数失败：" + err.Error()) // crypto/rand 在受支持的系统上不会失败
	}
	code := hex.EncodeToString(b[:])
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, p := range c.m { // 顺手清掉过期的
		if !now.Before(p.exp) {
			delete(c.m, k)
		}
	}
	c.m[code] = pendingConfirm{op: op, digest: digest, exp: now.Add(ConfirmTTL)}
	return code
}

// take 核对并作废确认码：存在、没过期、操作和参数摘要都相符才通过。只要码存在，不论是否通过都作废（一次性）。
// 通过时返回它（执行时遇到“忙”可以用 giveBack 放回）。
func (c *confirms) take(code, op, digest string) (pendingConfirm, error) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.m[code]
	if !ok {
		return pendingConfirm{}, ErrConfirm
	}
	delete(c.m, code)
	if !now.Before(p.exp) || subtle.ConstantTimeCompare([]byte(p.op), []byte(op)) != 1 || p.digest != digest {
		return pendingConfirm{}, ErrConfirm
	}
	return p, nil
}

// giveBack 把通过核对、但执行时遇到“已经有一个操作在进行”的确认码放回去（保留原来的过期时间）：
// 操作没有执行，用户不必重新预览。
func (c *confirms) giveBack(code string, p pendingConfirm) {
	if code == "" || p.op == "" || !c.now().Before(p.exp) {
		return
	}
	c.mu.Lock()
	c.m[code] = p
	c.mu.Unlock()
}

// ClaudePreviewer 给确认框看要结束的 Claude 进程（M4-4）。
type ClaudePreviewer interface {
	Preview(ctx context.Context) (claudeproc.Preview, error)
}

// ConfirmRequest 是需要确认的操作的请求体（可为空）。
type ConfirmRequest struct {
	Confirm string `json:"confirm,omitempty"`
}

// ConfirmNeeded 是不带确认码时的 409 应答：预览和确认码。
type ConfirmNeeded struct {
	Error      string             `json:"error"`
	Confirm    string             `json:"confirm"`
	ExpiresInS int                `json:"expires_in_s"`
	Preview    claudeproc.Preview `json:"preview"`
	Region     *RegionChange      `json:"region,omitempty"` // 地区会变的操作：从 A 变为 B
}

func (d *Daemon) preview(ctx context.Context) (claudeproc.Preview, error) {
	if d.opts.Sys.Claude == nil {
		return claudeproc.Preview{Processes: []claudeproc.Target{}, WillKill: true}, nil
	}
	return d.opts.Sys.Claude.Preview(ctx)
}

// handleRestore 是两步的恢复（m4-plan v2.3 决定 5）：状态已经是 RESTORED 时直接返回；
// 否则不带确认码回 409（预览 + 确认码），带正确的确认码才执行。
func (d *Daemon) handleRestore(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, errBody{"请求体超过 64 KiB"})
		return
	}
	var req ConfirmRequest
	if len(bytes.TrimSpace(b)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, errBody{"请求体不是合法的 JSON：只认 {\"confirm\": \"确认码\"}"})
			return
		}
	}
	if f, err := d.store.Load(); err == nil && f.State == state.Restored {
		writeJSON(w, http.StatusOK, d.Status()) // 已经是回国状态：不需要确认，什么都不做
		return
	}
	digest := digestOf(OpRestore)
	if strings.TrimSpace(req.Confirm) == "" {
		pv, err := d.preview(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errBody{usererror.Wrap("列出要结束的 Claude 进程", err, "本机网络没有改动，请再试一次。").Error()})
			return
		}
		d.opts.logf("恢复本机网络的预览：要结束的 Claude 进程 %d 个，没能确认的 %d 个", len(pv.Processes), len(pv.Unverified))
		writeJSON(w, http.StatusConflict, ConfirmNeeded{
			Error:      "恢复本机网络需要确认：会强制结束下面的 Claude 进程，未保存的内容会丢失。确认后带上确认码再调用一次（60 秒内有效）。",
			Confirm:    d.confirms.issue(OpRestore, digest),
			ExpiresInS: int(ConfirmTTL / time.Second),
			Preview:    pv,
		})
		return
	}
	held, err := d.confirms.take(req.Confirm, OpRestore, digest)
	if err != nil {
		writeJSON(w, http.StatusForbidden, errBody{usererror.New("恢复本机网络", "确认码无效、已过期或已经用过", "本机网络没有改动。请重新发起恢复，按新的预览确认。", err).Error()})
		return
	}
	if err := d.exec(w, r, d.m.Restore, true); errors.Is(err, state.ErrBusy) {
		d.confirms.giveBack(req.Confirm, held)
	}
}
