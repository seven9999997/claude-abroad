package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"claude-abroad/internal/agent"
	"claude-abroad/internal/managedproxy"
	"claude-abroad/internal/usernet"
	"go.yaml.in/yaml/v3"
)

// 主代理由 abroadd 持有（M5-4，m5-plan v2.2 决定 4、6、8）。存储在主代理的 profile 目录里（真实模式是
// --main-proxy-store，开发和测试是状态目录下的 secrets/main）：
//   - proxy/sources.json：managedproxy.Store（来源、节点、选中的节点、链）——订阅地址、节点的服务器、密码、密钥都在这里，
//     目录 0700、文件 0600、原子写（managedproxy.Write）；
//   - proxy/lan-confirm.json：局域网节点的用户确认，每条绑定“来源、节点、第一跳地址”，任一变了作废；
//   - proxy/applied.json：最近一次成功下发的选择（整份 Store 的快照）、配置摘要、第一跳名单，回滚和重启后重下发用。
// 查询接口只回名字、类型、节点数、更新时间、节点显示名和协议类型（views），不回服务器、端口、密码、订阅地址。

// 来源的种类（查询里给）。
const (
	SourceSubscription = "subscription"
	SourceLocal        = "local" // 本地配置文件内容或手动填写的节点
)

// ErrMainInUse 是要删除的来源里有正在使用的节点。
var ErrMainInUse = errors.New("来源正在使用")

// MainProxyNodeView 是查询里的一个节点（不含服务器、端口、密码）。
type MainProxyNodeView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	// UDP 是这个节点最近一次出口检查得到的 UDP 是否可用（补齐二：available、unavailable、unknown）；只在来源列表里给。
	UDP string `json:"udp,omitempty"`
}

// MainProxySourceView 是查询里的一个来源（不含订阅地址）。
type MainProxySourceView struct {
	ID      string              `json:"id"`
	Name    string              `json:"name"`
	Kind    string              `json:"kind"`
	Updated time.Time           `json:"updated"`
	Error   string              `json:"error,omitempty"`
	Skipped int                 `json:"skipped,omitempty"`
	Nodes   []MainProxyNodeView `json:"nodes"`
}

// MainProxyView 是 GET /v1/main-proxy。
type MainProxyView struct {
	Selected      *MainProxyNodeView  `json:"selected,omitempty"` // 保存的选择
	Chain         []MainProxyNodeView `json:"chain,omitempty"`
	Applied       bool                `json:"applied"`        // 保存的选择已经下发到虚拟机
	NeedsReselect bool                `json:"needs_reselect"` // 订阅更新后选中的节点不在了
	LANPending    bool                `json:"lan_pending"`    // 第一跳是局域网节点，还没有用户确认
	Sources       int                 `json:"sources"`
	Note          string              `json:"note,omitempty"`
}

type lanConfirm struct {
	Source string `json:"source"`
	Node   string `json:"node"`
	Hop    string `json:"hop"` // IPv4:端口
}

// appliedRecord 是最近一次成功下发的选择。
type appliedRecord struct {
	Store  managedproxy.Store `json:"store"`
	Digest string             `json:"digest"`
	Hops   usernet.MainHops   `json:"hops"`
}

// MainStore 是主代理的存储（并发安全；文件操作串行）。
type MainStore struct {
	Profile string
	mu      sync.Mutex
	// failSave 只给测试注入“写盘失败”（nil 表示正常）。
	failSave func() error
}

func (m *MainStore) file(name string) string { return filepath.Join(managedproxy.Dir(m.Profile), name) }

func (m *MainStore) load() (managedproxy.Store, error) { return managedproxy.Load(m.Profile) }
func (m *MainStore) save(s managedproxy.Store) error {
	if m.failSave != nil {
		if err := m.failSave(); err != nil {
			return err
		}
	}
	return s.Save(m.Profile)
}

func readJSONFile(path string, v any) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return managedproxy.Write(path, b)
}

func (m *MainStore) confirms() ([]lanConfirm, error) {
	var cs []lanConfirm
	_, err := readJSONFile(m.file("lan-confirm.json"), &cs)
	return cs, err
}

func (m *MainStore) applied() (*appliedRecord, error) {
	var a appliedRecord
	ok, err := readJSONFile(m.file("applied.json"), &a)
	if err != nil || !ok {
		return nil, err
	}
	return &a, nil
}

func (m *MainStore) saveApplied(a appliedRecord) error {
	return writeJSONFile(m.file("applied.json"), a)
}

// sourceOf 返回节点所在的来源。
func sourceOf(s managedproxy.Store, nodeID string) (managedproxy.Source, bool) {
	for _, src := range s.Sources {
		for _, n := range src.Nodes {
			if n.ID == nodeID {
				return src, true
			}
		}
	}
	return managedproxy.Source{}, false
}

func nodeView(s managedproxy.Store, id string) MainProxyNodeView {
	n, _ := s.Find(id)
	return MainProxyNodeView{ID: n.ID, Name: n.Name, Type: n.Type}
}

func sourceViews(s managedproxy.Store) []MainProxySourceView {
	out := []MainProxySourceView{}
	for _, src := range s.Sources {
		v := MainProxySourceView{ID: src.ID, Name: src.Name, Kind: SourceLocal, Updated: src.Updated, Error: src.Error, Skipped: len(src.Skipped), Nodes: []MainProxyNodeView{}}
		if src.URL != "" {
			v.Kind = SourceSubscription
		}
		for _, n := range src.Nodes {
			v.Nodes = append(v.Nodes, MainProxyNodeView{ID: n.ID, Name: n.Name, Type: n.Type})
		}
		out = append(out, v)
	}
	return out
}

// mainPlan 是一份选择算出的下发内容。
type mainPlan struct {
	store  managedproxy.Store
	yaml   []byte
	digest string
	hops   usernet.MainHops
	lan    []lanConfirm // 需要确认的局域网第一跳（已确认的 Confirmed=true 填进 hops）
}

// ErrMainLoopback、ErrMainLANPending 是计划阶段的拒绝原因。
var (
	ErrMainLoopback   = errors.New("主代理的第一跳是本机回环（本机的代理软件不能当主代理）")
	ErrMainLANPending = errors.New("主代理的第一跳是局域网节点，还没有确认")
)

// plan 按 s 的选择算出下发内容：虚拟机配置、第一跳名单（白名单只有链上第一个节点）、局域网确认的核对。
func (m *MainStore) plan(s managedproxy.Store) (mainPlan, error) {
	cfg, err := s.VMRuntimeConfig()
	if err != nil {
		return mainPlan{}, err
	}
	route, _ := s.Route()
	first := ""
	if len(route) > 0 {
		first = route[0]
	}
	src, _ := sourceOf(s, first)
	cs, err := m.confirms()
	if err != nil {
		return mainPlan{}, err
	}
	p := mainPlan{store: s, yaml: cfg.YAML, digest: agent.ConfigDigest(cfg.YAML), hops: usernet.MainHops{Hops: cfg.FirstHop}}
	if p.hops.Hops == nil {
		p.hops.Hops = []string{}
	}
	pending := false
	for _, a := range cfg.Allow {
		ap, err := netip.ParseAddrPort(a)
		if err != nil {
			return mainPlan{}, fmt.Errorf("第一跳的地址不认识：%w", err)
		}
		if ap.Addr().IsLoopback() {
			return mainPlan{}, ErrMainLoopback
		}
		want := lanConfirm{Source: src.ID, Node: first, Hop: ap.String()}
		ok := false
		for _, c := range cs {
			if c == want {
				ok = true
			}
		}
		p.lan = append(p.lan, want)
		if !ok {
			pending = true
			continue
		}
		p.hops.LAN = append(p.hops.LAN, usernet.MainLAN{Addr: ap.String(), Confirmed: true})
	}
	if pending {
		return p, ErrMainLANPending
	}
	if _, _, err := usernet.ValidateMainHops(p.hops); err != nil {
		return mainPlan{}, err
	}
	return p, nil
}

// confirmLAN 记下 nodeID 作为第一跳时的局域网确认（绑定来源、节点、第一跳地址）。
func (m *MainStore) confirmLAN(nodeID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.load()
	if err != nil {
		return 0, err
	}
	if _, ok := s.Find(nodeID); !ok {
		return 0, errors.New("没有这个节点")
	}
	cand := s
	cand.Selected, cand.Chain = nodeID, nil
	p, err := m.plan(cand)
	if err != nil && !errors.Is(err, ErrMainLANPending) {
		return 0, err
	}
	if len(p.lan) == 0 {
		return 0, errors.New("这个节点的第一跳不是局域网地址，不需要确认")
	}
	cs, err := m.confirms()
	if err != nil {
		return 0, err
	}
	for _, want := range p.lan {
		dup := false
		for _, c := range cs {
			if c == want {
				dup = true
			}
		}
		if !dup {
			cs = append(cs, want)
		}
	}
	return len(p.lan), writeJSONFile(m.file("lan-confirm.json"), cs)
}

// pruneConfirms 去掉已经不成立的确认（节点或来源没了、第一跳地址变了）：每次来源变化之后调用。
func (m *MainStore) pruneConfirms(s managedproxy.Store) error {
	cs, err := m.confirms()
	if err != nil || len(cs) == 0 {
		return err
	}
	var keep []lanConfirm
	for _, c := range cs {
		src, ok := sourceOf(s, c.Node)
		if !ok || src.ID != c.Source {
			continue
		}
		cand := s
		cand.Selected, cand.Chain = c.Node, nil
		p, err := m.plan(cand)
		if err != nil && !errors.Is(err, ErrMainLANPending) {
			continue
		}
		for _, x := range p.lan {
			if x == c {
				keep = append(keep, c)
				break
			}
		}
	}
	if len(keep) == len(cs) {
		return nil
	}
	return writeJSONFile(m.file("lan-confirm.json"), keep)
}

// ---- 来源 ----

// AddSourceRequest 是 POST /v1/main-proxy/sources。三种形式：Kind 为 subscription（URL）、yaml（Content）、node（Node）。
type AddSourceRequest struct {
	Kind    string         `json:"kind"`
	Name    string         `json:"name"`
	URL     string         `json:"url,omitempty"`
	Content string         `json:"content,omitempty"`
	Node    map[string]any `json:"node,omitempty"`
	ViaHost bool           `json:"via_host,omitempty"` // 未启用时经本机网络抓取订阅（要调用方显式选择）
}

// RefreshRequest 是 POST /v1/main-proxy/sources/{id}/refresh。
type RefreshRequest struct {
	ViaHost bool `json:"via_host,omitempty"`
}

// nodeYAML 把手动填写的单个节点变成一份只有它的配置（之后经 managedproxy 的白名单校验）。
func nodeYAML(name string, node map[string]any) ([]byte, error) {
	if len(node) == 0 {
		return nil, errors.New("没有填写节点")
	}
	n := map[string]any{}
	for k, v := range node {
		n[k] = v
	}
	n["name"] = strings.TrimSpace(name)
	return yaml.Marshal(map[string]any{"proxies": []any{n}})
}

// addSource 解析并加入一个来源（data 是已经拿到的配置内容；url 只对订阅有）。返回来源 ID。
func (m *MainStore) addSource(name, url string, data []byte) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.load()
	if err != nil {
		return "", err
	}
	id := managedproxy.Token()[:16]
	if err := s.Import(name, url, data, id, 0, ""); err != nil {
		return "", err
	}
	if err := m.save(s); err != nil {
		return "", err
	}
	return id, nil
}

// deleteSource 删除来源；保存的选择或已下发的选择用到它的节点时拒绝。
func (m *MainStore) deleteSource(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.load()
	if err != nil {
		return err
	}
	idx := -1
	for i, src := range s.Sources {
		if src.ID == id {
			idx = i
		}
	}
	if idx < 0 {
		return os.ErrNotExist
	}
	used := map[string]bool{}
	if r, err := s.Route(); err == nil {
		for _, n := range r {
			used[n] = true
		}
	}
	if a, _ := m.applied(); a != nil {
		if r, err := a.Store.Route(); err == nil {
			for _, n := range r {
				used[n] = true
			}
		}
	}
	for _, n := range s.Sources[idx].Nodes {
		if used[n.ID] {
			return fmt.Errorf("%w：节点 %q 正在作为主代理（或链上的一跳）使用，请先换成别的节点", ErrMainInUse, n.Name)
		}
	}
	s.Sources = append(s.Sources[:idx], s.Sources[idx+1:]...)
	if err := m.save(s); err != nil {
		return err
	}
	return m.pruneConfirms(s)
}

// refreshSource 用新拿到的内容更新订阅的节点列表：不自动下发；选中的节点不在了时保存的选择保留（标为需要重新选择），
// 正在运行的主代理（已下发的快照）不受影响。返回是否需要重新选择。
func (m *MainStore) refreshSource(id string, data []byte) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.load()
	if err != nil {
		return false, err
	}
	var src *managedproxy.Source
	for i := range s.Sources {
		if s.Sources[i].ID == id {
			src = &s.Sources[i]
		}
	}
	if src == nil {
		return false, os.ErrNotExist
	}
	// managedproxy.Import 在更新会移除当前路线的节点时拒绝；这里按 v2.2 决定 5 改为“更新、标需要重新选择”：
	// 先在不带选择的副本上导入，再把选择放回去。
	sel, chain := s.Selected, s.Chain
	tmp := s
	tmp.Selected, tmp.Chain = "", nil
	if err := tmp.Import(src.Name, src.URL, data, id, src.Interval, src.Usage); err != nil {
		return false, err
	}
	tmp.Selected, tmp.Chain = sel, chain
	if err := m.save(tmp); err != nil {
		return false, err
	}
	if err := m.pruneConfirms(tmp); err != nil {
		return false, err
	}
	_, rerr := tmp.Route()
	return sel != "" && rerr != nil, nil
}

// view 是 GET /v1/main-proxy。
func (m *MainStore) view() (MainProxyView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.load()
	if err != nil {
		return MainProxyView{}, err
	}
	v := MainProxyView{Sources: len(s.Sources)}
	if s.Selected != "" {
		if _, ok := s.Find(s.Selected); ok {
			nv := nodeView(s, s.Selected)
			v.Selected = &nv
		}
		if _, err := s.Route(); err != nil {
			v.NeedsReselect = true
			v.Note = "订阅更新后保存的选择不完整：请重新选择节点；正在运行的主代理不受影响"
		}
		for _, id := range s.Chain {
			v.Chain = append(v.Chain, nodeView(s, id))
		}
		if !v.NeedsReselect {
			p, perr := m.plan(s)
			if errors.Is(perr, ErrMainLANPending) {
				v.LANPending = true
			}
			if a, _ := m.applied(); a != nil && perr == nil && a.Digest == p.digest {
				v.Applied = true
			}
		}
	}
	return v, nil
}

// sortedIDs 用于稳定输出。
func sortedIDs(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
