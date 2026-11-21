package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"claude-abroad/internal/usererror"
)

// ExternalVM 的分流出口操作（M4-8）：实现 ExitVM。
//   - 启动标识、出口：经控制面（ctl.sock，agent）；
//   - 端点集合：经 abroad-vm 的诊断接口（host.sock，只接受 abroad-vm 的用户和 root）。
// 请求体里的配置不进任何日志；错误里只带状态码和 agent 给的简短说明（agent 的说明本身不含配置内容）。

func (v ExternalVM) ctlSock() string  { return filepath.Join(v.Dir, "ctl.sock") }
func (v ExternalVM) hostSock() string { return filepath.Join(v.Dir, "host.sock") }

// BootID 读 agent health 里的启动标识。
func (v ExternalVM) BootID(ctx context.Context) (string, error) {
	var h struct {
		BootID string `json:"boot_id"`
	}
	if err := getJSON(ctx, v.ctlSock(), "http://abroad-agent/v1/health", &h); err != nil {
		return "", ErrVMNotReady
	}
	if h.BootID == "" {
		return "", ErrVMNotReady
	}
	return h.BootID, nil
}

func doJSON(ctx context.Context, sock, method, url string, body any, timeout time.Duration, out any) (int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	resp, err := unixClient(sock, timeout).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if out != nil {
		_ = json.Unmarshal(rb, out)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rb, &e)
		msg := strings.TrimSpace(e.Error)
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return resp.StatusCode, fmt.Errorf("HTTP %d：%s", resp.StatusCode, msg)
	}
	return resp.StatusCode, nil
}

// PushEndpoints 整体替换 abroad-vm 出站白名单里分流出口的那一组。
func (v ExternalVM) PushEndpoints(ctx context.Context, eps []ExitEndpoint) error {
	if eps == nil {
		eps = []ExitEndpoint{}
	}
	if _, err := doJSON(ctx, v.hostSock(), http.MethodPut, "http://abroad-vm/v1/exit-endpoints",
		map[string]any{"endpoints": eps}, 15*time.Second, nil); err != nil {
		return usererror.Wrap("更新虚拟机的出站白名单（分流出口的端点）", err, "请确认 abroad-vm 在运行。")
	}
	return nil
}

type agentExitsResp struct {
	Exits []AgentExit `json:"exits"`
}

// PushExits 整体替换 agent 的分流出口。agent 回 422 时（部分出口没建起来）仍返回各出口的状态和错误。
func (v ExternalVM) PushExits(ctx context.Context, exits []StoredExit) ([]AgentExit, error) {
	if exits == nil {
		exits = []StoredExit{}
	}
	var out agentExitsResp
	code, err := doJSON(ctx, v.ctlSock(), http.MethodPut, "http://abroad-agent/v1/exits",
		map[string]any{"exits": exits}, ExitPushTimeout, &out)
	if err != nil {
		if code == http.StatusUnprocessableEntity && out.Exits != nil {
			return out.Exits, err
		}
		return nil, usererror.Wrap("把分流出口下发到虚拟机", err, "请确认虚拟机在运行。")
	}
	if out.Exits == nil {
		out.Exits = []AgentExit{}
	}
	return out.Exits, nil
}

// AgentExits 读 agent 报告的各出口的引擎状态。
func (v ExternalVM) AgentExits(ctx context.Context) ([]AgentExit, error) {
	var out agentExitsResp
	if err := getJSON(ctx, v.ctlSock(), "http://abroad-agent/v1/exits", &out); err != nil {
		return nil, err
	}
	return out.Exits, nil
}

var _ ExitVM = ExternalVM{}
