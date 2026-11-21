package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"

	"claude-abroad/internal/guestabi"
)

// SingBoxExit 是 sing-box 的一个出口：socks5 出站，指向虚拟机回环地址上的某个引擎端口。
type SingBoxExit struct {
	Tag    string // 出口标签（数据面帧头里的 exit），也是 socks 入站的用户名
	Port   int    // 端口
	Server string // 可选：空表示 127.0.0.1（虚拟机里的引擎）；开发用的多出口可以是网关上的回环映射地址（m3-plan v2.8 决定 7）
}

var exitTagRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

type sbConfig struct {
	Log       sbLog        `json:"log"`
	Inbounds  []sbInbound  `json:"inbounds"`
	Outbounds []sbOutbound `json:"outbounds"`
	Route     sbRoute      `json:"route"`
}

type sbLog struct {
	Level     string `json:"level"`
	Timestamp bool   `json:"timestamp"`
}

type sbUser struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type sbInbound struct {
	Type       string   `json:"type"`
	Tag        string   `json:"tag"`
	Listen     string   `json:"listen"`
	ListenPort int      `json:"listen_port"`
	Users      []sbUser `json:"users"`
}

type sbOutbound struct {
	Type       string `json:"type"`
	Tag        string `json:"tag"`
	Server     string `json:"server"`
	ServerPort int    `json:"server_port"`
	Version    string `json:"version"`
}

type sbRule struct {
	AuthUser []string `json:"auth_user,omitempty"`
	Network  []string `json:"network,omitempty"`
	Action   string   `json:"action"`
	Outbound string   `json:"outbound,omitempty"`
}

type sbRoute struct {
	Rules []sbRule `json:"rules"`
	Final string   `json:"final"`
}

// SingBoxConfig 生成分流核心 sing-box 的配置（设计文档 4.4、7.2；sing-box 1.14）。
//
// 未匹配即拒绝（docs/m1-plan.md 底线 1）：
//   - 没有 direct 出站；sing-box 1.13 起也没有 block 出站，拒绝靠末尾无条件的 reject 规则；
//   - outbounds 不能为空：为空时 sing-box 会自动补一个 direct 出站，是泄漏路径，所以出口表为空直接报错；
//   - route.final 显式指向 main（只是兜底：入站要求认证，每个用户名都有规则，走不到 final）。
//
// 入站只有一个 socks5，只听 127.0.0.1，每个出口一个用户（用户名 = 出口标签，口令每次开机随机），
// 数据面以 auth_user=<出口> 连进来，sing-box 按用户名选出口。
// 不写 dns、ntp、experimental、endpoints，不写任何网卡绑定字段。
func SingBoxConfig(exits []SingBoxExit, password string) ([]byte, error) {
	if len(exits) == 0 {
		return nil, errors.New("出口表为空，不生成 sing-box 配置（outbounds 为空时 sing-box 会自动补 direct 出站）")
	}
	if len(password) < 16 {
		return nil, errors.New("socks 口令太短")
	}
	c := sbConfig{
		Log: sbLog{Level: "warn", Timestamp: true},
		Inbounds: []sbInbound{{
			Type: "socks", Tag: "in", Listen: "127.0.0.1", ListenPort: guestabi.SingBoxSocksPort,
		}},
	}
	seen := map[string]bool{}
	hasMain := false
	for _, e := range exits {
		if !exitTagRe.MatchString(e.Tag) {
			return nil, fmt.Errorf("出口标签 %q 不合法", e.Tag)
		}
		if seen[e.Tag] {
			return nil, fmt.Errorf("出口标签 %q 重复", e.Tag)
		}
		if e.Port <= 0 || e.Port > 65535 || e.Port == guestabi.SingBoxSocksPort {
			return nil, fmt.Errorf("出口 %s 的端口 %d 不合法", e.Tag, e.Port)
		}
		server := e.Server
		switch server {
		case "":
			server = "127.0.0.1"
		case "127.0.0.1", guestabi.HostLoopbackIP:
		default:
			// 只收两个回环地址和出口命名空间的 veth 网段（M4-7）：不能借出口让虚拟机连任意地址。
			if a, err := netip.ParseAddr(server); err != nil || !a.Is4() || !ExitNet.Contains(a) {
				return nil, fmt.Errorf("出口 %s 的服务器只能是 127.0.0.1、%s 或 %s 里的地址", e.Tag, guestabi.HostLoopbackIP, ExitNet)
			}
		}
		seen[e.Tag] = true
		hasMain = hasMain || e.Tag == guestabi.ExitMain
		c.Inbounds[0].Users = append(c.Inbounds[0].Users, sbUser{Username: e.Tag, Password: password})
		c.Outbounds = append(c.Outbounds, sbOutbound{
			Type: "socks", Tag: e.Tag, Server: server, ServerPort: e.Port, Version: "5",
		})
		c.Route.Rules = append(c.Route.Rules, sbRule{AuthUser: []string{e.Tag}, Action: "route", Outbound: e.Tag})
	}
	if !hasMain {
		return nil, errors.New("出口表里没有 main（主代理是唯一必须有的出口）")
	}
	c.Route.Rules = append(c.Route.Rules, sbRule{Network: []string{"tcp", "udp"}, Action: "reject"})
	c.Route.Final = guestabi.ExitMain
	return json.MarshalIndent(c, "", "  ")
}

// SingBoxConfigCheck 解析配置并返回出口标签（测试和排错用）。
func SingBoxConfigCheck(b []byte) ([]string, error) {
	var c sbConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if len(c.Outbounds) == 0 || c.Route.Final == "" {
		return nil, errors.New("没有出站或没有 route.final")
	}
	var tags []string
	for _, o := range c.Outbounds {
		tags = append(tags, o.Tag)
	}
	return tags, nil
}
