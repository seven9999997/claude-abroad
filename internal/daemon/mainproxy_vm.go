package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"claude-abroad/internal/usererror"
	"claude-abroad/internal/usernet"
)

// MainProxyVM 是 daemon 和虚拟机之间关于主代理的操作（M5-4；ExternalVM 实现，测试用假的）。
type MainProxyVM interface {
	// BootID 是 agent health 里的启动标识（虚拟机原地重启后会变）。
	BootID(ctx context.Context) (string, error)
	// PushMainHops 整体替换 abroad-vm 出站白名单里主代理的那一组（host.sock PUT /v1/main-hops）。
	PushMainHops(ctx context.Context, m usernet.MainHops) error
	// PushMainProxy 把主代理配置下发给 agent（ctl.sock PUT /v1/main-proxy；agent 校验、换配置、等引擎端口就绪才返回）。
	PushMainProxy(ctx context.Context, yaml []byte) error
	// MainProxyDigest 读 agent 报告的当前主代理配置的摘要（没有时为空）。
	MainProxyDigest(ctx context.Context) (string, error)
	// HopPrivate 读 abroad-vm 的计数：第一跳域名的答案全是私有地址、因此被拒的次数（dns_hop_private）。
	HopPrivate(ctx context.Context) (int64, error)
}

// 主代理下发的时限（agent 的校验加等端口约 20 秒）。
const MainPushTimeout = 40 * time.Second

// PushMainHops 实现 MainProxyVM。
func (v ExternalVM) PushMainHops(ctx context.Context, m usernet.MainHops) error {
	if m.Hops == nil {
		m.Hops = []string{}
	}
	if m.LAN == nil {
		m.LAN = []usernet.MainLAN{}
	}
	if _, err := doJSON(ctx, v.hostSock(), http.MethodPut, "http://abroad-vm/v1/main-hops", m, 15*time.Second, nil); err != nil {
		return usererror.Wrap("更新虚拟机的出站白名单（主代理的第一跳）", err, "请确认 abroad-vm 在运行。")
	}
	return nil
}

// PushMainProxy 实现 MainProxyVM：请求体是 YAML 原文，不进任何日志；错误里只有状态码和 agent 的简短说明。
func (v ExternalVM) PushMainProxy(ctx context.Context, yaml []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://abroad-agent/v1/main-proxy", bytes.NewReader(yaml))
	if err != nil {
		return err
	}
	resp, err := unixClient(v.ctlSock(), MainPushTimeout).Do(req)
	if err != nil {
		return usererror.Wrap("把主代理配置下发到虚拟机", err, "请确认虚拟机在运行。")
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rb, &e)
		msg := strings.TrimSpace(e.Error)
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return usererror.Wrap("把主代理配置下发到虚拟机", fmt.Errorf("HTTP %d：%s", resp.StatusCode, msg), "请检查节点配置，或稍后再试。")
	}
	return nil
}

// MainProxyDigest 实现 MainProxyVM。
func (v ExternalVM) MainProxyDigest(ctx context.Context) (string, error) {
	var h struct {
		MainProxy struct {
			Digest string `json:"digest"`
		} `json:"main_proxy"`
	}
	if err := getJSON(ctx, v.ctlSock(), "http://abroad-agent/v1/health", &h); err != nil {
		return "", ErrVMNotReady
	}
	return h.MainProxy.Digest, nil
}

// HopPrivate 实现 MainProxyVM（host.sock 的 netstats 里的 dns_hop_private）。
func (v ExternalVM) HopPrivate(ctx context.Context) (int64, error) {
	var st struct {
		HopPrivate int64 `json:"dns_hop_private"`
	}
	if err := getJSON(ctx, v.hostSock(), "http://abroad-vm/v1/netstats", &st); err != nil {
		return 0, ErrVMNotReady
	}
	return st.HopPrivate, nil
}
