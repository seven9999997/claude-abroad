# 开发进度

> 每次会话收工时更新。最新的写在最上面。

## 当前状态

- 里程碑：**M2 开发完成（2026-11-18，architect 判定），进入 M3 分流**。M2 的“验收通过”等用户执行“需要用户人工处理”的第 1 步和第 5 步。
  - M1：开发完成 + 验收通过（证据 `scripts/dev/out/stage-a-20261118-151223.txt`）。
  - M2 状态：
    - **开发完成**：M2-1 到 M2-9c 全部验收通过；
    - **验收通过**待用户：清单第 1 步（远端节点的 tunsim 验证）、第 5 步（真机脚本的 10 步判定和采样）。采样拿回来后替换 sysnet、pf 里的 `.constructed` 样本，重跑解析测试，由 architect 判定。
  - 第 15 节风险在 M2 的结论：R20 已有结论（出站收紧到第一跳）；R1、R2、R6 等真机结果。
- 里程碑补充（2026-11-19）：**M3 的编码任务 M3-0 到 M3-8 全部验收通过**。M3-8 判定为“脚本完成、暂不交给用户”。
  - M3“开发完成”待定，只差一件事：阶段 9c 在会话里对真实虚拟机跑通一次（每次会话开始先跑 `scripts/dev/vm-up.sh --precheck-only`，通了就立刻补跑 9c，证据交 architect）。
  - M3“验收通过”待用户：清单第 6 步（R3）、第 7 步（M3 真机脚本，9c 跑通后才放行）。
  - 建议改 2（S4 的“主代理一侧有新连接”改为 main 的按出口计数）留到 M4 做出口检查时一起改。
- **当前任务**：
  ```
  下一步：继续 W-2a（范围按 design-windows 0.5 第 7–9 条调整），开工前先做公开仓库的隐私清理（2026-11-21：编码、Mac 上的单测、Windows 上的冒烟测试完成，待 architect 验收）
  - 要做什么：0 隐私清理（本机信息挪到不进版本库的 docs/LOCAL.md；scripts/dev/privacy-check.sh；CI 里加这一步；清理经 architect 确认前不推送）；1 W-2a 原来的四项：规则模型与离线求值、系统层、还原工具、第一级探查与 README；2 新增：探查前后记录别的提供者；远程执行的规程；系统层的冒烟测试接进自动构建的 Windows 任务。
  - 验收标准：W-2a 原来的 a–h，加 i 隐私检查（工作区和最近一次提交零命中，列出模式和挪走的段落标题，CI 里有这一步）、j 别的提供者的清单（纯函数汇总有单测，只有名字和数量）、k 冒烟测试（Windows 约束，只碰自己的提供者和回环上一条过滤器，无论成败都清理，如实写是否在 Windows 上跑过）、l 远程执行的规程写进 README（第一级探查本任务里不执行）。
  - 需要用户：是（Windows 电脑开远程登录并装公钥；建空仓库，隐私清理确认后再推送；知情两点。细节见 docs/LOCAL.md）。
  ```
- 设计文档：v0.18；Windows 平台层 design-windows v0.4。
- **构建与测试备忘（W-1 起）**：每个任务的验收都加 `GOOS=windows GOARCH=amd64 go build ./...` 和 `GOOS=windows GOARCH=amd64 go vet ./...`（Mac 的任务也一样）；Windows 上没实现的功能一律返回 `platform.Unsupported`，源码检查 `internal/srccheck` 的 `TestRepoWindowsPlaceholdersFail` 会抓“假装成功”的占位。
- M0：不再单独做 demo（2026-11-17 用户决定）。`docs/m0-plan.md` 作废，只保留 R2 脚本（`cmd/m0-r2-probe/`、`scripts/m0/`）。

### 用户的工作约定（2026-11-17）

- 付费项（Developer ID 签名、公证、SMAppService、Endpoint Security、Network Extension）全部推迟，不作为待办提醒。ad-hoc 签名照用。
- ~~主会话只开发~~（2026-11-18 放宽）：**除了不允许让宿主机断网，其他开发测试都可以做**，包括运行镜像构建、启动本项目的虚拟机、跑不涉及断网的 integration 测试。会让 Mac 断网的（如 R2 的 `run.sh`）和需要 sudo 的仍交给用户。细则见 `CLAUDE.md`“硬性限制”。

## M1 待办

| 任务 | 内容 | 状态 |
|---|---|---|
| M1-1 | 文档对齐 + `internal/wire`（帧头 v1、回应帧、UDP 分帧、yamux 会话） | **完成**（2026-11-17 复审通过） |
| M1-2a | `guest/manifest.json` + `internal/imagebuild` + `cmd/abroad-imagebuild`（build、verify），删 `fetch-proxy-core.sh` | **完成**（2026-11-17 审核通过，必须改已改） |
| M1-2b | abroad-agent 最小版（boot、薄 PID 1 的 init、serve + `/v1/health`、`/v1/shutdown`）并装进镜像；modules 相关代码移到 `internal/kmod` | **完成**（2026-11-18 验收通过，无必须改；4 条建议改已在 M1-3 开工时做完） |
| M1-3 | vmhost 最小版 + `scripts/dev/` → 阶段 A（开机后 health 可达，主会话自己跑，硬性验收） | **完成**（2026-11-18 验收通过；阶段 A 证据在 `scripts/dev/out/stage-a-*.txt`；panic 参数已按审核改为 `panic=0` 并重测） |
| M1-4a | vmhost 运行期巡检、`boot_id`、就绪回调、强停无效不起第二台 | **完成**（2026-11-18 验收通过；3 条建议改已做：唤醒判据加探测超时、OnReady 并发约定注释、Timing 逐字段兜底） |
| M1-4b | 引擎 supervisor、日志环形缓冲、sing-box 配置生成、`PUT /v1/main-proxy`、`GET /v1/logs` | **完成**（2026-11-18 验收通过；6 条建议改在 M1-4c 第 0 步做完） |
| M1-4c | 数据面（vsock 1080 上的 yamux → sing-box socks） | **完成**（2026-11-18 验收通过；4 条建议改在 M1-4d 第 0 步做完） |
| M1-4d | 对时（`PUT /v1/clock` + abroad-vm 的对时循环） | **完成**（2026-11-18 验收通过；建议改在 M1-4e 第 0 步做完） |
| M1-4e | data.sock → vsock 1080 桥 + 开发用探测程序，在真机上对真实 sing-box 跑 unknown_exit、exit_down、TCP、UDP（从 M1-5 提前） | **完成**（2026-11-18 验收通过；证据 `scripts/dev/out/stage-a-20261118-131622.txt`） |
| M1-5a | 虚拟机出站网络的宿主侧：补丁 0006 + `internal/usernet`（拨号器、出站策略、REFUSED DNS、配置组装），纯单测 | **完成**（2026-11-18 验收通过；5 条建议改在 M1-5b 第 0 步做完） |
| M1-5b | socketpair + `VZFileHandleNetworkDeviceAttachment` 接进 abroad-vm，放开 `Spec.NetFD`，agent 配 eth0，vm-up 加出站阶段 | **完成**（2026-11-18 验收通过；5 条建议改在 M1-6a 第 0 步做完） |
| M1-6a | agent 对主代理配置的二次校验（`internal/mihomocfg` 白名单）+ `managedproxy.VMRuntime` | **完成**（2026-11-18 复审通过；证据 `scripts/dev/out/stage-a-20261118-142327.txt`） |
| M1-6b | 回环白名单：网关虚拟地址经 NAT 映射到 127.0.0.1（只在白名单有回环项时）、abroad-vm 的白名单启动参数 | **完成**（2026-11-18 验收通过；3 条建议改在 M1-6c-1 第 0 步做完） |
| M1-6c-1 | VMRuntime 第一跳回环改写与白名单推导、`dial_ok_loopback` 计数、`abroad-dev socks`、进程内全链路测试 | **完成**（2026-11-18 验收通过；证据 `scripts/dev/out/stage-a-20261118-144546.txt`） |
| M1-6c-2 | `abroad-dev upstream`、`apply-main-proxy`，vm-up 的阶段 B、C（开发上游固定应答） | **完成**（2026-11-18 复审通过）。c3 查出 UDP 直连泄漏，已返工修复：<br>• 复现证据 `scripts/dev/out/stage-a-20261118-150341.txt`；<br>• 修复后证据 `scripts/dev/out/stage-a-20261118-151223.txt`。<br>4 条建议改在 M2-0 第 0 步做完。 |
| M1-7 | R1 脚本（`_abroadm0` + launchd，root 对照；需 sudo，用户执行）。顺序提前到 M1-3 之后 | **完成**（2026-11-18 验收通过；真机结果待用户执行） |

顺序（architect 2026-11-18）：M1-2b → M1-3 → M1-7 → M1-4 → M1-5 → M1-6。

完成状态：①**开发完成** = M1-1 到 M1-7 齐全且进程内全链路测试通过；②**验收通过** = 阶段 A、B、C 跑通（2026-11-18 起由主会话自己跑，证据放 `scripts/dev/out/`，architect 判定；B、C 需要用户提供节点）。

## M3 待办（architect 2026-11-18；细拆以 `docs/m3-plan.md` 审定为准）

M3 开工的 4 条决定（已回写 design v0.6）：①不支持本机代理软件当主代理；②“本机网络”只对看得清去向的连接生效；③直连应用的域名由直连拨号模块经本机原 DNS 解析，置顶域名永远不经本机 DNS，私有网段 PTR 本地回 NXDOMAIN；④出口 A、B 用开发上游模拟。

| 任务 | 内容 | 验收标准（摘要） | 状态 |
|---|---|---|---|
| M3-0 | 写 `docs/m3-plan.md` 并送审，含 design 回写、R14 只读调研 | m3-0 的 a–f | **完成**（2026-11-18 v2 复审通过，2 条补充写进 v2.1） |
| M3-1 | 规则引擎 `internal/rules` | m3-plan v2 的优先级表逐行覆盖，冲突组合也覆盖；性质测试：Claude 进程、置顶域名、解析落在 Anthropic 段、Anthropic 段、不透明 TLS/QUIC、443 超时这六种情况永远不会直连；default 不能是分流出口；出口不可用回退主代理；Parse 拒绝把 Claude 和代理软件设成直连 | **完成**（2026-11-19 复审通过：3.3 误拦已修，m3-plan v2.2；FuzzParse 60 秒 480 万次，证据 `scripts/dev/out/m3-1-fuzzparse-20261119.txt`；全量 -race 35 个包通过）|
| M3-2 | 首包识别 `internal/sniff`（TLS 含 ECH、QUIC Initial、HTTP Host） | 样本和构造样本；模糊测试；分片、超长、乱序；能区分“不是 TLS”和“是 TLS 但没有域名” | **完成**（2026-11-19 验收通过，无必须改；模糊测试 TLS 456 万、QUIC 305 万、HTTP 431 万次） |
| M3-3 | 应用识别 `internal/procinfo` | 路径到应用 ID 的纯单测；当前用户的只读实跑；R3 脚本（root 部分交用户） | **完成**（2026-11-19 验收通过，无必须改；FuzzPlist 345 万次；root 下连接表的可见性记为 R3 待验证项，见“需要用户人工处理”第 6 步） |
| M3-4 | 直连拨号模块（6.3）+ 直连 DNS 解析 | socketpair 测 fd 传递；只接受 root 对端；拒绝回环、组播等；置顶域名的解析被拒；**解析结果再判一次**（落在 Anthropic 段回 pinned）；`local_domains` 分配带标记的假地址；`abroad-vm direct` 能单独运行；超时和并发上限；源码检查 | **完成**（2026-11-19 验收通过，无必须改；会话内真实拨号被本机环境挡住，按如实报告接受；别名链判置顶在 M3-5 第 0 步做完） |
| M3-5 | 接进网络栈；UDP 负缓存；“拦截 UDP 443” | 进程内三应用同时走出口 A、B、直连；直连应用访问置顶域名、发 ECH 到真实 IP、Claude 进程、解析到 Anthropic 段都走主代理；帧头只填 fake 域名或原始 IP；闸门关闭时代理全拒；没有绕过规则引擎的路径 | **完成**（2026-11-19 验收通过，无必须改；NeedApp 有 local_domains 时也识别，architect 认可） |
| M3-6 | 停用状态下的 `offline_allow`；规则即时生效、可选断开 | DISABLED 下名单内应用可直连、置顶域名不可达；改规则后新连接按新规则走、旧连接按选项断或不断 | **完成**（2026-11-19 验收通过，无必须改；登记时检查代数、重判时刷新本地标记和应用识别，architect 认可） |
| M3-7 | 接进 daemon：规则读写接口、规则文件；daemon-sim 多出口多应用；vm-up 新阶段 | 本地接口改规则后的端到端；日志里没有域名和地址 | 交验收（2026-11-19）：规则文件与接口、直连配置下发、启用前本机 DNS、出口临时不可用、status 增补、真实模式接 procinfo、开发用多出口（abroad-vm 用 abroaddev 构建标签）、daemon-sim --multi 和 vm-up 阶段 9c；**验收通过（2026-11-19）**。全量 -race 40 个包通过。审核的必须改（直连配置带代数、`stale_config` 按需补发、改规则不再依赖下发）和建议改 2、3 已做，2026-11-19 再交验收；**阶段 9c 只做过语法检查、没有执行，被本机环境挡住**（绑物理网卡的连接被拒，证据 `scripts/dev/out/stage-a-20261119-165453.txt`、`direct-probe-20261119-165430.txt`） |
| M3-8 | 给用户的真机脚本 `scripts/m3/`（第 14 节 M3 完成标准，R3 的 root 部分、R15） | 脚本、README、空跑、命令核对，沿用 M2-9c 的安全约定；主会话不执行 | 未开始 |

- **带进 M3-8 的事**（architect 审 M3-4 时提出）：真机脚本里以专用用户身份跑一轮 300 次直连拨号的回显，确认跨用户、跨进程时在途 fd 同样 0 失败。
- **之后每次会话跑 vm-up 之前**：先跑 `scripts/dev/direct-probe.sh`（或 `scripts/dev/vm-up.sh --precheck-only`）；哪次绑物理网卡通了，把证据补进这里（M3-4 验收 h 的补跑）。

## M4 待办（architect 2026-11-19 判 M3-8 时给出；细拆以 `docs/m4-plan.md` 审定为准）

| 任务 | 内容 | 验收标准（摘要） | 状态 |
|---|---|---|---|
| M4-0 | 写 `docs/m4-plan.md` 并送审 | a–f | **完成**（2026-11-19，v1 结构通过；3 条必须改、6 条建议改写进 v2，随 M4-1 一起审） |
| M4-1 | **完成**（2026-11-19）探查 R19、R5：镜像里装 openvpn、wireguard-tools、nftables；虚拟机里建命名空间、veth、nft；同机起对端，WireGuard、OpenVPN 各通一条隧道 | 镜像可复现、校验哈希；隧道能通；断开时命名空间内只能连对端；结论写进 design 15；不行时给备选评估 | |
| M4-2 | **完成**（2026-11-19）出口检查与出口变化规则（HTTPS 探测、出口 IP 与位置多源三结果、UDP 探测、DoH、基准）；复用 Cage 的 exitpolicy | 四种变化规则表驱动测试；大陆段一律不通过；查询源全部异常是“无法判断”；以收到预期 HTTP 应答为准 | |
| M4-3 | **完成**（2026-11-20）闸门状态机：检查中、放行 90 秒、每 60 秒复查、关闭原因；按出口的可用状态；接进启用流程和监护 | 时钟注入测试；“无法判断”沿用 10 分钟；同国漂移不关；主代理不通过全关、分流出口不通过只回退它；M2、M3 不变量仍成立 | |
| M4-4 | **完成**（2026-11-20）Claude 进程清单与强制结束（10.1），两步确认的通用机制 | 假进程表测试；真实实现非 root 拒绝；会话里不执行；替换真实模式的跳过参数（参数保留到 M6） | |
| M4-5 | **完成**（2026-11-20）地区：目标地区确定与锁定；虚拟机内时区语言；Mac 时区、自动时区、语言（只拼命令）；原值记入状态文件并恢复；启用流程第 ⑥ 步 | 崩溃穷举加地区步骤后仍通过；恢复后原值逐项还原；地区只在三种用户操作时变；出口漂移不改地区 | |
| M4-6 | **完成**（2026-11-20：6a、6b）环境检测 17 项、调度、与闸门联动。6a：框架、调度、闸门联动、启用第 ⑦ 步、探测窗口，检查项 01、06、07、09、11；6b：02、03、08、10、04、05、12–17，出口历史、本机真实 IP、R10 | 每项三种结果有用例；人为制造泄漏能检出（进程内注入）；查询服务不可用是“无法判断”；警告不阻断；日志脱敏 | |
| M4-7 | **完成**（2026-11-21 architect 判定，含 v2.10 的必须改）虚拟机内分流出口引擎：命名空间、agent 的 socks 子命令、每出口断网规则、WireGuard、OpenVPN、订阅 mihomo；凭据下发 | 同机对端端到端；隧道断开不泄漏；凭据只在内存文件系统、不进日志；多引擎同时运行 | |
| M4-8 | **完成**（2026-11-21 architect 判定，无必须改，4 处说明确认） 出口管理接进 daemon：出口增删接口、sing-box 配置生成、规则出口标签校验、按出口检查与 UDP 回退 | 不装分流出口全走主代理；装上后各引擎可用；不可用回退主代理、恢复后走回；status 有按出口计数（同时把 M3-8 S4 的判定换成 main 的计数） | |
| M4-9 | 给用户的真机脚本：R4、R5、地区切换与还原、带人为泄漏的环境检测；另带进（architect 判 M4-6b）：在装了桌面版的机器上核对 R10（语言设置是否就在 config.json 的 locale、取值写法）；第 10 项和本机真实 IP 的测量对真实对端各验证一次，并核对日志和状态目录里没有真实 IP。另带进（architect 判 M4-4）：在装了对应软件的机器上核对 Claude Code 安装位置特征（Homebrew、IDE 插件、JetBrains）和桌面版的签名 Team ID；在用户知情的前提下验证真实的结束流程 | 沿用 M2-9c、M3-8 的安全约定；主会话不执行 | |

陆续记下的：

- （architect 判 M4-8 时提出，带进 M4-9）正式的按出口检查（HTTPS、出口 IP、大陆段、DoH）还没有对真实出口跑过（M4-8 的真实虚拟机阶段用的是 daemon-sim 里开发用的检查）：用真实服务商的配置时，核对各出口的检查结果和回退行为。
- （architect 复审 M4-7 时提出，带进 M4-9）订阅出口的 UDP 域名目标端到端连通：同机测试里对端没有真的 DoH 服务，只验证到“解析经节点、没有本地解析”；用真实节点验证能连通。
- （architect 判 M4-7 时提出，带进 M4-8）出口命名空间断网规则里的服务器端点现在按 IP 写。用户配置里的端点（以及订阅节点的服务器）是域名时，谁来解析、解析结果变化时规则怎么更新，在 M4-8 定：解析不能经主代理，也不能经别的出口；可选做法是沿用“第一跳域名由网关代解析”，并把结果同时用于宿主的出站白名单和命名空间的规则。
- （architect 判 M4-7 时提出，带进 M4-8）宿主的出站白名单要放行各出口的服务器端点；白名单变化时怎么更新（重启虚拟机宿主，还是动态下发）在 M4-8 定。
- （architect 判 M4-1 时提出，带进 M4-7）OpenVPN 推送的 DNS 由 agent 自己提供的固定脚本接收（用户配置里的脚本类指令一律拒绝）；开工时评估方式写进计划，拿不到时退到固定的公共解析器，仍只经隧道。另：第 0 步在 kmod 加载器里通用地支持 modules.softdep 的 pre:。
- （architect 审 M1-4e 时提出，M3-0 时转记）出口检查必须端到端：以“收到了预期的 HTTP 应答”为准，不能以回应帧 ok 或 CONNECT 成功为准。

- （architect 审 M2-7e 时提出）出口复查（设计第 8 节）关掉闸门时，要有办法让监护不按最小检查把它重新打开，例如给闸门加“关闭原因”。

## Windows 待办（architect 2026-11-20 排；用户 2026-11-20 决定两边一起做）

| 任务 | 内容 | 状态 |
|---|---|---|
| W-0 | **完成**（2026-11-20）设计文档 docs/design-windows.md（平台映射表、五条底线在 Windows 上靠什么保证、WR1–WR8 风险表、里程碑、哪些 Mac 结论要重新实测）；design.md 第 1、2、12、14、16 节；每个包的 Windows 交叉编译清点表；PROGRESS 的 Windows 待办和需要用户的表。不改代码。验收 a–f 见 architect 2026-11-20 的任务说明（M4-6b 验收后再确认一次） | 待开始（M4-6b 之后） |
| W-1 | 平台接口整理：全部包能交叉编译到 Windows，占位实现一律返回“本平台尚未支持”（design-windows 0.4） | **完成**（2026-11-21 architect 判定，无必须改；集中到 internal/platform 的做法确认） |
| W-2a | 断网规则的模型与离线求值（internal/wfprules）、系统层（internal/wfp）、独立的还原工具、第一级探查（只用动态过滤器）、scripts/win/ 的 README（design-windows 0.5） | 【待验收】（2026-11-21；系统层的冒烟测试已在用户的 Windows 电脑上经 SSH 跑通；第一级探查本任务里不执行） |
| W-2b | 第二、三级探查（持久过滤器、开机过滤器、重启）以及 WR2、WR3；等用户跑完第一级再做 | 待排 |
| ~~W-2~~ | （已拆成 W-2a、W-2b）WR1（WFP 持久过滤器、按 SID 放行）、WR2（Wintun + gVisor、再分发许可）、WR3（HCS 直接启动内核、只用 Hyper-V 套接字）的探查程序和用户脚本。**安全要求（architect 判 W-0）**：先交付独立的还原工具（只删我们 provider 下的全部过滤器、子层和 provider，可重复执行，空跑和命令行为先经 architect 审）；WR1 分三级（只用动态过滤器 → 持久过滤器加“到点自动还原”的计划任务 → 开机过滤器和重启），前一级通过才做下一级；每级开始前确认还原工具能运行（先空跑）、计划任务已注册；README 把应急步骤写在最前面（含“进安全模式执行还原工具”，在第二级验证安全模式下过滤器是否生效） | 待排（M4-8 之后） |
| W-3… | 按探查结论排 | — |

风险探查 WR1–WR8：WFP 持久过滤器与按身份放行；Wintun + gVisor 与许可；HCS 启动虚拟机（家庭版是否可用）；不写驱动的连接到进程；专用服务账户与 SID；DNS 接管与多网卡并行解析；时区和语言；Claude 在 Windows 上的安装位置与签名。

## M5 待办（陆续记下的）

- （architect 审 M2-9a 时提出）abroadd 以 root 运行时，冲突检查读 shell 配置要用“当前登录用户的主目录”，怎么取在 M5 定（9b-1 里 Home 为空，跳过这一项）。

- （architect 2026-11-21 回答 M4-8 的问题 3 时提出）主代理的配置由 abroadd 持有并负责下发，包括虚拟机重启后的重下发（现在由 abroad-dev 手动下发，虚拟机重启一次就要手动重新应用；不是泄漏：重启后主代理不通、出口检查不通过、闸门会关）。

## M6 待办（陆续记下的）

- （architect 2026-11-20 审 M4-5）发布构建不接受任何 `--dev-` 开头的参数（现在有 `--dev-skip-claude-kill`、`--dev-skip-region` 两个）。

- （architect 判 M4-1 时提出）构建用到的 Alpine 软件包（`guest/out/cache/apk`）随发布物料一起存档，发布流程里保留：Alpine 稳定分支更新后会删掉旧版本，固定的地址可能 404（design 7.1、R19）。
- （architect 审 m3-plan v1 时提出）发布构建用构建标签把 agent 的 `dev-exits` 整段编译掉，不只靠内核命令行的 `abroad.dev=1`。（M3-7 进展：abroad-vm 打开它的 `--dev-agent` 已经只在 `-tags abroaddev` 的构建里有；agent 一侧仍靠内核命令行判断，M6 再用构建标签去掉。）
- （architect 审 m3-plan v1 时提出）Claude 进程的识别加上代码签名校验（M3 只按路径和命令行识别）。

- （architect 审 M2-9b-1 时提出）发布构建不能带 `--dev-skip-claude-kill`（结束 Claude 的真实实现到位前，真实模式要显式带它才能启动）。
- （architect 审 M2-9b-1 时提出）abroadd 管理虚拟机的生命周期（M2 阶段虚拟机由外部启动），等 R1 有结论后在 M5、M6 做。

- （architect 审 M2-7b 时提出）`abroad restore --force` 遇到损坏的状态文件怎么处理。

## M2 待办（architect 2026-11-18；细拆以 `docs/m2-plan.md` 审定为准）

| 项 | 内容 | 验收标准 | 状态 |
|---|---|---|---|
| M2-0 | 写 M2 实施计划，含第 0 步收尾 | 计划交 architect 审 | **完成**（2026-11-18：第 0 步通过；计划有 8 条必须改，已改成 v2） |
| M2-1a | 出站收紧（宿主侧，方案 d）：放行集合 + 网关 DNS 代解析 + abroad-vm 自己的 DoH；默认收紧，`--outbound-open` 只给开发用 | 见 m2-plan v2 | **完成**（2026-11-18 验收通过；4 条建议改在 1b 第 0 步做完） |
| M2-1b | mihomo 的节点域名改由网关解析，`VMConfig.FirstHop`，`--print-first-hop`，实测 (a)(b)，vm-up 4g-iii。`Restart` 挪到 M2-7/M2-9 | 见 m2-plan v2 | **完成**（2026-11-18 验收通过；4 条建议改在 M2-2 第 0 步做完） |
| M2-2 | `PhysicalInterface` 挪到独立小包，改成“先用旧结果、后台刷新” | 单测；abroad-vm 不再 import managedproxy | **完成**（2026-11-18 验收通过；建议改“作废与在途查询的竞态”在 M2-4a 第 0 步做完） |
| M2-3（排在 M2-8 之后） | 真实本机 mihomo 的 UDP 中继实测：本机单独起一个 mihomo，只听回环 mixed-port，不开 TUN，不碰用户的 Clash | m2-plan 的 M2-3 a–h | **完成**（2026-11-18；回环 UDP 不放开，根因带进 M3） |
| M2-4a | fake-IP 地址池 + 落盘（只用标准库） | 纯单测，architect 的 a–k | **完成**（2026-11-18 复审通过） |
| M2-4b | DNS 服务、Bypass 名单、Upstream 接口、反查（第 0 步：清理临时文件、Prune 30 天、删 inflightGen） | m2-plan 的 a–j | **完成**（2026-11-18 验收通过；4 条建议改在 M2-5a 第 0 步做完） |
| M2-5a | `internal/tun` + `internal/stack/stack.go`（Handler、上限、入口过滤），含第 0 步 | m2-plan 的 a–k 加 2 条必须改的测试 | **完成**（2026-11-18 验收通过；3 条建议改在 M2-5b 第 0 步做） |
| M2-5b | `internal/stack/proxy.go` + 进程内全链路 | m2-plan 的 M2-5b 一节，a–k | **完成**（2026-11-18 验收通过；4 条建议改在 M2-5c 第 0 步做完） |
| M2-5c | `internal/tunsim` + `abroad-dev tunsim` + vm-up 阶段 E | 见 m2-plan | **完成**（2026-11-18 验收通过；3 条建议改在 M2-5d 第 0 步做完） |
| M2-5d | 经 Opener 的 DoH Upstream（进程内 TLS 假服务器单测；真机验证并入真实远端节点验证） | m2-plan 的 M2-5d 一节，a–l | **完成**（2026-11-18 复审通过：拨号加握手有独立期限） |
| M2-6a | pf 规则生成器（展开形式）、离线求值器、pf-syntax.sh | m2-plan 的 M2-6a 一节，a–i | **完成**（2026-11-18 验收通过；2 条改动在 M2-6b 第 0 步做） |
| M2-6b | pf 的 Controller、Matcher、假 pf、Ensure（含第 0 步：lo0 顺序、名字校验收紧） | m2-plan 的 M2-6b 一节，a–i | **完成**（2026-11-18 复审通过） |
| M2-6c | pf 巡检（每 5 秒重算并调用 Ensure，连续 3 次回调，主规则集不再引用时的处置） | m2-plan 的 M2-6c 一节，a–j | **完成**（2026-11-18 验收通过；M2-6 整体完成；2 条建议改在 M2-7a 第 0 步做） |
| M2-7a | pf 收尾两条；`internal/state` 状态文件；`internal/sysnet` 接口与假实现 | m2-plan 的 M2-7 一节，a–i | **完成**（2026-11-18 验收通过；3 条建议改在 M2-7b 第 0 步做） |
| M2-7b | 状态机（启用、停用、恢复、失败回滚、崩溃注入、不变量 I1–I3） | architect 审 7a 时给出的 a–k | **完成**（2026-11-18 复审通过；返工 1 条必须改、2 条建议改已做） |
| M2-7c | Recover、Boot、跨进程锁、pf 应用归到一处 | m2-plan 的 7c a–i | **完成**（2026-11-18 验收通过，无必须改） |
| M2-7e | 运行期监护：Protected 与闸门联动、路由和 DNS 巡检、utun 故障重建、出口检查重试（从 7c 拆出） | m2-plan 的 7e a–m | **完成**（2026-11-18 验收通过；2 条建议改在 7d 第 0 步做） |
| M2-7d | sysnet 的 darwin 实现（参数拼装、输出解析、只读实跑） | m2-plan 的 7d a–h | **完成**（2026-11-18 验收通过；M2-7 整体完成；3 条收尾在 M2-8 第 0 步做） |
| M2-8 | 启用前冲突检查，只读 | m2-plan 的 M2-8 a–g | **完成**（2026-11-18 验收通过，无必须改） |
| M2-9a | 路由冲突看整张表；abroadd 假系统组装和本地接口 | m2-plan 的 9a a–k | **完成**（2026-11-18 验收通过，无必须改） |
| M2-9b-1 | 真实系统层接线（命令模拟器验证，真实模式不开放） | m2-plan 的 9b-1 a–g | **完成**（2026-11-18 验收通过，无必须改；2 处收尾在 9b-2 第 0 步） |
| M2-9b-2 | 数据面接线，做完才开放真实模式（需要 root，会话里不运行） | m2-plan 的 9b-2 a–l | **完成**（2026-11-18 验收通过，无必须改；2 处收尾在 9c 第 0 步） |
| M2-9c | 给用户的真机脚本、README、restore.sh、采样清单（需要 R1、R2 结论） | m2-plan 的 9c a–i | **完成**（2026-11-18 复审通过：返工 4 条必须改、5 条建议改已做；证据 `scripts/m2/out/m2-dryrun-evidence-20261118-224628.txt`） |

- **pfctl**：会话里只有 `pfctl -n` 语法检查这一个例外，条件见 `docs/m2-plan.md` 0.1；其余用法一律不执行。
- M2-7 内部顺序（architect 2026-11-18）：7a → 7b → 7c → 7e → 7d。
- 执行顺序（architect 2026-11-18）：M2-1a → M2-1b → M2-2 → M2-4 → M2-5 → M2-6 → M2-7 → M2-8 → M2-3 → M2-9。
- **本机代理软件当主代理会回环**（architect 2026-11-18）：M2 的真机验证不能用本机 Clash 当主代理，必须用远端或局域网节点；M2-8 加阻断项。M3 开工时已拍板：不支持本机代理软件当主代理，不加自动直连规则（design v0.6 的 4.5）。
- **带进 M2-6c 的事**（architect 审 M2-6b 时提出）：巡检要记住上一次成功应用的规则。锚点内容既不等于期望、也不等于上一次的（可能是别的软件塞进了规则），按“原先不在”处理，同时 KillStates。
- **带进 M2-7c 的事**（architect 审 M2-7b 时提出）：
  - pf 的应用归到一处：现在 `PFController` 和 `pf.Patrol` 各自记“上一次应用的规则”，一方换过规则后另一方会把锚点当成别人改的、多清一次状态。7c 改成状态机的 PF 适配器委托给同一个 Patrol（设置 utun 后调 Step）。
  - “将要启用”标记改由 Controller 在真正调用 `-E` 之前经钩子落盘，巡检自己重新启用 pf 时也覆盖到；不再靠状态机在每次 Apply 之前置标记。
  - Boot 只要不是 RESTORED 就加载 pf（含步骤 6 的 RESTORING），由 Recover 从 ⑥ 做完（先再结束一次 Claude）。
- **带进 M2-8 的事**（architect 审 M2-7b 时提出）：从 DISABLED 启用时的冲突检查不能把我们自己的接管（路由、DNS、utun）当成冲突。
- **带进 M2-7c 的事**（architect 审 M2-7a 时提出）：`Store` 的锁只在进程内有效，abroadd、开机 guard、`abroad restore --force` 是不同进程，读改写会互相覆盖。7c 要定跨进程互斥（例如对状态目录加 flock）。
- **带进 M2-7c 的事**（architect 审 M2-6c 时提出）：Boot 读状态文件遇到 `ErrCorrupt` 时，按“非 RESTORED”处理，加载 pf。
- **带进 M2-7 的事**（architect 审 M2-6b 时提出）：
  - 路由、DNS、`route get 198.18.0.2` 的核对不在 pf 巡检里，归 M2-7；sysnet 要适配 `pf.NetSource`。
  - `Patrol.Protected()` 为 false 时，闸门必须关闭并通知用户；`ErrAnchorNotReferenced` 的通知要说明“别的软件替换了系统防火墙规则”。
- **带进 M2-7 的事**（architect 审 M2-6a 时提出）：
  - `-E` 成功到 token 落盘之间崩溃，会漏掉一个启用引用。状态机要先写“将要启用”再调用 `-E`，恢复路径要能处理 token 未知的情况。
  - Ensure 返回的新 token 要追加保存，恢复时全部释放。
- ~~带进 M3 的事~~（**不做**，architect 2026-11-18 M3 开工决定 1：不支持本机代理软件当主代理）（architect 审 M2-3 时提出）：本机回环第一跳的 UDP 不可用，根因是本机 mihomo 对 UDP ASSOCIATE 回的 BND 是 127.0.0.1（证据 `scripts/dev/out/stage-a-20261118-203909.txt`：握手成功、本机 mihomo 日志无 UDP 记录、netstats 只有两次回环拨号）。两种修法：给 mihomo 打补丁（BND 是回环时改用服务器地址）；或在虚拟机里加一个把回环 BND 转到网关的 UDP 中继（还依赖“BND 端口等于服务器端口”，别的代理软件未必成立）。M3 决定“本机代理软件能不能当主代理”时一起定。
- **带进 M2-9 第 0 步**（architect 审 M2-8 时提出）：路由冲突改成看整张路由表——用 `netstat -rn -f inet` 找出所有经虚拟网卡、且不是我们 utun 的路由，目的网段落在公网范围或覆盖 198.18.0.0/15 的算冲突，私有范围的不算（公司 VPN 的内网分段路由不该拦着启用）；冲突检查和监护的路由核对共用这个判断，现在的地址探测保留作“实际出口”的复核。同时把“启用后公司 VPN 的内网流量会被 pf 拦住”写进 design 5.3 已知限制。
- **带进 M2-9 的事**（architect 审 M2-7e 时提出）：巡检只由监护驱动（`Supervisor.Tick` 里的 `Patrol.Step`），组装时不要再单独起 `Patrol.Run`。
- **带进 M2-9 的事**（architect 审 M2-7c 时提出）：
  - 开机时 guard 和 abroadd 可能抢同一把锁，abroadd 拿到 `ErrLocked` 要等待重试，不能直接退出。
  - `Stack.Done()` 关闭、睡眠唤醒、网络变化这三类事件在组装层接到监护的 `Kick()`；后两类同时调用 `physif.Watcher.Invalidate()`。
- **带进 M2-9 的事**（architect 审 M2-6a 时提出），用户脚本要采集这些样本：
  - （M2-7d 追加，拿到后替换 `internal/sysnet/testdata/*.constructed*` 并重跑解析测试）`networksetup -getinfo` 在 IPv6 为 Off、Manual（看有没有前缀长度、字段名是什么）、Link-local 时的输出；`-setv6manual` 不给路由器时的行为；`route -n get` 经物理网卡和 `not in table` 时的输出；`route -n add` 已存在时 stderr 的原文；scutil 写入我们的键之后 `show` 的输出，以及系统是否把它当首选解析器（是否还需要 SupplementalMatchDomains、优先级）；networksetup 出错时的退出码和输出位置。
  - （M2-7b 追加）`pfctl -X <已释放的 token>` 的输出（目前按 stderr 含 “invalid token” 判为已失效）。
  - `pfctl -a <锚点> -sr`；
  - `pfctl -sr`（主规则集）；
  - `pfctl -s info` 的首行；
  - `pfctl -E` 在已启用和未启用两种情况下的输出；
  - `pfctl -a <锚点> -t abroad_lan -T show` 在表存在和不存在时的输出。
  - （architect 审 M2-6b 时追加）真机验证：启用前建一条本机回环 TCP 长连接，启用之后、以及手动清锚点触发重载之后，这条连接都仍能收发（lo0 那一行是 no state）。
  - （architect 审 M2-6b 计划时追加）root 下 `pfctl -o none -a <锚点> -f -` 实际加载后，`-a <锚点> -sr` 的输出，逐行对比 `-nvf -o none` 的样本；并确认 `-o none` 与 `-a` 一起用时不报错。
- **带进 M2-6b 的事**（architect 审 M2-5d 时提出）：`pfctl -a com.apple/250.abroad -sr` 的实际输出格式需要实测，而实测要 sudo，是用户的事。
  - 6b 里“锚点里的规则与期望一致”的比对，先写成可替换的接口，用假 pf 测；
  - 真实输出的样本在 M2-9 的用户脚本里采集；
  - 不要凭印象写死对 `-sr` 输出的解析。
- **带进 M2-7 的事**（architect 审 M2-5d 时提出）：不只是停用，**每次闸门由开变关**（出口检查不通过、换主代理、虚拟机重启）都要调用一次 `DoHUpstream.Close()`。这样会顺带取消没有等待者的在途拨号，也避免闸门重开后复用经旧节点建立的空闲连接。
- **带进 M2-7 的事**（architect 审 M2-5b、M2-5c 时提出）：停用第 ② 步的顺序是 关闸门 → `Proxy.DropAll()` → `DoHUpstream.Close()` → `wire.Dialer.Close()`。DoH 的连接不在 Proxy.flows 里，最后由 `wire.Dialer.Close()` 兜底。
  - yamux 没有公开的 reset，abort 在虚拟机那边看到的只是 FIN。虚拟机里的 proxyTCP 会让另一个方向再留最多 5 分钟（HalfCloseLinger）。宿主这边每条被 abort 的流最多还占 1 MiB 接收缓冲。
  - 关掉会话后，虚拟机的 serveSession 会立刻取消全部出站。
  - 这不是泄漏，而是资源占用和“停用后立即断干净”的问题。
- **带进 M2-9（R6）的事**（architect 审 M2-5b 时提出）：DNS 的 UDP 流和代理 UDP 流共用 MaxUDP=1024，每条 DNS 流占名额 5 秒（DNSIdle）。
  - 如果 mDNSResponder 每个查询都换源端口（待实测），持续约每秒 200 次查询就能占满。
  - R6 实测时看 `udp_rejected`，再决定是缩短 DNSIdle 还是给 DNS 单独配额。
- **带进 M2-7 的事**（architect 审 M2-5a 时提出）：
  - 配 utun 时，ifconfig 设的 MTU 要等于 `Device.MTU()`；
  - `Stack.Done()` 关闭（搬运 goroutine 意外退出）时当故障处理：重建 utun 和栈，期间闸门关闭。
- **带进 M3 的事**（architect 审 M2-5a 时提出）：UDP `udp_unsupported` 的负缓存；“拦截 UDP 443”选项。
- **带进 M3 的事**（architect 审 M2-4b 时提出）：
  - `Reverse`（真实地址 → 域名）只能用来把流量归到更严格的去向，不能作为判定“直连”的唯一依据。Claude 的服务如果和名单内的域名共用 CDN 地址，按 Reverse 判成名单域名再走直连，就是泄漏。
  - “真实 IP 目标 + Reverse 命中 + 规则为 direct”这条路径要单独审，默认仍走主代理。
- **带进 M2-9 或 M3 的事**：非 fake 段的 PTR 现在一律交主代理的 DoH，私有网段（如 192.168.x.x）的 PTR 也会发出去。到时决定是否改成本地回 NXDOMAIN。
- **带进 M2-7、M2-9 的事**（architect 审 M2-2 时提出）：
  - 睡眠唤醒或网络变化事件要调用 `physif.Watcher.Invalidate()`：macOS 睡眠期间单调时钟不走，唤醒后旧值的“年龄”偏小。不要改用墙上时钟。
  - `physif` 里 `selectPhysicalInterface` 的错误文案还写着“Cage 外网保持关闭”，等 M2-9 统一用 usererror 包装时再改。
  - `managedproxy.PhysicalInterface` 在 darwin 上现在会拒绝 bridge 等虚拟网卡，比 Cage 原来严，这是期望的行为。
- **带进 M2 的事**（都不阻塞开发）：
  - 真实远端节点验证，M2 验收前补；
  - R1 真机结果；
  - 真实本机 mihomo/Clash 的 UDP 中继实测（M2-3）。

## 需要用户人工处理

见本地文件 `docs/LOCAL.md`（不进版本库：含本机环境和账户信息）。

## 给 M1-4 的待办（architect 2026-11-18 审 M1-3 时提出）

- `/v1/health` 加 `boot_id`（serve 启动时随机生成）。
- 运行期巡检：连续 N 次不通就强停、走退避；`boot_id` 变了按“虚拟机重启过”处理。要避免睡眠唤醒的瞬间误判。
- Supervisor 提供“虚拟机已就绪”回调（每次 health 首次通过时触发），主代理配置和对时都挂在这里重新下发。
- 强停无效时不能再起第二台：`forceStop` 返回是否真的停了，没停就让 `Run` 返回错误；补 `stopOnForce=false` 的用例。
- M5 以系统用户运行前：`state.go` 打开锁文件和 pidfile 时加 `O_NOFOLLOW`。
- 以上前四条已在 M1-4a 完成。architect 审 M1-4a 时新增的提醒：
  - **M1-4d 对时**：定期对时挂在 OnReady 的 ctx 里；“唤醒后立即对时”要 monitor 暴露唤醒事件。客户机时钟在宿主睡眠期间不走，这条不能省。
  - **M1-6**：agent 收到 `main.yaml` 后做二次校验，作为 M1-6 的验收项。不能有 DIRECT 目标、`type: direct` 节点、tun、proxy-providers、rule-providers；mixed-port 和监听地址固定。原因：mihomo 的 DIRECT 会从虚拟机出站走真实 IP。
  - **M2 真机验证**：睡眠唤醒后对时是否真的触发；睡眠唤醒后 VZ 和 vsock 的表现；`ErrStuck` 退出后，在 launchd KeepAlive 下旧虚拟机是否随进程消失。

- architect 审 M1-4c 时的提醒：
  - **真实 sing-box 的行为还没验证**：假 socks 服务证明不了 UDP ASSOCIATE 的 BND 地址形式、UDP 回包来源的形式（是否映射地址、是否域名），也证明不了 CONNECT 是在出站接通后才应答还是先应答。由 M1-4e 在真机上验证。
  - **已知限制**：yamux 没有 RST，出站异常中断时宿主看到的是正常 EOF，要靠 TLS 等上层协议发现截断。M3 写宿主转发时要考虑这一点。

- **M3 待办**（architect 2026-11-18）：
  - 转发层照实透传：上游 EOF 就对应用正常关闭（FIN），不改成 RST，不重试，不换出口。
  - 单条连接的零字节 EOF 不计入出口健康和闸门，不能触发关闸门、断网或结束 Claude。出口是否可用只由 `exit_down`、`dial_failed` 和第 8 节的出口检查判断。
  - 出口检查必须端到端（给 M4）：以“收到了预期的 HTTP 应答”为准，不能以回应帧 ok 或 CONNECT 成功为准。
  - 可选（M3 时再定）：data 段加“ok 后零字节关闭”的计数，只用于诊断页提示。
- **M1-6 待办**（architect 审 M1-5a 时提出）：回环白名单需要“网关虚拟地址 → 127.0.0.1”的 NAT 映射，只在白名单里有回环项时才加；`PhysicalInterface` 以 `_abroad` 身份执行 networksetup、scutil 是否可用，并入 R1 的真机验证。
- **M2 待办**（architect 审 M1-5b 时提出）：
  - `managedproxy.PhysicalInterface` 每次查询起 3 个子进程，有流量时每 2 秒查一次，查询期间所有拨号都在等。改成“先用旧结果、后台刷新”或事件驱动。旧网卡仍是物理网卡，所以不构成泄漏。
  - abroad-vm 为了一个函数 import 了整个 managedproxy。把 `PhysicalInterface` 挪到独立的小包。
- **M2 待办**（architect 审 M1-6b 时提出）：主代理变化导致白名单变化时，重启 abroad-vm；重启期间闸门本来就是关的。
- **M6 待办**（architect 审 M2-1a 时提出）：
  - 发布构建里不能带 `--outbound-open`，用构建标签去掉，或启动时拒绝；
  - M2-6 开工时，把 design 6.1 的“监听 SystemConfiguration 通知”改成 M2 用轮询。
- **M5 待办**：“用户确认”局域网或本机的主代理节点。`VMRuntime` 只负责算出需要的白名单，abroadd 只能把用户确认过的节点的白名单传给 abroad-vm。
- **M6 待办**：发版前从干净的上游 tag 依次应用补丁 0001–0006，与 third_party 做 `diff -r`，写成 `scripts/dev/` 下的脚本。
- **M1-6c 待办**：VMRuntime 把服务器为 127.0.0.1 的节点改写成 HostLoopbackIP（192.168.127.254），abroad-vm 的白名单随主代理配置一起确定。
- **M1-6c 待办**（architect 审 M1-6a 计划时提出）：
  - 阶段 C 恢复经 mihomo 到回环的 TCP、UDP 正例（原 4e-2 的 d、g），并把 UDP 出站的真机覆盖补回来（4f 现在只测 TCP）；
  - 加一条 UDP 域名目标的探测：经开发上游（socks5 UDP）发往域名目标，前后读 netstats，断言没有新增指向 1.1.1.1、223.5.5.5 的直连拨号。

- **M2 待办**（architect 审 M1-6c-2 时提出）：
  - abroad-vm 拨号前的钩子（BeforeDial）把虚拟机出站收紧为两类地址：引擎 DoH 用的 IP（1.1.1.1、223.5.5.5），以及路线第一跳的地址。
  - 这样就算哪个引擎隐式退回直连，目标也到不了（设计 R20）。
- **M2 之前的待办**（architect 审 M1-6c-2 时提出）：用真实的本机 mihomo/Clash 当主代理，实测 UDP 中继经 192.168.127.254 能不能用（设计 4.5）。通过后再决定是否对用户放开“第一跳本机回环也转 UDP”（现在只有 `abroad-dev --loopback-udp` 能打开）。
- **M4 备注**（architect 审 M1-6c-2 时提出）：出口检查里的 UDP 探测以“收到应答”为准。回应帧 ok 不算数：路线不转 UDP 时，数据面照样回 ok，UDP 被 mihomo 的 `MATCH,REJECT` 静默丢弃。

## 已做的决定

- **2026-11-20 用户本人决定：Windows 版与 Mac 版现在就一起写代码**。用户原话：“能不能同时做windows版本”；主会话说明代价后给了三个选项（“先 Mac，同时写设计（推荐）”“现在就两边一起写代码”“Mac 做完再说”），用户选了“现在就两边一起写代码”。用户已知：这台 Mac 上验证不了 Windows 的系统层，需要用户另外提供 Windows 机器做验证；Mac 版完成时间会明显后延。design 第 1、2 节的改动留到 W-0。
  - architect 的排法：先做完 M4-6b → W-0（设计文档、风险清单、可移植性清点，不改代码）→ 之后“一个 Mac 任务、一个 Windows 任务”轮流：M4-7 → W-1（共用包能交叉编译到 Windows）→ M4-8 → W-2（WR1–WR3 的探查程序和用户脚本）→ M4-9 → W-3 及以后按探查结论排。某一边在等用户真机结果时另一边连续做。从 W-1 起每个任务的验收都加 `GOOS=windows GOARCH=amd64 go build ./...` 和 `go vet`。

- 2026-11-18 architect 决定 M1-6 的做法：
  - 先做二次校验（6a）：虚拟机有了真实出站之后，`MATCH,DIRECT`、`mode: direct`、空规则、IP 类规则都会从真实 IP 出去，或者在虚拟机里解析目标域名。所以主代理的规则只能是一条 `MATCH,<出口>`，按白名单校验。
  - 阶段 B、C 用本机回环上的开发用上游跑（6b、6c），不等用户提供节点；真实远端节点的验证在 M2 验收前补上。

- 2026-11-18 M1-5b 真机实测（证据 `scripts/dev/out/stage-a-20261118-135116.txt`）：
  - 虚拟机经用户态网络、出站策略和绑定物理网卡（en0）的拨号器出站，UDP、TCP 的 DNS 查询都成功（223.5.5.5）；网关 DNS 回 REFUSED。
  - 四个负例的 dial_ok 增量都是 0：10.0.0.1、网关 192.168.127.1:80、局域网地址都被策略拒绝，每项 policy_denied +10，因为 mihomo 重试了 10 次；169.254.169.254 的 policy_denied +0，它在策略检查之前就被丢弃。与 architect 的期望表一致。
  - 重启后出站仍然可用。health 开机后约 2 秒可达，因为不用再等网卡的 5 秒。

- 2026-11-18 M1-4e 真机实测（sing-box 1.14.4 musl、mihomo 1.19.34；证据 `scripts/dev/out/stage-a-20261118-131622.txt`）。M1-4c 留下的三个问题：
  - **UDP ASSOCIATE 的 BND 形式**：我们的 socks 客户端能处理真实 sing-box 回的形式，UDP 往返成功。
  - **UDP 回包来源**：没有被来源过滤误丢，目标写成 `::ffff:127.0.0.1` 也一样；两次探测前后 `udp_dropped` 都没变。
  - **CONNECT 的应答时机**：这条链路是**先应答、后拨号**。拨不通的 `127.0.0.1:9` 和被 `MATCH,REJECT` 的 `203.0.113.1:443`，结果都是 `ok` 之后零字节 EOF，而不是 `dial_failed`。REJECT 是立即 EOF；拨不通的要等约 2.5 秒才 EOF，因为 mihomo 内部重试了 10 次。所以没法用“是否立即 EOF”来判断。
- 2026-11-18 architect 决定（CONNECT 先应答）：原因在 mihomo，它的 socks 握手在拨号前就写出成功应答（核对过源码），而且没有配置项能改。即使 mihomo 肯等，ss、vmess、trojan 这类协议本身也不回传远端的连接结果。所以不绕开、不打补丁，改写设计 4.4 的语义：`ok` 只表示请求已交给出口引擎；`dial_failed` 只覆盖虚拟机内这一段。5.3 补一条已知限制。错误码表和代码都不变。

- 2026-11-18 M1-4b 真机发现：sing-box 1.14.4 的 `linux-arm64` 包是 glibc 动态链接的（解释器 `/lib/ld-linux-aarch64.so.1`），在 Alpine（musl）上报 `fork/exec … no such file or directory`。清单改用同版本的 `sing-box-1.14.4-linux-arm64-musl.tar.gz`（静态链接；SHA-256 b95e25f9…，取自 GitHub 发布接口的 digest 字段）。verify 新增检查：引擎的解释器和依赖库必须在镜像里。mihomo 本来就是静态链接。

- 2026-11-17 abroad-architect 审 M1-2a：
  - 三处偏离（自写 squashfs、内核与模块来源、SHA-256）都同意。它对照内核源码逐项核对了 squashfs 格式，没有发现错误；4 个 SHA-256 也独立核对无误。
  - 必须改 1 条已改：netboot 包里没有 `boot/config-virt`（实际文件名带版本号），已从需要的文件列表删掉。
  - 建议改 1、3、4、5、6 已改：
    - 按字节检查内核挂载约束的测试，含 128K/256K 边界文件和 320 个子目录；
    - 下载改为空闲 60 秒超时，不设总超时；
    - 缺文件时列出包里的 `-virt` 文件；
    - 符号链接目标不超过 4095 字节；
    - 删掉死代码。
  - 建议改 2（加独立的 squashfs 读取库做交叉回读）**没做**：本机经两个 Go 模块代理下载依赖都很慢，下不动 go-diskfs 已经验证过一次；格式正确性由 architect 的源码核对和阶段 A 实测保证。
- 2026-11-17 M1-2a 的实现选择：
  - **squashfs 自写**（`internal/imagebuild/squashfs.go`），不用 go-diskfs。原因：
    - go-diskfs 的源码包很大，经 goproxy.cn 和 proxy.golang.org 都下不下来；
    - 它从宿主目录取文件，属主跟随宿主。
  - 自写版只覆盖需要的部分：basic 目录、文件、符号链接 inode；数据 zlib 压缩；无分片、无 xattr；元数据不压缩。
  - 回读测试用的也是自写读取器，只能证明写读一致。本机没有能独立读 squashfs 的工具，内核兼容性要靠架构审核对照内核源码，以及用户的阶段 A 实测。
  - **内核和模块的来源**：netboot 3.22.8。
    - 启动模块（virtio_blk、squashfs）从 netboot 的 `initramfs-virt` 里取，并解压成 `.ko`；
    - 其余模块由 agent 从 `modloop-virt` 加载，它作为第二块只读盘挂上。
    - 构建时不需要 squashfs 读取器。
  - **SHA-256 来源**：
    - Alpine 两个文件取自官方 `.sha256`；
    - sing-box 1.14.4、mihomo 1.19.34 取自 GitHub 发布接口的 `digest` 字段，经网页抓取工具转述，可能抄错；
    - 若有误，构建时会报"校验不符"并停止，不会放过错误的文件。

- 2026-11-17 abroad-architect 审 M1-1：
  - 必须改 2 条已改：handshake 的 ctx 取消竞态；design 4.5 删去"改用本机原 DNS"。
  - 建议改 3、4、6、7、8、9 已改：yamux 开流超时与握手超时分开；StreamCloseTimeout 可配；host 拒绝 IP 字面量；App 拒绝控制字符；ErrDatagramTooLarge；补断言和用例。
  - 建议改 5（给 TCP 转发层的 CloseWrite 包装）留到 M1-4。
  - 给 M1-4 的提醒：agent 经出口拨号的超时（8 秒）要短于宿主的 OpenTimeout（10 秒）。
- 2026-11-17 abroad-architect 审 M1 计划：D1–D5 接受（纯 Go 构建镜像、agent 作 PID 1、tmpfs overlay 且 M1 不做数据盘、开发模式、补丁 0006），8 处必须改已写进 m1-plan v2 并回写 design v0.4。"建议改"全部采纳。
- 2026-11-17 用户决定：付费项推迟；M0 并入 M1（R1 并入 M1，R9/R18 移到 M6，R6 分段移到 M2）。
- 2026-11-17 abroad-architect 审 M0 计划（仍然有效的部分）：
  - fake-IP 段冲突不换地址段，记为风险 R18；做解析锁时加测"回环地址 + 端口"写法。
  - 正式 pf 锚点倾向改成排在最前的名字（如 `com.apple/100.abroad`），等 R2 实测后回写 6.1。
  - R2 结论要注明"未在有接管路由的情况下验证"，留到 M2。

## 会话日志

见本地文件 `docs/LOCAL.md`。
