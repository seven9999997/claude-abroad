# Claude 出国（Mac 版）最终验收清单（M6-10）

> 状态：v2（2026-11-27），按当前代码逐条重写，待 architect 审定。M6 的开发任务和补审都已完成。**用户的真机脚本一个都还没有执行**。本清单里“真机”只指用户执行脚本得到的结果；会话里做过的只有不改宿主网络的开发验证（在这台 Mac 上起虚拟机、只读查看本机状态），这类结论都写明“会话里”。
> 真机结论由用户执行脚本后贴回，由 architect 逐条判定，回填到第 9 节和各节的“结论”一栏，并写进设计第 15 节对应的风险行。
> 本清单引用的自动测试、脚本步骤和文件都由 `scripts/m6/check-acceptance.sh` 核对存在（它的反例在 `scripts/m6/e2e/selftest.sh` 里）。

结论的五种写法：

- **达到**：已实现，会话里的自动测试覆盖了。
- **达到替代程度**：用户决定不购买付费开发者账号（2026-11-27），用替代做法实现，这是这一版的最终状态；写明弱在哪里、以后如果购买账号从哪里接上。
- **待真机**：已实现，但是否有效只能在真机上确认；写明哪个脚本的哪一步、贴回什么。
- **这一版不做**：写明原因（因为没有付费开发者账号，或别的原因）和去向。
- **没有做**：这一版没有做，也不在 P1–P8 里；写明原因。

引用的写法：`go:<包目录>#<测试名>`、`swift:<测试函数名>`、`script:<脚本路径>#<步骤编号>`、`file:<路径>`。

## 1. 设计第 14 节 M6 的完成标准

| # | 完成标准 | 结论 | 依据 | 弱在哪里；这一版到此为止（以后如果购买账号从哪里接上） |
|---|---|---|---|---|
| 1 | 在全新的机器上拖入“应用程序”即可安装、使用 | **达到替代程度** + **待真机** | 开发安装脚本代替 DMG 和 SMAppService：`script:scripts/m6/install/install.sh#M6-11b`（安装后只读核对首次准备、完整性、回国拦截），`script:scripts/m6/install/selftest.sh#M6-11b`（在临时目录里真装真卸）；端到端 `script:scripts/m6/e2e/run.sh#I1` | 要从源码构建、要 sudo 执行脚本，非开发者用不了；只能装在执行脚本的那台 Mac 上；没有签名的更新通道（重装即升级）。这一版到此为止；以后如果购买账号，按 P1–P3 接上（安装位置和后台服务注册的接口见 m6-plan 第 8 节） |
| 2 | 回国后 Claude 连不上 | **达到**（机制；每一道各自拦不住什么见第 4 节）+ **待真机**（对各程序是否有效） | 五道管控见第 4 节。第一道 `go:internal/resolverlock#TestWriteIdempotent`；第四道（v0.68 起可选、默认关，打开之后）`go:internal/daemon#TestClaudeProxyStateTable`；第五道 `go:internal/state#TestReturnBlockNoGap`、`go:internal/daemon#TestReturnBlockSwitch`；启用、停用、回国全流程不碰解析锁 `go:internal/daemon#TestResolverLockInDaemon`；回国时结束不了 Claude 就中止、什么都不改 `go:internal/daemon#TestRestoreLeftovers` | 解析锁对自带加密 DNS 的程序、开着别的代理软件时无效（R9、R18，`scripts/m6/resolver`）；第五道本机另有代理软件接管时拦不住；Anthropic 新增地址段要等新版本。对真实地址段的效果只能用发布构建验证（第 7 节“没有做”） |
| 3 | 回国后 Claude 被打开后马上结束 | **达到替代程度** + **待真机** | 第三道：`go:internal/daemon#TestLaunchGuardRuleA`、`go:internal/claudeproc#TestGuardTermThenKill`；开发构建只针对测试目录 `go:internal/claudeproc#TestFakeDirGuard`；端到端 `script:scripts/m6/e2e/run.sh#L1` | 没有 Endpoint Security（P5），不能在程序运行之前拒绝：每 200 毫秒扫描、发现即结束，Claude 可能已经运行零点几秒（这一版的最终状态：Endpoint Security 要付费账号，这一版不做）；只结束签名确认过的进程，npm、Homebrew 装的 Claude Code 第三道不覆盖（靠第一、五道，第四道打开时还有第四道；v0.68 起第四道默认关）。端到端的开发构建只验证机制，对真实 Claude 的效果要发布构建（第 7 节） |
| 4 | 一键卸载后恢复正常并完全清理 | **达到** + **待真机** | `go:internal/daemon#TestUninstallRestoresEverything`、`go:internal/daemon#TestUninstallFromEachState`、`go:internal/daemon#TestReturnBlockUninstall`（三种起点卸载后锚点空、引用全部释放、pf 回到安装前）、`go:internal/daemon#TestUninstalledRejectsChanges`；命令行 `go:cmd/abroad#TestCLIUninstall`；开发安装的卸载和快照对照 `script:scripts/m6/e2e/run.sh#U1` | 第 3 步“注销后台服务、删除专用用户和数据”由安装器完成（现在是开发安装的卸载脚本）；快照对照里 pf 是否启用那一行的格式没在真机核对过，认不出时记“无法判断”、不参与对照；卸载后，Claude 出国的 pf 启用引用和锚点规则都不留下；pf 是否启用还受系统和别的软件持有的引用影响。卸载对照时，如果安装前没启用、卸载后启用，而启用引用都属于别的程序、我们的锚点是空的，这不算不一致，会写一条说明（2026-11-27 真机，设计 v0.70a） |
| 5 | 未签名的脚本调用回国接口被拒绝 | **达到替代程度** | `go:internal/api#TestClassifyAndAllowed`、`go:internal/daemon#TestSocketSecurity`；三句话见下 | 见下 |

**“未签名的脚本调用回国接口被拒绝”**（三句话）：

- **达到的**：回国是“敏感”级别。控制台用户以及它运行的任何脚本（包括 npm 包、Claude Code 自己）调用回国接口一律被拒（403），要本人授权：应用里目前给出一条要在终端执行的命令、由你自己去执行（系统授权等 M5-3 的 E1、E2 结论，见第 6 节），命令行必须 `sudo`。卸载同样是敏感级别。
- **没达到的**：没有“签名”这个维度，调用方签名校验现在是明确标着“未启用”的实现；以 root 运行的脚本可以直接调用。普通级别的操作（启用、停用、重新检测）本机控制台用户的任何程序都能做，这一点写在 status 的 `security` 字段、应用“设置 → 高级”和 `abroad status` 里。
- **这一版到此为止**：用户决定不购买付费开发者账号（2026-11-27），调用方签名校验（P4）这一版不做，“未启用”的实现和接口保留；临时加严成敏感级的操作保持敏感级。以后如果购买账号，从调用方校验的接口接上，再决定这些操作是否放回普通级。

第 14 节 M6 一行里的打包内容：`.app` 打包（达到，`file:scripts/dev/app-bundle.sh`，临时签名）；Developer ID 签名与公证、DMG、SMAppService（这一版不做 P1–P3，因为没有付费账号）；Claude 域名解析锁、启动管控、一键卸载、另存的应急命令行、应急恢复文档（达到，见第 4 节和 `file:docs/emergency.md`）；Endpoint Security（这一版不做 P5，因为没有付费账号）。

## 2. 设计第 1 节的目标

| 编号 | 目标（简称） | 结论 | 依据 |
|---|---|---|---|
| G1 | 本机所有流量经 Claude 出国，没有绕过的直连 | **达到** + **待真机** | pf 默认拦截全部出站 `go:internal/pf#TestRenderGolden`；状态机全程不泄漏的不变量 `go:internal/state#TestCrashExhaustive`；真机：M2、M3 脚本 |
| G2 | 服务停止、崩溃、停用后断网，只有“恢复本机网络”才回到直连 | **达到** + **待真机** | `go:internal/state#TestRebootExhaustive`、开机时的 guard `go:internal/guard#TestLoadForActiveStates`；真机：M2 脚本、端到端 `script:scripts/m6/e2e/run.sh#B1` |
| G3 | 主代理（订阅、本地配置、手动节点、链式） | **达到** + **待真机** | M5 走查 `file:docs/m5-walkthrough.md`；真机：M2、M5 第二级 |
| G4 | 分流出口（OpenVPN、WireGuard、mihomo、分享链接） | **达到**（内置引擎）+ **待真机**；引擎的安装与回滚**这一版不做**（P7：没有可信的更新通道） | 分享链接 `go:internal/exitconf#TestShareLinksUnknownParams`（不认识的参数一律拒绝）；真机：M4 脚本。六种分享链接的字段映射待真机（第 7 节） |
| G4.1 | 按应用分流；Anthropic 域名永远不走本机网络 | **达到** + **待真机** | 置顶规则 `go:internal/rules#TestPinnedBuiltin`；真机：M3 脚本 |
| G5 | 出口检查通过才放行；时区和语言跟随出口，回国还原 | **达到** + **待真机** | `go:internal/gatesm#TestGateProperty`；真机：M4 脚本、端到端 `script:scripts/m6/e2e/run.sh#R1` |
| G6 | 一键启用、停用、回国、切换地区；回国二次确认并先结束 Claude | **达到**（流程和顺序）+ **待真机**（真实结束 Claude） | `go:internal/daemon#TestRestoreTwoStep`、`go:internal/state#TestRestoreKillFails`（会话里一律是假的进程表和信号发送器）。真实的信号发送器从来没有运行过：用户的脚本默认只预览、不发信号；只有 M4 脚本显式设 `M4_REAL_KILL=1`，或者用发布构建时，才会真的结束本机的 Claude（第 7 节“没有做”） |
| G7 | Mac 应用和命令行 | **达到替代程度** + **待真机** | 应用临时签名、开发安装（S1）；M5 走查 |
| G8 | 出国后的环境检测 17 项 | **达到** + **待真机** | M4、M5 第二级 |
| G9 | 启动管控：回国后 Claude 连不上、不允许启动，只有卸载才解除 | 同第 1 节第 2、3 行 | 第 4 节 |
| G9.1 | 回国后没被拦住的 Claude Code 流量不从本机 IP 出去（**v0.68 起可选，默认关**：用户 2026-11-27“caibao这个兜底做个开关吧，默认不开启”） | **达到**（机制，只有自动测试）；**真实效果暂不测（用户 2026-11-27 决定）** | 默认关：首次准备不写托管设置、端口不监听、冲突检查里没有它的两项 `go:internal/daemon#TestClaudeProxyDefaultOff`；打开、关闭、三步回滚、卸载、重启后保持 `go:internal/daemon#TestClaudeProxyTurnOn`、`go:internal/daemon#TestClaudeProxyTurnOnRollback`、`go:internal/daemon#TestClaudeProxyTurnOff`、`go:internal/daemon#TestClaudeProxyUninstall`、`go:internal/daemon#TestClaudeProxyPersistsAcrossRestart`；命令行 `go:cmd/abroad#TestCLIClaudeProxy`；应用 `swift:claudeCodeOffByDefault`。打开之后，第四道：托管设置指向本机代理端口，回国时一律 403 `go:internal/daemon#TestClaudeProxyStateTable`。依赖两个没有在真机上验证过的前提（都来自 Claude Code 官方文档的描述）：① Claude Code 会采用托管设置里的代理变量（R21）；② 代理不通或被拒时它不回退直连（R22）。补测时用 `script:scripts/m6/layer4/run.sh#T2a` |
| G9.2 | 回国后按 Anthropic 地址段拦截 | **达到**（机制）+ **待真机** | 第五道：`go:internal/state#TestCrashExhaustiveReturnBlock`、`go:internal/daemon#TestReturnBlockGuardAdoptedAfterReboot`；开发构建只拦测试段 `go:internal/daemon#TestReturnBlockSetFromPrepare`；真机（测试段）：`script:scripts/m6/e2e/run.sh#RB1`、`script:scripts/m6/e2e/run.sh#RB3` |
| G10 | 一键卸载 | 同第 1 节第 4 行 | — |

**“回国干净”的改写**（设计 v0.56）：回国恢复的是路由、DNS、IPv6、时区、语言（`go:internal/state#TestNormalCycle`）；启动管控——解析锁、托管设置（第四道打开时才有；v0.68 起默认关）、第三道、回国拦截的 pf 规则和一个 pf 启用引用——回国不碰，一直保留，只有一键卸载才解除（`go:internal/daemon#TestReturnBlockUninstall`）。回国后 pf 保持启用。

## 3. 五条底线

| 底线 | 靠什么保证 | 自动测试 | 待真机 |
|---|---|---|---|
| **不泄漏**（含 DNS、UDP、IPv6、崩溃和重启的间隙） | pf 默认拦截全部出站，只放行回环、utun、`_abroad`、DHCP、局域网；IPv6 逐个网络服务关闭；域名置顶；开机时 guard 在 abroadd 之前加载断网规则 | `go:internal/state#TestCrashExhaustive`、`go:internal/state#TestRebootExhaustive`（每一步崩溃、重启后的不变量含“有接管痕迹时 pf 拦着公网”）、`go:internal/guard#TestRaceWithAbroadd` | M2 脚本；端到端 B1 |
| **失败即断网** | pf 规则不随进程消失；闸门；完整性校验不过时只保留断网规则、不接管 | `go:internal/daemon#TestIntegrityFails`、`go:internal/daemon#TestCorruptStart` | M2 脚本（kill -9） |
| **回国干净**（按上面的改写） | 恢复流程逐项还原；回国拦截换组没有空窗；卸载解除全部管控 | `go:internal/state#TestRestoreFromEachState`、`go:internal/state#TestReturnBlockNoGap`、`go:internal/daemon#TestUninstallRestoresEverything` | 端到端 U1、R1 |
| **不误杀** | 只结束签名确认过的 Claude 进程；“无法判断”不关闸门、不结束 | `go:internal/claudeproc#TestAutoJudgeNegatives`、`go:internal/claudeproc#TestKillNeverSelfOrInit`、`go:internal/gatesm#TestRecheckResults` | M4 脚本；端到端 L1（只针对测试目录） |
| **敏感操作要本人授权** | 对端 uid 三级权限；回国、卸载、改主代理等是敏感级别；应用里目前给出终端命令（系统授权等 M5-3） | `go:internal/api#TestClassifyAndAllowed`、`go:internal/daemon#TestSocketSecurity` | M5 第二级、M5-3 的 E1/E2 |

## 4. 启动管控的五道

| 道 | 做什么 | 覆盖 | 已知拦不住 | 开发构建下针对什么 | 自动测试 | 真机 |
|---|---|---|---|---|---|---|
| 一 解析锁 | `/etc/resolver` 下的文件把内置的 Claude 域名（`claude.ai`、`claude.com`、`anthropic.com`、`claudeusercontent.com`、`claudemcpcontent.com`）交给一个不应答的解析地址（解析失败）；首次准备写入，只有卸载删除 | 用系统解析器的程序 | 自带加密 DNS 的浏览器；开着别的代理软件（R9、R18）；用户追加的置顶域名不在锁里 | 只锁测试域名 `lock-test.claude-abroad.invalid` | `go:internal/resolverlock#TestDomainsFromBuiltinPinned`、`go:internal/resolverlock#TestBackfill` | `scripts/m6/resolver` |
| 二 启动拦截（Endpoint Security） | 程序运行之前拒绝 | — | 没有这一道 | — | — | 这一版不做（P5，没有付费账号） |
| 三 发现即结束 | 每 200 毫秒扫描，只结束签名确认过的 Claude（口径 A–D） | Claude 桌面版、原生安装的 Claude Code | 运行零点几秒内；npm、Homebrew 装的 Claude Code；扫描失败时不结束 | 只结束安装目录 `fake-claude` 里的测试进程 | `go:internal/daemon#TestLaunchGuardRuleB`、`go:internal/claudeproc#TestCoverage`；拦下时的通知 `swift:launchBlockedNotifications`；“打开 Claude”只在已启用且检测通过时可点 `swift:openClaudeConditions` | 端到端 L1 |
| 四 Claude Code 的本机代理（**可选，默认关**，v0.68） | 关着时什么都不写、不监听（这一道不起作用，回国后靠第一、三、五道）；打开（`sudo abroad claude-proxy on`）后托管设置把 Claude Code 指向 `127.0.0.1:47821`；已启用时按规则转发，其余一律 403，不回退直连 | Claude Code（读托管设置的版本） | Claude 桌面版和浏览器；Claude Code 不读托管设置或绕开代理时（R21、R22）。这两个前提有任何一个不成立，这一道对相应的情况就不起作用，那时靠第一、三、五道；两条暂不测（用户 2026-11-27 决定） | 打开也只监听、不写托管设置（测试模式）`go:internal/daemon#TestClaudeProxyTestMode` | `go:internal/daemon#TestClaudeProxyRules`、`go:internal/claudecfg#TestWriteRemove`；默认关 `go:internal/daemon#TestClaudeProxyDefaultOff`；覆盖范围按开关生成 `go:internal/claudeproc#TestCoverage` | `scripts/m6/layer4`（T0–T5，暂不测）；R23 在端到端 CC1 |
| 五 回国拦截 | 回国后 pf 锚点里只留“立即拒绝到 Anthropic 登记地址段”的两行，留一个 pf 启用引用；装上即生效，只有卸载才解除 | 本机任何程序（包括浏览器）直连这些地址 | 本机另有代理软件接管时（出口是那个代理）；Anthropic 新增地址段；共用 CDN 的域名（不和域名名单联动） | 只拦测试段 `192.0.2.0/24`、`2001:db8:abd::/48`（文档保留地址）；选择文件读不出时不加载 | `go:internal/guard#TestRestoredReturnBlockTable`、`go:internal/rescue#TestReturnBlockKept`、`go:internal/pf#TestPatrolRestoredVerifyNoReloadLoop`、`go:internal/srccheck#TestReturnBlockTestNetsDevOnly`；界面 `swift:returnBlockInSettings`、`swift:returnBlockConfirmAndHome`、`swift:returnBlockTestTag` | 端到端 RB1、RB2、B3、RB3、K1 |

**没打开管控时**（abroadd 不带 `--launch-control`，M2–M5 的真机脚本就是这样）：不做首次准备，不写解析锁和托管设置，不接第三道，不监听本机代理端口，回国拦截不生效；回国就是清空锚点、释放全部启用引用（`go:internal/daemon#TestLaunchControlOff`、`go:internal/daemon#TestPrepareOffByDefault`、`go:internal/state#TestReturnBlockOffUnchanged`；应用设置页显示“未启用” `swift:settingsLaunchControlOff`）。卸载第 2 步写“这次安装没有打开启动管控，不需要解除”（`go:internal/daemon#TestUninstallWithoutLaunchControl`）。

**卸载之后还能做什么**：只读的查询照常；改动类接口只允许回国、再次卸载、设备体检、清空 Claude 访问过的域名，其余回 `uninstalled`（409，“已经一键卸载，要继续使用请重新安装”）（`go:internal/daemon#TestUninstalledRejectsChanges`）。卸载后巡检、guard、应急程序都不再加载回国拦截。

**诊断包里有什么**（`GET /v1/diagnostics`）：版本、状态（事件、闸门、出口摘要等，不含出口 IP、订阅地址、节点地址）、最近的环境检测、启动管控、Claude 访问过的域名的**条数**（用户显式选择 `include_domains=1` 才带清单）；最近经本机代理端口的连接默认去掉目的域名；所有 `协议://…` 整段换成占位文字；时间一律 UTC；最后再统一脱敏（`go:internal/daemon#TestDiagnosticsNoHostnamesRealFlows`、`go:internal/daemon#TestDiagnosticsURLAndUTC`、`go:internal/daemon#TestClaudeDomainsRecorded`）。

**应急**：后台服务不在或状态文件损坏时用应急程序 `abroad-rescue`（先结束 Claude，再写“已回国”，打开了管控时第 4 步换成回国拦截、不是清空）；手动步骤见 `file:docs/emergency.md`（含只去掉回国拦截的办法）。依据 `go:internal/rescue#TestOrder`、`go:internal/rescue#TestRefusals`；真机 `script:scripts/m6/e2e/run.sh#E1`。

## 5. 设计第 15 节的风险

| # | 风险（简称） | 状态 | 去向 |
|---|---|---|---|
| R1 | 虚拟机能否在后台服务里以 `_abroad` 运行 | **暂不测（用户 2026-11-27 决定）** | R1 脚本。这三份验证的是“启用之后会不会整机断网且恢复不了”；不测，答案只能在第一次真的点“启用”时揭晓，而那时没有自动恢复（断网时依次用应用里的“一键回国”、`sudo abroad restore`、`sudo abroad-rescue`，都不需要网络）。用户在装好的开发版里做了“应用主代理”或“启用”时，那次的状态、事件、诊断包可以作为部分证据，由 architect 判定算到什么程度；收尾时由用户决定补测还是定为不测。开发版里第一次“应用主代理”就是这种运行方式（会拉起虚拟机） |
| R2 | pf 的 `user` 规则能否匹配虚拟机和直连拨号的连接 | **暂不测（用户 2026-11-27 决定）** | M0 的 R2 脚本。这三份验证的是“启用之后会不会整机断网且恢复不了”；不测，答案只能在第一次真的点“启用”时揭晓，而那时没有自动恢复（断网时依次用应用里的“一键回国”、`sudo abroad restore`、`sudo abroad-rescue`，都不需要网络）。用户在装好的开发版里做了“应用主代理”或“启用”时，那次的状态、事件、诊断包可以作为部分证据，由 architect 判定算到什么程度；收尾时由用户决定补测还是定为不测 |
| R3 | 查连接表识别进程的准确率 | **部分结论** | 普通用户只能看到自己的套接字（M3-3）；root 下的可见性待 R3 脚本 |
| R4 | 改时区、语言的效果和地区格式写法 | **待真机** | M4 脚本 |
| R5 | OpenVPN 改写配置后在 netns 里运行 | **有结论：可行**（M4-1，会话里在虚拟机内验证） | 真实服务商配置的效果待 M4 脚本 |
| R6 | 全链路的吞吐、延迟、CPU | **待真机** | M2 的分段测量 |
| R7 | 系统服务在断网规则下的表现 | **待真机** | 启用期间看系统日志；“系统服务直连”预设没有做（第 7 节） |
| R8 | 后台服务的开机时机 | SMAppService **不适用：这一版不做**（没有付费账号）；开发用 LaunchDaemon 的开机时机 **待真机** | 端到端 B1（启用状态重启）、B3（已回国状态重启） |
| R9 | 解析锁对桌面版、Claude Code、浏览器是否有效 | **待真机** | `scripts/m6/resolver`；拦不住的由第五道补（本机另有代理软件时除外） |
| R9.1 | 能否拿到 Endpoint Security 授权 | **不适用：这一版不做** | 要付费账号才能申请；用户 2026-11-27 决定不购买 |
| R10 | 桌面版界面语言存在哪里 | **有调查结论**（2026-11-20，会话里只读查看本机文件） | 设计第 15 节 R10 行 |
| R11 | 是否改用 Network Extension | **书面对比，维持现方案** | 没有授权，未实测 |
| R12 | 程序被替换 | **达到替代程度** | SHA-256 清单（`go:internal/integrity#TestCheckProblems`）；abroadd、guard 自己不在清单里；运行中被替换不会发现。有签名后改用系统校验（P6） |
| R13 | 名称带“Claude”对公证的影响 | **不适用：这一版不做** | 不公证（用户 2026-11-27 决定不购买付费账号）；以后如果公证，提交时观察 |
| R14 | Anthropic 是否使用专用 IP 段 | **有结论**（单一观测点，会话里查询解析结果和 ARIN 登记） | 置顶规则以域名为主；第五道按 ARIN 登记的两段拦（2026-11-26 查询），新增地址段要等新版本 |
| R15 | Docker、OrbStack、Parallels、UTM 的流量 | **待真机** | 没有专门的脚本，启用后在各软件里访问外网，结果写进已知限制 |
| R16 | 公共 Wi-Fi 认证页 | **待真机** | “仅认证”模式没有做（第 7 节：先一键回国完成认证再启用） |
| R17 | 各种方式安装的 Claude Code 是否都带签名 | **部分结论**（会话里只读查看本机原生安装的签名：有 Team ID） | npm、Homebrew 第三道不覆盖；其余安装方式的抽查交用户 |
| R18 | 有别的代理软件时解析锁失效 | **待真机** | `scripts/m6/resolver` 里有 Clash 时多看一步 |
| R19 | 纯 Go 镜像构建是否够用 | **有结论：够用**（M4-1，会话里构建镜像并起虚拟机） | 发版物料存档了 24 个 Alpine 软件包和 4 个上游文件，SHA-256 核对一致（M6-8c） |
| R20 | 引擎在出口不可用时退回直连 | **有结论，已修复**（M1-6c-2：会话里用开发脚本在这台 Mac 上起真实虚拟机复现了 mihomo 的 UDP 隐式直连并修复；出站收紧到路线第一跳兜底）；**用户的真机脚本尚未执行** | M2、M4 的真机脚本里经出口发 UDP 再核对；每次升级引擎版本、新增出口类型时重跑 |
| R21 | 托管设置的代理变量能否盖过启动环境 | **暂不测（用户 2026-11-27 决定）** | v0.68 起第四道默认关：关着时这一条不影响任何人，只有打开了第四道的人才受它影响。只能通过写入真实的托管设置来验证，写入期间这台 Mac 上所有 Claude Code 都改连本机代理端口，会影响平时的用法，所以推后；收尾时由用户决定补测还是定为不测。补测用 `script:scripts/m6/layer4/run.sh#T1a` |
| R22 | 代理端口不通或 403 时 Claude Code 是否回退直连 | **暂不测（用户 2026-11-27 决定）** | 同 R21（默认关着时不影响任何人，打开的人才受它影响）。补测用 `script:scripts/m6/layer4/run.sh#T2b` |
| R23 | 回环连接能否按本地端口找到对端进程 | **待真机** | 端到端 `script:scripts/m6/e2e/run.sh#CC1`（启用后先打开第四道（开发安装只监听、不写托管设置），用 curl 经本机代理端口，做完关掉；2026-11-27 从第四层挪来） |

另外两项待真机：装上即启用 pf 对这台 Mac 上别的用 pf 的软件有没有影响（只取一个引用、只写自己的锚点，按设计没有影响；端到端的快照对照确认）；回国拦截的定向清理连接状态（`script:scripts/m6/e2e/run.sh#K1`）。

## 6. M2–M5 遗留的验收项

| 项 | 内容 | 状态 |
|---|---|---|
| M2 | 真机脚本的第 1 步（R1）和第 5 步（M2 脚本）；M2 的“验收通过”等这两步 | **暂不测（用户 2026-11-27 决定）**：同 R1、R2 |
| M3、M4 | 各自的真机脚本；R3 脚本 | 待用户 |
| M5 | 第一级（假系统）和第二级（真机）走查，`file:docs/m5-walkthrough.md` | 待用户 |
| M5-3 系统授权 | E1、E2 探查 `file:scripts/m5/probe/run-e1e2.sh`；结论决定应用里的本人授权走系统授权还是一直用终端退路 | 遗留项，待用户 |

## 7. 这一版不做、没有做、替代做法

**这一版不做的项**（用户 2026-11-27 决定不购买付费开发者账号；每项的原因写在“原因”一栏；代码里留的接口和“未启用”的实现不动，位置见 m6-plan 第 8 节）：

| # | 项 | 原因 | 现在的做法 |
|---|---|---|---|
| P1 | Developer ID 签名、公证 | 没有付费账号 | 临时签名 |
| P2 | DMG | 没有付费账号（未签名的安装包会被系统拦下） | 开发安装脚本 |
| P3 | SMAppService | 没有付费账号 | 开发用的 LaunchDaemon |
| P4 | 调用方签名校验 | 没有付费账号（没有稳定的签名身份可校验） | “未启用”的实现；状态和设置里写明 |
| P5 | Endpoint Security（第二道） | 没有付费账号（授权要申请） | 没有这一道 |
| P6 | 系统的代码签名校验 | 没有付费账号 | SHA-256 清单 |
| P7 | Sparkle 更新；引擎的联网下载；**引擎的安装与回滚**（M6-6b） | 自动更新：没有付费账号；引擎的安装与回滚、联网下载：**不因为账号**，没有可信的更新通道（M6-6b 已定这一版不做） | 重新构建镜像并重新执行开发安装；引擎只随镜像内置 |
| P8 | 系统授权（M5-3） | **不因为账号，仍然要做**：等 E1、E2 的结果 | 终端退路 |

**替代做法**（m6-plan 第 2 节 S1–S7）：开发安装代替 DMG 和 SMAppService（S1）；只有第一、三、四、五道（S2；第四道 v0.68 起可选、默认关）；SHA-256 清单（S3）；对端 uid 三级权限（S4、S5）；手动重装代替自动更新（S6）；不分发、自行构建（S7）。**用户决定不购买付费开发者账号，第 1 节完成标准里第 1、5 两条以替代做法为最终状态**，第 3 条同样以替代做法为最终状态（没有第二道）。

**设计里写了、这一版没有或换了做法的功能**（2026-11-27 对照设计第 4、5、7、8、9、10、11 节逐节核对，并略读第 1、2、3、6、12 节、m5 走查清单的“与原型不同”、m6-plan 第 1、2、8、10 节；每一项都在代码里查过。P1–P8 不重复列）：

| 设计章节 | 这一版的现状 | 归类 | 原因 | 对你的后果 |
|---|---|---|---|---|
| 5.3、R7：可选的“系统服务直连”预设 | 没有这个预设。系统进程识别成 `system:<文件名>`，可以自己一条条写规则（如 `abroad app set system:apsd direct`） | 没有做 | R7 还没有实测结论，先不定预设 | 启用后推送、iCloud 和 App Store 的后台下载、时间同步都和普通应用一样走主代理，对外是出口 IP；主代理不支持 UDP 时 NTP 被拦、时间同步失败；停用（断网）或闸门关着时这些服务全部断网（收不到推送、iCloud 不同步）。后果是按规则推出的，R7 实测后修正 |
| 6.1、R16：认证页临时放行、“仅认证”模式 | 没有任何放行认证页的机制（`captive.apple.com` 只在不分配假地址的名单里） | 没有做 | R16 没有实测，没有安全的放行办法 | 已启用或已停用时，公共 Wi-Fi 的认证页打不开（认证前主代理连不上）。办法：先“一键回国”（两次确认、会结束 Claude），在本机网络下完成认证，再重新启用 |
| 6.1：“允许局域网”开关（默认开） | **已补**（2026-11-27，补齐三）：后台持久化的设置 `GET/PUT /v1/settings/lan`，关是收紧、从关到开要本人授权；关掉后断网和接管规则里都没有局域网表，回环、DHCP、专用用户、虚拟机网桥、回国拦截不受影响；开机 guard 的参数随之变化。命令行 `abroad lan show|on|off`，应用设置页的开关。测试 `go:internal/daemon#TestAllowLANSwitch`、`go:internal/state#TestCrashExhaustiveNoLAN`、`go:internal/state#TestRebootExhaustiveNoLAN`、`go:cmd/abroad#TestCLINoFakeAndLAN`、`swift:noFakeListAndAllowLAN`。真机效果待端到端 `script:scripts/m6/e2e/run.sh#LAN1` | 已补 | — | — |
| 4.3：不分配假地址的名单“内置、可增删”，含投屏和局域网发现的域名 | **已补**（2026-11-27，补齐三）：内置四类不动、删不掉；用户追加的进规则文档的 `bypass_extra`（条件写入、追加是放松）；Claude 的域名、置顶域名加不进，已在名单里、之后被加进置顶的按置顶处理；投屏类域名由用户自己追加。命令行 `abroad rules nofake …`，应用分流页一小节。测试 `go:internal/rules#TestBypassExtra`、`go:internal/fakedns#TestBypassUser`、`go:internal/daemon#TestBypassExtraRules`、`go:internal/api#TestRulesLevelBypassExtra` | 已补 | — | — |
| 5.1：可选“继承父进程” | 识别器有这个字段，但 abroadd 没有打开 | 没有做 | — | 不能给“终端里启动的所有程序”统一设规则，只能按每个程序的路径设 |
| 5.1：共享、系统进程在界面上单独显示并说明 | **已补**（2026-11-27，补齐四）：`/v1/apps` 每行带种类（app、system、path、unknown），只有规则的应用在 `rule_kinds`；“被很多应用共用”的系统进程清单 `procinfo.SharedSystem`（nsurlsessiond、apsd、cloudd、mDNSResponder、trustd、timed，在开发用的 Mac 上看到的）带说明；应用分流页系统进程单列、默认折叠，给共用的设规则前提醒；命令行 `apps` 同样单列。测试 `go:internal/procinfo#TestAppKindAndShared`、`go:internal/daemon#TestAppKinds`、`go:cmd/abroad#TestCLIHistoryAndKinds`、`swift:systemProcessesAndSharedPrompt` | 已补 | — | — |
| 4.2、5.2：拦截 UDP 443；`offline_allow`、`local_domains`、`pinned_extra` | **已补**（2026-11-27，M6 之后的补齐一）：命令行 `abroad rules local|pinned|offline ls|add|rm`、`rules udp443 show|on|off`，应用“应用分流”页的“域名与其他规则”；规则提交带条件头（读和写之间被别处改过时不覆盖）。测试 `go:cmd/abroad#TestCLIRuleLists`、`go:cmd/abroad#TestCLIRulesConflictRetry`、`go:internal/daemon#TestRulesConditionalWrite`、`swift:ruleListsLoadAddRemoveToggle`、`swift:ruleListsConflictReloadsNoRetry` | 已补 | — | — |
| 4.2、7.2：首页和菜单栏的“UDP 不可用”、节点列表标出是否支持 UDP | **已补**（2026-11-27，补齐二）：状态 `main_udp`、节点的 `udp`，首页和菜单栏的说明，主代理页的节点标记，命令行 `status`、`main nodes`。测试 `go:internal/exitcheck#TestUDPAvailability`、`go:internal/daemon#TestMainUDPRecording`、`go:cmd/abroad#TestCLIUDPAndFallbackOutput`、`swift:udpNoticeOnlyWhenEnabledAndUnavailable`、`swift:nodeUDPLabels` | 已补 | — | — |
| 5.2、11.2：标出正在回退到主代理的规则；“分流出口回退”“UDP 不可用”的通知 | **已补**（2026-11-27，补齐二）：`/v1/apps` 的 `fallbacks`、出口的 `affected`，分流页的回退标记、出口页的受影响规则、两种新通知。测试 `go:internal/daemon#TestRuleFallbackMarks`、`swift:fallbackMarksInPages`、`swift:udpAndExitNotifications`；回退仍走主代理 `go:internal/daemon#TestExitsAvailabilityAndFallback` | 已补 | — | — |
| 6.1：pf 规则连续被清掉时通知并指出可能是哪个软件 | 有事件 `repeatedly_cleared`，应用不转成通知，也不指认软件 | 部分做了 | — | 只能在事件里看到 |
| 7.1：控制面的调试接口（exec） | 明确禁止（不能注册调试、终端类接口） | 改了做法 | 敏感接口不提供（m5-plan） | 没有调试终端 |
| 10.1：要结束的进程清单可在“设置”里增删 | 清单内置，设置页只能看 | 改了做法 | 同上 | 改不了清单 |
| 7.2：经本机网络下载订阅时读系统代理和 PAC | 经直连拨号，不读系统代理 | 改了做法 | 设计 11.1 的 M5-4 段 | 必须经公司代理才能上网的环境下，首次下载订阅会失败；改用本地配置文件导入 |
| 7.2：本地配置上限 8 MiB、可以粘贴 | 应用里上限 2 MiB，只能选文件 | 改了做法 | 和后台请求体的上限一致（m5 走查清单“与原型不同”） | 大文件导入不了 |
| G4、7.3：分流出口可以填 Clash 订阅地址、自动更新；原型里的手动节点 | 出口只有 WireGuard、OpenVPN、Clash 配置、分享链接四种，都从文件或标准输入导入 | 没有做 | — | 订阅出口要自己下载内容再导入，节点变了要重新导入 |
| 7.3：shadowsocks-rust、Xray | 没有 | 推迟（设计本来写的是第二阶段） | 第二阶段 | 用分享链接（sing-box 能导入的）代替 |
| 8：主代理页面选“出口变化规则” | 应用里没有，只有命令行 `abroad main apply --rule` | 改了做法 | — | 应用里应用主代理只能用默认规则 |
| 8.1、11.1：环境检测的历史 | **已补**（2026-11-27，补齐四）：每一轮记时间、总判定、各项的标识和结果（不记说明），相同时只更新时间，上限 200 条；状态目录里 0600 的文件；`GET/DELETE /v1/envcheck/history`；诊断包带上；应用环境检测页一节、命令行 `check history [clear]`。测试 `go:internal/envcheck#TestRunHistory`、`go:internal/daemon#TestEnvHistoryAPI`、`swift:envHistoryRows` | 已补 | — | — |
| 8.1：每次 Claude 启动前结果超过 10 分钟就先补跑 | 只有点应用里的“打开 Claude”时补跑 | 改了做法 | 在启动那一刻补跑要 Endpoint Security（P5） | 从别处直接启动的 Claude 不触发补跑（回国状态下会被第三道结束） |
| 9.3：“重启这些应用”按钮、“需要重启 Mac”窗口、“待重启”提醒、浏览器语言和定位的提示 | 应用里都没有 | 没有做 | — | 改了语言后，要自己知道得重新登录或重启，访达和菜单栏才变成新语言 |
| 10.2：检测通过时通知“可以打开 Claude 了” | 只有地区同步完成那一次带“打开 Claude”按钮 | 部分做了 | — | Claude 被拦下后、地区没变时检测通过，没有通知，要自己去点 |
| 10.2：解析锁的备选写法（回环地址加端口）可选 | 只实现了生成，不能选 | 没有做（等 R18） | R18 没有结论 | 回国后本机开着 TUN 劫持 DNS 时这道锁失效（第五道仍有效，除非那个代理接管了出口） |
| 10.3：应用里的卸载入口、代为把应用移到废纸篓、卸载后引导重启 | 设置里写“应用里的卸载入口还没有提供（要等系统授权）”和终端命令；只能 `sudo abroad uninstall` 或开发安装的卸载脚本（卸载是敏感操作，应用里的入口等 M5-3 的系统授权，M5-3 等 E1、E2 的结果） | 改了做法 | 正式安装包这一版不做（P2、P3，没有付费账号）；应用里的入口等 E1、E2 | 只能在终端里卸载，应用要自己删 |
| 11.2：设置里生成诊断包 | **已补**（2026-11-27，补齐五）：设置“高级”里“导出诊断包…”，默认不带域名清单（勾选才带），用户在保存面板里选位置，内容是后台原样；终端命令 `abroad diagnostics` 仍写在说明里。测试 `swift:diagnosticsExportIsVerbatim`、`swift:diagnosticsExportOutcomes`、`swift:diagnosticsExportKeepsNoContent`、`swift:diagnosticsHelpText` | 已补 | — | — |
| 10.4：系统代理“帮我关闭”；启用前提示本机 DNS 有问题 | 冲突检查里没有这两项；本机 DNS 只在启用时读不出来才记一条事件 | 没有做 | 设计本来写的是 M2 不做 | 要自己去关系统代理；本机 DNS 配错时事先没有提示 |
| 11.1：应用用系统签名校验确认连的是真的 abroadd | 没有 | 这一版不做（同 P4） | 没有付费账号（用户 2026-11-27 决定不购买） | 应用不能确认自己连的后台是真的（本机 socket 的属主和权限仍限制了谁能冒充） |
| 10.1：两次确认（确认对话框加警告框） | 合成一个确认框，“确认”3 秒后才能点 | 改了做法 | m5-plan v2.9 决定 2 | 只点一次确认 |

**没有做**（其余）：

| 项 | 原因和后果 |
|---|---|
| 用发布构建验证管控的真实效果（第三道结束真实 Claude、第五道拦真实地址段） | 没有写脚本。发布构建装上的那一刻起，这台 Mac 直连 Anthropic 地址段的流量全部被拒，规则不分网卡，经 VPN 隧道出去、目的地仍是这些地址的包同样被拒，正在运行的 Claude Code 会话会断开，直到卸载；第三道会结束本机真实的 Claude 进程。要不要做、在哪台机器上做，由用户决定 |
| 六种分享链接的字段映射核对 | 没有拿真实的链接核对过（单测只核对转换结果的形状）。不认识的参数和取值一律拒绝导入：映射有误时用户看到的是导入失败，不是连到配置不对的地方。用户导入自己的链接后，出口检查通过即算核对过（待真机） |
| “允许局域网”关掉后的真机效果 | **待真机**（端到端 LAN1，要设 `M6E_LAN_PROBE`）：关掉后经物理网卡连局域网设备失败、打开后恢复；自动测试只证明了规则文本和求值 |
| 回国拦截的定向清理连接状态默认打开 | macOS 上的写法和效果待端到端 K1；abroadd 的 `--return-block-kill-states` 默认关 |
| Windows 版 | 暂停（用户 2026-11-25 决定） |

## 8. 用户要做的事

全部用开发构建；命令都在仓库根目录执行，详细步骤以各脚本的 README 为准。**会断网的几项会让正在运行的 Claude Code 会话断开**，要在不需要会话的时候做。

| 顺序 | 做什么 | 在哪台机器 | 命令 | 大约多久 | 会不会断网、要不要 sudo | 看到什么算通过 | 出问题怎么恢复 | 贴回什么 |
|---|---|---|---|---|---|---|---|---|
| 1a | M5 第一级走查 | 这台 Mac | `scripts/dev/app-bundle.sh`，再 `scripts/m5/walk-fake.sh` | 30–60 分钟 | 不断网，不要 sudo | 清单“第一级”逐项符合 | Ctrl+C | 走查清单的回填表 |
| 1b | 系统授权探查 E1、E2 | 这台 Mac | `scripts/m5/probe/run-e1e2.sh`（见 `scripts/m5/probe/README.md`） | 约 3 分钟 | 不断网 | 按 README | 按 README | 输出 |
| 1c | 解析锁实测（R9、R18） | 这台或另一台 Mac | `sudo scripts/m6/resolver/run.sh`（可先 `--dry-run`） | 约 10–15 分钟（估计） | 不断网，要 sudo；锁真实域名期间本机 Claude 可能连不上 | 每种程序都得到“锁有效”或“锁无效”的结论（无效本身是合法的结果，不是你做错了；按 README） | `sudo scripts/m6/resolver/restore.sh` | `scripts/m6/resolver/out/` |
| 2 | R1 → R2 → M2 → R3 → M3 → M4 → M5 第二级走查：**暂不测（用户 2026-11-27 决定**：不出国时用户要靠本机另有的代理线路用 AI，不能关掉；直接装开发版。后果见第 5 节 R1、R2；R3、M3、M4、M5 第二级同样暂不测；收尾时再问补测还是定为不测）。以下保留供补测 | 这台 Mac | 各自 README；用最新代码重新编译 | R1 3–5 分钟；R2 断网 1–2 分钟；M2 5–8 分钟；M3 10–12 分钟；M4 15–20 分钟；M5 第二级 30–45 分钟 | 会断网，要 sudo，会话会断开；前提检查会要求先关掉别的代理软件的隧道（TUN、VPN）和系统代理 | 各脚本的结果文件里全部通过 | 各自的 `restore.sh`；仍不行重启电脑 | 各脚本 README 指定的文件 |
| 3 | 端到端（第一、二、三段） | 这台 Mac | `sudo -E scripts/m6/e2e/run.sh`，重启后 `--after-reboot`，再重启后 `--after-reboot-2` | 第一段 20–30 分钟，第二段 15–20 分钟，第三段约 10 分钟（估计），中间重启两次 | 启用期间和第一次重启后一两分钟断网，要 sudo；第二次重启是已回国状态、不断网 | `scripts/m6/e2e/out/m6e-results.txt` 里 I1、P1–P5、CC1、RB1–RB3、L1、B1–B3、E1–E3、U1、R1 都“通过”（RB 的连通性一项可以是“无法区分”，以锚点和状态为准），K1 记下 pfctl 的输出 | `sudo scripts/m6/e2e/restore.sh`（不需要网络）；死人开关默认 3600 秒后自动执行 | 结果文件和 `scripts/m6/e2e/out/`、`scripts/m6/install/out/` 的日志 |
| 4 | 第四层实测（R21、R22）：**暂不测（用户 2026-11-27 决定**：写入托管设置期间所有 Claude Code 改连本机代理端口，影响平时的用法；收尾时再决定补测还是定为不测；R23 已挪进端到端 CC1）。以下保留供补测：排在端到端之后 | 这台 Mac（用户 2026-11-27 答复）。做之前关掉所有 Claude Code 会话；期间 Claude Code 的出口是 Claude 出国的主代理；T 步里有两步故意连不上；做完回国、卸载、确认分片目录里没有我们的文件再恢复平时用法（见 `file:scripts/m6/layer4/README.md`） | 开发安装 → 应用里启用 → `sudo -E scripts/m6/layer4/run.sh` → 回国 → 卸载脚本 | 约 15–20 分钟（估计），加安装和卸载 | 在已启用状态下做；写入托管设置期间那台 Mac 上的 Claude Code 都经本机代理端口 | 结果文件里 T0–T5 的判定（按 README） | `sudo scripts/m6/layer4/restore.sh`；之后卸载脚本 | `scripts/m6/layer4/out/` |
| 5 | 决定：要不要用发布构建验证真实效果（是否购买付费开发者账号：2026-11-27 已决定不购买） | — | — | — | — | — | — | 你的决定 |

## 9. 回填

| 项 | 用户执行日期 | 结果（贴回的文件） | architect 判定 | 写回设计第 15 节 |
|---|---|---|---|---|
| M5 第一级走查 | | | | |
| E1、E2（M5-3） | | | | |
| 解析锁实测（R9、R18） | | | | |
| 开发版第一次重装的卸载（真机，2026-11-27） | 2026-11-27 | 对照只差 pf.enabled（安装前 Disabled、卸载后 Enabled）；启用引用里只有系统的 pfd，我们的锚点空 | 不是我们的残留；卸载对照改为接受别的程序持有的引用（设计 v0.70a） | — |
| 开发版安装后“应用主代理”（真机，2026-11-27） | 2026-11-27 | 五项出口检查都是 channel_down，虚拟机管理器 idle | 缺口（回国状态不拉起虚拟机（M6-2a）而应用主代理要经虚拟机（M5-4））。**已修，待用户重装后再做一次应用主代理**（借用虚拟机，设计 v0.70，计划 docs/vm-borrow-plan.md v2；测试 `go:internal/daemon#TestBorrowApplyMainRestored`、`go:internal/daemon#TestEnableFromRestoredManagedPushesAfterStart`） | — |
| R1 | 暂不测（用户 2026-11-27 决定） | | | |
| R2 | 暂不测（用户 2026-11-27 决定） | | | |
| M2 | 暂不测（用户 2026-11-27 决定） | | | |
| R3 | | | | |
| M3 | | | | |
| M4 | | | | |
| M5 第二级走查 | | | | |
| 端到端（含 B1、B3、RB1–RB3、K1、U1） | | | | |
| 第四层实测（R21、R22） | 暂不测（用户 2026-11-27 决定） | | | |
| 发布构建验证（如果做） | | | | |

用户贴回各脚本的结果后，由 architect 逐条判定，把第 1、2、5 节的“待真机”改成实测结论。在真机结果回来之前，项目的状态是“会话里能做的都做完了，等用户的真机验证和几项决定”。
