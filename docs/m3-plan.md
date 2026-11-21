# M3 实施计划：分流

> 状态：v2.11（2026-11-19：M3-8 实施计划审核：三处偏差的结论、两条必须改、三条建议改，见 M3-8 一节末尾）。v2.10（2026-11-19：M3-7 验收通过；M3-8 的 7 条设计决定和验收标准 a–j，见 M3-8 一节末尾）。v2.9（2026-11-19：M3-7 审核的必须改：直连配置带代数、按需补发，见 M3-7 一节末尾）。v2.8（2026-11-19：M3-7 的 7 条设计决定，同时作为 M3-7 的实施计划，见 M3-7 一节末尾）。v2.7（2026-11-19：M3-6 的 5 条设计决定，补充 4 的条件改为三者都为空，见 M3-6 一节末尾）。v2.6（2026-11-19：M3-4 验收的判定、别名链判置顶、M3-5 的 6 条补充和 M3-6 的一条补充，见 M3-5、M3-6 两节末尾）。v2.5（2026-11-19：M3-3 验收的两件事判定、M3-4 的 6 条补充，见 M3-4 一节末尾）。v2.4（2026-11-19：M3-3 的 8 条补充决定和实测的连接表布局、权限结论，见 M3-3 一节末尾）。v2.3（2026-11-19：M3-2 的 6 条补充决定，见 M3-2 一节末尾）。v2.2（2026-11-19：3.3 的条件改为“此时的出口是 main”，architect 审 M3-1 时指出）。v2.1（2026-11-18，复审通过；v2.1 加了 architect 复审时的 2 条补充：优先级表 2.7、`unknown` 不能写进规则）。v2 按 abroad-architect 审 v1 的 4 条必须改、5 条建议改和 4 点拍板修订；改动的段落标了【v2】。
> 依据：设计文档 v0.6 第 4.2、4.3、5、6.3、10、14 节；architect 2026-11-18 给出的 M3 开工决定（4 条）和 M3 待办清单。

## 0. 总原则

### 0.1 会话里能做什么

沿用 m2-plan 0.1，宿主机唯一的禁区是断网。M3 新增的部分大多不碰宿主网络：

- **纯计算**：规则引擎、首包识别、应用 ID 映射、帧格式。全部单测，必要时加模糊测试。
- **只读实跑**：查连接表（`sysctl net.inet.tcp.pcblist_n`）、读进程路径和父进程（`kern.procargs2`、`kern.proc.pid`）、读应用包的 Info.plist。这些都不需要 root，也不改系统状态；以当前用户身份运行，查别的用户（root）的进程可能失败，这一部分交给用户。
- **进程内全链路**：`tun.Pipe` → 网络栈 → 规则 → 假出口或假直连拨号器，沿用 M2 的测试台。
- **真实虚拟机**：vm-up 新阶段，多出口用开发上游模拟（决定 4），经 data.sock 和 daemon-sim 跑，不接管宿主网络。
- **直连拨号模块的真实拨号**：开发模式以当前用户运行，绑物理网卡连公网（和 abroad-vm 现在的出站一样），不改路由、不加载 pf。

**会话里绝不执行**：沿用 m2-plan 0.1 的清单（sudo、pfctl 除 `-n` 例外、networksetup -set*、ifconfig、route 写操作、scutil 写操作、创建 utun）。

### 0.2 需要用户的部分

都归到 M3-8 的真机脚本，标“需要用户”：
- Chrome、curl、微信同时运行的分流验证（需要真的接管）；
- 把 Chrome 设成本机网络后访问 claude.ai，出口仍是主代理；
- R3 的 root 部分（识别 root 进程和系统进程的连接）；
- R15（Docker、OrbStack 等）；
- 直连拨号模块以 `_abroad` 身份运行时，pf 的 `user _abroad` 是否放行（和 R2 同一个问题，R2 的结论直接沿用）。

### 0.3 M3 开工的 4 条决定（已回写 design v0.6）

1. 不支持把本机代理软件当主代理（4.5、10.4）。
2. “本机网络”只对看得清去向的连接生效（5.2）。
3. 直连应用的域名由直连拨号模块用本机原 DNS 解析，置顶域名永远不经本机 DNS；`local_domains` 同路；私有网段 PTR 本地回 NXDOMAIN（4.3、6.3）。
4. M3 的出口 A、B 用开发上游模拟（14）。

### 0.4 R14 的结论（已写进 design 15）

只读，单一观测点：
- **解析结果**：claude.ai、claude.com、anthropic.com，以及 www、console、api 三个 anthropic.com 子域：
  - A 记录都在 `160.79.104.0/21`；
  - AAAA 记录在 `2607:6bc0::/48`。
- **地址段的归属**：
  - RDAP 显示这个 /21 登记给 Anthropic, PBC；
  - Anthropic 文档公布的入站地址段是 `160.79.104.0/23`。
- **CDN**：从这个观测点看，用的是专用地址段，没有和别的网站共用 CDN。
- **ECH**：HTTPS 记录有 ipv4hint，目前没有 ech 参数。

对 M3 的影响：
- 地址段只作“更严格”的补充匹配：目的地址在段内就当成置顶，不能作为放行依据。
- 决定 2 的方向不变。ECH 和“没有明文域名”的分支仍然必须改走主代理，理由有三：
  - 子域可能托管在别处；
  - 地址段以后可能变；
  - 浏览器以后可能对它启用 ECH。

## 1. 任务总览与执行顺序

| 任务 | 内容 | 依赖 | 需要用户 |
|---|---|---|---|
| M3-1 | 规则引擎 `internal/rules` | 无 | 否 |
| M3-2 | 首包识别 `internal/sniff` | 无 | 否 |
| M3-3 | 应用识别 `internal/procinfo` | 无 | 否（root 部分归 M3-8） |
| M3-4 | 直连拨号模块 `internal/directdial` + 直连 DNS + fakedns 的 `local_domains`、私有 PTR | M3-1 的置顶匹配 | 否 |
| M3-5 | 接进网络栈：`internal/stack` 的流处理改为“识别 → 判定 → 开流、直连或拦截” | M3-1 到 M3-4 | 否 |
| M3-6 | 停用状态下的 `offline_allow`；规则修改即时生效、可选断开受影响的连接 | M3-5 | 否 |
| M3-7 | 接进 daemon：规则读写接口、规则文件；daemon-sim 多出口多应用；vm-up 新阶段 | M3-6 | 否 |
| M3-8 | 给用户的真机脚本 `scripts/m3/` | M3-7 | **是**（主会话不执行） |

**顺序**：按 architect 建议的 M3-1 → M3-2 → M3-3 → M3-4 → M3-5 → M3-6 → M3-7 → M3-8，不调整。
- M3-1、M3-2、M3-3 互不依赖，都是纯计算或只读，先做能尽早把接口定死。
- M3-4 要用 M3-1 的置顶匹配（拒绝置顶域名的解析），排在后面。

每个任务做完单独交验收。

## 2. 各任务

### M3-1 规则引擎 `internal/rules`

**文件**：`internal/rules/{config.go, match.go, decide.go, pinned.go}` 及测试。

**接口**：

```go
// 应用 ID：5.1 的各类。
type AppID string // "com.google.Chrome"、"path:/opt/homebrew/bin/git"、"system:mDNSResponder"、"unknown"

type Kind int // Proxy、Direct、Block
type Action struct {
    Kind Kind
    Exit string // Kind==Proxy 时的出口标签
}

// Config 是 5.2 的 YAML，加 block_udp_443。
type Config struct {
    Default       Action
    Rules         []Rule // {App AppID; Action Action}，按顺序匹配
    OfflineAllow  []AppID
    LocalDomains  []string
    PinnedExtra   []string // 用户追加的置顶域名，内置的永远在
    BlockUDP443   bool
}
func Parse(b []byte) (*Config, error)       // 校验见下
func (c *Config) Marshal() ([]byte, error)

// 去向信息：网络栈收集，规则引擎只读。
type SniffKind int // None（没嗅探）、NotTLS、TLSName、TLSNoName、TLSECH、QUICName、QUICNoName、HTTPHost、Timeout
type Dest struct {
    Net        string         // "tcp" / "udp"
    Addr       netip.AddrPort // 原始目的地址（假地址或真实地址）
    FakeDomain string         // fake-IP 映射出的域名；空表示不是假地址
    Sniff      SniffKind
    SniffName  string         // TLSName、QUICName、HTTPHost 时的域名
    Reverse    string         // 反查表的结果，只用来变严格
    Local      bool           // 【v2】fake 映射带“本地域名”标记（local_domains，4.3）
    Refused    bool           // 【v2】直连拨号模块回了 pinned（解析结果落在 Anthropic 地址段），Router 带着它重判
}

// 【v2】连接属于谁：来自 procinfo。
type Who struct {
    App    AppID
    Claude bool // 属于 10.1 清单里的 Claude 进程（M3 按路径和命令行识别，代码签名校验在 M6）
}

type Env struct {
    State   State             // Enabled、Disabled
    GateOpen bool
    ExitUp  func(exit string) bool // 出口是否可用（M3 由 daemon 按配置和 exit_down 给出，M4 换成出口检查）
    UDPOK   func(exit string) bool // 出口是否支持 UDP（含负缓存）
}

type Reason string // "pinned"、"opaque_tls"、"port443_timeout"、"anthropic_net"、"exit_fallback"、"gate_closed"、"offline"…
type Decision struct {
    Action   Action
    Reason   Reason
    Fallback bool // 规则指向的出口不可用而回退到主代理
}

// NeedSniff：只有应用规则（或 offline_allow）可能得到 Direct、且没有假地址映射时才需要首包。
func (c *Config) NeedSniff(w Who, d Dest, e Env) bool
func (c *Config) Decide(w Who, d Dest, e Env) Decision
func IsPinned(name string, extra []string) bool
func InAnthropicNet(a netip.Addr) bool
```

**判定的优先级表**【v2，重写】：Decide 按下面的阶段从上到下执行，每个阶段内按行号从小到大，命中的第一行生效。两条规则冲突时，阶段靠前、行号较小的优先。

**阶段 1：候选动作**

| 行 | 条件 | 候选动作 | Reason |
|---|---|---|---|
| 1.1 | 应用规则匹配到 `block` | Block，后面任何阶段都不会放松 | `rule_block` |
| 1.2 | `d.Local`，并且状态是 ENABLED | Direct，不看应用规则 | `local_domain` |
| 1.3 | 状态是 DISABLED，并且应用在 offline_allow 里 | Direct | `offline` |
| 1.4 | 状态是 DISABLED，其他情况 | Block | `offline` |
| 1.5 | 应用规则匹配到 `direct` 或 `proxy:X` | 规则的动作 | `rule` |
| 1.6 | 都不匹配 | Default（`proxy:main` 或 `block`） | `default` |

**阶段 2：直连的收紧**（只在候选动作是 Direct 时执行；命中任意一行就改成 `proxy:main`，Reason 取第一条命中的）

| 行 | 条件 | Reason |
|---|---|---|
| 2.1 | `w.Claude`（Claude 进程） | `claude_process` |
| 2.2 | 置顶域名：依次看 FakeDomain、SniffName、Reverse | `pinned` |
| 2.3 | `d.Refused`（直连拨号模块解析出的地址落在 Anthropic 地址段） | `pinned_resolved` |
| 2.4 | 目的地址（真实 IP）在 Anthropic 地址段内 | `anthropic_net` |
| 2.5 | 没有 fake 映射，首包是 TLSNoName、TLSECH 或 QUICNoName | `opaque_tls` |
| 2.6 | 没有 fake 映射，首包超时（Timeout），并且端口是 443 | `port443_timeout` |
| 2.7 | 【v2.1】没有 fake 映射，并且没有任何首包结果（`Sniff` 是 None） | `no_sniff` |

2.7 是兜底：要不要读首包由 `NeedSniff` 决定，能不能直连由 `Decide` 决定。两者以后一旦不一致，也不会出现没看首包就直连的路径，`Decide` 自己就是安全的。

阶段 2 的结果都是 `proxy:main`，行号只决定 Reason。改成 `proxy:main` 之后继续执行阶段 3 到 5。在 DISABLED 状态下，阶段 5 会把它拦下，所以停用时置顶域名和 Claude 进程都不可达。

**阶段 3：出口回退**（只在动作是 `proxy:X` 时执行）

| 行 | 条件 | 结果 |
|---|---|---|
| 3.1 | X 不是 main，并且 `!ExitUp(X)` | `proxy:main`，`Fallback=true`，Reason `exit_fallback` |
| 3.2 | 是 UDP，X 不是 main，并且 `!UDPOK(X)` | `proxy:main`，Reason `udp_fallback` |
| 3.3 | 【v2.2】是 UDP，此时的出口是 main（规则本来指向 main，或经 3.1、3.2 回退而来），并且 `!UDPOK(main)` | Block，Reason `udp_unsupported` |

主代理本身不可用时，不退回直连：交给阶段 5 的闸门处理，闸门在主代理不可用时本来就是关的。

**阶段 4：拦截 UDP 443**

| 行 | 条件 | 结果 |
|---|---|---|
| 4.1 | 开了 BlockUDP443，并且是 UDP、端口 443，动作是 Direct 或 Proxy | Block，Reason `block_udp443` |

**阶段 5：闸门与状态**

| 行 | 条件 | 结果 |
|---|---|---|
| 5.1 | 动作是 Proxy，并且闸门关闭（含 DISABLED） | Block，Reason `gate_closed` |
| 5.2 | 其他情况 | 保持 |

**由表直接得出的结论**（性质测试逐条断言）：
- Claude 进程、置顶域名、Refused、Anthropic 地址段、看不清去向的 TLS/QUIC、443 端口超时，这六种情况在任何配置和状态下都不会得到 Direct（阶段 2）。
- DISABLED 下只有 offline_allow 里的应用可能得到 Direct（1.3、1.4），而且同样受阶段 2 约束。
- `local_domains` 只在 ENABLED 下才会无视应用规则（1.2），也受阶段 2 约束。
- `block` 规则永远不会被放松（1.1）。
- 主代理不可用、出口不可用都不会退回直连（阶段 3 只会产出 Proxy 或 Block）。

**配置校验**（Parse）：
- default 只能是 `proxy:main` 或 `block`；
- 规则里的出口标签合法（与 `agent.exitTagRe` 同一个字符集）；
- 应用 ID 合法；
- `local_domains` 和置顶域名不能有交集，包括通配展开后的交集；
- 内置置顶域名不能删除；
- 已知代理软件的应用 ID 不能设成 `direct`，也不能进 offline_allow。名单见 10.4：Clash Verge、ClashX、Surge、FlClash、sing-box、V2rayU 等，放在 `rules/proxyapps.go`。
- 【v2.1】应用 ID `unknown` 不能出现在 rules 或 offline_allow 里，任何动作都不行；识别不到的连接只走 default。`system:` 开头的可以配规则。
- 【v2】10.1 清单里 Claude 的应用 ID（Claude 桌面版、Claude Code）不能设成 `direct`，也不能进 offline_allow。清单放在 `rules/claudeapps.go`，procinfo 共用这份清单。
- 未知字段报错。

**测试**：
- 表驱动：优先级表的每一行各至少一组用例，另外为每对可能冲突的行准备组合用例（例如 1.2 加 2.1、1.3 加 2.2、2.5 加 fake 映射）。
- 性质测试（`testing/quick` 或自写随机生成器）：生成上万组随机配置、应用、去向、环境，断言：
  - 【v2】`w.Claude` 为真、去向是置顶域名（任何来源）、`d.Refused`、TLS 或 QUIC 不透明、在 Anthropic 地址段内、首包超时且是 443 端口，这些情况下都不会得到 Direct；
  - 【v2】`d.Local` 只在 ENABLED 下带来 Direct，并且同样受阶段 2 约束；
  - Default 永远不会是分流出口；
  - 闸门关闭时永远不会得到 Proxy；
  - DISABLED 下不在 offline_allow 的应用永远不会得到 Direct；
  - Decide 是纯函数：同样的输入两次结果相同。
  - 【v2.1】没有 fake 映射且 `Sniff` 为 None 时，永远不会得到 Direct；
  - 【v2.1】一致性：凡是 `Decide` 在给出某个首包结果时可能得到 Direct 的输入，`NeedSniff` 都必须为真。
- Parse 的反例逐条覆盖。

### M3-2 首包识别 `internal/sniff`

**文件**：`internal/sniff/{sniff.go, tls.go, quic.go, http.go}`，测试数据放 `testdata/`。

**接口**：

```go
type Result struct {
    Kind rules.SniffKind
    Name string
}
const MaxPeek = 16 << 10          // 最多读 16 KiB（分片的 ClientHello、带 PQ 密钥共享的大 ClientHello）
const Timeout = 300 * time.Millisecond

// TCP：从 conn 读首包，返回结果和已读字节（转发时先发这些）。超时返回 Kind=Timeout。
func PeekTCP(ctx context.Context, conn net.Conn) (Result, []byte, error)
// UDP：只看第一个数据报。
func QUIC(datagram []byte) Result
// 纯函数，单测用
func TLS(b []byte) (Result, needMore bool)
func HTTP(b []byte) (Result, needMore bool)
```

**要点**：
- TLS：
  - 支持 ClientHello 跨多个 TLS 记录（分片）；
  - 出现 `encrypted_client_hello`（0xfe0d）扩展时返回 TLSECH，即使外层带了 SNI，因为外层 SNI 是公开名，不是真实目标；
  - 没有 SNI 时返回 TLSNoName；
  - SNI 不是合法主机名时也按 TLSNoName 处理。
- QUIC：
  - 只认 v1 和 v2 的 Initial 包；
  - 按 RFC 9001 用 HKDF 和 AES-GCM 解出 CRYPTO 帧，只用标准库的 `crypto/hkdf`、`crypto/aes`、`crypto/cipher`；
  - CRYPTO 帧乱序时按偏移重组，只处理第一个数据报里的帧；
  - ClientHello 跨数据报时返回 QUICNoName，宁可严格；
  - 解不开或不是 Initial 时：长头且版本是 QUIC 的返回 QUICNoName，其他返回 NotTLS。
- HTTP：
  - 认请求行（方法在白名单里）和 `Host` 头，返回 HTTPHost；
  - `CONNECT` 请求取请求目标；
  - 首包是 HTTP 但没有 Host 时按 NotTLS 处理，因为明文 HTTP 不会承载 Claude。
- **区分“不是 TLS”和“是 TLS 但没有域名”**：
  - 首字节是 0x16、版本字段像 TLS，但解析失败或超出 MaxPeek 时返回 TLSNoName，不返回 NotTLS；
  - 只有明显不是 TLS、HTTP、QUIC 时才返回 NotTLS。

**测试**：
- 样本：
  - 用 Go 的 `crypto/tls` 客户端生成 ClientHello：带 SNI、不带 SNI、带 ECH（`tls.Config.EncryptedClientHelloConfigList` 加构造的配置）；
  - 用 RFC 9001 附录 A 的 QUIC Initial 样本；
  - 构造的分片样本、超长样本、乱序 CRYPTO 帧。
- 只读抓取：本机 `curl` 和 Go 程序经回环发出的 ClientHello，写进 testdata，脱敏后不带任何本机信息。
- 模糊测试：`FuzzTLS`、`FuzzQUIC`、`FuzzHTTP`，断言不崩溃、读取不越界、`needMore` 收敛。会话里各跑 60 秒，结果存证据。
- PeekTCP 的时限：
  - 服务端先说话的场景，到 300 毫秒返回 Timeout；
  - 慢速发送（每 10 毫秒 1 字节）的场景，到时限按已读内容判定（TLS 头已出现则返回 TLSNoName）。

**【v2.3】architect 的补充决定**（原则：凡是说不清的，一律不能落到 NotTLS，因为只有 NotTLS 能让直连成立）：
1. TCP 到时限时按已读内容判：
   - 一个字节都没收到：Timeout；
   - 已读内容是 TLS 记录头的前缀（哪怕只有 1 个字节 0x16），或是没收完的 ClientHello：TLSNoName；
   - 握手类型不是 ClientHello：TLSNoName；
   - 像 HTTP 请求但头部没收完、还没看到 Host：NotTLS。
2. UDP 的 QUIC 判定：
   - 长头，版本是 v1 或 v2，能解出 SNI：QUICName；
   - 首字节高两位都是 1，并且数据报不小于 1200 字节，但解不开、版本不认识或 ClientHello 跨数据报：QUICNoName；
   - 其他：NotTLS。
3. 名字归一用 `rules.NormalizeName`，返回归一后的形式；归一失败按没有域名处理（TLSNoName、QUICNoName；HTTP 为 NotTLS）。
4. PeekTCP 一旦能下结论就立即返回；返回时清掉自己设的读期限；已读字节原样交回，一个不多、一个不少。
5. 本包不打日志，返回的错误里不带读到的内容。
6. 已知限制（design 5.3）：Chrome 开安全 DNS 时所有 TLS 连接都带 ECH 扩展（没有配置时发占位的），分不出来，设成本机网络的浏览器加安全 DNS 的连接全部走主代理。

### M3-3 应用识别 `internal/procinfo`

**文件**：
- `internal/procinfo/{procinfo.go, appid.go, plist.go}`；
- `internal/procinfo/{pcb_darwin.go, proc_darwin.go}`，只做 sysctl 读取，不执行外部命令；
- `internal/procinfo/fake.go`。

**接口**：

```go
type Proc struct{ PID, PPID int; Path string }
type Table interface { // pcblist 的读取，可替换
    Lookup(net string, local, remote netip.AddrPort) (pid int, ok bool)
}
type Procs interface { // 进程信息，可替换
    Info(pid int) (Proc, error)
}
type Identifier struct {
    Table Table; Procs Procs
    Bundle func(appPath string) (string, error) // 读 Info.plist 的 CFBundleIdentifier
    InheritParent bool                           // 5.1 的“继承父进程”
    Retries int; RetryDelay time.Duration        // R3：查不到时短重试，默认 3 次、每次 20 毫秒
}
type Result struct { App rules.AppID; PID int; Claude bool }
func (i *Identifier) Identify(net string, src, dst netip.AddrPort) Result // 查不到 → App="unknown"
func AppIDForPath(path string, bundle func(string) (string, error)) rules.AppID // 纯函数
```

**要点**：
- **连接表**：
  - 用 `unix.SysctlRaw("net.inet.tcp.pcblist_n")` 和 `net.inet.udp.pcblist_n` 读取，解析 `xinpcb_n` 和 `xsocket_n`；
  - 进程优先取 `so_e_pid`（委托方），为 0 时取 `so_last_pid`；
  - 匹配条件是“本地端口 + 远端地址和端口”，UDP 未连接的套接字只按本地端口匹配；
  - 【v2】匹配到多个不同的进程时（例如端口复用），按识别失败处理，返回 `unknown`，不取第一个；
  - 结构布局写成带版本检查的解析器，长度字段不符时整体报错，不猜。
- **进程信息**：路径用 `kern.procargs2`，父进程用 `kern.proc.pid`，都不用 cgo，也不执行外部命令。
- **应用 ID**：
  - 路径里最外层的 `.app` 读 Info.plist，取 Bundle ID；
  - 不在应用包里的，返回 `path:<路径>`；
  - 系统进程归入共享或系统进程，ID 写成 `system:<名字>`；
  - **Claude 进程优先**：按 10.1 的清单识别，先于“继承父进程”判定，终端里启动的 `claude` 永远识别成 Claude Code。
  - 【v2】`Result.Claude` 传给规则引擎的 `Who.Claude`（优先级表 2.1）。只要进程本身或它往上任一级父进程命中清单，就置为真，例如 Claude Code 启动的 `git`、`node` 子进程。父进程最多往上找 16 级。
  - 【v2】**限制**：M3 只能按路径和命令行参数识别 Claude 进程，代码签名校验在 M6。改名或挪了位置的 Claude 可能识别不到，这时靠置顶域名和 Anthropic 地址段兜底，这两条不依赖应用识别。design 5.3 已写明。
- **Info.plist**【v2，architect 拍板：自己写】：
  - 只取 `CFBundleIdentifier` 一个键，支持 XML 和 bplist00 两种格式，不新增依赖；
  - 输入来自任意应用包，是不可信输入，而解析在 root 进程里做，所以要有上限：
    - 文件不超过 1 MiB；
    - 嵌套深度不超过 16；
    - 对象数不超过 65536；
    - bplist 的偏移表和引用越界时报错；
  - 模糊测试 `FuzzPlist`：不崩溃，不死循环，内存有界。
- **缓存**：
  - 识别结果不缓存，5.1 要求每条新连接重新查；
  - Bundle ID 按 .app 路径缓存，路径的 mtime 变化时失效；
  - 【v2】连接表快照可以短时共享：并发的 Identify 共用同一份不超过 20 毫秒的快照（singleflight 加时间戳）。5.1 禁止的是复用“识别结果”，不是复用快照。查不到时的重试一律重新读取，不用旧快照。
- 识别失败按 Default 规则处理，规则引擎里的 AppID 记为 `unknown`。

**测试**：
- AppIDForPath 表驱动：
  - Chrome Helper 的嵌套 .app；
  - Homebrew 的路径；
  - 系统进程；
  - Claude Code（npm、原生安装、Homebrew 三种路径）；
  - 继承父进程开、关两种情况。
- plist 解析：
  - 只读取本机 `/Applications`、`/System/Applications` 下的几个 Info.plist 作样本，样本里只有 Bundle ID，不带本机信息；
  - 构造的 bplist 反例。
- pcblist 解析：用构造的二进制样本，以及只读抓取的本机样本，脱敏后只保留结构。
- **只读实跑**：带 `//go:build integration`，会话里按放宽后的规则运行。本测试进程自己发起若干 TCP、UDP 连接到本机回环监听端口，Identify 应识别出本测试进程的路径。
- **R3 脚本** `scripts/m3/r3-accuracy.sh`：
  - 1000 个 curl 并发连本机回环服务，统计识别率和认错率；
  - 【v2】同时统计单次识别的耗时（P50、P99）和读取快照的次数；
  - 会话里以当前用户跑一次，存证据；
  - root 进程的部分写进 M3-8。

**【v2.4】architect 的补充决定**：
1. 连接表的布局按每条记录自带的长度和种类走，不认识就整体报错。**实测布局**（2026-11-19，macOS 27.2.1，26B131，只读 sysctl）：
   - 开头和结尾各一个 24 字节的 xinpgen；记录按 8 字节对齐；
   - 每个 pcb 由 XSO_INPCB 开头，后面依次是 SOCKET、RCVBUF、SNDBUF、STATS（TCP 还有 TCPCB）；
   - 各种类的长度：INPCB 104、SOCKET 104、RCVBUF 32、SNDBUF 32、STATS 136、TCPCB 204；
   - xinpcb_n 的字段：xi_inpp 在 8、fport 在 16、lport 在 18（都是网络字节序）、inp_vflag 在 44（0x1 是 IPv4，0x2 是 IPv6）、远端地址联合体在 48（IPv4 在 +12）、本地地址联合体在 64；
   - xsocket_n 的字段：so_pcb 在 28（等于 xi_inpp，用来对上连接记录）、xso_protocol 在 36、so_uid 在 64、so_last_pid 在 68、so_e_pid 在 72。
2. **权限实测**（普通用户，同上系统）：
   - 连接表：**只能看到本进程自己的套接字**，同一用户的其他进程（例如子进程 nc）也看不到。`netstat -anv` 以普通用户运行时一行都列不出，而 `net.inet.tcp.pcbcount` 是 264。
   - 进程信息：`kern.proc.pid`（父进程号）对任何进程都能读；`kern.procargs2`（路径和参数）只能读自己用户的进程，读 root 的进程（例如 1 号）返回 EINVAL。
   - 结论：真实模式下 abroadd 是 root，可以识别；会话里只能测本进程自己的连接。识别别的进程、root 和系统进程的部分，放到 `scripts/m3/r3-accuracy.sh`（需要 sudo，交给用户）和 M3-8。
3. 识别不到就是 `unknown`：连接表里找不到、匹配到多个进程、进程已退出、路径读不出来、Info.plist 读不出来或解析失败（不退回成 `path:`）。
4. 进程号复用：读出路径后，再用一份**在读出路径之后才开始读**的连接表复核归属，不一致就是 `unknown`。这只能缩小窗口，已写进 design 5.3。
5. Claude 判定：应用 ID 命中 `rules.IsClaudeApp`；或者是 node、bun，并且参数里有 `@anthropic-ai/claude-code`，或有文件名是 `claude` 的绝对路径参数（npm 全局安装的形式）；或者往上 16 级以内的祖先满足以上任一条。祖先读不出来时只按已读到的判。不受“继承父进程”影响。
6. “继承父进程”只改变 `path:` 应用的应用 ID：沿父进程往上，遇到终端类应用就停。终端名单在 `procinfo.TerminalApps`。
7. 系统进程：名单里的（mDNSResponder、nsurlsessiond、apsd、com.apple.WebKit.Networking 等），以及不在 .app 包里、位于 `/System/`、`/usr/libexec/`、`/usr/sbin/`、`/sbin/` 下的，都归为 `system:<文件名>`。
8. 本包不执行外部命令，只用 sysctl 和只读地读文件。

**实现时的补充**：
- Bundle ID 一律归一成小写，`rules.Parse` 和规则匹配也按同样方式归一（`rules.NormalizeAppID`）。
- 快照共享：查找时可以共享 20 毫秒内的快照；重试只用比上一次更新的读取；复核只用读出路径之后才开始的读取。这样并发时不重复读，正确性和“每次新读”相同。

### M3-4 直连拨号模块 `internal/directdial`

**归属**：
- 按 design 6.3，服务端放在 abroad-vm 进程里，以 `_abroad` 身份运行；开发模式下以当前用户运行。
- **生命周期和虚拟机分开**：
  - 它是 abroad-vm 进程里独立的 goroutine 和 socket；
  - 停用时可以选择关闭虚拟机（10 停用 ③），但 abroad-vm 进程和直连拨号模块继续运行，`offline_allow` 要用它。
  - 【v2，architect 拍板】做成独立的库，入口同时能单独运行（`abroad-vm direct` 子命令）。R1 如果不通过，虚拟机宿主要改用 root，直连拨号就必须由另一个以专用用户运行的进程来做，到时不用返工。

**文件**：
- `internal/directdial/{proto.go, server.go, client.go, resolve.go}`；
- `internal/fdpass`：从 `cmd/m0-r2-probe/fdpass.go` 抽出来，m0 探针改为引用它；
- `cmd/abroad-vm` 加 `--direct-sock` 参数。

**接口**：

```go
// 请求、应答：2 字节长度 + JSON（同 wire 的帧头风格），应答成功时随附 1 个 fd（SCM_RIGHTS）。
type Request struct { V int; Net string; Host string; IP string; Port uint16 } // Host 与 IP 二选一
type Reply   struct { OK bool; Code string }                                  // Code：bad_request、pinned、forbidden_dst、resolve_failed、dial_failed、busy、timeout
type Server struct {
    Resolver   Resolver           // 启用前记下的本机 DNS 服务器，绑物理网卡查询
    Dialer     usernet.BoundDialer // 复用 4.5 的绑物理网卡拨号
    AllowUID   func(uid uint32) bool // 生产只收 0；开发模式收自己
    MaxInflight int                // 默认 256
    Timeout    time.Duration       // 默认 8 秒（含解析）
}
func (s *Server) Serve(ctx context.Context, ln *net.UnixListener) error
type Client struct{ Path string }
func (c *Client) Dial(ctx context.Context, network string, host string, ip netip.Addr, port uint16) (net.Conn, error)
func (c *Client) DialUDP(ctx context.Context, ...) (*net.UDPConn, error) // 已连接的 UDP 套接字
type Resolver interface{ LookupA(ctx context.Context, host string) ([]netip.Addr, error) }
```

**要点**：
- 服务端：
  - 用 `LOCAL_PEERCRED` 取对端 uid，不符合就直接关闭连接；
  - 请求合法性复用 wire 的严格解码风格，未知字段报错；
  - 拒绝的目的地址：回环、0/8、169.254/16、组播、255.255.255.255、198.18.0.0/15、IPv6；
  - 私有网段放行，`local_domains` 的公司内网可能解析到私有地址；
  - Host 命中置顶域名（`rules.IsPinned`），或 IP 在 Anthropic 的地址段内，返回 `pinned`，不解析也不拨号。服务端独立再判一次，不信任 abroadd 一侧的判断。
  - 【v2】**对解析出来的地址再判一次**：对每个结果地址重新执行全部目的地址检查（回环、保留段、fake 段、组播、Anthropic 地址段）：
    - 任何一个结果落在 Anthropic 地址段，整个请求返回 `pinned`；
    - 其余被禁止的地址从结果里剔除，剔完为空返回 `forbidden_dst`；
    - 只拨剩下的地址。
  - 【v2】Router 收到 `pinned` 时，带上 `Dest.Refused=true` 重新调用 Decide，结果是 `proxy:main`，闸门关着时拦截。不直接失败了事，也不重试直连。
- 解析：
  - 只发 A 查询，经 UDP 53 到启用前记下的本机 DNS 服务器（状态文件里已有，m2-plan 7a），用绑物理网卡的套接字；
  - 结果不缓存，或只按 TTL 缓存，最长 60 秒；
  - 【v2】应答被截断（TC 位）时改用 TCP 重查一次，TCP 同样绑物理网卡；仍然失败就返回 `resolve_failed`；
  - 本机 DNS 服务器本身是 198.18.0.2 或回环地址时拒绝，返回 `resolve_failed`，防止绕回我们自己或别的代理软件。
  - 【v2】启用前记下的本机 DNS 为空、是回环地址或是 198.18.0.2 时，直连应用的域名连接都会失败。这种情况加进启用前冲突检查（`internal/preflight`），只提示，不阻断。
- 客户端：每次拨号一条 unix 连接，取到 fd 后包成 `net.Conn`，设置 `SO_NOSIGPIPE`。
- fakedns 一侧【v2，改】：
  - `local_domains` 的域名**同样分配假地址**，映射上带“本地域名”标记；Pool 的条目和落盘格式各加一个字段，旧文件读入时这个字段为假。
  - 网络栈看到带这个标记的连接，设置 `Dest.Local=true`。ENABLED 下按优先级表 1.2 判为直连，由直连拨号模块用本机 DNS 解析后连接，不看应用规则。
  - 置顶域名、Claude 进程、解析结果落在 Anthropic 地址段，这三条仍然优先（阶段 2）。
  - 这样公司内网的其他网段（不在本机直连网段、会进 utun）也能连通，不会被误交给主代理。
  - `*.local` 的组播 DNS（mDNS）不经过我们的 DNS，本来就不受影响。
  - 私有网段（10/8、172.16/12、192.168/16、169.254/16）的 PTR 在本地回 NXDOMAIN。

**测试**：
- 进程内用 `socketpair` 和临时目录里的 unix socket 测 fd 传递：
  - 拿到的 fd 能收发；
  - 对端 uid 不符被拒；
  - 并发超过上限回 `busy`；
  - 解析加拨号超时回 `timeout`。
- 反例逐条：回环、组播、fake 段、IPv6、置顶域名、Anthropic 地址段、本机 DNS 服务器是 198.18.0.2。
- 【v2】解析结果的复查：
  - 假的本机 DNS 把一个普通域名解析到 160.79.104.10，应返回 `pinned`，拨号次数为 0；
  - 解析结果混有回环地址和公网地址时，只拨公网地址；
  - 解析结果全是被禁止的地址时，返回 `forbidden_dst`；
  - 应答被截断时改用 TCP 重查。
- 【v2】`local_domains`：分配到的假地址带标记，落盘后读回标记仍在，旧格式的文件能读入。
- 【v2】`abroad-vm direct` 子命令单独运行时能正常提供服务（进程内测试）。
- **源码检查**（扩展 `internal/srccheck`，新增 `NoDial`）：
  - 检查范围：`internal/stack`、`internal/rules`、`internal/daemon`、`internal/fakedns` 等 abroadd 一侧的包；
  - 不准调用 `net.Dial*`、`net.Dialer`、`(*net.Dialer).Dial*`、`net.ListenPacket`，也不准自己建 `syscall.Socket`、`unix.Socket`；
  - 白名单只有 wire 的 unix 拨号、directdial.Client、DoH 经 Opener 的路径，按文件逐个列出。
- **会话里的真实拨号**：开发模式下，用 abroad-vm 的直连拨号模块连一个公网地址的 443，例如开发上游以外的 `1.1.1.1:443`。拨号绑物理网卡，和 4.5 现有的出站一样，不改宿主网络。

**【v2.5】M3-3 验收时两件事的判定**：
1. 普通用户只能看到本进程的套接字：会话里跑 self 模式，子进程识别在非 root 时跳过，curl 版的 R3 交给用户以 root 执行。root 下的可见性记为 R3 的待验证项（design 15），只阻塞 M3 的真机验收，不阻塞 M3-4 到 M3-7（它们用假的识别器）。
2. 1000 档的目标以 root 下 curl 版的结果为准。测法改成“总数 1000，分 10 批，每批并发 100”和“一次并发 300”两档；目标：认错两档都为 0；识别率分批那档 ≥95%、并发 300 那档 ≥90%；99 分位分批那档 ≤20ms。祖先链暂不缓存（进程执行新程序后进程号和启动时间不变、路径会变，缓存不安全）。
   - 实现时的补充：第一次查找只共享不早于“识别开始”才开始读的快照（更早的快照里不会有这条连接，用它只会白白等一次重试）。按新测法会话里的 self 模式两档都达标（`scripts/m3/out/r3-self-20261119.txt`）。

**【v2.5】M3-4 的补充决定**：
1. 不要单独的“只解析”请求种类。服务端只有“拨 TCP”和“拨 UDP”两种请求，目标是域名或 IP 二选一（`local_domains` 已改为假地址加强制直连，解析只发生在拨号内部）。
2. 解析应答要校验：事务号和问题段必须与请求一致；只取 A 记录，同一应答里的别名链跟着走，最多 8 级；结果最多 16 个地址；截断时改用 TCP 重查；单次查询 3 秒，整体受 8 秒上限约束；不缓存。
3. 对解析结果逐个再判，然后按顺序尝试连接，最多试 3 个地址，每个地址拨号前再判一次。
4. 客户端核对服务端身份：连上 socket 后取对端 uid，必须等于配置的专用用户（开发模式是当前用户），否则不发请求。
5. 请求和应答的大小上限 1 KiB，一条连接只处理一个请求。
6. 服务端不打带目标的日志，只记各种结果的计数，经只读接口给诊断用。

### M3-5 接进网络栈

**文件**：
- `internal/stack/proxy.go` 拆成 `router.go`（判定）和 `forward.go`（转发：代理、直连、拦截）；
- 新增 `udpneg.go`（负缓存）。

**接口变化**：

```go
type Router struct {
    Proxy   *Proxy               // 现有的开流部分（Opener、DropAll 等）
    Rules   func() *rules.Config // 原子读取当前配置（M3-6 热替换）
    Env     func() rules.Env
    Ident   AppIdentifier         // procinfo.Identifier 的接口
    Direct  DirectDialer          // directdial.Client 的接口
    Pool    FakePool; Reverse ReverseTable
}
func (r *Router) AcceptTCP(f Flow) bool
func (r *Router) TCP(ctx context.Context, f Flow, conn net.Conn)
func (r *Router) AcceptUDP(f Flow) bool
func (r *Router) UDP(ctx context.Context, f Flow, pc net.PacketConn)
```

**流程**：

**AcceptTCP、AcceptUDP**：
- 地址判定沿用 M2：fake 段查不到映射、保留地址、198.18.0.2 的非 53 端口，都拒绝。
- 闸门关闭时：
  - 当前配置不可能得出直连就在 SYN 时拒绝，保持 M2 的立即 RST。判断条件是：状态是 ENABLED 而且没有任何 direct 规则，或者状态是 DISABLED 而且 offline_allow 为空。
  - 否则先接受，到 TCP、UDP 里再判。

**TCP 和 UDP 里的处理**：
1. Identify 识别应用。
2. 组装 Dest：取 fake 映射，查反查表。
3. NeedSniff 为真时才读首包：TCP 用 PeekTCP，UDP 用第一个数据报。
4. Decide。
5. 按动作转发：
   - Proxy【v2，改】：开流。帧头的目的只有两种：有 fake 映射时填映射的域名，否则填原始的真实 IP，与 M2 相同。嗅探出的名字只用于判定，不写进帧头，也不写日志。`app` 填应用 ID。
   - Direct：调用 `Direct.Dial`，目标是 fake 域名时传 Host，否则传 IP。
     - 【v2】返回 `pinned` 时，带上 `Dest.Refused=true` 重新调用 Decide，按结果走主代理或拦截。对 TCP 来说，这时还没有向应用回过任何数据，可以直接改道。
   - Block：TCP 用 abort 发 RST，UDP 丢弃。
   - 读到的首包字节先发给上游。

**UDP**：
- 开流时收到 `udp_unsupported`，就按出口记负缓存 60 秒。负缓存期间 `Env.UDPOK(exit)` 为假，下一条流按 M3-1 的第 4 步回退。
- 本条流不重试、不换出口，与 M2 相同。

**透传语义**：落实 architect 审 M1-4e 时给的 M3 待办。
- 上游 EOF 就对应用半关闭（FIN），不改成 RST，不重试，不换出口。
- 单条连接的零字节 EOF 不计入出口健康和闸门。
- 新增诊断计数 `zero_byte_after_ok`，只用于诊断页，不参与任何判定。

**测试**：
- 进程内全链路，沿用 M2 的 `tun.Pipe` 测试台。
- 三个假应用同时运行，用假 Identifier 按源端口区分：
  - A 走出口 a；
  - B 走出口 b；
  - C 直连到假直连拨号器。
  - 断言各自到达正确的一端，字节一致。
- 设成直连的应用访问以下几种去向，都应走 main，而且假直连拨号器的计数为 0：
  - 置顶域名的假地址；
  - 发带 ECH 的 ClientHello 到真实 IP；
  - 发不带 SNI 的 ClientHello 到真实 IP；
  - 目的地址在 Anthropic 的地址段内；
  - 首包超时，而且端口是 443。
- 设成直连的应用发自有协议的首包到真实 IP，应该直连。
- 【v2】Claude 进程（假 Identifier 标 `Claude=true`）的规则是直连时，访问普通域名也走 main。
- 【v2】直连应用访问的普通域名被假本机 DNS 解析到 160.79.104.10 时，直连拨号次数为 0，连接经 main 到达。
- 【v2】帧头的目的：真实 IP 目的加任意 SNI，出口收到的目的是那个 IP，不是 SNI 里的名字。
- 【v2】`local_domains`：
  - 本地域名经直连到达；
  - 本地域名解析到 Anthropic 地址段时，走 main；
  - Claude 进程访问本地域名时，走 main；
  - DISABLED 下不在 offline_allow 的应用访问本地域名，被拦截。
- 闸门关闭时：
  - 走代理的连接全部被拒，并分别核对两种情况：SYN 时就拒（RST），以及先接受、判定后 abort；
  - 直连规则仍然生效。
- 负缓存：
  - 出口 a 回 `udp_unsupported` 后 60 秒内，新 UDP 流走 main；
  - main 也不支持 UDP 时拦截。
- “拦截 UDP 443”的选项。
- **没有绕过规则引擎的路径**：
  - 源码检查：`forward.go` 里的 `Proxy.open` 和 `Direct.Dial` 只从 Router 的 `dispatch(Decision)` 调用，用 AST 检查调用点；
  - 性质测试：随机流量和配置下，转发层收到的每条流都带着 Decide 的结果，并与独立调用 Decide 的结果一致。

**【v2.6】M3-4 验收时的判定**：
1. 在途 fd 被内核冲掉：服务端持有副本直到客户端收下（已写进 design 6.3）。M3-8 的真机脚本里以专用用户身份跑一轮 300 次直连拨号的回显，确认跨用户、跨进程时同样 0 失败。
2. 本机当前拒绝绑物理网卡的连接：验收 h 按“如实报告”接受。之后会话里跑 vm-up 之前先跑 `scripts/dev/direct-probe.sh`，哪次通了就把证据补进 PROGRESS；vm-up 加了前置检查（`--precheck-only` 可以单独跑），绑物理网卡连不出去时给出明确提示并停下。到 M3-7 的真实虚拟机阶段仍不通时，那个阶段标为“被本机环境挡住”，请用户看一下代理软件的设置，其余开发照常。
3. 建议改 1（已在 M3-5 第 0 步做完）：别名链上出现的每个名字也判置顶，命中或名字不合法就回 `pinned`，不拨号。

**【v2.6】M3-5 的补充决定**：
1. 判定只有一个入口：每条流恰好经过一次“识别应用 → 组装去向 → 必要时读首包 → `rules.Decide` → 分发”。分发函数是唯一能调用“开代理流”和“直连拨号”的地方。直连回 `pinned` 时带着 `Refused` 再判一次，这是唯一允许的第二次判定，它的结果不可能是直连。
2. 直连的连接和走代理的连接分开登记：`DropAll` 只断走代理的；另提供按条件断开直连连接的方法（M3-6 用）。
3. UDP：每条流（五元组）判定一次，用第一个数据报做首包识别；判定期间到达的后续数据报先缓存，上限 8 个或 16 KiB，超过就丢；直连的 UDP 用拨号模块给的已连接套接字；空闲超时与走代理的 UDP 相同。
4. 识别应用不能拖慢所有连接：当前配置没有任何应用规则、offline_allow 为空、**local_domains 也为空**时，不调用识别器（`rules.Config.NeedApp`）；用测试固定识别器调用数为 0。【v2.7 更正】有 local_domains 时仍要识别：本地域名会带来直连，而 Claude 进程访问本地域名要改走主代理。
5. 计数：按 `Reason` 计数；另加“直连成功”“直连失败（按应答码）”“首包识别的各种结果”“ok 之后零字节关闭”。只有计数，不带域名、地址、应用。
6. 闸门关闭时：不可能直连时在 SYN 就拒，可能直连时先接受再判。“可能直连”做成 `rules.Config.MayDirect(state)`，不在网络栈里另写条件。

### M3-6 停用时的 offline_allow、规则热更新

**文件**：
- `internal/stack/router.go`（配置原子替换、按受影响的连接断开）；
- `internal/rules/diff.go`（新旧配置对一条连接的判定是否变了）。

**接口**：

```go
func (r *Router) SetRules(c *rules.Config, dropAffected bool) // 原子替换；dropAffected 时对每条在途连接按新配置重判，结果不同的 abort
```

每条在途连接记录判定时的输入（应用、Dest），供重判用。只存在内存里，不落盘、不写日志。

**测试**：
- DISABLED 状态下：
  - offline_allow 里的应用可以直连；
  - 不在名单里的应用被拦截；
  - 名单里的应用访问置顶域名，同样被拦截。
- 改规则后：
  - 新连接按新规则走；
  - 不勾选断开时，旧连接不断；
  - 勾选断开时，判定结果变了的旧连接被断，没变的不断。
- 停用时 `DropAll` 只断走代理的连接；offline_allow 里直连的连接不断，按 10 的“断开所有走代理的连接”。

**【v2.6】补充**：**停用时，不在 offline_allow 里的应用的直连连接必须断开**，否则“停用即断网”不成立（用 M3-5 的“按条件断开直连连接”）。

**【v2.7】M3-6 的设计决定**：
1. 每条在途的流记下判定时的输入：应用、去向（含首包结果）、当时的动作。只在内存里，不落盘、不进日志。
2. 提供“按当前配置和状态重判所有在途流”的操作（`stack.Proxy.Recheck`），结果与当时的动作不同的就断开（种类不同或出口不同都算）。调用时机：状态变化时必做；规则修改且勾选“断开受影响的连接”时做，不勾选就不重判、旧连接保持（design 5.2）。
3. 状态机增加可选依赖“状态接收方”（`state.DataState`）：停用一开始（关闸门之前）通知停用；启用在写入 ENABLED 之后、开闸门之前通知启用；恢复一开始通知停用；启用失败回滚通知停用；Recover 先通知停用，文件状态是 ENABLED 时再通知启用。数据面在 daemon 启动时初始是停用。网络栈只认启用和其他。监护因故障关闸门时状态仍是启用：只断走代理的，直连照常（design 6.2）。
4. 规则的替换是原子的：一条流的整个判定过程用同一份配置（流开始时取一次；取配置之后规则或状态变了、而重判那时这条流还没登记的，登记判定时按新的再判一次）。替换时同步 fake-IP DNS 的本地域名名单（`Proxy.SyncRules`），同步失败则整体保持旧配置。**直连拨号模块那一侧的置顶追加名单在 M3-6 先固定为只有内置名单**（abroadd 一侧的判定已经包含追加名单，直连模块是第二层），经客户端下发留给 M3-7。
5. 重判用记下来的首包结果，不重新读首包。另外两处按新配置刷新：fake 映射的本地标记按新的 local_domains 重算；当时没识别应用而新配置要用的，现场识别一次（流的五元组还在连接表里）。

### M3-7 接进 daemon

**文件**：
- `internal/daemon/{rules_api.go, rulesfile.go}`；
- `internal/daemonsim` 扩展；
- `cmd/abroad-dev`；
- `internal/agent/singbox.go`：开发用的多出口；
- `scripts/dev/vm-up.sh`：新阶段。

**本地接口**：

| 接口 | 用途 | 说明 |
|---|---|---|
| `GET /v1/rules` | 读规则 | |
| `PUT /v1/rules?drop_affected=1` | 改规则 | 只有属主能调用（沿用 9a 的对端 uid 校验）；校验失败回 422，带 usererror 文案 |

【v2】把应用改成“本机网络”属于敏感操作。M5 之前，只有状态目录的属主能调用规则接口（真实模式下是 root）。三级权限在 M5 做。

**规则文件**：
- 路径是 `/Library/Application Support/claude-abroad/rules.yaml`，开发模式放在状态目录里；
- root 所有，权限 0600；
- 原子写入，复用 state 的原子写；
- 读不出来时用内置默认值（`default: proxy:main`，没有规则），并发一条事件。不当成致命错误，因为默认值是最严格的。

**开发用多出口**（决定 4）：

`SingBoxExit` 加一个可选的 `Server` 字段：
- 只收 `127.0.0.1`（引擎端口）和 `guestabi.HostLoopbackIP`；
- 后者指向本机回环上的开发上游，经回环白名单。

agent 新增 `PUT /v1/dev-exits`：
- 只在内核命令行带 `abroad.dev=1` 时注册；
- 生产镜像不带这个参数。【v2，architect 拍板】发布构建还要用构建标签把 `dev-exits` 整段编译掉，不只靠内核命令行；已记进 M6 待办。

出口标签：
- `wg-a`、`clash-b` 这类标签，各接一个 `abroad-dev upstream` 实例；
- 开发上游需要加一个“回显自己名字”的应答，这样能判断走的是哪个出口。

**daemon-sim**：
- 用假 Identifier 模拟三个应用，按源端口段区分；
- 经真实虚拟机跑出口 A、出口 B 和直连；
- 直连用真实的直连拨号模块，开发模式下绑物理网卡连公网，不改宿主网络。

**vm-up 新阶段 9c**：
- 改规则前后各跑一轮，核对每个应用的去向；
- 设成直连的应用访问置顶域名时，断言走了 main：开发上游 main 的计数加一，直连拨号模块的计数不变；
- 断言 daemon 日志里没有域名、IP 和应用路径。

**日志**：
- 默认级别不写域名、地址、应用路径，只记 Reason 的计数；
- 调试级别也要脱敏，复用 `internal/redact`。

**【v2.8】M3-7 的设计决定（也是实施计划）**：
0. 第 0 步：网络栈没被告知状态时按停用处理（architect 审 M3-6 的建议改 1）；tunsim 和测试台显式设成启用。
1. 直连拨号模块的运行时配置由 abroadd 下发：协议加“配置”请求，内容是本机原 DNS 服务器（最多 4 个）和置顶追加名单；上限 64 KiB；服务端逐项校验（DNS 用 `checkDNSServer`，回环、198.18.0.2 剔除；域名用 `rules.NormalizeName`），通过后原子替换，同样要过对端身份校验。下发时机：abroadd 启动后、每次启用时、每次规则变化时。收到第一次配置之前，按域名的请求回 `resolve_failed`，按 IP 的不受影响。服务端的置顶判断（含别名链）用“内置 + 下发的追加名单”。`--direct-dns` 只留给单独运行的开发用法。
2. 启用前的本机 DNS：`sysnet.DNS` 加只读方法读当前系统的首选解析器（`scutil --dns`）；状态机在从 RESTORED 启用的第 ① 步写进状态文件，从 DISABLED 再启用时不覆盖，恢复后清掉。读到回环或 198.18.0.2 也照样记下（直连拨号模块会拒绝用它）。
3. 规则文件与接口：状态目录下的 `rules.yaml`（0600，原子写）。启动时读不出来或校验不过：用内置默认配置，发事件，不退出。`GET /v1/rules` 返回 YAML；`PUT /v1/rules?drop_affected=0|1`，请求体上限 64 KiB，顺序固定：解析校验 → 写文件 → 下发直连配置 → 替换网络栈的规则（含同步 fakedns）；任何一步失败，内存和文件都回到旧配置，返回错误；校验失败回 422。敏感操作，M5 之前只有属主能调用；与启用、停用、恢复共用忙标记，冲突回 409。
4. 出口是否可用（M3 的临时做法，M4 换成出口检查）：对分流出口开流收到 `unknown_exit` 或 `exit_down` 后，30 秒内按不可用处理（命中它的新连接回退主代理），之后重新尝试；时钟可注入；status 列出当前不可用的出口名。
5. 应用识别：真实模式用 procinfo 的真实实现（root）；daemon-sim 和进程内测试用按源端口段区分的假识别器。
6. status 增加：规则摘要（各种动作的条数、拦截 UDP 443、本地域名和置顶追加名单的条数）、网络栈的计数、直连拨号模块是否连上和是否已下发配置。不含域名、地址、应用路径。
7. 开发用的多出口按 v2 已定的做：`SingBoxExit.Server` 只收两个回环地址；agent 的 `dev-exits` 只在内核命令行带开发标记时注册；发布构建去掉它的事在 M6 待办。

**【v2.9】M3-7 审核的必须改**：直连拨号模块重启后丢配置。改为：配置带代数（abroadd 维护，内容每变一次加一）；每个拨号请求带代数；服务端没有配置或代数不一致时回 `stale_config`（按 IP 的请求同样检查），不解析、不拨号；客户端收到后下发当前配置（并发时只下发一次）并重试这一次，只重试一次，再失败按拨号失败处理。`applyRules` 和状态切换不再同步下发；规则修改的顺序简化为“解析校验 → 写文件 → 替换网络栈的规则（含同步 fakedns、直连配置的代数）”。启动后的主动下发保留作预热，失败不算错。`stale_config` 在网络栈里按“直连失败”计数，不当成 pinned，也不触发重判。单独运行（`abroad-vm direct --dns`）时没有下发，按代数 0 处理。

### M3-8 给用户的真机脚本 `scripts/m3/`（需要用户）

沿用 M2-9c 的全部安全约定：
- 死人开关（先建目录、核对存活）；
- 中途失败或 Ctrl-C 时用 trap 立即还原；
- DRY_RUN 模式和 `check-commands.sh` 的命令核对；
- 持久快照；
- 按 PID 文件结束进程；
- 前提检查：上次运行留下的状态、系统代理是否开着；
- 测试用户 `_abroadm3`。

脚本内容：
1. 前提：R1、R2、M2 的真机脚本都已通过，否则拒绝运行。
2. 启用后同时运行 Chrome（出口 A）、curl（出口 B）、微信（直连）：
   - 出口 A、B 是开发上游，接在用户提供的节点后面，或者直接用两个不同的节点；
   - 判定各自的出口：经出口时由开发上游回显自己的名字，直连时核对 netstats 的直连计数。
3. 把 Chrome 设成直连后访问 claude.ai：
   - 抓包或读直连拨号模块的计数，确认没有发往 160.79.104.0/21 的直连；
   - 主代理的计数增长。
4. R3 的 root 部分：识别 root 进程、`mDNSResponder` 等系统进程的连接，统计识别率。
5. R15：Docker、OrbStack 等，按用户本机装了什么来测，没装就跳过并记下来。
6. 直连拨号模块以 `_abroadm3` 身份运行时，pf 的 `user` 规则是否放行，与 R2 的结论对照。
7. 还原，并采样。

交付：README、空跑证据、命令核对、自测。主会话不执行真机部分。

**【v2.10】M3-8 的设计决定**（architect 2026-11-19，M3-7 验收时给出）：

1. **公共脚本库**：`scripts/m2/lib.sh` 里的死人开关、还原 trap、空跑、按 PID 文件结束进程、测试用户、脱敏、自测护栏抽到 `scripts/lib/`，m2、m3 共用；测试用户、状态目录、服务标签做成参数。抽完后 m2 的自测、空跑、命令核对照样通过，空跑的改动类命令清单与抽库前逐条相同。
2. **出口 A、B**：本机起两个开发上游，以测试用户 `_abroadm3` 运行（断网规则只放行它，所以能出站）。它们是本机直出，只用来区分路径，不代表真实的分流出口（README 写明）。虚拟机用带 `abroaddev` 标签构建的 abroad-vm，打开开发用多出口，出口 A、B 指向这两个上游；主代理仍是用户的远端节点。
3. **判定谁走了哪个出口**：每个应用访问带随机标记的地址，对应的上游把标记记进自己的日志，脚本在日志里找。Chrome 用 `open -a`；curl 直接执行；直连的应用默认用系统的 `nc` 连一个可达端口，看直连拨号模块的成功计数；`M3_INTERACTIVE=1` 时额外让用户手动操作一次微信，只看计数。没装 Chrome 的记“跳过”。
4. **Chrome 设成本机网络后访问 claude.ai**，四条同时成立才通过：规则引擎的 `pinned` 计数增长；主代理一侧有新连接；物理网卡上 `tcpdump` 只读抓 20 秒、目的 160.79.104.0/21 的包数为 0；同时段物理网卡 53 端口没有对 claude.ai、anthropic.com 的查询。只统计包数，不保存内容。
5. **其余步骤**：R3 的 root 部分调用 `scripts/m3/r3-accuracy.sh`，另抽查几个系统进程；在途 fd：直连拨号模块以测试用户运行，从 root 一侧拨 300 次回显，0 失败；直连拨号模块以测试用户运行时是否被 pf 放行（对照 R2）；停用后 offline_allow 的应用访问普通地址能通、访问 claude.ai 不通；R15：Docker、OrbStack 在运行时各做一次容器内访问并记录，没有就跳过。
6. **前提检查**（在 M2 的基础上）：环境变量确认 M2 真机脚本已通过；开发标签构建的 abroad-vm 存在；Chrome 正在运行时提醒先退出。
7. 主会话不执行真机部分，只做语法检查、空跑、命令核对、自测。

**验收标准**：a 抽库后 m2 不退步（自测、空跑清单逐条相同、check-commands）；b `bash -n` 全过，兼容 bash 3.2；c 普通用户 `DRY_RUN=1` 跑完 run.sh、restore.sh，退出码 0，环境不变；d 命令核对（禁止项同 M2，另加 tcpdump 只能只读、不写文件），反例各一条被拦下；e 顺序与安全约定（建目录 → 死人开关 → trap → 快照 → 其余；restore：停进程和服务 → 清网络 → 清 pf → 备份 abroadd 状态目录 → 删用户 → 取消死人开关；上游和抓包进程按 PID 文件结束并核对命令行），自测覆盖；f `--check-only` 在本机现状下被拦下并列出原因；g 判定函数的离线自测（含“抓到 1 个发往 160.79.104.0/21 的包”判不通过）；h README 内容齐全；i **交付前的硬条件**：阶段 9c 在会话里对真实虚拟机跑通一次（本机环境仍挡着时 a–h 照常交验收，判“脚本完成、暂不交给用户”）；j build、vet、arm64 build、全量 -race 通过。

**【v2.11】M3-8 实施计划的审核结论**（architect 2026-11-19）：

- 偏差 1（同意）：开发上游在本机应答、不出站（internal/devupstream 绝不向外拨号），出口 A、B 只用来区分路径；仍以测试用户运行；标记放在域名里，判定看 A 的日志里有、B 的日志里没有这个 host。决定 2 里“能出站、本机直出”的说法作废。
- 偏差 2（同意并加强）：在途 fd 的检查连公网（默认 1.1.1.1:443，`M3_DIRECT_TARGET` 可配），拿到 fd 后做一次完整 TLS 握手（服务器名可配，证书照常校验），握手成功才算成功；300 次分批、并发不超过 10；分别统计拨号失败和握手失败，要求 0 失败；握手全部失败时记“跳过：目标不可达”并提示换目标。
- 偏差 3（同意）：`direct-probe --own-config` 用很大的代数自带配置（只在开发工具里）；探针结束后检查 abroadd 一侧自动补发、直连恢复（验证 v2.9 的自愈路径）。
- 必须改 1：S3、S8 的直连目标用域名（可配置），经系统解析拿到假地址，由直连拨号模块解析后连接。原因：`nc -z` 不发数据，裸 IP 的 443 等不到首包，按 `port443_timeout` 改走主代理。另加一小步：`nc` 连裸 IP 的 443，预期走主代理（`port443_timeout` 增长、直连计数不变）。
- 必须改 2：Chrome 以用户身份启动（`sudo -u "$SUDO_USER"`），临时配置目录单独建、属主改成该用户、700；结束时从锁文件取 PID 并核对命令行；restore 删除临时目录；空跑清单里能看到这几条。
- 建议改 3：53 端口的抓包文本中途失败时 restore 也删；日志和贴回文件里只写计数。
- 建议改 4：Chrome 新配置的后台连接会进出口 A 的日志，判定只看带本次标记的那一条，不对总行数断言。
- 建议改 5：ident-sample 只输出计数和 `system:` 名单，不输出 `path:` 应用 ID。

**新增验收**：k 直连探针批量模式的单测（进程内 directdial 服务端 + 本地 TLS 假服务器：握手成功、握手失败、拨号失败三种计数；并发不超过设定值；输出里没有地址）；l 空跑输出里 S3、S8 的直连目标是域名，有“裸 IP 的 443 走主代理”一小步，Chrome 启动带 `sudo -u`，临时配置目录的建立、改属主、删除都在清单里；m 判定函数的离线自测：直连计数没增长但 `port443_timeout` 增长时“裸 IP 的 443”判通过，S3 那种情况判不通过。

## 3. “不泄漏”由谁证明

| 命题 | 离线证明（任务、测试） | 真机证明（M3-8 的步骤） |
|---|---|---|
| 置顶域名不会直连 | M3-1 性质测试（任何配置不会把置顶判成 Direct）；M3-4 直连拨号模块独立拒绝置顶域名；M3-5 全链路（直连应用访问置顶假地址走 main，直连计数为 0） | 第 3 步：Chrome 设成直连后访问 claude.ai，没有发往 Anthropic 地址段的直连 |
| 看不清去向的 TLS/QUIC 不会直连 | M3-2 分类正确（ECH、无 SNI、解析失败都是 NoName，不是 NotTLS）；M3-1 性质测试；M3-5 全链路（ECH、无 SNI 发往真实 IP 走 main） | 第 3 步，浏览器开安全 DNS 时再测一次（README 里写明怎么开） |
| 直连只经专用用户的拨号模块 | M3-4 源码检查 `NoDial`（abroadd 一侧没有直接拨号）；M3-5 源码检查（转发只从 dispatch 调用）；fd 来自直连拨号模块的进程 | 第 6 步：直连拨号模块以专用用户身份运行，pf 只放行这个用户；对照 R2 |
| 置顶域名不经本机 DNS 解析 | M3-4：解析请求的反例（置顶域名回 `pinned`，假的本机 DNS 服务器收不到查询）；fakedns 的 `local_domains` 与置顶域名不能有交集（M3-1 的校验） | 第 3 步同时抓 53 端口，确认本机 DNS 服务器没有收到 claude.ai 的查询 |
| 停用状态下置顶域名不可达 | M3-1 性质测试（DISABLED 下置顶域名永远是 Block）；M3-6（offline_allow 里的应用访问置顶域名被拦截） | 第 2 步之后停用，用 offline_allow 里的应用访问 claude.ai，应该不通 |
| 应用识别出错时的兜底 | M3-3 识别不到时返回 `unknown`，用 Default 处理；M3-1：unknown 永远不会命中 direct 规则，Default 只能是 main 或 block；置顶规则不依赖应用识别 | 第 4 步：R3 的认错率；认错时置顶规则仍然兜底（第 3 步覆盖） |
| 【v2】Claude 进程的流量不会直连（与规则和域名无关） | M3-1：优先级表 2.1，性质测试；Parse 拒绝把 Claude 的应用 ID 设成 direct 或放进 offline_allow。M3-3：子进程也标 Claude。M3-5 全链路。识别不到时，由置顶域名和 Anthropic 地址段兜底 | 第 3 步加测：在终端里启动 Claude Code，把终端设成直连，抓包确认没有直连 |
| 【v2】解析结果落在 Anthropic 地址段的普通域名不会直连 | M3-4 解析结果的复查（返回 `pinned`）；M3-5（改走 main，直连次数为 0） | 第 3 步一并抓包确认 |
| 【v2】走代理时不按首包里的名字改写目的 | M3-5：帧头只填 fake 域名或原始 IP 的测试 | 不需要真机 |
| 主代理、出口不可用时不会退回直连 | M3-1：出口不可用时回退到 main，main 不可用时拦截，永远不会得到 Direct；M3-5：负缓存的回退 | 第 2 步：关掉出口 A 的上游，Chrome 的连接走 main，不会直连 |

## 4. “带进 M3 的事”逐条着落

| 来源 | 内容 | 着落 |
|---|---|---|
| architect 审 M2-3 | 本机回环第一跳的 UDP 不可用，两种修法 | **不做**：决定 1，本机代理软件不支持当主代理；design 4.5 已改 |
| m2-plan 第 5 节、PROGRESS“本机代理软件当主代理会回环” | 是否给代理软件自动加一条直连规则，与 10.4 冲突 | **不做**：决定 1 之后，代理软件不会被当作主代理，不需要直连规则；10.4“代理软件不能设为本机网络”在 M3-1 的配置校验里落实 |
| architect 审 M2-5a | UDP `udp_unsupported` 的负缓存；“拦截 UDP 443”选项 | M3-5（负缓存、`BlockUDP443` 的执行）；M3-1（配置项和判定） |
| architect 审 M2-4b | 反查表只能变严格；“真实 IP + 反查命中 + direct”这条路径单独审，默认走 main | M3-1 判定顺序第 3 步，以及性质测试；M3-5 全链路用例；design 5.2 已写明 |
| M2 遗留 | 私有网段的 PTR 要不要本地回 NXDOMAIN | M3-4（fakedns 部分）；design 4.3 已写明 |
| architect 审 M1-4c | yamux 没有 RST，宿主转发要考虑这一点 | M3-5 透传语义：上游 EOF 对应用半关闭 |
| architect 审 M1-4e（M3 待办） | 照实透传，不改成 RST、不重试、不换出口 | M3-5 |
| 同上 | 单条连接的零字节 EOF 不计入出口健康和闸门 | M3-5（计数只用于诊断）；M3-1 的 ExitUp 只看 `exit_down`、`dial_failed` 和配置 |
| 同上 | 出口检查必须端到端 | **M4**（原文就写着给 M4），PROGRESS 的 M4 待办里记下 |
| 同上（可选） | data 段加“ok 后零字节关闭”的计数 | **做**，放在 M3-5，宿主一侧的 `zero_byte_after_ok` 计数，只给诊断用；虚拟机一侧的 data 段不改 |
| m2-plan 7a | 启用前的本机 DNS 服务器存进状态文件，给 M3 的直连解析用 | M3-4 读取它；daemon 启动直连拨号模块时传进去（M3-7） |
| design 4.3（M2-4b） | `local_domains` 推到 M3 | 【v2】M3-4（分配假地址并带“本地域名”标记）+ M3-1（优先级表 1.2、校验）+ M3-5（全链路） |

## 5. 可以复用的部分

| 来源 | 复用的内容 |
|---|---|
| `cmd/m0-r2-probe/fdpass.go` | 抽成 `internal/fdpass`，给 M3-4 用 |
| `internal/usernet` 的绑物理网卡拨号器、`internal/physif` | 直连拨号模块的拨号（M3-4） |
| `internal/wire` 的严格解码风格 | 直连拨号模块的请求和应答格式 |
| `internal/daemon` 的对端 uid 校验（`peer_darwin.go`） | 直连拨号模块的对端校验；规则接口的属主校验 |
| `internal/state` 的原子写 | 规则文件 |
| `internal/srccheck` | 扩展出 `NoDial` 和调用点检查 |
| `internal/redact` | 日志脱敏 |
| M2 的 `tun.Pipe` 测试台、`daemonsim`、`devupstream` | M3-5、M3-7 |
| `scripts/m2/lib.sh` | M3-8 的脚本库：死人开关、trap、DRY_RUN、命令核对，原样复用或抽成 `scripts/lib/` |
| Cage | Cage 里没有应用识别、首包识别、fd 传递的代码，这三块都要新写；`inetaf/tcpproxy` 有 SNI 解析，但不认 ECH 和分片，不用它 |

## 6. architect 拍板的结论（v1 的 4 个问题）

1. Info.plist：自己写，加大小、深度、对象数的上限和模糊测试（M3-3）。
2. 直连拨号模块：做成独立的库，现在挂在 abroad-vm 进程里，生命周期和虚拟机分开；同时提供 `abroad-vm direct` 子命令，可以单独运行（M3-4）。
3. 闸门关闭但配置可能产生直连时，TCP 先接受、判定后重置，可以接受。没有任何 direct 规则、offline_allow 也为空时，保持在 SYN 时就拒绝（M3-5）。
4. `dev-exits` 只在内核命令行带 `abroad.dev=1` 时注册，这样够用；另外记一条 M6 待办：发布构建用构建标签把它编译掉。`Server` 字段只收两个回环地址的限制保留（M3-7）。
