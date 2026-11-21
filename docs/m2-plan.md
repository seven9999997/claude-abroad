# M2 实施计划：接管

> 状态：v2（2026-11-18）。按 abroad-architect 审 v1 的 8 条必须改和采纳的建议改修订，不再整份送审，随 M2-1a 交验收。
> 依据：设计文档 v0.5 第 3、4.1–4.3、4.5、6、10、10.4、14 节；architect 审 M1-6c-2 时给的 M2 待办清单。

## 0. 总原则

### 0.1 会话里能做什么

宿主机唯一的禁区是断网（`CLAUDE.md`“硬性限制”）。M2 里凡是会碰宿主网络的操作，都藏在可替换的接口后面：
- 会话里只跑假实现；
- 真机实现只做薄的执行层（拼命令行、解析输出），用单测核对拼出来的参数和对输出的解析；
- 真正执行的部分写成脚本交给用户。

**会话里绝不执行**：`sudo`、`pfctl`（唯一例外见下）、`networksetup -set*`、`ifconfig`、`route add/delete/change`、`scutil` 的写操作、创建 utun。

**`pfctl -n` 的例外**（architect 2026-11-18；审 M2-6a 时扩为三种写死的形式）：
- 只能经固定脚本 `scripts/dev/pf-syntax.sh` 调用，命令只有四种写死的形式：
  - `/sbin/pfctl -n -f <文件>`；
  - `/sbin/pfctl -n -v -f <文件>`；
  - `/sbin/pfctl -n -f -`（规则从 stdin 读）；
  - `/sbin/pfctl -n -v -o none -f <文件>`（M2-6b 加，关闭优化器时的打印形式）。
- 不带 sudo，EUID 为 0 时脚本拒绝运行；检查用的规则里，用户用本机存在的 `nobody`。
- 非 root 下 `-n` 能完成解析（M2-6a 实测：反例报 syntax error）。
- 其他用法（`-s`、`-a … -f`、`-E`、`-X`、`-F`、`-k`、`-d`、`-e`）一律不执行。
- 主会话执行前再确认一遍它不属于 `CLAUDE.md` 的禁止项：`-n` 不加载规则，非 root 也打不开 `/dev/pf`。

**会话里允许的只读查询**：
- `networksetup -listallnetworkservices`、`-getv6`、`-getwebproxy` 等 get 类；
- `scutil --proxy`、`--nwi`、`--dns`；
- `route -n get`、`netstat -rn`、`ifconfig -l`；
- `ps`；
- 读 shell 配置文件。

### 0.2 需要 root 或会断网的步骤

下表的步骤一律藏在接口后面，真机部分只出脚本：

| 步骤 | 为什么 | 接口 | 会话里用 |
|---|---|---|---|
| 加载、清除 pf 锚点，`pfctl -E/-X` | 需要 root，加载后断网 | `pf.Controller` | `pf.Fake` |
| 创建 utun、设地址 | 需要 root | `tun.Device`、`tun.Creator` | `tun.Pipe`（内存管道设备） |
| 添加、删除 0/1 和 128/1 路由 | 需要 root，会改路由 | `sysnet.Routes` | `sysnet.Fake` |
| 写入、删除 `State:/Network/Service/claude-abroad/DNS` | 需要 root，会改 DNS | `sysnet.DNS` | `sysnet.Fake` |
| `networksetup -setv6off/-setv6automatic` | 需要 root，会改网卡 | `sysnet.IPv6` | `sysnet.Fake` |
| 结束 Claude 进程（10.1） | 不断网，但属于 M6 | `state.ClaudeKiller` | M2 只用假实现，M6 补真实实现 |

### 0.3 依赖用户的事（不阻塞开发，阻塞 M2 验收）

- **R2 结论**：pf 的 `user _abroad` 能否匹配 IP_BOUND_IF 发出的连接。决定 M2-1 的出站收紧只是加固，还是要当主方案。
- **R1 结论**：abroad-vm 能否以 `_abroad` 身份运行。M2-9 的真机脚本要用。
- **真实远端节点**（M2 验收前）。
- **M2-9 真机脚本**由用户执行。

### 0.4 执行顺序

M2-1a → M2-1b → M2-2 → M2-4 → M2-5 → M2-6 → M2-7 → M2-8 → M2-3 → M2-9。每个任务做完单独交 architect 验收。

M2-3 不在关键路径上：本机代理软件当主代理时会回环（见第 5 节），M2 的真机验证不能用它。

## 1. 现有 Cage 包：复用什么、缺什么

| 包 | M2 用到的 | 不用或缺的 |
|---|---|---|
| `internal/gate` | `EgressGate` 的状态机部分（closed、checking、open，租约，`Close(reason)`，连接登记，断开时全部关掉）。M2 的闸门直接包一层：网络栈每开一条代理数据流前问 `Allows`，闸门关闭时 TCP 回 RST、UDP 丢弃 | `Egress`、`DialContext`、`ExchangeDNS` 绑的是 Cage 的出站代理路径，M2 不用。安全报告（`SafetyReport`）属于 M4 出口检查。M2 只用最小检查：经 main 出口端到端拿到 HTTP 204 |
| `internal/netguard` | 不用 | 它是 Lima 时代的虚拟机出站策略，已被 `internal/usernet` 取代；M6 打包前评估删除 |
| `internal/exitpolicy` | 不用 | M4 出口检查 |
| `internal/host` | `EnsurePrivateDir`、`ProtectFile`、`CheckPrivateFile`，用于状态目录和 state.json | 缺“拒绝符号链接打开”（`O_NOFOLLOW`），M2-7 补进 `state` 包自己的原子写 |
| `internal/diagnostics` | `Store`、`Stream`、`Redact`，用于 abroadd 的日志 | profile 路径是 Cage 的，加一个按目录构造的入口 `NewAt(dir)` |
| `internal/hostproxy` | 不用 | 冲突检查读系统代理用 `scutil --proxy`，不经它 |
| `internal/usernet` | M2-1 的 `BeforeDial` 钩子已经留好 | — |
| `internal/wire`、`internal/devsocks` | 网络栈到虚拟机直接用 `wire.Dialer.Open` | — |

## 2. 任务

### M2-1 虚拟机出站收紧：网关 DNS 代解析（方案 d，architect 拍板）

**目标**：就算哪个引擎隐式退回直连（R20），虚拟机也只能连白名单和路线第一跳。

**为什么不选 v1 的 (a)、(b)、(c)**：
- (a)：mihomo 对多个 nameserver 并发取最快的结果，两边的解析结果会不一致，造成误拒；而且 (a) 要对虚拟机放开 DoH 的 IP，引擎一旦退回直连 DoH，目标域名就能从真实 IP 查出去。
- (b)：挡不住 443 端口上的直连。
- (c)：改动面大，IP 还会过期。

**方案 (d)**：
- abroad-vm 的网关 DNS（192.168.127.1:53）只应答登记过的第一跳域名的 A 查询，其余一律 REFUSED。
- 解析由 abroad-vm 自己经 DoH 完成（1.1.1.1、223.5.5.5，绑物理网卡）。
- 答给虚拟机的地址同时进放行集合，两边看到的必然是同一份结果。
- DoH 的 IP 不再对虚拟机放行。
- **默认收紧**：不给参数时放行集合只有 `--allow-outbound` 的项；开发时要恢复旧行为，必须显式加 `--outbound-open`。netstats 报 `outbound_mode`，M2-7 开闸门前核对它是 `first_hop`。

#### M2-1a（宿主侧）

- `internal/usernet/firsthop.go`：
  - `FirstHop{Host, Ports []PortRange}`；
  - `ParseFirstHops`：最多 8 项，主机是公网 IPv4 字面量或合法主机名，端口支持区间（给 hysteria2 的端口跳跃用）。
- `internal/usernet/outbound.go`：`Outbound` 放行集合，`BeforeDial` 只放行三类：
  - 白名单项；
  - 字面量第一跳，且端口相符；
  - 网关 DNS 答过的地址，且端口属于该域名的第一跳。

  其余计 `hop_denied`。答过的地址保留到最后一次应答后 1 小时，每个域名最多 64 个。
- `internal/usernet/gatewaydns.go`：`GatewayDNS`。
  - 只处理单问题、IN 类，其余 REFUSED；
  - 登记域名的 A 查询只回公网 IPv4，最多 16 条，全被滤掉或解析失败回 SERVFAIL；
  - 登记域名的其他类型回空的 NOERROR；
  - 计数 `dns_answered`、`dns_refused`。
- `internal/usernet/doh.go`：
  - 同时问两个 DoH，取先成功的；
  - `Proxy: nil`，证书按 IP 校验；
  - 单次 5 秒，应答上限 4 KiB，按域名合并并发查询，TTL 夹在 30 秒到 10 分钟；
  - 解析失败时有 1 小时内的旧结果就回旧结果（TTL 30 秒）。
- `internal/usernet/dialer.go`：
  - `DialResolver` 只连 DoH 的 IP:443，绑物理网卡，计 `resolver_dial_ok`，不计入 `dial_ok`；
  - BeforeDial 的拒绝改计 `hop_denied`。
- `cmd/abroad-vm`：
  - `--first-hop`，可重复；`--outbound-open`，与 `--first-hop` 互斥，启动时打醒目日志；
  - netstats 加 `outbound_mode`、`first_hop_count`、`hop_denied`、`dns_answered`、`dns_refused`、`resolver_dial_ok`。
- vm-up：
  - 阶段 A 用 `--outbound-open`；
  - 新增 6c/4g，收紧模式：(i) 第一跳放行；(ii) 端口不符、不在路线上的目标被拒；
  - 阶段 B、C 在收紧模式下跑。
- 4g-iii（网关 DNS 的真机验证）挪到 M2-1b：要让 mihomo 用网关解析节点域名，必须改 mihomocfg，而 1a 不改虚拟机里的配置。

#### M2-1b（虚拟机侧与组装）

**结果**（2026-11-18）：
- (a) mihomo 接受裸 IP 的 UDP nameserver（`192.168.127.1`）来解析节点域名，真机 4g-iii-a 通过。DNS 超时 5 秒；缓存过期后先用旧结果（TTL 1），同时在后台重查（`dns/resolver.go`）。
- (b) 链式路线的第二跳写成域名时，mihomo 把域名原样交给前一跳：开发上游收到 ATYP=3 的 connect，网关 DNS 计数不变。所以只登记第一跳就够了。

- mihomocfg 和 VMRuntime：`default-nameserver`、`proxy-server-nameserver` 改成网关地址（明文 UDP，`192.168.127.1`）；`nameserver`（`#组`）不变。
- `VMConfig.FirstHop`，`abroad-dev --print-first-hop`。
- ~~`Restart(newArgs)`~~ 挪走（architect 审 1a 时决定）：现在没有宿主进程负责拉起 abroad-vm，1b 里写它没有调用方。
  - 接口放进 M2-7 的 `VMControl`：期望的启动参数变了就重启 abroad-vm，重启期间闸门关闭；
  - 真实实现放进 M2-9。
- **会改变第一跳拨号目标的字段**（2026-11-18 对照 mihomo v1.19.34 源码 `adapter/outbound/*.go` 核对，只看导入白名单里的 12 种节点类型）：

  | 字段 | 节点类型 | 作用 | 处理 |
  |---|---|---|---|
  | `server`、`port` | 全部 | 拨号目标 | 并入 FirstHop（回环、局域网走 Allow） |
  | `ports`（`mport` 导入时归一成它） | hysteria、hysteria2 | 端口跳跃 | 区间并入 FirstHop |
  | `port-range` | mieru | 端口区间 | 并入 FirstHop |
  | `ip` | tuic | 替代 server 作拨号地址 | 并入 FirstHop，只收公网 IPv4，指向局域网或不是 IP 时拒绝 |
  | `realm-opts.server-url`、`stun-servers` | hysteria2 | 另连 realm 服务器和 STUN | 拒绝 |
  | `xhttp-opts.download-settings.server/port` | vless | 下载走另一台服务器 | 拒绝（整个 download-settings） |
  | `dialer-proxy` | 全部 | 链式 | 已按路线处理，只取第一跳 |
  | `plugin-opts.host`（obfs、v2ray、shadow-tls、restls、jls） | ss | 只是 TLS/HTTP 的主机名，拨号仍是 server:port | 不处理 |
  | kcptun 插件 | ss | 用 server:port 的 UDP | 不处理（端口同 port） |
  | `sni`、`servername`、`ws-opts.headers.Host` | 多种 | 只是主机名 | 不处理 |
  | `ech-opts.query-server-name` | 多种 | 经 `ProxyServerHostResolver` 查 HTTPS 记录（走网关 DNS，没登记就是 REFUSED） | 不处理（ECH 失败即断，不泄漏） |
  | `interface-name`、`routing-mark` | 全部 | 改网卡 | 已在导入和二次校验时拒绝 |

  漏掉的字段不会泄漏，只会连不上：abroad-vm 只放行 FirstHop。这张表的作用是给出明确的错误。
- 实测：
  - mihomo 是否接受裸 IP 的 UDP nameserver 来解析节点域名；
  - 链式路线里第二跳是域名时，mihomo 是本地解析还是交给前一跳。默认只登记第一跳；第二跳被本地解析时得到 REFUSED，路线不通但不泄漏；实测后再决定是否把整条路线的域名登记为“只解析、不放行”。
- vm-up 补 4g-iii：
  - 查 dns.alidns.com 得到应答，`dns_answered` +1；
  - 查别的域名得到 REFUSED，`dns_refused` +1；
  - 随后对答出的 IP:443 拨号成功。

### M2-3 真实本机 mihomo 的 UDP 中继实测

**状态**：完成（architect 2026-11-18）。结论：独立的 mihomo 1.19.34 对 UDP ASSOCIATE 回的 BND 是 127.0.0.1，虚拟机里的 mihomo 按它发数据报出不了虚拟机，UDP 不可用、TCP 正常；回环 UDP **不放开**，`--loopback-udp` 只留在开发工具；根因带进 M3。host-mihomo.sh 和阶段 D 保留作回归，判定固定为“TCP 通、UDP 不通、不退回直连”。证据 `scripts/dev/out/stage-a-20261118-203909.txt`。

**目标**：验证本机代理软件当主代理时，UDP 中继经 192.168.127.254 能不能用（设计 4.5）。

**做法**：
- 在本机起一个**独立的** mihomo：
  - darwin-arm64，下载到 `scripts/dev/out/`，校验 sha256；
  - 只听 `127.0.0.1` 上的 mixed-port；
  - `tun.enable: false`，不碰用户正在用的 Clash；
  - 它唯一的出站是开发上游（`abroad-dev upstream`，socks5，`udp: true`），规则 `MATCH,up`，**没有 DIRECT**。
- 整条链：虚拟机 mihomo → 192.168.127.254:mixed-port（本机 mihomo）→ 开发上游。不向外拨号。

**文件**：
- `scripts/dev/host-mihomo.sh`（新）：下载、校验、生成配置、起停。
- `scripts/dev/vm-up.sh`：加阶段 D，`--loopback-udp`，用 UDP 探测 198.51.100.7:53。
- `guest/manifest.json` 或单独的 dev 清单：记下 darwin 版的版本号和 sha256。

**判定**：
- 回显成功，就把 mihomo 写进“UDP 中继与 TCP 同端口、可用”的名单；
- 失败时记下 BND 地址和 mihomo 日志，结论回写 design 4.5；
- 由 architect 决定是否对用户放开回环 UDP。

Clash Verge、ClashX 等其他软件以后有人用时再测，不在本任务里。

**补充约束**（architect 2026-11-18，审 M2-8 时给出；全部为了不碰用户的 Clash 和宿主机网络）：
1. 只按 PID 文件停进程：脚本启动的 mihomo 把 PID 写进 `scripts/dev/out/` 下的文件；停止时先核对该 PID 的可执行文件路径就是脚本下载的那一个，再结束它。**禁止 `pkill`、`killall` 或按进程名匹配**（用户的 Clash 内核也叫 mihomo）。
2. 配置收紧：`mixed-port` 用不常用的高位端口，启动前检查空闲，被占用就报错退出；`bind-address: 127.0.0.1`、`allow-lan: false`；`tun.enable: false`；不开 DNS 监听；不开 external-controller；关掉 geo 数据的自动下载和更新；代理只有一个，指向开发上游；规则只有 `MATCH,up`；全文不出现 `DIRECT`；工作目录在 `scripts/dev/out/` 下，不读写 `~/.config/mihomo`。
3. 下载：版本和 sha256 写死在清单里，校验不过就退出；不带 sudo，以 root 运行时拒绝。
4. 回环 UDP 是否对用户放开，由 architect 验收时决定；开关保持现状，只在开发工具里可开。

**验收标准**（architect 2026-11-18）：
- a. host-mihomo.sh：带多余参数、以 root 运行时拒绝；sha256 不符时退出且不留下可执行文件（临时改错跑一次验证）；端口被占用时报错退出；start 后 status 显示在运行且只监听 127.0.0.1。
- b. 生成的配置由脚本自查并记入证据：tun 关闭、bind-address 127.0.0.1、allow-lan 关闭、没有 external-controller 和 DNS 监听、代理只有一个、规则只有 `MATCH,up`、全文没有 `DIRECT`。
- c. 不碰用户环境，脚本开始和结束各采一次并比较：除本脚本启动的之外，名字含 mihomo 或 clash 的进程 PID 集合不变；`route -n get 1.1.1.1` 的接口不变；`scutil --proxy` 输出不变；脚本源码里没有 `pkill`、`killall`、`sudo`、`networksetup`、`pfctl`、`ifconfig`、`route add/delete/change`。
- d. 阶段 D：TCP 探测经这条链拿到 204；UDP 探测有回显，或失败并留下足够现场（两端 mihomo 日志片段、UDP ASSOCIATE 返回的 BND 地址和端口，脱敏）；开发上游的计数显示请求确实经本机 mihomo 到达；绑物理网卡的拨号数（`dial_ok − dial_ok_loopback`）前后不增长。
- e. 反例：不启动本机 mihomo 直接跑阶段 D，TCP 和 UDP 都失败并报出明确原因，开发上游没有收到请求，绑物理网卡的拨号数不增长（没有退回直连）。
- f. 收尾：脚本退出后（含中途出错）本脚本启动的 mihomo、开发上游、虚拟机都已停止，端口释放，PID 文件删除；再跑一次 stop 不报错；vm-up 原有阶段 A、B、C、E 仍通过。
- g. build、vet、arm64 build、`go test -race -count=1 ./internal/... ./cmd/...` 全过；会话里不执行改动宿主机网络的命令、不执行 pfctl、不停不改用户的 Clash。
- h. 交验收时给出证据文件路径、UDP 是否回显、失败时 BND 地址的形态（127.0.0.1、0.0.0.0 还是别的）；是否放开和 design 4.5 怎么改由 architect 判定后回写。

**需要用户**：否。不碰用户的 Clash，不改网络。执行顺序挪到 M2-8 之后。

### M2-2 PhysicalInterface 挪包并改成后台刷新

**结果**（2026-11-18）：已按 architect 写死的语义实现。Watcher 的阈值：新鲜期 2 秒，MaxStale 30 秒，失败缓存 1 秒。拨号器删掉了自己的缓存，加了 `OnBindFailed`。第 0 步把 M2-1b 的 4 条建议改都做了，其中第 4 条“端口字段按类型读取”也做了。

**文件**：
- `internal/physif/physif.go`（新）：
  ```go
  func Query(ctx context.Context) (string, error)
  type Watcher struct{ /* 缓存 + 后台刷新 */ }
  func NewWatcher(q func(context.Context) (string, error), every time.Duration) *Watcher
  func (w *Watcher) Current(ctx context.Context) (string, error) // 有旧值先返回旧值，同时触发后台刷新；没有值时同步查一次
  func (w *Watcher) Invalidate()                                 // 绑定失败时调用
  ```
- `managedproxy.PhysicalInterface` 改成调用 `physif.Query`，Cage 的行为不变。
- `usernet.Dialer.Interface` 改为注入 `Watcher.Current`。
- `cmd/abroad-vm` 不再 import managedproxy，加 source_test 断言。
- `physif.Query` 的结果是 utun、lo、bridge、vmenet 开头的网卡时，一律当失败处理。

**测试**：
- 旧值立即返回，后台只跑一个刷新；
- `Invalidate` 后同步查；
- 查询失败时：有旧值仍用旧值，并只在 TTL 内使用；
- 并发调用只查一次。

**需要用户**：否。

### M2-4 fake-IP DNS（`internal/fakedns`）：拆成 4a、4b（architect 2026-11-18）

#### M2-4a 地址池与落盘（只用标准库）

- `pool.go`：
  ```go
  func NewPool(first, last netip.Addr) (*Pool, error) // 可分配范围（含两端）
  func DefaultPool() *Pool                            // 198.18.1.0–198.19.255.254
  func Normalize(name string) (string, error)         // 小写、去末尾点；空名、"."、超长、非法字符拒绝
  func (p *Pool) Assign(name string, now time.Time) (netip.Addr, error) // ErrBadName、ErrPoolFull
  func (p *Pool) Lookup(a netip.Addr) (string, bool)
  func (p *Pool) Acquire(a netip.Addr, now time.Time) (name string, release func(), ok bool)
  func (p *Pool) Stats() Stats // 映射数、淘汰次数、池满次数，不含域名
  ```
  - 起点用 sha256（重启后不变），冲突顺延；找空位用“下一个空位”并查集。淘汰直接复用地址，所以空位只减不增。
  - 淘汰用 LRU 链表，链表里只放引用数为 0 的映射：表头最久没用，满 30 分钟才可淘汰；没有可淘汰的回 ErrPoolFull，绝不提前复用。
  - 原计划的 `Touch` 改成 `Acquire`/release：查找和引用计数加一在同一把锁里完成，release 幂等，释放时刷新最近使用时间。
  - 最近使用时间用墙上时钟，比 now 还晚的按 now 处理（时钟回拨时不会因此被淘汰）。
- `persist.go`：
  - 文件格式：魔数、版本、条数、每条（地址、最近使用、域名），末尾 sha256。上限 48 MiB。
  - `Load` 全有或全无，加载后引用数为 0。
  - `SaveFile`：临时文件 → fsync → rename，权限 0600，失败不留临时文件。
  - `LoadFile`：用 O_NOFOLLOW 打开，只接受普通文件，文件不存在返回 ErrNoFile。
  - `Run(ctx, path, every, onErr)`：有改动才落盘，退出时做最后一次。

**结果**（2026-11-18）：默认池在 -race 下填满 130815 条用时约 0.6 秒；FuzzLoad 跑 16 秒没有发现问题。

#### M2-4b DNS 服务、Bypass 名单、Upstream 接口、“真实地址 → 域名”反查（architect 2026-11-18 定稿）

**第 0 步**（M2-4a 的建议改）：
- LoadFile 时清理同目录下残留的 `.fakeip-*.tmp`。
- `Prune(now, maxAge) int`：删掉 `refs==0` 且超过 maxAge 没用的映射，删完整体重建并查集。
  - `refs>0` 的映射保留；epoch 不变，旧的 release 仍然有效。
  - 腾出的空位能被新域名按哈希起点重新占用。
- `MaxAge = 30 天`。Run 启动时 Prune 一次，之后每 24 小时一次（间隔可注入，测试不真等）。
- 删掉 watcher.go 里只写不读的 `inflightGen`。

**文件**：`internal/fakedns/` 下新增 `server.go`、`bypass.go`、`reverse.go` 及测试。
- `TestImportsStdlibOnly` 收窄到 `pool.go`、`persist.go`。
- 新文件只能 import 标准库和 `github.com/miekg/dns`，不能 import usernet、managedproxy、physif、wire。

**接口**：
```go
type Upstream interface { Exchange(ctx context.Context, q *dns.Msg) (*dns.Msg, error) } // 经主代理的 DoH，真实实现在 M2-5/M2-9 接
type Server struct{ Pool *Pool; Upstream Upstream; Bypass *Bypass; Reverse *Reverse; Now func() time.Time }
func (s *Server) Handle(ctx context.Context, q *dns.Msg) *dns.Msg
func (s *Server) ServeUDP(ctx context.Context, pc net.PacketConn) error
func (s *Server) ServeTCP(ctx context.Context, ln net.Listener) error
```

**行为**：
- A 查询（IN 类），不在 Bypass 名单：`Pool.Assign`，回一条 A，TTL 1 秒。
  - `ErrPoolFull` → SERVFAIL 并计数；
  - `ErrBadName` → 交 Upstream，不分配。
- AAAA：回空的 NOERROR，不问 Upstream（名单内的域名也一样）。
- HTTPS、SVCB（类型 65、64）：回空的 NOERROR，不问 Upstream（名单内的域名也一样）。
- PTR：198.18.0.0/15 的回 NXDOMAIN；其他 PTR 交 Upstream。
- 名单内域名的 A：交 Upstream，把应答里每个 A 记进 Reverse（真实地址 → 域名）。上限 4096 条，按 LRU 淘汰；保留时间取 TTL 和 30 分钟中较长的。
- 其他类型：交 Upstream。
- Upstream 出错、超时（默认 5 秒）或为 nil：SERVFAIL。没有任何退回本机解析器的路径。
- 多问题、非 IN 类、opcode 不是 QUERY：回 FORMERR 或 NOTIMP，不问 Upstream。
- 应答回显问题段和 ID，RA=1；UDP 应答超过对方的 EDNS 大小时置 TC。
- **Bypass 名单**：内置名单，可增删；支持精确匹配和 `*.` 后缀匹配，域名先用 `Normalize` 规范化。
  - 内置项：`time.apple.com`、`time-ios.apple.com`、`time-macos.apple.com`、`captive.apple.com`、`*.lan`、`*.local`。
  - **硬性保护**：`anthropic.com`、`claude.ai`、`claude.com` 及其子域永远不能进名单。`Add` 时拒绝，匹配时也再判一次。
- `local_domains` 在 M2 不实现。
- 统计只有计数（各类型查询数、SERVFAIL 数、Upstream 失败数），不含域名；日志不写域名。

**验收标准**：
- a. 表驱动测试打 `Handle`，覆盖每个分支。用假 Upstream 记调用次数，断言 AAAA、HTTPS、SVCB、fake 段 PTR、格式错误的请求都不调用 Upstream。
- b. 同一域名查两次、大小写和末尾点不同，得到同一地址；`Pool.Lookup` 能查回域名。
- c. 池满（小池加注入时钟）回 SERVFAIL，计数加一，池内映射不变。
- d. Upstream 返回错误、超时、为 nil，三种情况都回 SERVFAIL。
- e. 源码断言：非测试文件里不出现 `net.Resolver`、`net.Lookup`、`net.DefaultResolver`、`/etc/resolv.conf`，也没有直连 53 端口的 `net.Dial(`。
- f. Bypass：
  - 精确匹配、通配匹配、增删；
  - Claude 相关域名 `Add` 被拒；即使绕过 `Add` 直接塞进名单，查询时仍然分配假地址；
  - 名单内的 A 应答写进 Reverse，查得回；超过上限时淘汰最旧的。
- g. 在 127.0.0.1:0 上起 `ServeUDP`、`ServeTCP`，用 miekg 客户端往返一次；超大应答触发 TC；ctx 取消后两个 Serve 都返回，不泄漏 goroutine。
- h. 50 个 goroutine 混合并发查询，`-race` 下通过。
- i. 4a 的测试全过；build、vet、arm64 交叉编译、`go test -race ./internal/... ./cmd/...` 全过。
- j. 文档：本节；design 4.3 和 16；PROGRESS。

### M2-5 网络栈：拆成 5a、5b、5c（architect 2026-11-18）

| 项 | 内容 |
|---|---|
| M2-5a | `internal/tun`（Device、Pipe、`utun_darwin.go` 只编译）+ `internal/stack/stack.go`（gVisor 栈、Handler 接口、流数上限、丢 IPv6/ICMP），含第 0 步 |
| M2-5b | `internal/stack/proxy.go`：闸门、fakedns 查找与 Acquire/release、wire 拨号与双向转发、UDP 每五元组一条流、53 端口交 `fakedns.Server`；进程内全链路（`tun.Pipe` → stack → wire → `agent.DataPlane` → 假 socks），另加“连接存续期间映射不被淘汰、关闭后 release” |
| M2-5c | `internal/tunsim` + `abroad-dev tunsim` + vm-up 阶段 E（经 data.sock 进真实虚拟机，拿到 204、UDP 回显） |
| M2-5d | 经 `Opener` 的 DoH `Upstream`（fake-IP DNS 把非 A 查询交主代理），用进程内 TLS 假服务器做单测；真机验证并入“真实远端节点验证” |

#### M2-5a 实施计划（草案，待 architect 审）

**第 0 步**（M2-4b 的建议改，在 `internal/fakedns/server.go`）：
1. `upstream()`：应答的 Answer 里只要有 A 落在 198.18.0.0/15，整个应答改成 SERVFAIL，计新计数 `upstream_fake`（同时计入 servfail），不写进 Reverse。
2. ANY（类型 255）回 NOTIMP，不问 Upstream。
3. `ServeTCP` 加连接上限 `MaxTCPConns = 64`，超过的连接 Accept 之后立即关闭，计 `tcp_rejected`。
4. 拆出 `ServeConn(ctx, c net.Conn)`（单条 TCP 连接的循环）和 `HandlePacket(ctx, in []byte) []byte`（解包 → Handle → 按 EDNS 截断 → 打包；解不开或是应答包时返回 nil）。`ServeTCP`、`ServeUDP` 改成调用它们。

**依赖**：
- 直接用 go.mod 里钉住的 `gvisor.dev/gvisor v0.0.0-20240916094835-a174eb65023f`，从 indirect 变成 direct，版本号不变（用 `git diff go.mod` 证明）。
- 不引入 wireguard-go。

**文件**：

- `internal/tun/tun.go`：
  ```go
  type Device interface {
      Read(p []byte) (int, error)  // 一次一个完整 IP 包（不含地址族头）
      Write(p []byte) (int, error) // 一次一个完整 IP 包
      MTU() int
      Name() string
      Close() error // 幂等；关闭后 Read/Write 返回 ErrClosed
  }
  var ErrClosed = errors.New("tun 设备已关闭")
  ```
- `internal/tun/pipe.go`：`Pipe(mtu int) (a, b Device)`。
  - 两端各一个有界队列（默认 256 个包）；Write 拷贝一份包，队列满时阻塞，任一端 Close 后返回 ErrClosed。
  - 一端 Close 后，两端的 Read 都返回 ErrClosed。
  - 包边界保持；超过 MTU 的包 Write 报错。
- `internal/tun/utun_darwin.go`：
  - 用 `x/sys/unix`：`socket(AF_SYSTEM, SOCK_DGRAM, SYSPROTO_CONTROL)` → `CTLIOCGINFO` 取 `com.apple.net.utun_control` 的 id → `connect(sockaddr_ctl{unit})`。unit 为 0 时由系统分配，用 `getsockopt(UTUN_OPT_IFNAME)` 取名。
  - Read/Write 处理 4 字节地址族头（AF_INET 或 AF_INET6，网络字节序）。读到 IPv6 包照样交上去，由栈丢弃并计数。
  - 只要求能编译；真实打开 utun 的测试放 `//go:build integration && darwin`，会话里不跑。不配地址和路由（M2-7、M2-9）。
  - 这里写 utun 的实现，包注释里写明为什么不用 wireguard-go，回写 design 4.1。
- `internal/stack/stack.go`：
  ```go
  type Flow struct{ Src, Dst netip.AddrPort }
  type Handler interface {
      AcceptTCP(id Flow) bool // 握手前调用；false → 对 SYN 回 RST，不建立连接
      TCP(ctx context.Context, id Flow, conn net.Conn)
      AcceptUDP(id Flow) bool // 新五元组的第一个包；false → 丢弃，不建流
      UDP(ctx context.Context, id Flow, conn net.PacketConn)
  }
  type Options struct {
      MaxTCP      int           // 默认 4096
      MaxUDP      int           // 默认 1024
      UDPIdle     time.Duration // 默认 60 秒
      TCPInFlight int           // 握手中的上限，默认 512（gVisor forwarder 的 maxInFlight）
  }
  func New(dev tun.Device, h Handler, o Options) (*Stack, error)
  func (s *Stack) Stats() Stats // 计数：tcp_active、udp_active、tcp_rejected、udp_rejected、dropped_ipv6、dropped_icmp、dropped_other
  func (s *Stack) Close() error // 关设备、断开所有流、等所有 goroutine 退出，幂等
  ```
  - gVisor 的 `stack.New`：只注册 `ipv4.NewProtocol`、`tcp.NewProtocol`、`udp.NewProtocol`，不注册 ipv6 和 icmp。
  - NIC 用 `channel.New`，开 promiscuous 和 spoofing，路由表只有一条 `0.0.0.0/0 → NIC`，不配任何通往宿主的路由。
  - 两个 goroutine 搬包：
    - 设备 → 栈：读包，先看版本号。IPv6 计 `dropped_ipv6` 直接丢；IPv4 里协议是 ICMP 的计 `dropped_icmp` 直接丢；不是 TCP、UDP 的计 `dropped_other` 丢；其余 `InjectInbound`。
    - 栈 → 设备：`ReadContext` 后写回设备。
    - 入口就丢，栈不回任何包（也不回 ICMP 不可达）。
  - TCP：`tcp.NewForwarder(s, rcvWnd 0（默认）, TCPInFlight, fn)`。fn 里先算 Flow，流数达到 MaxTCP 或 `AcceptTCP` 返回 false 时 `r.Complete(true)` 回 RST；否则 `CreateEndpoint`、`gonet.NewTCPConn`，在新 goroutine 里调 `h.TCP`，返回后关连接、计数减一。
  - UDP：`udp.NewForwarder`。fn 里流数达到 MaxUDP 或 `AcceptUDP` 返回 false 时直接返回（gVisor 丢掉这个包，不建端点）；否则 `CreateEndpoint`、`gonet.NewUDPConn`，用一层包装实现空闲超时：每次读写成功都刷新，到 UDPIdle 没有活动就关闭；然后调 `h.UDP`。gVisor 按五元组把后续包投递到同一个端点，所以同一五元组自然复用同一条流。
  - 每个流的 ctx 都派生自 Stack 的 ctx，Close 时取消并关掉所有连接，WaitGroup 等全部退出。

**architect 审计划后补充**（2026-11-18；2 条必须改，建议改全部采纳）：
- **必须改 1：TCP forwarder 的每条路径都调用 `Complete`**。
  - gVisor 的 inFlight 表项只有 `Complete` 才删除；漏掉的话，累计建过 TCPInFlight 条连接后，所有新 SYN 都会被静默丢弃。
  - 路径：拒绝时 `Complete(true)`；`CreateEndpoint` 成功时 `Complete(false)`；失败时 `Complete(true)` 并归还名额。
  - MaxTCP 的名额用原子预占（CAS），之后任何失败都归还。
  - 测试：TCPInFlight 设为 4 时，顺序建立并关闭 20 条以上连接（中间夹几条被拒的）；并发 2×MaxTCP 条拨号，tcp_active 的峰值不超过 MaxTCP。
- **必须改 2：utun 的 Close 必须唤醒阻塞中的 Read**。`unix.SetNonblock` 之后用 `os.NewFile` 包装，读写走 `*os.File`，`os.ErrClosed` 映射成 `tun.ErrClosed`。integration 测试加“1 秒内唤醒”一条（需要 root，用户执行）。
- **建议改**：
  - **AcceptUDP**：在“设备 → 栈”的搬运 goroutine 上同步调用，不能阻塞、不能做 I/O，接口注释写明；`h.UDP` 在新 goroutine 里调。
  - **h.UDP 返回即关端点、归还名额**：写进接口约定，并加测试。
  - **UDP 包装层固定对端**：`WriteTo` 忽略传入的地址，一律发回 `Flow.Src`；`ReadFrom` 返回 `Flow.Src`。
  - **去掉 `Options.MTU`**，改用 `dev.MTU()`。
  - **UDP `CreateEndpoint` 失败**按丢包处理，并归还名额。
  - **入口过滤**：
    - 不足 20 字节、版本号既不是 4 也不是 6、IHL 非法的包，计入 `dropped_other`；
    - IPv4 分片按协议号判断，TCP、UDP 的分片照常交给栈重组。
  - **Stats**：加 `tcp_inflight_drop`，取自 gVisor 的 `ForwardMaxInFlightDrop`。
  - **Close 的顺序**：取消 ctx → 关设备 → 等两个搬运 goroutine → 关所有连接 → 等所有 handler → gVisor 的 `Close` 和 `Wait`。接口注释写明 Handler 在 ctx 取消或连接关闭后必须返回。
  - **utun 细节**：Write 按 IP 版本号填 4 字节头；Read 的缓冲区比包小时报错，不静默截断；读路径不每包分配。
- **默认上限**（MaxTCP 4096、TCPInFlight 512、MaxUDP 1024、UDPIdle 60 秒）作为初值，M2-9 做 R6 实测时复核。

**测试**（`internal/stack/stack_test.go`、`internal/tun/pipe_test.go`）：
- **造包方式：用第二个 gVisor 栈当客户端**，挂在 `tun.Pipe` 的另一端。
  - 客户端用 `gonet.DialTCP` / `gonet.DialUDP` 发起连接，这样握手、半关闭、RST 都是真实的 TCP 行为，不用手搓状态机。
  - IPv6 包和 ICMP echo 用 gVisor 的 `header` 包手搓字节，直接写进 Pipe。
- 覆盖验收标准 a–k：
  - Flow 源和目的一致；双向数据；半关闭；
  - AcceptTCP 返回 false 时客户端拨号收到 connection refused，TCP 回调没被调用；
  - UDP 往返时，回包的源地址是原目的地址；同一五元组复用；注入短 UDPIdle 后流被回收；AcceptUDP 返回 false 时没有回包；
  - IPv6、ICMP 被丢，计数加一，Handler 没被调用；
  - 流数上限、释放后能再建；
  - Close 之后 goroutine 数回到起点附近，重复 Close 不 panic；
  - 源码断言：不 import wireguard，不出现 `net.Dial`、`net.Listen`。
- `utun_darwin.go`：`GOOS=darwin go vet` 通过即可；`internal/tun/utun_integration_test.go`（打开真实 utun，需要 root）会话里不跑。

#### M2-5b 代理处理器与进程内全链路（architect 2026-11-18 审定，即计划）

**第 0 步**（M2-5a 的建议改）：
1. utun 读包可能被静默截断：`rbuf` 多留 1 字节，`n > 4+mtu` 时丢弃并计数，然后继续读。
2. `udpFlow` 加 `closed` 标志：Close 之后，touch 不再重装定时器。
3. Stack 加 `Done() <-chan struct{}` 和 `Err() error`：任一搬运 goroutine 因 Close 之外的原因退出时关闭 Done。M2-7、M2-9 把它当故障处理（重建 utun 和栈，期间闸门关闭）。
4. Handler 注释补一句：AcceptTCP 可能在 Close 之后被调用，必须没有副作用。

**范围**：
- 没有规则引擎、应用识别、首包嗅探（都在 M3），所有被接受的流一律走 `guestabi.ExitMain`。
- 闸门只定义接口，真实实现在 M4。

**接口**（`internal/stack/proxy.go`）：
```go
type Gate interface{ Open() bool }               // 必须不阻塞（原子读）；AcceptUDP 在搬运 goroutine 上调用它
type Opener interface{ Open(ctx context.Context, h wire.Header) (net.Conn, error) } // *wire.Dialer 满足
type Proxy struct {
    Pool   *fakedns.Pool
    DNS    *fakedns.Server
    Gate   Gate
    Opener Opener
    Now    func() time.Time
    // DNSAddr 默认 198.18.0.2:53；DNSIdle 默认 5 秒；MaxDNSTCP 默认 64
}
func (p *Proxy) Stats() ProxyStats // 只有计数，不含域名、地址
func (p *Proxy) DropAll()          // 断开所有走代理的流（不含 DNS），给“停用”第 ② 步用
```
`Proxy` 实现 `stack.Handler`。

**目的地址的判定**（AcceptTCP、AcceptUDP 共用同一个纯函数，按顺序判断）：
1. 目的是 DNSAddr：接受，不看闸门。
2. 目的 IP 是 198.18.0.2 但端口不是 53，或者落在 198.18.0.0/24：拒绝。
3. 目的在 198.18.0.0/15：`Pool.Lookup` 查不到就拒绝，计 `no_mapping`，不猜。
4. 目的是 0.0.0.0/8、127/8、169.254/16、224/4 或 255.255.255.255：拒绝，计 `bad_dst`。
5. 闸门关闭：拒绝，计 `gate_closed`。
6. 其余接受：fake 段的目的用域名，真实 IP 直接用 IP。

**TCP**：
- DNS 连接：受 MaxDNSTCP 限制，超出就直接关；否则交给 `DNS.ServeConn`。
- 代理连接：
  1. `Acquire`（失败就关连接）；
  2. 再查一次闸门；
  3. `Opener.Open`，帧头 `{V, Exit: main, Net: tcp, Host 或 IP, Port}`，App 留空；
  4. 失败就关连接，并按错误码计数。不重试、不换出口、没有任何直连路径；
  5. 双向转发，半关闭照实透传；两个方向都结束、ctx 取消或 DropAll 时关两端。
- 零字节 EOF 只计数，不计入任何健康判定。

**UDP**：
- DNS 流：循环读包，每个包起一个 goroutine 调 `HandlePacket` 后回写，每条流内并发上限 16。DNSIdle 内没有新包、也没有在途查询时返回。
- 代理流：`Acquire` → 查闸门 → `Open`（Net: udp）→ `wire.NewDatagramConn` → 两个方向各一个 goroutine，任一方向出错就关两端并返回。
  - `udp_unsupported` 和其他 Open 失败：丢弃该流并计数，不改走直连。
  - 缓冲区 65535 字节。
- AcceptUDP 里只做纯判定（Lookup 加原子读闸门），不做 I/O。

**验收标准 a–k**：architect 2026-11-18 的任务说明原文，见 PROGRESS 会话日志。

**实现中补充的两点**（2026-11-18，交验收时一并说明）：
- **强制断开时先设过去的期限再关**（`abort`）。wire 的流是 yamux，它的 Close 只关写方向，读会一直等到对端结束；只调 Close 的话，ctx 取消或 DropAll 时转发 goroutine 会挂到上游自己结束。真机上表现为 Stack.Close 卡住。
- **任一方向读到错误（RST、被关、超时）时两端一起强制断开**；只有读到正常 EOF 才半关闭对端。否则应用 RST 后、上游一直不说话时，这条流和它的映射引用会一直挂着。

#### M2-5c 实施计划（architect 2026-11-18 审定）

**审计划时的补充**（没有必须改；6 条建议改全部采纳）：
- devupstream 的认证协商：
  - 对方提供了方法 0：仍选方法 0，回 `{5,0}`（阶段 C 里 mihomo 走的就是这条路）；
  - 只提供方法 2：回 `{5,2}`，读完 RFC 1929 子协商后回 `{1,0}`；
  - 两种都没提供：回 `{5,0xFF}` 并关连接。
  - 三种情况各有单测；`TestNoDialing` 仍要通过。
- Run 的收尾顺序按“停用第 ② 步”来：关闸门 → DropAll → `wire.Dialer.Close`（只关 Run 自己建的）→ `Stack.Close`。
- 步骤 f 增加 UDP 检查：关闸门后向假地址:UDPPort 发一个包，1 秒内不应收到回显，Opener 调用次数不变。
  - 下面三种情况都算 f 失败：OpensAfterGate 不等于 OpensBeforeGate；Proxy 的 GateClosed 没有增长；Dropped 没有增长。
- 步骤 a 失败时，b、c、f 直接记失败（原因写“步骤 a 未通过”）；d、e、g 照常跑。
- 步骤 e 的无映射地址不写死：如果和 a 拿到的假地址相同就换一个。
- 阶段 E 的位置和诊断：
  - 插在阶段 C 汇总之后、`term_wait SOCKS_PID` 之前，用自己的 netstats 起止点；
  - 只在开发上游新增的日志行里找；tunsim 用 `--timeout 15s`；
  - 失败时写诊断（开发上游新增行、mihomo 和 sing-box 日志尾部、health 的 data 段）；
  - 结束后断言 active_streams 回到 0；
  - 重新下发主代理后，断言输出是 `applied udp=true`。
- 反例测试还要断言：`ExitDown >= 1`，e、f 里的 DNS 检查和 g 仍然 ok。

**实现中的调整：方案 A**（architect 2026-11-18 拍板）：
- **问题**：真机阶段 E 里，步骤 c 的 UDP 发往假地址（域名 tunsim.test）没有回显。
  - 虚拟机里的 mihomo 对 UDP 的域名目标，会先经主代理的 DoH 在本地解析（design 7.2，阶段 C 的 c4）。
  - 开发上游答不了 DoH，所以包被丢掉。失败方向是断，不是泄漏。
- **做法**：
  - `Config.UDPTarget`（命令行 `--udp-target`）：给了就让步骤 c 发往这个真实 IP，步骤名为 `c_udp_realip`；不给仍发往假地址，步骤名为 `c_udp_fake`。两种都要求原样回显、回包源地址等于发送的目的地址。
  - 步骤 f 关闸门后的 UDP 检查也发往同一个目的地址。
  - `--udp-target` 必须是 IPv4，端口非 0，且不在 198.18.0.0/15、0/8、127/8、169.254/16、224/4、255.255.255.255 内；不合法时退出 2。
  - 阶段 E 传 `--udp-target 198.51.100.7:4500`，断言开发上游新增 `udp ATYP=1 198.51.100.7:4500 echo`，并且没有 `connect 1.1.1.1:443`。
- **覆盖缺口**：“UDP 域名目标在真机上端到端回显”分三段各自被证明：
  - 进程内 run_test：假地址还原成域名，带 Net: udp 开流；
  - 阶段 C 的 c4：虚拟机里域名 UDP 经主代理的 DoH 解析，不直连；
  - 阶段 E：真实 IP 的 UDP 在真机上回显。
  
  整条连起来的真机正例并入“真实远端节点验证”。

**第 0 步**（M2-5b 的建议改，已做完）：
- `TestChainAppFinFirst`：应用先 FIN、上游读到 EOF 后才应答，-race 下连跑 20 次都通过；
- 被取消打断的 Open 不计入 `open_failed_other`，测试 `TestCanceledOpenNotCountedAsFailure`；
- 3、4 条写进了 PROGRESS；M2-5d 已拆出。

**文件**：

1. `internal/tunsim/client.go`（新，开发用，不进发布包）：把 stack_test.go 里的 gVisor 客户端栈做成非测试代码。
   ```go
   type Client struct{ /* gVisor 栈 + channel 端点 + 两个搬运 goroutine */ }
   func NewClient(dev tun.Device, src netip.Addr) (*Client, error) // src 默认 198.18.0.1
   func (c *Client) DialTCP(ctx context.Context, dst netip.AddrPort) (net.Conn, error)
   func (c *Client) DialUDP() (net.PacketConn, error) // 未连接的 UDP（便于核对回包源地址）
   func (c *Client) LookupA(ctx context.Context, name string, dns netip.AddrPort) (netip.Addr, error) // miekg/dns 打包
   func (c *Client) Close() error
   ```
2. `internal/tunsim/run.go`（新）：
   ```go
   type Config struct {
       DataSock string        // abroad-vm 的 data.sock
       Name     string        // 默认 tunsim.test
       UDPPort  int           // 默认 4500
       RealIP   netip.AddrPort // 默认 203.0.113.9:80
       Timeout  time.Duration // 单步上限，默认 30 秒
       Opener   stack.Opener  // 测试注入；为 nil 时用连 unix DataSock 的 wire.Dialer
   }
   type Step struct{ Name string; OK bool; Err string `json:",omitempty"` }
   type Report struct {
       OK                bool
       Steps             []Step
       OpensBeforeGate   int64 // 关闸门前 Opener 的调用次数
       OpensAfterGate    int64 // 步骤 f 结束时的调用次数（应与关闸门前相同）
       Stack             stack.Stats
       Proxy             stack.ProxyStats
   }
   func Run(ctx context.Context, c Config) Report
   ```
   - `tun.Pipe(1500)`：一端接 `stack.New` 加 `stack.Proxy`；另一端接 Client。Proxy 的组件：
     - Pool 用 `fakedns.DefaultPool`；
     - DNS 用 `fakedns.Server`，Upstream 为 nil，Bypass 用内置名单；
     - Gate 可开关，初始打开；
     - Opener 包一层计数后交给 wire.Dialer，Dial 只连 `unix` 的 DataSock。
   - 按顺序跑 a–g，每步都有超时，失败时记下原因后继续跑后面的步骤（g 始终执行）：
     - a. 查 Name 的 A：地址在 198.18.0.0/15 内、不在 198.18.0.0/24 内；
     - b. TCP 连假地址:80，发 `GET /generate_204`（Host 为 Name，Connection: close）：要求 204，并读到 EOF；
     - c. UDP 向假地址:UDPPort 发 32 字节随机数据：要求收到原样回显，回包源地址等于假地址:端口；
     - d. TCP 连 RealIP，同样 GET：要求 204；
     - e. 连 198.19.255.250:443（没有映射）：要求 connection refused；
     - f. 先建一条保持不关的 TCP（连假地址:80，不发请求），记下 OpensBeforeGate；然后关闸门并 DropAll。要求：
       - 这条流被断开；
       - 新 TCP（连假地址:80）被拒；
       - DNS 仍应答；
       - 记下 OpensAfterGate，应等于 OpensBeforeGate。
     - g. `Stack.Close` 在 10 秒内返回。
   - 报告里只有步骤名、ok、失败原因、计数。失败原因用 `redact.String` 脱敏后再写。
3. `internal/devupstream/devupstream.go`：问候时如果对方只提供“用户名口令”认证（方法 2），接受任意用户名和口令。这样进程内测试里 `agent.DataPlane` 的 SOCKS 客户端能直接连它。它只听 127.0.0.1，无认证的行为不变；补一条单测。
4. `cmd/abroad-dev/main.go`：新增子命令 `tunsim --data-sock <路径> [--name tunsim.test] [--udp-port 4500] [--timeout 30s]`。
   - 输出一行 JSON 报告；全部通过退出 0，任一步失败退出 1，参数错误退出 2；
   - data.sock 不存在时退出 1，并给出可读的错误；
   - 文件头的用法注释同步更新。
5. `scripts/dev/vm-up.sh`：阶段 E，接在阶段 C 之后，同一台虚拟机，默认收紧模式。
   - 先重新下发 `--loopback-udp` 的开发上游主代理。c6 下发的是链式配置，c5 之后的配置 udp=false，所以要重下。
   - 记下 health 的 `data.total_streams` 和 netstats，然后运行 `abroad-dev tunsim`。断言：
     - 退出码为 0，JSON 里每一步都 ok；
     - 开发上游日志新增的行里有三类：
       - `connect`、ATYP=3、host=tunsim.test、204；
       - `udp`、ATYP=3、host=tunsim.test、echo；
       - `connect`、ATYP=1、203.0.113.9、204；
     - netstats 增量：dial_ok 等于 dial_ok_loopback，policy_denied +0，bind_failed +0；
     - total_streams 的增量等于 Report 里的 OpensAfterGate，说明关闸门之后没有再开流。
6. `scripts/dev/README.md`：补阶段 E 的说明。

**测试**：
- `internal/tunsim/run_test.go`：进程内跑整条链。
  - 组成：真实的 `agent.DataPlane`、devupstream 当 sing-box，wire 经 net.Pipe 注入 Config.Opener。
  - 正例：a–g 全部 ok，开发上游日志有上面三类条目，OpensAfterGate 等于 OpensBeforeGate。
  - 反例：出口表的 Lookup 返回“出口不可用”，期望步骤 b 失败、报告写明原因、整体不 ok。
- `internal/tunsim/source_test.go`：非测试文件里不出现 OpenUtun、exec.Command、pfctl、networksetup、ifconfig、route、net.Listen、net.Resolver、net.Lookup；Dial 只允许 `"unix"`。
- `cmd/abroad-dev/main_test.go`：
  - 参数错误退出 2；
  - data.sock 不存在时退出 1，错误可读；
  - 用临时 unix socket 加进程内 DataPlane 跑一遍，退出 0。
- 真机：主会话自己跑 `scripts/dev/vm-up.sh --no-panic`，阶段 A 到 E 全部通过。证据文件路径写进 PROGRESS；脚本退出后进程都已停、socket 都已清理。

#### M2-5d 经主代理的 DoH Upstream（architect 2026-11-18 拍板，即计划）

**第 0 步**（M2-5c 的建议改，在 `internal/tunsim/run.go`）：
- “已有的 TCP 被断开”的判定抽成 `holdDropProblem`，读期限单独设成 `min(Timeout, 5s)`，读超时时报“没有被断开（读超时）”；
- `gate_closed` 要求至少增长 2（新 TCP、UDP 各一次）；
- 建 UDP 端点失败时记一条问题，不再静默跳过。

**设计**：
- `internal/fakedns/doh.go`：`DoHUpstream` 实现 `Upstream`。
  - 自己定义 `StreamOpener{Open(ctx, wire.Header) (net.Conn, error)}`，不引用 stack 包。
- **目标**：帧头固定为 `Exit=main, Net=tcp, IP=1.1.1.1, Port=443`，Host 为空。URL 是 `https://1.1.1.1/dns-query`；`DialTLSContext` 忽略传入的地址，永远开这条流。
- **TLS**：校验证书，ServerName 为 `1.1.1.1`，默认用系统根证书，`RootCAs` 可注入给测试；不允许 `InsecureSkipVerify`。
- **HTTP**：
  - POST `application/dns-message`，发出去的 ID 置 0；
  - `Proxy` 为 nil，不跟随重定向；
  - 只接受 200 且 Content-Type 正确；应答体不超过 64 KiB；解不开或问题段不一致都算错误；
  - 连接复用，空闲 30 秒关闭。
- **闸门**：闸门关着时 `Exchange` 直接返回错误，Server 答 SERVFAIL，不调用 Open。`Close()` 关掉空闲连接。
- **错误信息**：不带查询的域名。
- **tunsim**：
  - `Config.Upstream`（测试注入）和 `DoHName`。DoHName 非空时，在 e 之后、f 之前加一步 `h_doh`：经 198.18.0.2:53 查 TXT，要求 NOERROR 且有应答；
  - f 里加一条：关闸门后同样的查询得到 SERVFAIL，Opener 调用次数不变；
  - DoH 用的 Opener 是同一个 countingOpener。
- **命令行**：`abroad-dev tunsim --doh-name <域名>` 时用真实的 DoHUpstream（系统根证书）。vm-up 阶段 E 不带这个参数（开发上游答不了 DoH）。

**验收标准 a–l**：见 architect 2026-11-18 的任务说明（PROGRESS 会话日志里有摘要）。

### M2-6 拆成 6a、6b（architect 2026-11-18）

#### M2-6a 规则生成器、求值器、语法检查脚本（architect 2026-11-18 定稿，即计划）

**三条决定**：
1. Render 输出展开后的规范形式（不用宏、不用花括号列表，每个接口、协议、目的网段各一行，只保留 `<abroad_lan>` 这一张表）。design 6.1 的规则块保留作示意。
2. DHCP 只放行到 255.255.255.255 和私有范围（RFC1918、100.64/10、169.254/16）的 `udp 68 → 67`。
3. `Eval(rules []byte, pkt Packet) (pass bool, err error)`：遇到不认识的行返回错误，不猜；没有任何规则命中时不放行。

**文件**：
- `internal/pf/rules.go`：
  - `Config{UtunName, LAN, VMIfs, User}`、`Render`、`LANPrefixes`、`Digest`；
  - 校验：接口名只允许小写字母加数字、不超过 15 字符；User 只允许字母、数字、下划线；LAN 每一项都必须整个落在允许的私有范围内（只是相交不算）。
- `internal/pf/eval.go`：`Packet{If, User, Proto, SrcPort, DstPort, Dst}`、`Eval`。
- `scripts/dev/pf-syntax.sh`：
  - 命令写死为 `/sbin/pfctl -n -f <文件>`；EUID 为 0 时拒绝；不带 sudo；不接受其他参数；
  - 规则由 `abroad-dev pf-render` 生成，User 用 nobody。

**验收标准 a–i**：见 architect 2026-11-18 的任务说明，PROGRESS 会话日志里有摘要。

#### M2-6b Controller、假 pf、Ensure（architect 2026-11-18 定稿，即计划；巡检拆到 6c）

**第 0 步**（M2-6a 的建议改，必做）：
- lo0 放行挪到 inet6 拦截之前（`::1` 放行）。求值表、性质测试（“IPv6 且非 lo0 一律不放行”）、golden 同步更新。
- 名字校验收紧：
  - UtunName：`^utun[0-9]{1,4}$`；
  - VMIfs：`^(bridge|vmenet|vmnet|vnic)[0-9]{1,4}$`，前缀表是包内变量，R15 实测后再扩；
  - User：`^_[a-z0-9_]{1,30}$`，或者恰好等于 `nobody`。

**设计决定**：
1. **Runner 带 stderr**：`Run(ctx, args, stdin) (stdout, stderr []byte, err error)`。`-E` 的 token 可能打在 stderr，两路合并后用 `Token\s*:\s*(\d+)` 取。
2. **比对不写死 `-sr` 的格式**：
   - Status 返回原始文本，比对经 Matcher 接口；
   - 默认实现做规范化：折叠空白，去掉 pass 行尾 pf 自动补的默认修饰，不比 table 行（表内容另行比对），其余任何差异都算不同；
   - 规范化规则依据 `pf-syntax.sh` 里 `-n -v -f` 的实测样本来写；
   - `-nvf` 的打印和已加载锚点的 `-sr` 是否逐行一致，样本在 M2-9 的用户脚本里采集，所以 Matcher 保持可替换。
3. **规则怎么送进 pfctl 按实测定**：`pf-syntax.sh` 里加 `-n -f -` 的正例和反例。正例退出 0、反例非 0，就让 Load 用 `-a <锚点> -f -`（经 stdin）；否则写到私有目录下的 0600 文件，再 `-f <路径>`。
4. **Status 增加 Referenced**：主规则集（`pfctl -s rules`）是否还有 `anchor "com.apple/*"`。没有引用时，Ensure 做完其余步骤后返回 `ErrAnchorNotReferenced`，处置在 6c 定。
5. **KillStates 用 `pfctl -F states`**；Anchor 是字段，校验 `^com\.apple/[0-9]{3}\.[a-z0-9]+$`；token 校验 `^[0-9]+$`。
6. design 6.1 的“监听 SystemConfiguration 通知”改成 M2 用轮询（已改）。

**实测后的决定（方案 A，architect 2026-11-18）**：
- 第一次实测（`-n -v -f`，证据 `scripts/dev/out/pf-full-20261118-183910.nvf.txt`）发现 pfctl 默认的优化器会改写规则：
  - 6 条 DHCP 被合并成自动表 `<__automatic_0>`；
  - 连续的 pass 之间被重排；
  - user 被换成 UID；
  - 补上了 `flags S/SA keep state` 等默认修饰。
- 因此 Load 的参数写死为 `-o none -a <锚点> -f -`。
- **Matcher**：把 pf 打印的每一行还原成 Render 的写法，再和期望文本（去掉 table 行）逐行比对，必须完全相等。只做以下还原，以 `-o none` 的样本为准：
  - 去掉行尾的 ` flags S/SA keep state` 或 ` keep state`；
  - `port = N` 改成 `port N`；
  - `user = <UID 或用户名>` 改成 `user <User>`；
  - 没有源端口条件时，`inet from any to X` 改成 `inet to X`。

  还原后仍不相等的一律判不同，不做任何容错。Matcher 用 (User, UID) 构造，UID 由调用方注入，internal/pf 里不查 os/user。表内容另行比对。
- **`-o none` 样本的自查**（证据 `scripts/dev/out/pf-syntax-20261118-184117.txt`，三份 `*.nvf-onone.txt`）：
  - 三份都没有 `__automatic` 表；
  - 规则顺序与 Render 相同；
  - 规则行数与 Render 一致（不含 table 行）：full 19、boot 15、nolan 15；
  - 与 Render 的差异只有上面四种还原。
  
  符合预期，按方案 A 继续。
- **Load 后复核不一致时**：新规则保留，返回 ErrVerifyFailed，不 Flush。真机打印如有出入，后果是启用时报错，不是泄漏；等 M2-9 采到样本再修 Matcher。
- **FakePF 的打印**：按 `-o none` 样本的形式。Load 的参数里没有 `-o none` 时改用优化后的形式（至少把 DHCP 合并成 `<__automatic_0>`），这样漏掉 `-o none` 时 Ensure 的测试会因 ErrVerifyFailed 失败。

**复审的改动**（architect 2026-11-18）：
1. （必须改）Referenced 按行判断：某一行去掉首尾空白后以 `anchor "com.apple/*"` 开头才算。FakePF.DropMainRef 只去掉 anchor 行，保留 scrub-anchor 行。
2. （设计改动）lo0、utun 两行加 `no state`。
   - Eval 只在 `on <接口> all` 之后接受 `no state`，其他位置仍报不认识；
   - 重新采样 `-n -v -o none`，据此更新 Matcher、FakePF 的打印和 testdata 样本；打印形式出乎意料时停下来找 architect。
3. （建议改，采纳）期望规则里没有 `<abroad_lan>` 时不比对表（关闭“允许局域网”后旧表可能残留，没有规则引用它，无害）。
4. （带进 6c）锚点内容既不等于期望、也不等于上一次成功应用的规则时，按“原先不在”处理，同时 KillStates。

**文件**：`internal/pf/{controller.go, matcher.go, exec.go, fake.go}` 及测试。ExecRunner 只调用 `/sbin/pfctl`，本任务里不被任何测试或命令执行，cmd/ 下也不引用。

**验收标准 a–i**：见 architect 2026-11-18 的任务说明（PROGRESS 会话日志里有摘要）。

#### M2-6c 巡检（architect 2026-11-18 定稿，即计划）

1. **巡检放在 `internal/pf`**：自己定义窄接口 `NetSource{Snapshot(ctx) (NetSnapshot{Direct, VMIfs}, error)}`，由 M2-7 的 sysnet 来适配。路由、DNS、`route get 198.18.0.2` 的核对归 M2-7。
2. **`Ensure(ctx, expected, previous)`**：锚点和 expected 不同时：
   - previous 非空且锚点等于 previous：只 Load（我们自己换了网络）；
   - 否则：Load 后紧跟 KillStates，结果里 `Foreign = true`（别人改的）。
3. **`Step(ctx)` 是一轮巡检**；`Run(ctx)` 按 Interval（默认 5 秒）循环调用它。两者之间用互斥锁串行，状态机也可以直接调用 Step。
4. **期望规则算不出来时不放松**：
   - NetSource 或 Render 出错：沿用上一次成功的期望规则；
   - 从未成功过：用最小配置（只有 User 和当前 utun）；
   - 名字不合规的网桥直接丢掉，并发 VMIfDropped 事件。
5. **`Protected()`**：
   - ErrAnchorNotReferenced、ErrVerifyFailed：当轮就置 false；
   - 其他 Ensure 错误：连续 2 轮才置 false；
   - 任意一轮成功：置 true。
   
   巡检自己不碰闸门，也不改主规则集。
6. **“连续 3 次”**：连续 3 轮都发生了修复（锚点为空、被别人改、pf 被关），就回调一次 RepeatedlyCleared；出现一轮没有修复就重新计数。
7. **新 token 经 `OnToken(tok)` 交出去**；OnToken 为 nil 时 Step 返回错误（不调用 Enable）。

**验收标准 a–j**：见 architect 2026-11-18 的任务说明，PROGRESS 会话日志里有摘要。

#### （原 M2-6 v2 计划原文，供 6b、6c 参考）



下面保留 v2 计划原文，供 6b 参考：

（原 M2-6）pf 规则生成与巡检（`internal/pf`）

**文件**：
- `internal/pf/rules.go`：
  ```go
  type Config struct {
      UtunName string         // 为空时（开机 Boot 时还没有 utun）不输出那一行
      LAN      []netip.Prefix // 允许局域网关闭时为空；只收私有范围（见下）
      VMIfs    []string
      User     string         // _abroad
  }
  func Render(c Config) ([]byte, error) // 设计 6.1 的规则；输入要做校验，接口名和用户名都按白名单字符
  ```
  **`<abroad_lan>` 只收私有范围内的直连网段**：RFC1918、100.64/10、169.254/16、224/4。直连网段是公网地址时不进表。
  原因：恶意的 Wi-Fi DHCP 可以把 Anthropic 的地址段发成直连网段，它比 0/1、128/1 更具体，流量会离开 utun。
  ```go
  func Digest(rules []byte) string
  ```
- `internal/pf/eval.go`：一个只认我们自己生成的规则子集的求值器：
  ```go
  func Eval(rules []byte, pkt Packet) (pass bool) // Packet：接口、用户、协议、源端口、目的端口、目的地址、地址族
  ```
  用途：用一张“包 → 期望结果”的表证明规则语义。这是“不泄漏”的离线证明，真机由 M2-9 的脚本补。
- `internal/pf/controller.go`：
  ```go
  type Runner interface{ Run(ctx context.Context, args []string, stdin []byte) (stdout []byte, err error) }
  type Controller struct{ R Runner; Anchor string } // com.apple/250.abroad
  func (c *Controller) Load(ctx context.Context, rules []byte) error
  func (c *Controller) Flush(ctx context.Context) error
  func (c *Controller) Enable(ctx context.Context) (token string, err error)  // 解析 pfctl -E 的输出
  func (c *Controller) Release(ctx context.Context, token string) error
  func (c *Controller) Status(ctx context.Context) (enabled bool, digest string, err error)
  func (c *Controller) KillStates(ctx context.Context) error // 清掉加载规则之前已有的状态
  ```
  `ExecRunner` 只调用 `/sbin/pfctl` 的绝对路径。
- `internal/pf/fake.go`：内存里的假 pf。能模拟“别的软件清掉锚点”“pf 被关掉”。
- **加载后清状态**：pf 先查状态表、后查规则。别的软件的 `pass keep state` 留下的状态（例如网络共享），在我们加载规则后仍能让旧连接从 en0 继续走。
  - 在“锚点原先不在 → 加载”的三个时机调用 `KillStates`：启用第 ② 步、开机 Boot、巡检发现锚点被清后重载。
  - 摘要一致的巡检不调用。
- `internal/pf/patrol.go`：每 5 秒核对 pf 已启用、锚点摘要一致，路由、DNS、`route get 198.18.0.2` 的检查经 `sysnet` 接口。
  - **每轮按当前网络重算期望规则**：经只读接口 `sysnet.Interfaces` 取网卡和网段，重算 `Config` 再比摘要，不同就重载整个锚点。M2 用轮询，不做 SystemConfiguration 通知。
  - 不一致时重新加载并记日志；
  - 连续 3 次被清掉时发通知（M2 只回调，通知界面属于 M5）。
- `pfctl -n` 语法检查：经 `scripts/dev/pf-syntax.sh`（见 0.1）。root 下用真实用户名的检查仍在 M2-9 的脚本里。

**测试**：
- Render 的 golden 测试；
- 求值器的表包括：
  - inet6 被拦；
  - lo0 放行；
  - utun 放行；
  - DHCP 放行；
  - `_abroad` 放行；
  - LAN 网段放行，“允许局域网”关闭时被拦；
  - 直连网段是公网地址（例如恶意 DHCP 发了 160.79.104.0/23）时不进表，到它的流量被拦；
  - vmif 到私网放行、到公网被拦；
  - 其他接口（en0、新出现的 utun9）被拦；
  - 非 `_abroad` 用户到公网被拦；
- 用假 Runner 核对 Controller 拼出的参数和对输出的解析（包括 `-E` 返回 token 的几种格式）；
- 巡检：用假 pf 模拟被清、被关，断言 5 秒内恢复，连续 3 次时回调；
- 换网段后一轮内规则更新，旧网段不再放行；
- 用假 Runner 断言 `KillStates` 在三个时机紧跟加载调用，摘要一致的巡检不调用。

**需要用户**：否（真机加载放在 M2-9）。

### M2-7 状态机、崩溃恢复、原值快照（`internal/state`、`internal/sysnet`）

**拆分**（architect 2026-11-18）：
- **7a**：pf 收尾两条；`internal/state` 的状态文件；`internal/sysnet` 的接口和假实现。
- **7b**：状态机（启用、停用、恢复、失败回滚、崩溃注入、不变量 I1–I3）。
- **7c**：Recover、Boot，Patrol.Protected 与闸门联动，路由和 DNS 的巡检。
- **7d**：sysnet 的 darwin 实现，只做参数拼装和输出解析的单测，会话里不执行。
- **7e**（architect 2026-11-18 审 7b 时从 7c 拆出）：运行期监护——Protected 与闸门联动、路由和 DNS 的巡检、utun 故障重建、出口检查不通过后的重试。
- 顺序：7a → 7b → 7c → 7e → 7d。
- 7c 的范围改为：Recover、Boot、跨进程锁、pf 应用归到一处。

下面的原文是已审过的计划，各子任务照它做。

**7a 补充决定**：
1. **pf 收尾**：
   - 复核不一致时：EnsureResult 增加 `ShownAfterLoad`；Patrol 记下“期望规则 + 复核时的原始打印”。下一轮期望没变、锚点原始打印逐字节相同时，不 Load、不 Kill、不计修复，直接按 ErrVerifyFailed 处理。
   - Ensure 把 Status 阶段的错误包成 `ErrStatusUnavailable`，只有它走“连续 2 轮”；Status 之后任何一步失败都当轮判失效。
   - OnToken、OnEvent 的注释写明：回调里不能再调用 Patrol 的方法。
2. **状态文件的字段**：
   - `version`；
   - `state`（RESTORED、ENABLING、ENABLED、DISABLED、RESTORING）、`origin`、`step`；
   - `pf_tokens []string`：追加保存；
   - `pf_enable_pending bool`：调用 `-E` 之前置 true 并落盘，拿到 token 后与 token 一起落盘时清掉；
   - `utun`；
   - `ipv6_original`：按网络服务保存，能表示 Automatic、Manual（地址、前缀长度、路由器）、LinkLocal、Off；
   - `ipv6_snapshot_taken bool`；
   - `orig_dns []地址`。
3. **读取**：
   - 文件不存在：RESTORED；
   - 内容不对：返回 `ErrCorrupt`（用 usererror 包装），**不能当成 RESTORED**。包括坏 JSON、未知 version、未知 state、未知字段、超过 1 MiB、符号链接、不是普通文件、权限或属主不对。
4. **写入**：
   - 临时文件（O_NOFOLLOW，0600）→ fsync → rename → 目录 fsync；
   - 目录要求 0700，且属主是指定的 uid（开发模式是当前用户，正式是 root，做成参数）；
   - 提供“只在没拍过快照时写 IPv6 原值”的方法，重入时不覆盖。
5. **sysnet**：
   - 接口：Routes、DNS、IPv6，再加只读的 Interfaces（返回直连网段和网桥），并给出到 `pf.NetSource` 的适配；
   - `sysnet.Fake`：内存实现，带初始值、记录调用序列、FailNext、每次改动后的回调钩子（7b 用来查不变量），`Snapshot()` 能和初始值比较是否完全相等；
   - 7a 不写 darwin 实现，包里不出现 os/exec。
6. 状态文件和日志里不写出口 IP、订阅地址、凭据。

**文件**：
- `internal/sysnet/sysnet.go`：
  ```go
  type Routes interface {
      Add(ctx context.Context, dst netip.Prefix, ifName string) error
      Delete(ctx context.Context, dst netip.Prefix) error
      Present(ctx context.Context) (bool, error)
  }
  type DNS interface {
      Set(ctx context.Context, server netip.Addr) error
      Remove(ctx context.Context) error
      Present(ctx context.Context) (bool, error)
  }
  type IPv6 interface {
      Snapshot(ctx context.Context) (map[string]IPv6Setting, error) // 网络服务 → 原值
      Off(ctx context.Context, service string) error
      Restore(ctx context.Context, service string, s IPv6Setting) error
  }
  type Interfaces interface {
      Interfaces(ctx context.Context) (NetInfo, error) // 只读：直连网段、网桥
  }
  func NetSource(i Interfaces) pf.NetSource
  ```
  - （7a 实现时定）`IPv6Setting` 定义在 sysnet，`state.IPv6Setting` 是它的别名，避免 state 与 sysnet 循环依赖；
  - Routes、DNS 都有 `Present`，同一个类型放不下，所以 `Fake` 用 `Routes()`、`DNS()`、`IPv6()` 三个视图实现接口；
  - `Fake.OnChange` 在每次**成功的**改动调用之后恰好回调一次（不持锁），失败的调用和只读调用不回调；
  - 源码检查放在 `internal/srccheck`（AST：os/exec 导入、exec.Command 等调用、字符串里的命令名），state 和 sysnet 的测试共用。
  - darwin 实现拼 `route`、`scutil`、`networksetup` 的命令行，只做参数拼装和输出解析的单测；
  - `sysnet.Fake` 是内存实现。
- `internal/state/file.go`：
  - `state.json` 原子写：临时文件、fsync、rename、目录 fsync，`O_NOFOLLOW`，0600；
  - 正式路径 `/Library/Application Support/claude-abroad/state.json`；开发模式用 `--state-dir`。
  - 内容：状态、来源状态（从 RESTORED 还是 DISABLED 启用）、pf token、utun 名、IPv6 原值（按网络服务）、启用前的本机 DNS 服务器（给 M3 的直连解析用）、当前步骤序号。
  - IPv6 原值要能表示并还原 Automatic、Manual（地址、前缀、路由器）、LinkLocal、Off。
  - **IPv6 原值快照在一次 RESTORED → ENABLING 周期内只写一次**，重入时不得用已改过的值覆盖。
- **不变量**（替换 v1 里错误的“任何时刻”说法）：
  - I1：闸门打开 ⇒ 状态是 ENABLED，且 pf 已加载。
  - I2：任何接管痕迹存在（路由、DNS 键、IPv6 已关、utun）⇒ pf 已加载。也就是 pf 最先加载、最后清除。
  - I3：Boot 或 Recover 返回后，状态不是 RESTORED ⇒ pf 已加载。
  - 例外（architect 审 M2-7b）：恢复的 ⑥ 开始之后（步骤序号 6），锚点可以为空，此时不得存在任何接管痕迹（路由、DNS、被我们关掉的 IPv6、utun），闸门必须关着。I3 不加例外：Boot 看到步骤 6 的 RESTORING 照样加载 pf。
- **顺序约束**：
  - ENABLING 必须先 fsync 落盘，才能加载 pf；
  - RESTORED 只能在清除 pf 之后写；
  - `pfctl -E` 之前先写步骤标记，拿到 token 立刻落盘。
- `internal/state/machine.go`：设计第 10 节的启用、停用、恢复。
  - 每一步可以重入，执行前先写当前步骤；
  - 失败时按来源状态回滚。
  - 依赖全部注入：
    ```go
    type Deps struct {
        PF      PF
        Tun     tun.Creator
        Routes  sysnet.Routes
        DNS     sysnet.DNS
        IPv6    sysnet.IPv6
        VM      VMControl
        Check   ExitCheck
        Gate    Gate
        Killer  ClaudeKiller
        Preflight Preflight
    }
    ```
  - M2 里出口检查只做最小版：经 main 拿到 204。地区和环境检测（⑥⑦）在 M2 是空步骤，M4 补上。
- `internal/state/recover.go`：abroadd 启动时读 state.json：
  - `ENABLING`：按来源回滚；
  - `ENABLED`、`DISABLED`：重新建立 pf、utun、路由、DNS，闸门先关着，检查通过后再开；
  - `RESTORING`：继续恢复；
  - `RESTORED`：什么都不做。
- `internal/state/boot.go`：abroad-guard 的逻辑。所有非 RESTORED 状态都加载 pf，此时没有 utun，`Render` 不输出 utun 那一行；加载后 `KillStates`。`cmd/abroad-guard` 和 plist 放到 M6。回写设计 6.2。

**测试**：
- 每个步骤之后注入崩溃（丢弃内存状态，用同一份 state.json 和假系统重新恢复），断言：
  - 每次假系统改动后 I1、I2 都成立，Recover 返回后 I3 成立；
  - 恢复后的系统状态与“没崩溃时走完”一致；
  - 恢复后 IPv6、DNS、路由、utun 与**假系统的初始值**一致（不与 state.json 里的快照比对）。
- 崩溃注入至少覆盖：
  - 第一个网络服务 setv6off 之后崩溃；
  - 写入 `pfctl -E` 步骤标记之后、token 落盘之前崩溃。
- 每一步失败时的回滚：
  - 从 RESTORED 来的回到 RESTORED，假系统完全复原；
  - 从 DISABLED 来的回到 DISABLED，pf 仍在。
- 恢复本机网络时，结束 Claude 进程失败就中止，不动网络。
- state.json 损坏、被换成符号链接时拒绝并报 usererror。

**需要用户**：否。

**7b 设计决定**（architect 2026-11-18，审 7a 时给出）：
1. 7b 不含 Recover 和 Boot：崩溃注入（丢内存状态后恢复）归 7c。7b 用“失败后再次调用同一操作或调用恢复”证明每一步可重入。
2. 写入 ENABLED 在前，打开闸门在后（I1；design 第 10 节 ⑧ 已改）。
3. 启用顺序：
   - ⓪ 冲突检查，不通过就什么都不改，连 ENABLING 也不写；
   - ① 写 ENABLING（带来源）并落盘；
   - ② 置 pending，按“无 utun”的配置加载 pf；拿到 token 就追加，没有新 token 就清 pending；
   - ③ 拍 IPv6 快照（只拍一次），逐个网络服务关 IPv6；建 utun 并把名字写进状态文件；按带 utun 的配置重新应用 pf；加 0.0.0.0/1、128.0.0.0/1 两条路由；设 DNS；
   - ④ 启动虚拟机；⑤ 出口检查（M2 最小版）；⑥⑦ 空步骤；⑧ 写 ENABLED，开闸门。
4. 失败回滚：
   - 来源 RESTORED：按“恢复”的 ②–⑦ 走，不结束 Claude，最后回到 RESTORED；
   - 来源 DISABLED：关闸门，写回 DISABLED，pf、utun、路由、DNS 保留；
   - 回滚自己也失败：状态留在 ENABLING，pf 保持加载，返回错误，留给下次调用或 7c 的 Recover。
5. 停用：关闸门 → 断开所有走代理的连接（注入的 `Teardown`，真实顺序在 M2-9 组装）→ 可选关虚拟机 → 写 DISABLED。
6. 恢复：⓪ 结束 Claude 并确认已退出（失败就中止，什么都不改）→ ① 写 RESTORING → ② 关虚拟机 → ③ 删 DNS、路由、utun → ④ 按快照还原 IPv6 → ⑤ 空步骤 → ⑥ 清空锚点、释放全部 token → ⑦ MarkRestored。各步遇到 `sysnet.ErrNotFound` 按成功处理；快照里已消失的网络服务跳过并发事件。
7. token 未知：恢复时 pending 仍为 true，照常清空锚点、释放已知 token，发事件，不执行 `pfctl -d`（design 6.2）。
8. 同一时刻只允许一个操作，并发调用返回 `ErrBusy`。
9. 依赖全部注入，接口定义在 state 包：PF（按 utun 应用、清除并释放 token、是否已加载）、Tun（建、关，返回名字）、Routes/DNS/IPv6（sysnet）、VM、ExitCheck、Gate、Teardown、Killer、Preflight、事件回调。授权属于 M5，方法注释写明“调用方负责授权”。
10. 测试台：pf 用真实 `pf.Controller` + `pf.FakePF` 经适配器接成 state.PF；网络用 `sysnet.Fake`；其余用假实现，都带“每次改动后回调”的钩子。

**7b 审核后的改动**（architect 2026-11-18）：
- 锚点清空之后释放启用引用是尽力而为：任何 Release 错误都发 `EvTokenReleaseFailed`，把该 token 从文件里去掉，继续走到 RESTORED；`ErrTokenInvalid` 只用来区分事件文案。
- 回滚、以及 Restore 写入 RESTORING 之后的步骤，用 `context.WithoutCancel(ctx)` 加独立的 60 秒超时，不跟着调用方的 ctx 一起取消。
- 测试台在 Flush、Release 之后做正面断言：文件状态是 RESTORING（或回滚中的 ENABLING）、步骤序号 6、没有任何接管痕迹、闸门关着。

**带进 7c 的决定**（architect 审 M2-7b）：Boot 不做特殊处理，只要不是 RESTORED 就加载 pf（包括步骤 6 的 RESTORING）；接着由 Recover 从 ⑥ 做完：先再结束一次 Claude，再清锚点、写 RESTORED。

**7b 第 0 步**：目录 fsync 失败返回错误；加 `ClearEnablePending()`；`origin` 只能是空、RESTORED 或 DISABLED。

**7b 验收标准**（architect 2026-11-18）：
- a. 第 0 步：目录 fsync 失败（注入）时 Save 返回错误；ClearEnablePending 往返正确；origin 为 ENABLED、ENABLING、RESTORING 时写入被拒、读到时报 ErrCorrupt。
- b. 正常路径 RESTORED → ENABLED → DISABLED → ENABLED → RESTORED：每个阶段的状态文件、闸门、假系统与预期一致；最终 sysnet.Fake 的 Snapshot 与初始值 Equal，FakePF 锚点为空且全部 token 已释放，状态文件是干净的 RESTORED，utun 已关。
- c. 顺序断言（各假实现的调用记录合并成一条时间线）：ENABLING 落盘早于第一次 pf 加载；第一次 pf 加载早于任何 sysnet 改动和建 utun；pending 落盘早于 `-E`；ENABLED 落盘早于开闸门；恢复时 pf 清除晚于所有 sysnet 还原和关 utun；RESTORED 落盘晚于 pf 清除。
- d. 不变量（每次假系统、假 pf、闸门、utun 改动后的回调里检查，b、e、f、g 的所有用例都开着）：I1 闸门开 ⇒ 文件状态是 ENABLED 且 pf 已加载；I2 任一接管痕迹存在（路由、DNS、任一服务的 IPv6 被我们关掉、utun）⇒ pf 已加载；泄漏断言：文件状态不是 RESTORED 或任一接管痕迹存在时，FakePF.Eval 对 en0 上普通用户到公网的 tcp、udp 都不放行。
- e. 启用的每一步失败（表驱动：冲突检查、每一次状态文件写入、pf 加载、`-E`、IPv6 快照、每个服务的关闭、建 utun、pf 重新应用、每条路由、DNS、虚拟机、出口检查，各注入一次）：来源 RESTORED 结束时 RESTORED、假系统与初始值 Equal、锚点为空、闸门关、返回 usererror；来源 DISABLED 结束时 DISABLED、pf 仍加载、闸门关、utun/路由/DNS 仍在；冲突检查失败时没有任何改动、状态文件未被写入；出口检查失败时闸门从未打开过。
- f. 回滚失败和重入：启用失败、回滚中再注入一次失败 → 状态留在 ENABLING、pf 已加载、返回错误，之后 Restore 能走完并与初始值 Equal，另一例之后再 Enable 能成功到 ENABLED；恢复的每一步失败（表驱动）→ 状态 RESTORING、pf 已加载，再次 Restore 走完并与初始值 Equal；ENABLED 时再 Enable、DISABLED 时再 Disable、RESTORED 时再 Restore 不产生任何改动调用。
- g. 恢复：结束 Claude 失败时中止，假系统、pf、状态文件都没有变化；从 ENABLED、DISABLED、ENABLING、RESTORING 四种状态出发都能恢复到与初始值 Equal。
- h. IPv6：初始值含四种模式（Manual 带地址、前缀、路由器）；启用在关掉第一个服务后失败并回到 DISABLED 来源，或再次启用时，快照不被覆盖；最终恢复后四种模式逐项与初始值相等；快照里的服务在恢复前被从假系统删掉，恢复仍完成且有事件。
- i. token：正常周期结束后 FakePF 没有未释放的 token；中途 pf 被 Disable 后再次应用拿到第二个 token，恢复时两个都释放；pending 为 true 且文件里没有对应 token 时，恢复仍完成、有事件、调用记录里没有 `-d`。
- j. 并发：两个 goroutine 同时调用 Enable 和 Restore，恰有一个返回 ErrBusy；-race 通过。
- k. state 包的源码检查仍通过；go build、go vet、GOOS=linux GOARCH=arm64 go build、go test -race -count=1 ./internal/... ./cmd/... 全过；会话里不执行 pfctl、不改宿主机网络。

**7c 设计决定**（architect 2026-11-18，审 7b 时给出）：
1. **pf 的应用归到同一个 `pf.Patrol`**：状态机的 PF 适配器设置 utun 名后调 `Patrol.Step`，不再自己调 Ensure。清除经 `Patrol.Flush(ctx)`：清空锚点、忘掉上一次应用的规则、把巡检置为暂停。暂停期间 Step 什么都不做，`Protected()` 为 false，下一次 Apply 才恢复（否则恢复的 ⑥ 清完锚点后，巡检下一轮会把规则加载回去）。
2. **“将要启用”标记由 Controller 的钩子落盘**：`pf.Controller.BeforeEnable func() error` 在真正调用 `-E` 之前调用，返回错误就不调 `-E`。组装时接到 `Store.SetEnablePending`，`OnToken` 接到追加 token。状态机不再在每次 Apply 之前置标记；token 追加失败时沿用“留在内存、回滚和恢复前补存”。
3. **Boot**：输入只有 Store 和 PF。文件不存在或 RESTORED：不调用任何 pf 操作。其余一律按“无 utun”应用 pf，包括 `ErrCorrupt` 和步骤 6 的 RESTORING；文件损坏时 token 存不进去，照样加载并发事件（design 6.2）。
4. **Recover**（abroadd 启动时）：状态不是 RESTORED 时第一件事是确保 pf 已加载，之后任何一步失败都不影响这一点（I3）。然后：
   - RESTORED：什么都不做；
   - ENABLING：来源 RESTORED 走恢复的 ②–⑦（不结束 Claude）；来源 DISABLED 按 DISABLED 重建后写回 DISABLED；
   - DISABLED：重建 utun（名字写回文件）、按新 utun 应用 pf、补路由和 DNS，闸门保持关闭；
   - ENABLED：同 DISABLED 重建，再启动虚拟机、做出口检查，通过后开闸门；不通过时状态仍是 ENABLED、闸门关、发事件、返回错误（重试归 7e）；
   - RESTORING：先再结束一次 Claude，失败就停在 RESTORING、pf 保持加载、返回错误；成功则从断点接着做（步骤 6 直接从 ⑥ 开始）；
   - `ErrCorrupt`：确保 pf 已加载，发事件，返回 usererror（`restore --force` 的处理属于 M6）。
   - 路由“已存在”、DNS“已存在”按成功处理。
5. **跨进程互斥**：`Store.Lock()` 对状态目录下的锁文件加排他、非阻塞的 flock，返回释放函数；拿不到返回 `ErrLocked`（usererror）。abroadd 存续期间一直持有；Boot 运行期间持有；将来的 `restore --force` 同样。Recover、Boot 没持锁时拒绝执行。
6. **测试里的崩溃和重启**：
   - 崩溃：丢弃状态机和内存里的东西（没存下的 token、巡检记住的规则）；假 utun 关闭，经它的路由消失；闸门换新的（关闭）；FakePF、DNS、IPv6、状态文件、虚拟机保留。
   - 重启：在崩溃基础上 FakePF 整个复位（锚点空、未启用、token 清空），DNS 和路由清掉，虚拟机停掉，IPv6 保留；先 Boot 再 Recover。
   - `pf.FakePF.Reset()`、`sysnet.Fake` 的“删掉经某个 utun 的路由”是测试辅助，不算被测系统的改动，不触发不变量回调。

**7c 验收标准**（architect 2026-11-18）：
- a. pf：BeforeEnable 在 `-E` 之前恰好调用一次，返回错误时没有 `-E`、Ensure 返回该错误，pf 已启用时不调用；Patrol.Flush 之后连续 3 轮 Step 没有任何 pfctl 调用、锚点为空、Protected 为 false；再次 Apply 后恢复巡检，首轮是 Load 加 Kill；Run 与 Flush 并发 -race 通过，Flush 返回后锚点不会再被加载。
- b. 归到一处：启用后换网段，Patrol 一轮 Step 只 Load 不 Kill；随后停用再启用不产生 KillStates；整个正常周期 KillStates 只在首次加载时出现一次；标记落盘早于 `-E` 的断言既覆盖状态机发起的应用，也覆盖巡检发现 pf 被关后自己重新启用，后者的新 token 出现在状态文件里。
- c. 7b 的全部用例在新适配器下仍通过，不变量检查照常开着。
- d. 崩溃注入穷举：对“从 RESTORED 启用”“从 DISABLED 启用”“停用”“恢复”，空跑数出事件数 N（每次状态文件写入、假系统、pf、utun、闸门、虚拟机改动各算一个），k = 1..N 各跑一例：第 k 个事件之后崩溃，新建状态机调 Recover。断言：恢复中不变量成立（步骤 6 之后按正面断言）；Recover 返回后 I3；终态符合决定 4；之后 Restore 回到初始值、锚点为空、干净的 RESTORED；FakePF 未释放的 token 至多 1 个，且只出现在“`-E` 之后、token 落盘之前”崩溃的几例，此时有“token 未知”事件。单独点名两例：第一个网络服务关掉 IPv6 之后崩溃；标记落盘之后、token 落盘之前崩溃。
- e. 重启注入：同一组崩溃点改用重启，先 Boot 再 Recover。额外：Boot 返回后文件不是 RESTORED 时 en0 普通用户到公网不放行、首次加载带 KillStates；文件是 RESTORED 或不存在时 Boot 没有任何 pfctl 调用；终态与 d 相同，之后 Restore 回到初始值。
- f. Recover 分支：ENABLED 且出口检查失败 → 仍 ENABLED、闸门关、有事件、pf 生效，修好后再 Recover 闸门打开；RESTORING 且结束 Claude 失败 → 停在 RESTORING、pf 生效、没有网络改动，之后成功一次走到 RESTORED；RESTORING 步骤 6（重启后）→ Boot 加载 pf，Recover 只做 ⑥⑦，没有路由、DNS、IPv6 调用；重建时路由、DNS 已存在不报错；RESTORED 没有任何调用。
- g. 损坏的状态文件：Boot 加载 pf、有事件、不崩溃；Recover 确保 pf 已加载、返回 ErrCorrupt 的 usererror、不改路由/DNS/IPv6；符号链接同样处理，且不跟随链接写入。
- h. 锁：同目录第二个 Store 的 Lock 返回 ErrLocked，释放后可再拿；持锁的 fd 关闭后锁自动释放；没持锁时 Recover、Boot 拒绝执行且没有改动；锁文件 0600，目标是符号链接时拒绝。
- i. 源码检查仍通过；go build、go vet、arm64 build、`go test -race -count=1 ./internal/... ./cmd/...` 全过；单包测试 3 分钟以内（超过就去掉假实现里的等待，不减少崩溃点）；会话里不执行 pfctl、不改宿主机网络。

**7e 设计决定**（architect 2026-11-18，审 7c 时给出）：
- **接口约定**（第 0 步）：`Routes.Add` 成功返回时这条目的网段必须经指定接口；已存在且接口相同返回 `ErrExists`（调用方按成功处理）；已存在但接口不同要改到指定接口，改不了就返回错误，不能按“已存在”算成功。`DNS.Set` 同理：已设且服务器相同才是 `ErrExists`，不同就改写。darwin 的 `route change` 在 7d 做。
1. 监护是可单独调用的 `Tick(ctx)`，`Run(ctx)` 按间隔（默认 5 秒）循环，`Kick()` 让它立刻跑一轮。Tick 与启用、停用、恢复、Recover 互斥：拿不到状态机的忙标记就跳过这一轮，不排队。
2. 每轮按文件状态：RESTORED 什么都不做；ENABLING、RESTORING 只跑 pf 巡检一轮（巡检已暂停时不动），不碰路由、DNS、闸门；DISABLED、ENABLED 跑 pf 巡检一轮再核对 4 项。
3. 4 项：`Patrol.Protected()` 为真；utun 还活着；到 1.1.1.1、129.0.0.1、198.18.0.2 的路由都经我们的 utun；我们设的 DNS 还在。
4. 任一项不通过：闸门开着就关闸门、调一次 Teardown（每次由开变关恰好一次）、发带原因的事件，状态文件不改；然后当轮修复（utun 没了用 rebuild 重建并写回名字；路由或 DNS 不对按新约定重新 Add、Set），修复后本轮再核对一次，仍不通过就保持闸门关闭并发事件，下一轮再试。Protected 为假没有可修的动作。
5. 重新开闸门：状态 ENABLED、4 项全部通过、闸门关着时，启动虚拟机（幂等）、做出口检查，通过才开；不通过按 5 秒起、每次翻倍、上限 60 秒退避，时钟可注入。DISABLED 下任何情况都不开。闸门开着且 4 项通过时不做出口检查（运行中的出口复查属于设计第 8 节）。
6. 接口增补：`sysnet.Routes.Via(ctx, addr)`；`state.Tun.Alive()`；`state.Gate.IsOpen()`；`sysnet.Fake` 带一条初始默认路由、`Via` 按最长前缀匹配、加“别的软件插入或改写一条路由”“删掉我们的 DNS”的测试辅助（不回调）；假 utun 加“意外死亡”的辅助。

**7e 审核补记**（architect 2026-11-18）：
- 状态文件读不出来时 Tick 关闸门（若开）、跑一轮巡检、返回错误，不修复也不开闸门（同意主会话的处理）。
- 7d 第 0 步：verify 区分“查到是坏的”（当轮处理）和“查询出错”（`Routes.Via`、`DNS.Present` 返回错误，连续 2 轮才算不通过，成功一轮清零）；`rebuild` 在 `!Alive() || name == ""` 时先 Close 再 Open（Tun 接口注释写明死后可能仍返回旧名字）。

**7e 验收标准**（architect 2026-11-18）：除 Run 的测试外都直接调 Tick、注入时钟；7b、7c 的不变量检查照常开着。
- a. 第 0 步：Add 同接口返回 ErrExists 且不算改动，不同接口改写成功并算一次改动，DNS.Set 同理；Via 的最长前缀匹配（默认路由、两条 /1、更具体的 /16）；7b、7c 全部用例通过。
- b. 无事发生：ENABLED 一切正常时连续 5 轮 Tick 没有改动调用、闸门保持打开、Teardown 0 次、没有出口检查；RESTORED 时 Tick 没有任何调用（包括 pfctl）。
- c. pf 保护失效：DropMainRef 后一轮 Tick 关闸门、Teardown 恰 1 次、有事件、文件仍 ENABLED；再 3 轮 Teardown 仍 1 次；恢复引用后一轮内出口检查通过并重新开闸门。
- d. 锚点被清：ClearAnchor 后一轮内巡检恢复，闸门不关（Protected 仍为真，不误杀）。
- e. utun 死亡：一轮内关闸门、Teardown 1 次，重建 utun（新名字写进文件）、pf 按新名字应用、三个地址的路由经新 utun、DNS 在；同一轮或下一轮出口检查通过后重新开闸门；全程泄漏断言成立，旧 utun 名上到公网不放行。
- f. 路由被抢：别的软件插入 198.18.0.0/16 经 utun9 → 一轮内关闸门、有“修复后仍不通过”事件、闸门保持关闭，撤掉后恢复并重开；0.0.0.0/1 被改到 utun9 → 一轮内改回并重开；我们的两条路由被删 → 一轮内补回。
- g. DNS 被删：一轮内补回，其间闸门关过一次，Teardown 1 次。
- h. 退避：出口检查持续失败时检查调用时刻是 0、5、15、35、75、135 秒（之后每 60 秒），期间闸门始终关闭；恢复后下一次到点即打开、退避归零；Recover 在 ENABLED 下因出口检查失败返回后，由 Tick 接手并最终打开。
- i. DISABLED：utun 死亡、路由被删、DNS 被删都会被修复；任何情况下闸门都不打开，没有出口检查和虚拟机启动的调用。
- j. ENABLING、RESTORING（无操作在跑）：Tick 只有 pf 巡检的调用；步骤 6 的 RESTORING（巡检已暂停）下 Tick 没有任何 pfctl 调用，锚点保持为空。
- k. 互斥：Enable 卡在冲突检查里时调 Tick，Tick 立即返回且没有任何调用；Tick 运行中调 Restore 得到 ErrBusy；Run 与 Enable、Disable、Restore 交替并发 -race 通过；之后 Restore，假系统与初始值 Equal。
- l. Run：间隔 10 毫秒量级，ctx 取消后 1 秒内退出、不泄漏 goroutine；Kick 能让下一轮立刻开始。
- m. 源码检查仍通过；build、vet、arm64 build、`go test -race -count=1 ./internal/... ./cmd/...` 全过；state 包 3 分钟以内；会话里不执行 pfctl、不改宿主机网络。

**7d 设计决定**（architect 2026-11-18，审 7e 时给出）：
1. 只做薄的执行层：拼命令行、解析输出。所有命令经 `Runner.Run(ctx, path, args, stdin)`；真实的 `ExecRunner` 只允许 `/sbin/route`、`/usr/sbin/networksetup`、`/usr/sbin/scutil`、`/sbin/ifconfig`、`/usr/sbin/netstat`，其他一律拒绝。
2. 会话里只允许只读查询，用代码保证：`ReadOnlyRunner` 包在 ExecRunner 外面，只放行 `route -n get <地址>`、`netstat -rn -f inet`、`networksetup -listallnetworkservices`、`networksetup -getinfo <服务>`、`scutil --dns`、`scutil --nwi`、经 stdin 且只含 `show`、`list` 行的 `scutil`；`integration` 标签下的本机实跑只能用它。改动类命令只做参数拼装的单测，真机执行在 M2-9 的用户脚本里。
3. 各接口的命令：
   - Routes：Add 用 `route -n add -net <网段> -interface <utun>`；报已存在时用 Via 判断，同接口返回 ErrExists，不同接口用 `route -n change -net <网段> -interface <utun>`，改不了返回错误；Delete 用 `route -n delete -net <网段>`，不在表里返回 ErrNotFound；Via 用 `route -n get <地址>` 取 `interface:` 一行。
   - DNS：键 `State:/Network/Service/claude-abroad/DNS`；Set 经 scutil stdin（`d.init`、`d.add ServerAddresses * <地址>`、`set <键>`）；Present 用 `show <键>` 核对服务器地址；Remove 用 `remove <键>`。写入后系统是否把它当首选解析器在 M2-9 实测。
   - IPv6：Snapshot 用 `-listallnetworkservices`（跳过首行说明和带 `*` 的停用项）加逐个 `-getinfo`；Off 用 `-setv6off`；Restore 按模式 `-setv6automatic`、`-setv6LinkLocal`、`-setv6manual <服务> <地址> <前缀长度> <路由器>`；服务名进参数前校验：非空、不含换行、不以 `-` 开头。
   - Interfaces：用 `net.Interfaces()`，不执行命令；直连网段取已启用、非回环、非 utun、非网桥接口的 IPv4 网段；网桥取名字符合 `pf.ValidVMIf` 且已启用的接口。
   - utun 配置：拼装函数 `ifconfig <utun> inet <地址> <对端> mtu <MTU> up`；接成 `state.Tun` 放 M2-9。
4. 解析依据实测样本：用 ReadOnlyRunner 在本机采集，脱敏（本机 IP、MAC、路由器地址换成文档用地址）后放 `testdata/`。Manual 模式的 `-getinfo` 采不到时，解析对“Manual 但没有前缀长度”返回明确错误，不猜 64，并记进 M2-9 采样清单。遇到与预期不符的格式，停下来找 architect。
5. 源码检查：sysnet 里只有 `exec.go` 可以导入 os/exec，路径字面量只能是上面 5 个；其余文件仍过 `srccheck.NoExec` 的导入和调用检查，命令名的字符串检查对 darwin 实现文件放开；全仓库除 sysnet 的测试和 exec.go 外没有对 `sysnet.ExecRunner` 的引用。

**7d 验收标准**（architect 2026-11-18）：
- a. 第 0 步两条的测试通过（Via 注入一次错误闸门不关、Teardown 0；连续两轮关闸门；DNS.Present 同样两例；“死了但名字还在”的 utun 一轮内重建出新名字）；7b、7c、7e 现有用例全过。
- b. 参数拼装：用记录型 Runner 逐字核对路径、参数和 stdin：Add 三种走向、Delete 不在表里、DNS 的 Set/Present/Remove、IPv6 四种模式的 Restore 和 Off、utun 配置；非法输入在调用 Runner 之前报错（服务名带换行、以 `-` 开头、为空；接口名不是 `utun<数字>`；网段或地址无效）。
- c. 输出解析全部用 testdata 样本：`route -n get` 经 utun、经 en0、没有路由；`-listallnetworkservices` 含首行说明和带 `*` 的停用项；`-getinfo` 的 Automatic、Off 必须是实测样本，Manual、LinkLocal 没有实测样本时用构造样本并在文件名里标明；Manual 缺前缀长度返回明确错误；scutil 键存在与不存在；不认识的格式返回错误。
- d. ExecRunner 拒绝不在白名单的路径（相对路径、`/bin/sh`、带 `..`）且不执行；ReadOnlyRunner 拒绝 `route -n add/delete/change`、`networksetup -setv6off/-setv6automatic`、stdin 含 `set`/`remove`/`d.add` 的 scutil、任何 ifconfig，底层 0 次调用；白名单各放行一例。
- e. Interfaces 用注入的接口列表单测：回环、utun、bridge100、en0 带 IPv4 和 IPv6、未启用接口、公网直连网段；结果不含 IPv6 和回环，网桥只含合规名字，公网网段照原样返回，喂给 `pf.LANPrefixes` 后被丢掉。
- f. 只读实跑（`//go:build integration`，`go test -tags integration -run ReadOnly ./internal/sysnet/`）：Snapshot 列出本机网络服务且每项 Valid；Via(1.1.1.1) 非空；DNS.Present 为 false 且不报错；Interfaces 至少一个。全程只经 ReadOnlyRunner；命令清单和结果摘要（不含地址）写进 `scripts/dev/out/`。
- g. 源码检查：sysnet 只有 exec.go 导入 os/exec、路径字面量只有那 5 个；全仓库非测试代码没有对 `sysnet.ExecRunner` 的引用；state、pf 的源码检查仍通过。
- h. build、vet、arm64 build、`go test -race -count=1 ./internal/... ./cmd/...` 全过；会话里除 f 的只读查询外不执行 route、networksetup、scutil、ifconfig 的改动类命令，不执行 pfctl。

### M2-8 启用前冲突检查（`internal/preflight`）

**状态**：M2-7（7a–7e）整体完成；M2-8 完成（architect 2026-11-18 验收通过，无必须改）。

**M2-8 第 0 步**（architect 审 7d 时提出）：
1. DNS 写入补 `d.add SupplementalMatchDomains * ""`（设计 4.1；“最高优先级”和是否成为首选解析器留 M2-9 实测）。
2. `MacRoutes.Delete` 删除前用 Via 查网段内的地址：不经我们的 utun 或 `Iface()` 为空返回 ErrNotFound、不执行 delete；经我们的才删。
3. IPv6 增加模式 `unknown`（读到了服务、但没法原样还原：Manual 缺前缀或路由器、不认识的模式、`-getinfo` 输出不认识）：Snapshot 对它返回 unknown 不整体失败（`-listallnetworkservices` 本身不认识仍整体报错）；状态机不关、不还原，启用时发事件；状态文件接受它。

**M2-8 设计决定**（architect 2026-11-18）：
1. 阻断：系统代理、PAC、自动发现任一开启；别的软件的虚拟网卡占着默认路由或 0.0.0.0/1、128.0.0.0/1；只有 IPv6；主代理第一跳是本机回环。只提示：代理软件在运行；shell 配置里有代理环境变量；网络共享已开启。
2. 不把自己的接管当冲突：检查带参数“我们自己的 utun 名”（从 DISABLED 启用或 ENABLING 重试时非空），经它的默认路由和两条 /1 不算冲突。
3. 只读由代码保证：命令经 `sysnet.Runner`，preflight 内部一律再包一层 `ReadOnlyRunner`；只读白名单增补 `scutil --proxy`、`route -n get default`、`/bin/ps -axo comm`；ExecRunner 路径白名单加 `/bin/ps`，源码检查的路径数改为 6。
4. 文件只读，经注入的 `fs.FS`：shell 配置读 `.zshrc`、`.zprofile`、`.bash_profile`、`.bashrc`、`.profile`，单个文件上限 1 MiB；只认未注释的赋值行，变量名不分大小写：`http_proxy`、`https_proxy`、`all_proxy`；结果只给文件名和行号。网络共享读 `com.apple.nat.plist`，只支持 XML；二进制或读不出来给“无法判断”的提示，不阻断。
5. 接到状态机：检查器实现 `state.Preflight`，有阻断项时 Check 返回 usererror 并列出每一项和处理办法；全部结果（含提示项）可另行取出；“帮我关闭系统代理”M2 不做。
6. 输出脱敏：代理地址、端口、PAC 地址、环境变量的值、进程的完整路径都不出现在结果和日志里，只写“已配置”和软件名。
7. 命令失败不放行：任一检查的命令失败或输出不认识，产生“无法判断”的阻断项（网络共享除外）。

**M2-8 验收标准**（architect 2026-11-18）：
- a. 第 0 步三条的测试通过（Delete 三种走向逐字核对；Snapshot 混合样本；含 unknown 服务的启用和恢复全程没有对它的 Off、Restore 调用、有事件、恢复后 Equal；7c 穷举的“从 RESTORED 启用”“恢复”两组初始系统带一个 unknown 服务）；7b、7c、7e、7d 全过，穷举的崩溃点数量不减少。
- b. 每个检查项用样本单测，各有“有冲突”“没有冲突”“命令失败或输出不认识”三种：系统代理、PAC、自动发现各产生阻断项；默认路由或 0.0.0.0/1 经别的 utun 阻断，传入自己的 utun 名后同样的路由不算冲突；只有 IPv6 阻断；第一跳是回环阻断；代理软件在运行提示且只有软件名；shell 配置提示，带文件名和行号，注释行、`MY_HTTP_PROXY_NOTE` 这类不报，大小写两种都报；网络共享开启提示，二进制或读不出来“无法判断”的提示；命令失败或输出不认识“无法判断”的阻断项。
- c. 脱敏：样本里放入可识别的代理地址、端口、PAC 地址、环境变量值、进程完整路径，断言所有结果文字和 Check 的错误信息里都不含这些字符串。
- d. 只读：记录型 Runner 收到的每条命令都在只读白名单内；增补三条各有放行用例，`scutil --proxy` 带 stdin、`route -n get` 带额外参数、`ps` 的其他参数被拒；preflight 非测试文件过 `srccheck.NoExec`，没有 os/exec、没有写文件的调用；sysnet 路径字面量检查改为 6 个并通过。
- e. 接到状态机：有阻断项时 Check 返回 usererror、列出处理办法；只有提示项时返回 nil、提示项可另行取出；真实检查器加样本 Runner 接进 7b 测试台跑一例启用被阻断（没有改动、状态文件未写入）；再跑一例从 DISABLED 启用（自己的 utun 和路由不算冲突，启用成功）。
- f. 只读实跑（`go test -tags integration -run ReadOnly ./internal/preflight/`）不报错，证据写进 `scripts/dev/out/`，只报种类和数量；本机开着别的代理软件的 TUN，预期至少一条“别的虚拟网卡占着路由”的阻断和一条“代理软件在运行”的提示，不符时说明原因。
- g. build、vet、arm64 build、`go test -race -count=1 ./internal/... ./cmd/...` 全过；会话里只执行只读查询，不执行改动类命令、不执行 pfctl、不碰用户的 Clash。


**文件**：
- `internal/preflight/preflight.go`：
  ```go
  type Finding struct {
      Kind   string
      Detail string
      Fix    string
      Block  bool
  }
  type Probe interface{ Run(ctx context.Context, name string, args ...string) ([]byte, error) } // 只读命令
  func Check(ctx context.Context, p Probe, fs fs.FS, home string) ([]Finding, error)
  ```
- 检查项（设计 10.4）：
  - 系统代理、PAC、自动发现：`scutil --proxy`；
  - 别的虚拟网卡占着默认路由：`route -n get default`，再加 `netstat -rn -f inet` 看 0/1、128/1 是否被 utun 占着；
  - 正在运行的代理软件：`ps -axo comm`，对照已知名单；
  - shell 配置里的代理环境变量：读 `~/.zshrc`、`~/.zprofile`、`~/.bash_profile`、`~/.bashrc`、`~/.profile`，报文件名和行号，不回显变量值；
  - 网络共享：读 `/Library/Preferences/SystemConfiguration/com.apple.nat.plist` 的 `Enabled`，只读；
  - 只有 IPv6：`scutil --nwi`。
- **阻断项**：主代理第一跳是本机回环时不允许启用（见第 5 节）。
- “帮我关闭系统代理”属于写操作，M2 只留接口，实现放到 M5。
- 输出脱敏：代理地址只给出“已配置”，不写出地址。

**测试**：
- 用录制的命令输出做单测，样本放在 `testdata/`，已脱敏；
- 加一个 integration 标签的本机只读实跑，只读不写，会话里可以跑；
- 本机实跑的结果交 architect 时，只报 Finding 的种类和数量。

**需要用户**：否。

### M2-9 abroadd 组装和真机验证脚本

**拆分**（architect 2026-11-18）：
- **9a**：路由冲突判断（第 0 步）；abroadd 的组装层和本地接口，只有假系统模式；进程内端到端测试。
- **9b**：真实模式的接线（真实的 pf、sysnet、utun 配置、数据面：网络栈、fake-IP DNS、闸门、虚拟机控制）；只编译和做接线的单测，会话里不以真实模式运行。
- **9c**：给用户的真机脚本、README、restore.sh、采样清单；交给用户执行，是 M2 验收的最后一步（需要 R1、R2 的结论）。

**9a 设计决定**（architect 2026-11-18）：
1. 路由冲突看整张表（第 0 步）：sysnet 解析 `netstat -rn -f inet`（认 `default`、`10`、`192.168.1`、`128.0/1` 等缩写；用只读采样的真实输出，脱敏后放 testdata）。冲突 = 经虚拟网卡、且不是我们 utun 的路由里，目的是 default，或目的网段不完全落在私有、回环、链路本地、组播、保留范围内，或与 198.18.0.0/15 重叠；经虚拟网卡但目的解析不了的行按冲突算。虚拟网卡名字前缀表从 preflight 挪到 sysnet 共用。`sysnet.Routes` 加 `Conflicts(ctx, 我们的 utun) ([]接口名, error)`；冲突检查用它替换现在的判断，地址探测保留作实际出口的复核，任一发现都阻断；监护的路由核对加上它，有冲突算“路由不对”，修不了，闸门保持关闭并发事件。
2. 组装层放 `internal/daemon`，`cmd/abroadd/main.go` 只解析参数和处理信号；依赖经一个结构体注入。
3. 9a 只有假系统模式：不带 `--fake-system` 打印“真实模式尚未实现”并以非零退出，不做任何事；全仓库非测试代码仍没有对 `pf.ExecRunner`、`sysnet.ExecRunner` 的引用。
4. 数据面先用接口隔开：utun、虚拟机、出口检查、断开连接用假实现；闸门写真实的原子开关，满足 `state.Gate`，并经适配视图满足 `stack.Gate`、`fakedns.Gate`。
5. 启动：拿锁（`ErrLocked` 每 500 毫秒重试，最多 30 秒）→ Recover（返回错误记日志继续运行；文件损坏时只开放状态查询和恢复）→ 启动监护（巡检只由监护驱动）→ 监听本地接口。SIGTERM/SIGINT：停止接收请求，等正在进行的操作结束（最多 60 秒），停监护，关闸门，释放锁，退出；**退出时不恢复网络、不清 pf**，状态文件保持原样。
6. 本地接口：unix socket 上的 HTTP，`POST /v1/enable`、`/v1/disable`、`/v1/restore`，`GET /v1/status`；socket 在状态目录里（目录 0700、socket 0600），已有 socket 文件确认不是符号链接后删掉重建；每个连接取对端 uid，必须等于状态目录属主，否则直接断开。成功返回 JSON；有操作在进行 409；冲突检查阻断 422（带处理办法）；其他失败 500（usererror 三段文字）。status：状态、闸门、pf 保护是否有效、utun 名、最近 50 条事件（种类、时间、不敏感细节）、最近一次冲突检查的提示项；不含出口 IP、节点地址、token。请求体上限 64 KiB，未知路径 404，方法不对 405。
7. 事件和日志：状态机、巡检、监护的事件汇到一个环形缓冲，同时写日志，日志经 `internal/redact`。

**9a 验收标准**（architect 2026-11-18）：
- a. 路由表解析：实测样本（脱敏）里认出本机那组分段路由且全部判为冲突；构造样本覆盖 default 经 utun、只有 8/5 经 utun、10/8 经 utun（不算）、198.18.0.0/16 经 utun（算）、经虚拟网卡但目的不认识（算）、经 en0（不算）、经我们自己 utun（不算）；缩写写法各一例。
- b. 冲突检查：只有 8/5 经别的 utun 时阻断，只有 10/8 时不阻断；原有用例全过；只读实跑仍是阻断 1、提示 1（种类相同）。
- c. 监护：别的软件加 8/5 经 utun9 后一轮内关闸门、有“修复后仍不通过”、没有删改这条路由，撤掉后重开；加 10/8 经 utun9 闸门不关；原有监护、崩溃穷举全过。
- d. 进程内端到端（全部经 unix socket HTTP，假系统，不变量照常开着）：启用 → status ENABLED、闸门开、保护有效 → 停用 → 再启用 → 恢复 → status RESTORED，假系统与初始值 Equal；启用中再发启用得 409；冲突检查阻断得 422（带处理办法、状态文件未写入）；结束 Claude 失败时恢复得 500（三段文字、系统没有改动）。
- e. 重启恢复：ENABLED 下走退出流程停掉 daemon，pf 仍生效、状态文件仍 ENABLED、en0 普通用户到公网不放行；同一套假系统和状态目录新建实例，status ENABLED 且闸门开；不走退出流程直接丢弃（模拟被杀），新实例同样恢复；状态文件损坏时启动：进程不退出，status 能查，启用被拒，pf 生效。
- f. 锁：第二个实例在第一个持锁时启动，时限内报错退出且没有改动；第一个退出后第二个能启动。
- g. 接口安全：socket 0600、目录 0700；对端 uid 校验（注入返回不同 uid 的函数，连接被断开且没有触达处理函数）；已有 socket 路径是符号链接时拒绝启动；超过 64 KiB 被拒；未知路径 404；方法不对 405；status 里搜不到注入的 token 值和样本地址。
- h. 监护在 daemon 里跑着：假 pf 清掉锚点，不发请求，两个监护间隔内恢复；换掉主规则集引用后闸门关闭，status 显示保护失效和对应事件。
- i. 没有假系统参数时以非零退出，标准错误有“真实模式尚未实现”，状态目录没有被创建；全仓库源码检查仍通过。
- j. 实跑证据：以 `--fake-system` 启动编出来的二进制，curl 走一遍，证据里有每一步的 HTTP 状态码和 status 的 JSON；结束后进程已退出，socket 已删除。
- k. build、vet、arm64 build、`go test -race -count=1 ./internal/... ./cmd/...` 全过；会话里不执行改动宿主机网络的命令、不执行 pfctl、不以 root 运行。

**9a 状态**：完成（architect 2026-11-18 验收通过，无必须改）。

**9b 拆分**（architect 2026-11-18）：
- **9b-1**：真实系统层的接线。真实的 pf、路由、DNS、IPv6、网卡信息、utun 配置、冲突检查接进 daemon，用“命令模拟器”跑完整流程；真实模式在命令行上仍然不开放。
- **9b-2**：数据面的接线。utun 上挂网络栈、fake-IP DNS、代理处理器、经主代理的 DoH、断开连接、虚拟机和出口检查；做完才在命令行上开放真实模式（需要 root，会话里不运行）。

**9b-1 设计决定**（architect 2026-11-18）：
1. 命令模拟器 `sysnet.SimRunner`：实现 Runner，底下是 `sysnet.Fake`；按真实命令的输出格式作答并把改动落到 Fake 上；输出格式以 testdata 样本为准，报错文字用真实实现识别的那几句（File exists、not in table、No such key、is not a recognized network service）；不认识的命令返回错误并记录。局限写在注释里：格式有一部分来自构造样本，真机是否一致由 9c 验证。
2. `NewRealSystem`：所有会碰系统的东西都是参数（pf 的 Runner、sysnet 的 Runner、打开 utun 的函数、查用户 uid 的函数、接口列表的来源）；它负责接对 PrintMatcher 的 UID、MacRoutes.Iface、MacDNS.Server、冲突检查的 OurUtun 和 FirstHop；查不到放行用户（默认 `_abroad`）时返回 usererror，不发任何命令。
3. 生产执行器只在一个文件里出现（`internal/daemon/realexec.go`）：一个函数返回 `pf.ExecRunner` 和 `sysnet.ExecRunner`，第一步检查有效 uid 为 0；全仓库对这两个 ExecRunner 的引用只允许出现在这个文件里；9b-1 里没有任何代码路径调用它，`abroadd` 不带 `--fake-system` 时仍打印“真实模式尚未实现”。
4. utun 的真实实现分两半：9b-1 做“打开设备 + ifconfig 配地址和 MTU + 关闭”（打开设备的函数可注入，测试用 `tun.Pipe`；MTU 取 `Device.MTU()`；`Alive()` 在设备关闭后为假）；挂网络栈留给 9b-2，预留“设备打开后、关闭前”的钩子。
5. 虚拟机、出口检查、断开连接、结束 Claude 在 9b-1 里继续用假实现，由参数传入。
6. 冲突检查在真实系统里的输入：Root 用只读的根文件系统；Home 可为空（跳过 shell 配置；root 下取登录用户主目录留到 M5）；FirstHop 从主代理的存储里取，没有主代理时启用被拒绝（“前提：主代理已应用”）。

**9b-1 验收标准**（architect 2026-11-18）：
- a. 模拟器一致性：同一串操作分别对“Fake 的视图”和“真实实现 + SimRunner”执行，逐步比较错误类别和 Fake 快照，完全一致（加路由、重复加、改接口、删、删不存在；Via、Conflicts；设 DNS、重复设、改写、删、删不存在；IPv6 快照、关闭、四种模式还原、对不存在的服务操作）；不认识的命令返回错误且有测试。
- b. 状态机和监护在“真实实现 + 模拟器”后端下通过，不变量照常开着：正常周期、四种状态恢复、unknown 服务、崩溃和重启穷举（崩溃点不少于假后端，变化要说明）、监护的 utun 死亡、路由被抢（含 8/5）、路由被删、DNS 被删、查询出错连续 2 轮；state 包 5 分钟以内。
- c. daemon 端到端（9a 的 d、e、h）改用 NewRealSystem（模拟器 + FakePF + 注入的 tun.Pipe）再跑并通过；另断言 utun 的 ifconfig 里 MTU 等于设备 MTU、PrintMatcher 用注入的 uid、巡检加载的规则里的用户名是配置的放行用户。
- d. 命令审计：c 的全流程里每条命令的路径在 6 个白名单路径或 `/sbin/pfctl` 之内；pfctl 只有 `-s info`、`-s rules`、`-a <锚点> -s rules`、`-a <锚点> -t abroad_lan -T show`、`-o none -a <锚点> -f -`、`-a <锚点> -F all`、`-F states`、`-E`、`-X <数字>`；没有 `-d`、`-e`、不带 `-a` 的 `-f` 和 `-F all`；networksetup 只有列服务、`-getinfo`、`-setv6off/automatic/LinkLocal/manual`；route 只有对两条 /1 的 add、change、delete 和 get；去重后的命令形态写成 golden 文件，新形态出现时测试失败（9c 放进给用户的 README）。
- e. 启动时的拒绝：查不到放行用户时 NewRealSystem 返回 usererror、命令数 0；没有主代理时启用被拒绝、提示先应用主代理、系统没有改动。
- f. 生产执行器：非 root 调用返回错误（会话里直接测）；两个 ExecRunner 的引用只出现在 realexec.go；全仓库没有对这个函数的调用（AST 检查）；abroadd 不带 `--fake-system` 时行为与 9a 相同。
- g. build、vet、arm64 build、`go test -race -count=1 ./internal/... ./cmd/...` 全过；会话里不执行改动宿主机网络的命令、不执行 pfctl、不以 root 运行。

**9b-1 状态**：完成（architect 2026-11-18 验收通过，无必须改）。命令清单 golden 29 行已逐条审过。

**9b-2 第 0 步**（architect 审 9b-1 时提出）：
1. 模拟器应答冲突检查的只读命令（`scutil --proxy`、`scutil --nwi`、`ps -axo comm`、`route -n get default`，输出用 testdata 里“没有冲突”的样本），把真实的冲突检查器接进 real 分支的端到端，验证 `OurUtun`、`FirstHop` 接对了（从 DISABLED 再启用时自己的 utun 不被判冲突）。
2. 审计流程加一步：别的软件把 0.0.0.0/1 改到 utun9，监护把它改回，让 `route -n change` 出现在 golden 里。

**9b-2 设计决定**（architect 2026-11-18）：
1. 数据面分两层：进程级（daemon 存续期间一份）是 fake-IP 地址池、DNS 服务（含直连名单和反查表）、经主代理的 DoH、连虚拟机数据通道的拨号器、代理处理器；设备级（每个 utun 一份）是网络栈，在 utun 打开的钩子里建、关闭的钩子里拆。utun 重建后应用缓存的假地址仍能用。
2. utun 是否活着包含网络栈：`Stack.Done()` 关闭时 `Alive()` 为假，并立刻 Kick 监护。
3. 断开连接的顺序固定：`Proxy.DropAll()` → DoH `Close()` → 拨号器 `Close()`；每次闸门由开变关都执行（停用、监护关闸门、恢复）；之后重新开闸门时新连接能建立（拨号器关闭后能重新拨号，做不到就每次重建）。
4. M2 的虚拟机由外部管理（design 3.1、10）：`VM.Start` 在时限内（默认 60 秒）等到控制面 health 通过、且 `host.sock` netstats 的 `outbound_mode` 是 `first_hop`，否则失败并提示虚拟机没有收紧出站；`VM.Stop` 什么都不做。
5. 出口检查最小版：经数据通道、出口 main，对可配置的主机（`--exit-check-host`）发 `GET /generate_204`，10 秒内拿到 204 才通过；不经过闸门。
6. 主代理：`--main-proxy-store <目录>`，用 managedproxy 的存储读第一跳接给冲突检查；读不出来或没有主代理时启用被拒；下发给虚拟机仍由开发工具在启用前完成。
7. 结束 Claude：真实模式下没有真实实现，必须显式带 `--dev-skip-claude-kill` 才能启动，否则拒绝；带了之后启动时和每次恢复时打醒目日志；发布构建不能带（M6 待办）。
8. 真实模式入口：`cmd/abroadd` 不带 `--fake-system` 时先调 `ProductionRunners`（非 root 在这里就退出，不创建任何目录）；状态目录默认 `/Library/Application Support/claude-abroad`（属主 root）；其余参数 `--vm-dir`、`--pf-user`、`--main-proxy-store`、`--exit-check-host`、`--dev-skip-claude-kill`；全仓库对 `ProductionRunners` 的调用只允许在这一处。
9. 会话里验证真实数据面：开发子命令 `abroad-dev daemon-sim`——进程内启动 daemon，系统层用假的（FakePF、假网络），数据面用真的，utun 用 `tun.Pipe`，管道另一头接 tunsim 的客户端；经本地接口发启用、停用、恢复，在各阶段跑客户端探测；tunsim 拆出“只有客户端”的入口；接进 vm-up 作为新阶段，对真实的虚拟机跑，不碰宿主机网络。
10. 没有直连路径：daemon 数据面代码里除了 unix socket 之外不允许有任何拨号或监听。

**9b-2 验收标准**（architect 2026-11-18）：
- a. 第 0 步：real 分支端到端用真实冲突检查器跑通（含从 DISABLED 再启用）；没有主代理存储时启用被拒；golden 里有 `route -n change`，审计通过。
- b. 进程内数据面端到端（模拟器 + FakePF，utun 用管道，数据通道接进程内的 agent 数据面或等价假对端，全部经本地接口）：启用后 DNS 查 A 得假地址、连假地址的 TCP 拿到 204、UDP 有回显；停用后已有 TCP 在 1 秒内断开、新 TCP 被拒、开流次数不再增长、需要上游的 DNS 查询得 SERVFAIL；再启用后同一域名解析到同一假地址、TCP 重新可用；恢复后管道已关闭、没有残留 goroutine。
- c. utun 重建后映射还在：启用后记下某域名的假地址，从外面让设备死掉，监护两个间隔内重建，新设备上对同一假地址直接 TCP（不重新解析）拿到 204。
- d. 网络栈意外退出：搬运协程出错退出、`Stack.Done` 关闭 → `Alive` 变假；监护被 Kick（间隔设得很长来证明）；期间闸门关；重建后重开。
- e. 断开顺序：每次闸门由开变关时调用序列恰好是 DropAll、DoH Close、拨号器 Close 各一次；覆盖停用、监护因保护失效关闸门、恢复。
- f. 外部虚拟机（假的 ctl 和 host socket）：health 通过且 first_hop 通过；health 一直不通过到时限失败、启用回滚干净；outbound_mode 是别的值失败、指出没有收紧出站；socket 不存在失败、指出虚拟机没有启动；Stop 没有任何调用。
- g. 出口检查：204 通过；200、超时、连接失败都失败、启用回滚、闸门从未打开；检查时闸门是关的，且确实经数据通道开了一条流（开流次数加一）。
- h. 没有直连：AST 检查 internal/daemon 非测试文件，`net.Dial`、`DialContext`、`Listen`、`ListenPacket` 等调用的网络类型字面量只能是 "unix"，没有 net/http 默认客户端；数据通道对端换成“全部拒绝”时所有流都失败、没有别的出口。
- i. 真实模式入口：非 root 运行不带 `--fake-system` 的 abroadd 非零退出、提示需要 root、状态目录没有被创建（会话里直接测）；不带 `--dev-skip-claude-kill` 的拒绝写成纯函数单测；AST 检查对 `ProductionRunners` 的调用只在 cmd/abroadd/main.go 一处；两个 ExecRunner 的引用仍只在 realexec.go。
- j. 对真实虚拟机的阶段（vm-up 新阶段，证据进 scripts/dev/out/）：daemon-sim 启用后客户端 DNS、假地址 TCP、UDP、真实 IP 的 TCP 各步通过；停用后同样探测全部失败、开流次数不增长；再启用后恢复；最后恢复到 RESTORED；阶段前后用户环境比对不变；netstats 绑物理网卡的拨号数只在预期步骤增长；vm-up 原有各阶段仍通过。
- k. 日志不泄漏：j 的阶段把 daemon 日志存下来，搜不到探测用的域名、目的地址和假地址。
- l. build、vet、arm64 build、`go test -race -count=1 ./internal/... ./cmd/...` 全过；会话里不以 root 运行、不执行改动宿主机网络的命令、不执行 pfctl、不碰用户的 Clash。

**9b-2 状态**：完成（architect 2026-11-18 验收通过，无必须改）。

**9c 第 0 步**（architect 审 9b-2 时提出）：
1. 查清 vm-up 9b 里“启用阶段探测结束 → 停用生效”之间多出的一条流：daemon-sim 按种类（出口、协议、是否 DoH、是否出口检查）记每次开流，不记目的，证据按阶段列出；结论写进这里。若结论是“停用请求到达之后仍有新流被接受”，那是 bug，停下来找 architect。
2. 阶段 E 在下发主代理后先循环探测到一次 204（最多 10 秒）再开始各步；之后仍出现 zero_eof 就记下现场交回。

**9c 第 0 步结论**（2026-11-18）：
- 多出的那一条流是**出口检查**（`exit_check/main/tcp`），出现在“启用后的探测结束 → 停用请求之前”。来源是 daemon-sim 测试台自己：每个阶段新建 tunsim 客户端、用完就 Close，而客户端 Close 会关掉它拿到的设备（管道另一头），管道一端关了两端都失效，网络栈退出，监护判“utun 没了”，关闸门、重建、重开闸门，重开前做了一次出口检查。不是“停用请求到达之后仍有新流被接受”，也不是产品的问题（真机上 utun 另一头是内核）。修法：daemon-sim 每个管道只建一个客户端、各阶段复用；vm-up 的 9b 加断言“整个过程中监护没有关过闸门”。修复后各阶段结束时的开流次数是 [4, 4, 8, 8]，“探测结束后、停用请求之前”为“无”（证据 `scripts/dev/out/stage-a-20261118-223254.txt`）。
- 阶段 E 加了“下发主代理后先等到一次 204”，连续 4 轮都是第 1 次探测就拿到 204，没有再出现 zero_eof。

**9c 复审的改动**（architect 2026-11-18：4 处必须改、5 条建议改，都做了）：
1. 死人开关：先建状态目录，再启动死人开关（只有建状态目录可以在它之前，check-commands.sh 相应调整）；启动后核对 PID 文件和进程都在，不在就退出；restore.sh 要的 `SUDO_USER` 显式写进死人开关的环境。
2. PID：abroad-vm 用它自己写的 `vm/abroad-vm.pid`；长连接测试程序拆成 `scripts/m2/longconn.py`，自己写 PID；`m2_kill_pidfile` 改为核对完整命令行（不看 comm）；`/usr/bin/python3` 不可用时第 8 步记“跳过”。
3. IPv6 快照另存到重启不丢的 `/Library/Application Support/claude-abroad-m2-snap`，restore.sh 优先读它，全部还原成功后才删；README 写明重启后仍要执行一次 restore.sh。abroadd 的状态目录继续放 `/var/run`（重启后读到 RESTORED，不会在没有虚拟机时重新接管），lib.sh 注释写明理由。
4. restore.sh 在清完 pf 之后把 `abroadd` 状态目录改名为 `abroadd.bak-<时间>`；run.sh 的前提检查和安装 LaunchDaemon 之前都断言 `abroadd/state.json` 不存在。
5. 建议改：中途退出（非零、INT、TERM）自动执行一次 restore.sh（`m2_arm_restore_trap`，有防重复标志）；前提检查补系统代理（HTTP、HTTPS、SOCKS、PAC、自动发现）；`kill -9` 之前核对 PID 是数字；主代理存储保存后 `chmod -R go-rwx`；第 8 步改称“启用前的外部长连接已断开”，README 说明是综合效果；第 9 步要调大 `M2_DEADMAN_SECS`。
6. 不用 root 的自测 `scripts/m2/selftest.sh`（`M2_TEST_STATE` 只在自测里覆盖状态目录，以 root 运行时禁止，且只能配空跑或 `--check-only`）。

**9c 设计决定**（architect 2026-11-18）：
1. 沿用 `scripts/m0/lib.sh` 的做法：状态记在 root 所有的目录；改系统前先启动死人开关（默认 15 分钟后自动执行 restore）；`DRY_RUN=1` 只打印会执行的改动命令。
2. 专用测试用户 `_abroadm2`（沿用 M0 建用户的方式）：abroad-vm 以它运行，abroadd 用 `--pf-user _abroadm2`；restore 时删除。
3. restore.sh 不依赖 abroadd 和它的状态文件格式：启用前自己拍快照（每个网络服务的 IPv6 模式、路由表、DNS 键是否存在、pf 是否启用）；还原：按 PID 文件结束本脚本启动的 abroadd 和 abroad-vm → 卸载开发用 LaunchDaemon → 清空我们的锚点 → 释放状态文件里能读到的 token（读不到跳过）→ 两条 /1 路由先查后删、只删经 utun 的 → 删我们的 DNS 键 → 按快照还原 IPv6 → 删测试用户 → 取消死人开关。不执行 `pfctl -d`、不按名字结束进程、不碰用户的 Clash；可重复执行。
4. 启动前的前提检查，不满足就退出并说明：别的代理软件的 TUN 已关（只读路由表判断）；主代理存储里是远端或局域网节点、不是本机回环；所有网络服务的 IPv6 都是 Automatic 或 Off（有 Manual 拒绝）；提醒先结束 Claude Code 会话和其他要紧的网络任务。
5. 步骤按下面 v2 原文的 10 项；每步独立记“通过/不通过/跳过”，一步不通过不影响后面能做的步骤，最后汇总；会断网的步骤开始前打印预计时长。
6. 采样：PROGRESS 里“带进 M2-9”的采样清单逐条落到脚本里，输出写进单独的采样文件，脚本自己脱敏（本机地址、MAC、出口 IP、节点地址）。
7. README：在哪台机器、用哪个账号、怎么跑；总时长和断网时段；脚本会执行的全部改动类命令（`internal/daemon/testdata/commands.golden` 加上脚本自己额外用到的）；每一步看什么结果；判定表；出问题怎么办（restore.sh，重启电脑后 pf 自动消失）；贴回哪些文件。
8. 主会话不执行这些脚本，只做语法检查和 `DRY_RUN=1` 空跑（不需要 root，不改任何东西）。

**真机验证的前提**：真实模式下 pf 只放行“放行用户”的出站，abroad-vm 必须以那个专用用户运行；这正是 R1 要验证的（Virtualization.framework 能否在非登录用户下跑）。顺序 R1 → R2 → M2 脚本；脚本第一步以专用测试用户启动 abroad-vm，起不来就停下并说明这是 R1 的结论。R1 的备选方案（虚拟机宿主用 root、出站进程单独以专用用户运行）现在没有实现，需要时由 architect 排任务。

**9c 验收标准**（architect 2026-11-18）：
- a. 第 0 步：证据里按阶段列出每次开流的种类，多出的那一条有明确来源，结论写进 m2-plan；阶段 E 加了“先等到 204”，连续 3 轮通过或交回现场。
- b. `bash -n` 对 run.sh、restore.sh、lib.sh 通过；有 shellcheck 时没有错误级别告警。
- c. `DRY_RUN=1` 的 run.sh、restore.sh 以普通用户跑完、退出码 0、列出每一步会执行的改动命令；空跑前后用户环境比对不变（别的代理进程、默认出口、系统代理）。
- d. 命令核对：从空跑输出取出全部改动类命令，每条都落在 README 的清单里，没有 `pfctl -d`、`pfctl -e`、不带锚点的 `pfctl -f` 和 `-F all`、`pkill`、`killall`、`networksetup -setdnsservers`、对默认路由的 `route delete`。
- e. 死人开关在空跑输出里出现在第一条改动命令之前；restore.sh 的每一步在空跑里都有，顺序是先停进程和服务、再清网络、最后清 pf、然后删用户。
- f. 前提检查：本机现状下（别的 TUN 开着）直接运行 `run.sh --check-only`（只读、不需要 root），预期被“别的 TUN 开着”拦下并给出明确提示，证据记下原因种类。
- g. 采样清单逐条对照表（PROGRESS 的每条采样项 → 脚本步骤和输出文件名），没有遗漏。
- h. README 含决定 7 全部内容，判定表覆盖 10 个步骤，每步写明“通过的样子”和“不通过时贴回什么”。
- i. build、vet、arm64 build、`go test -race -count=1 ./internal/... ./cmd/...` 全过；会话里不以 root 运行、不执行真实的 run.sh 和 restore.sh、不执行改动宿主机网络的命令、不执行 pfctl。

下面是 v2 计划的原文（9b、9c 照它做）：

**文件**：
- `cmd/abroadd/main.go`：组装 tun、stack、fakedns、pf、sysnet、state、vmhost、gate。
  - 开发模式参数：`--state-dir`、`--fake-system`（全部用假实现，可以在会话里跑，配合 `tun.Pipe` 和 `abroad-dev tunsim`）；
  - 真实模式需要 root；
  - 本地 API 只给一个最小的 unix socket：`POST /v1/enable`、`/v1/disable`、`/v1/restore`，`GET /v1/status`。三级权限和签名校验属于 M5。
  - socket 权限：
    - 真实模式：socket 属 root、0600，所在目录 0700；
    - 开发模式：放在 0700 的状态目录里。
  - 真实模式下 `ClaudeKiller` 没有实现，必须显式加 `--dev-skip-claude-kill` 才能跑，并打醒目日志，防止带着假实现发布。
- `scripts/m2/`：给用户的真机脚本，带 README 和 restore.sh，沿用 `scripts/m0/lib.sh` 的死人开关和残留清理：
  1. `pfctl -nf` 语法检查生成的规则；
  2. 启用：
     - 普通用户 `curl` 公网 IP 经 utun 走虚拟机，拿到出口 IP，日志里脱敏；
     - `dig` 拿到假地址；
     - IPv6 不通；
  3. `kill -9 abroadd`：断网，`curl` 失败；launchd 拉起后恢复；
  4. `pfctl -a com.apple/250.abroad -F all`：5 秒内恢复；
  5. 局域网打印机或路由器后台可达；
  6. 恢复：路由、DNS、IPv6、pf 与启用前的快照逐项比对；
  7. R6 性能：iperf3 和网页加载，包括域名 UDP（QUIC）的首包延迟。
  8. 启用前建一条长连接，启用后确认它被断开（验证 `KillStates`）。
  9. 睡眠唤醒后的对时和 vsock（进度单里欠的真机验证）；切换 Wi-Fi；观察 `pfctl -s References`。
  10. 开发用的 LaunchDaemon plist（KeepAlive）由脚本安装，restore.sh 负责移除；restore.sh 不依赖 abroadd。
- 真机验证必须用远端节点或局域网设备上的节点，不能用本机 Clash 当主代理（见第 5 节）。
- 需要先有 R1、R2 的结论。abroad-vm 以 `_abroad` 运行时，要等 R1 的结论再决定按哪种方案跑。

**测试**：
- `--fake-system` 下的进程内端到端测试：启用、停用、恢复，以及中途 `kill` 后重启恢复，会话里跑；
- 真机部分由用户执行脚本。

**需要用户**：是，真机脚本由用户执行。会话里只交付脚本。

## 3. “不泄漏”和“失败即断网”由谁证明

| 命题 | 离线证明（会话里跑） | 真机证明（用户执行，M2-9） |
|---|---|---|
| 启用后非 `_abroad` 的流量只能走 utun | M2-6 的求值器表 | 脚本第 2 步 |
| IPv6 不泄漏 | 求值器：inet6 一律被拦；状态机启用时关掉每个网络服务的 IPv6 | 脚本第 2 步，`curl -6` 失败 |
| DNS 不泄漏 | fakedns 单测：A 只回假地址、AAAA 回空、其他类型经主代理；名单里的域名也经主代理的 DoH；没有退回本机解析器的代码路径（加 source_test 断言） | 脚本第 2 步，抓 en0 上 53 端口应为 0 包 |
| abroadd 崩溃到重启之间断网 | M2-7 每步崩溃注入，断言不变量 I1–I3；utun 消失后路由失效，流量落到默认路由，被 pf 的“其余一律拦截”挡住（求值器：en0 上非 `_abroad` 用户的包被拦） | 脚本第 3 步 |
| pf 被别的软件清掉后恢复 | M2-6 巡检测试，用假 pf | 脚本第 4 步 |
| 虚拟机里的引擎退回直连也出不去 | M2-1a 的 Outbound/GatewayDNS 单测，加 vm-up 的 4g | M2 验收时用真实节点再跑一次 |
| 加载规则之前就存在的 pf 状态不能放出旧连接 | M2-6 用假 Runner 断言 `KillStates` 紧跟加载调用 | 脚本第 8 步 |
| 恶意 DHCP 发的公网“直连网段”不被放行 | M2-6 求值器表 | — |
| 恢复后完全还原 | M2-7 快照比对 | 脚本第 6 步 |

## 4. 恢复路径

- **原值快照**：放在 `state.json`（同一个文件，原子写，0600，root 所有）。只记我们改动过的东西：
  - 每个网络服务的 IPv6 原值；
  - pf token；
  - utun 名；
  - 启用前的 DNS 服务器。
- **不改用户的网络服务配置**：DNS 用的是我们自己的 scutil 键，恢复时删掉它；路由是我们自己加的两条，原默认路由一直没动。
- **崩溃后**：
  - launchd 拉起 abroadd，按第 2 节 M2-7 的 `recover.go` 继续执行或回滚；
  - abroadd 起不来时，`sudo abroad restore --force`（M6）读同一份 state.json 恢复；
  - 再不行就重启电脑，pf 规则不跨重启保留（设计 6.2）。

## 5. 新发现的设计问题（architect 2026-11-18）

本机代理软件当主代理时会回环：
- 它连自己节点的流量也进 utun，又被送回主代理；
- 它自己直接出站会被 pf 拦掉，因为不是 `_abroad`。

所以：
- M2 的真机验证不能用本机 Clash 当主代理；
- M2-8 加阻断项；
- M3 开工时由 architect 拍板：这类软件需要自动加一条直连规则，但这和 10.4“代理软件不能设为本机网络”冲突。

## 6. 第 0 步（M1 收尾，本计划之前做完）

1. vm-up 的 c4 改成断言：`ext_delta == 0`，并且开发上游新增的请求只有 `connect 1.1.1.1:443`。
2. design 7.2、R6、16 回写：UDP 的域名目标经主代理的 DoH 解析后再转发。
3. `go test -count=50 ./internal/agent/` 的完整输出存到 `scripts/dev/out/`。
4. PROGRESS 的措辞更正；写入 M1 完成和 M2 待办清单。
