package agent

import (
	"claude-abroad/internal/mihomocfg"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// MaxMainProxyBody 是主代理配置（mihomo YAML）的上限。
const MaxMainProxyBody = 4 << 20

// 下发主代理的两段时限：校验最多 MainProxyCheckTimeout，等端口最多 MainProxyReadyTimeout。
// 两者之和（约 20 秒）必须小于控制面 http.Server 的 WriteTimeout（30 秒，见 NewServer），
// 否则应答写不出去，调用方只看到连接断开。
const (
	MainProxyCheckTimeout = 10 * time.Second
	MainProxyReadyTimeout = 10 * time.Second
)

// MainProxy 处理 PUT /v1/main-proxy（设计文档 7.2、7.4）：
//
//	先按白名单做二次校验（internal/mihomocfg），再写临时文件（0600）并用 `mihomo -t` 校验；失败回 422，旧配置和旧进程原样保留；
//	通过后原子改名为 main.yaml，重启 mihomo，等端口可连：成功 200，超时 504。
//
// 请求体和配置内容不进任何日志、应答（凭据只经 ctl.sock 的持有者下发）。全程串行。
type MainProxy struct {
	Path   string // main.yaml 在本机文件系统上的路径（Env.path 之后）
	Check  func(ctx context.Context, file string) ([]byte, error)
	Engine *Engine
	Base   context.Context // 引擎循环的生命周期
	Wait   time.Duration
	Logf   func(format string, a ...any)

	mu         sync.Mutex
	configured atomic.Bool
	digest     atomic.Pointer[string] // 当前配置的摘要（sha256 前 16 个十六进制字符；M5-4：abroadd 比对是否是自己下发的）
}

// Digest 返回当前主代理配置的摘要（没有下发过时为空）。摘要不含配置内容。
func (m *MainProxy) Digest() string {
	if p := m.digest.Load(); p != nil {
		return *p
	}
	return ""
}

// ConfigDigest 是主代理配置的摘要（abroadd 下发时用同一个函数算，好比对）。
func ConfigDigest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// Configured 表示是否下发过可用的主代理配置。
func (m *MainProxy) Configured() bool { return m.configured.Load() }

func (m *MainProxy) logf(format string, a ...any) {
	if m.Logf != nil {
		m.Logf(format, a...)
	}
}

func (m *MainProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", http.MethodPut)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxMainProxyBody+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "读取请求体失败"})
		return
	}
	if len(body) > MaxMainProxyBody {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("主代理配置超过 %d MiB", MaxMainProxyBody>>20)})
		return
	}
	if len(body) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "主代理配置为空"})
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	dg := ConfigDigest(body)
	code, msg := m.apply(r.Context(), body)
	clear(body)
	if code == http.StatusOK {
		m.digest.Store(&dg)
	}
	if code == http.StatusOK {
		writeJSON(w, code, map[string]any{"ok": true})
		return
	}
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}

func (m *MainProxy) apply(ctx context.Context, body []byte) (int, string) {
	// 二次校验（设计 7.2）：先于写文件和 mihomo -t。不通过就保留原配置、不重启；错误不回显配置内容。
	if err := mihomocfg.Validate(body); err != nil {
		m.logf("主代理：配置没有通过二次校验，保留原配置")
		return http.StatusUnprocessableEntity, err.Error()
	}
	dir := filepath.Dir(m.Path)
	f, err := os.CreateTemp(dir, ".main-*.yaml")
	if err != nil {
		m.logf("主代理：建临时文件失败：%v", err)
		return http.StatusInternalServerError, "写配置失败"
	}
	tmp := f.Name()
	defer os.Remove(tmp) // 改名成功后这里删不到任何东西
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return http.StatusInternalServerError, "写配置失败"
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		return http.StatusInternalServerError, "写配置失败"
	}
	if err := f.Close(); err != nil {
		return http.StatusInternalServerError, "写配置失败"
	}

	cctx, cancel := context.WithTimeout(ctx, MainProxyCheckTimeout)
	out, err := m.Check(cctx, tmp)
	cancel()
	if err != nil {
		msg := shortErr(out, err)
		m.logf("主代理：配置校验失败，保留原配置")
		return http.StatusUnprocessableEntity, "mihomo 配置校验失败：" + msg
	}
	if err := os.Rename(tmp, m.Path); err != nil {
		m.logf("主代理：替换配置失败：%v", err)
		return http.StatusInternalServerError, "替换配置失败"
	}
	m.configured.Store(true)
	m.logf("主代理：配置已更新，重启 mihomo")

	gen := m.Engine.Restart(m.Base)
	wait := m.Wait
	if wait <= 0 {
		wait = MainProxyReadyTimeout
	}
	wctx, wcancel := context.WithTimeout(ctx, wait)
	defer wcancel()
	if err := m.Engine.WaitRunning(wctx, gen); err != nil {
		if errors.Is(err, ErrEngineNotRunning) {
			return http.StatusUnprocessableEntity, err.Error()
		}
		return http.StatusGatewayTimeout, fmt.Sprintf("mihomo %s 内没有就绪（会继续自动重试，见 /v1/health）", wait)
	}
	return http.StatusOK, ""
}
