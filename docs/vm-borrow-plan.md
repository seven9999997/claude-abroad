# 借用虚拟机修复：实施计划（交架构顾问审）

> 版本：v1（2026-11-27）。只是计划，代码还没改。
> 依据：`docs/PROGRESS.md` 的“2026-11-27 第一条真机结果”和“借用虚拟机修复的验收标准（前任 architect 2026-11-27 原文）”（做法九条、验收 a–g）；设计 v0.69 第 7.2、10 节的“M6-2a 起”一段、11.1。
> 会话里的边界：不改宿主机网络，不碰本机会话依赖的代理（见 `docs/LOCAL.md`），不用 sudo，不运行真实模式，不提交。

## 0. 问题和根因（核对代码后）

- `internal/daemon/vmmgr.go` 的 `startVMManager`：状态文件是 `RESTORED` 时调用 `SetWanted(false)`，管理器停在 `idle`。
- `internal/daemon/region_api.go` 的 `handleApplyMainProxy`：直接调 `Sys.Baseline.Probe` → `exitcheck.Checker.Check(ctx,"main",…)`。Checker 的 `Opener` 是 `dp.Streams()`，经 `data.sock` 进虚拟机。虚拟机没运行时 `wire.Dialer` 拨不通，`causeOf` 把错误归成 `CauseChannelDown`，五项都是 `channel_down`。`recheck.go` 的 `failedItems` 把原因码原样拼进 `usererror`（“https（channel_down）；…”），命令行和应用都把这段话显示给用户。
- 核对时**另外发现一个同类缺口**（同一组合，用户修好这一步以后马上会碰到）：`enableWithMain` 在 `m.Enable` 之前调 `ensureMainPushed`。由 abroadd 管理虚拟机、状态是回国、又有“待下发”的选择（`pending.json`）时，这一步对没运行的虚拟机 `PushMainHops`，必然失败，启用被拒。原因和本缺口相同：M6-2a 之前的代码都假定虚拟机是外部启动、一直在。假的 `FakeMainVM` 永远成功，所以测试没发现。计划把它一起修（第 1 节第 6 步）。**这一步请顾问确认是否并入本任务。**
- 另一个相关的点：`handleApplyMainProxy` 现在不下发保存的选择（注释写着“主代理配置下发给虚拟机仍由开发工具在此之前完成”）。借来的虚拟机是新起的，里面没有主代理配置，所以借用时必须先按保存的选择下发，再检查。

## 1. 改哪些文件、按什么顺序、每处改什么

按依赖顺序，每一步做完都能编译、单测通过。

### 第 1 步：面向用户的原因文字（不依赖借用，先做）

| 文件 | 函数 | 改什么 |
|---|---|---|
| `internal/exitcheck/text.go`（新） | `CauseText(c Cause) string`、`ItemLabel(name string) string` | 每个 `Cause` 一句中文（见第 6 节的表）；每个检查项一个中文名（https→“HTTPS 访问”、ip→“出口 IP”、mainland→“是否在大陆”、rule→“出口变化规则”、doh→“加密 DNS”、geo→“位置”、udp→“UDP”）。表里没有的原因按“原因不明”处理，不回显原值 |
| `internal/exitcheck/result.go` | 不改判定表 | `Cause` 常量和 JSON 里的 `cause` 字段保持不变（它们是给程序看的稳定代码，和 `api.ErrorCode` 一样），只是不再拼进给人看的文字 |
| `internal/daemon/recheck.go` | `failedItems` | 改成“HTTPS 访问：连不上；出口 IP：超时”这种格式（`ItemLabel` + `CauseText`）；只有 `channel_down` 时另给一句（第 6 节） |
| `internal/envcheck/exit_items.go` | 第 88、90 行 | `string(rule.Cause)` 换成 `exitcheck.CauseText(rule.Cause)` |
| `internal/daemon/baseline.go` | `Probe` | 失败时返回带类型的错误 `*ProbeFailure{Report, Unreachable bool, OnlyChannel bool}`（外面仍包 `usererror`，文字用新的 `failedItems`）。`Unreachable`：没通过的项全是网络类原因（超时、连不上、限流、格式、通道）；`OnlyChannel`：全是 `channel_down`。明确不通过（大陆、证书不对、规则）照旧是普通的“没有通过”，代码不变 |

### 第 2 步：`internal/vmmgr` 加“借用”入口

文件 `internal/vmmgr/manager.go`：

- `Manager` 新字段（都在 `mu` 下）：`borrowed bool`；`failKind`（最近一次失败的类别：`launch`（`Launcher.Launch` 返回错误）、`notReady`（拉起后时限内没健康）、`exited`（运行中意外退出））。
- `isWanted()` 改为 `wanted || borrowed`。`Run`、`supervise` 的其余逻辑不动：借用期间和常驻时同样拉起、健康检查、加清 `ReasonVM`、健康时 `Kick`。
- `Run` 里在拉起失败处记 `failKind=launch`；`supervise` 返回错误处按情形记 `notReady`/`exited`。
- 新方法 `Borrow(ctx context.Context) (release func(context.Context) error, borrowed bool, err error)`：
  1. 锁内：`closed` → `ErrClosed`；`wanted` 为真（常驻）→ 不借用，`borrowed=false`，在 `Timing.BorrowWait` 内等到 `running`（`failed`/`stuck` 立即返回带原因的错误），`release` 是空操作。
  2. 否则 `borrowed=true`、清零 `Failures`，`poke`；在 `BorrowWait` 内等：`running` → 成功；`backoff`（第一次失败就算）/`failed`/`stuck`/`closed` → 立即失败，**不重试**；超时 → 失败。失败时 `Borrow` 自己先调用 `release` 停掉再返回。
  3. 失败的错误类型 `*BorrowError{Kind, Err}`，`Kind` 是 `KindLaunch`（拉不起来）、`KindNotReady`（等健康超时，含 `BorrowWait` 到点）、`KindStuck`。
- `release(ctx)`：锁内 `borrowed=false`；若此刻 `wanted` 为真（只可能是被直接调用 `SetWanted(true)`，正常流程被互斥挡住）→ 交还，不停，返回 nil；否则清零 `Failures`（借用期间的失败不计入常驻时的“连续失败到上限”），`poke`，等到 `idle`（`stuck` → `ErrStuck`；`closed` → nil），时限 `StopWait+KillWait+5s`。可以重复调用（第二次是空操作）。
- `Timing` 加 `BorrowWait`（默认 `StartTimeout+10s`=100 秒）；`DefaultTiming` 同步加；`timing()` 对旧的测试取值里 `BorrowWait==0` 时用 `StartTimeout+10s`。
- `Status` 加 `Borrowed bool \`json:"borrowed,omitempty"\``。
- 包注释的不变量加一条：“回国状态虚拟机不常驻；只在一次互斥的操作内部被借用，操作结束时已经停掉；借用期间失败不重试”。

### 第 3 步：daemon 的借用框架（新文件 `internal/daemon/vmborrow.go`）

- 类型 `vmUse struct{ borrowed bool; release func(context.Context) error }`。
- `func (d *Daemon) borrowVM(ctx, op string) (vmUse, error)`：
  - `d.vmm != nil`（由管理器管理）→ `d.vmm.Borrow`，错误转成 `*VMUnavailable{Kind}`；
  - 否则 `Sys.VM` 实现 `interface{ Probe(context.Context) error }`（`ExternalVM`）→ 只确认在运行，不拉起也不停；不在 → `*VMUnavailable{Kind: KindExternalDown}`；
  - 否则（假系统的 `FakeVM`）→ 当作可用。
- `func (d *Daemon) withVM(ctx, op string, fn func(ctx context.Context, u vmUse) error) error`：所有“要经虚拟机”的接口统一走这里：
  - 状态是 `ENABLED`：不加锁、不借用（和现在相同）；先确认虚拟机就绪（管理器：`Ready()`；外部：`Probe`），不就绪 → `VMUnavailable{KindNotRunning}`（带阶段的中文名），然后调 `fn`。
  - 其他状态：`d.m.Exclusive(...)` 里做（和启用、停用、回国、卸载、改规则、改出口互斥；忙时得到 `state.ErrBusy`）：
    1. 锁内重读状态：`ENABLED`（入口之后刚好变了）→ 回“忙，请再试一次”；`ENABLING`/`RESTORING`（崩溃后 Recover 没走完留下的）→ `ErrStateIncomplete`（“上一次启用或回国没有完成：请先执行恢复本机网络，再试”），不借用；
    2. `d.borrowing.Store(true)`，`borrowVM`；
    3. `fn(ctx, u)`；
    4. **不论 `fn` 成败**，`u.release(ctx)`（用 `context.WithoutCancel`，调用方断开也要停）；
    5. `d.borrowing.Store(false)`，清进度，`d.sup.Kick()`（借用期间监护被跳过的那一轮立刻补上，见风险 1）。
  - 整体时限 `BorrowTimeout = 4*time.Minute`（100 秒等健康 + 下发 + 检查 25 秒 + 停 25 秒，留余量；应用的请求时限 300 秒、命令行 5 分钟）。
- 进度：`func (d *Daemon) borrowStep(op, step string)` 写 `d.borrow atomic.Pointer[BorrowView]` 并发事件（来源 `vm_borrow`）。步骤：`starting`（正在启动虚拟机）→ `pushing`（正在下发主代理）→ `checking`（正在检查出口）→ `stopping`（正在关闭虚拟机）→ `done` / `failed`（detail 是第 6 节的中文原因）。没借用（已启用、外部启动）时只有 `checking`。
- 错误到应答：`func writeVMErr(w, status int, err error)`：`state.ErrBusy` → 409 `busy`；`VMUnavailable` → 503 `vm_unavailable`；下发失败 → 502 `main_push_failed`；`ProbeFailure` 且 `OnlyChannel` → 503 `vm_unavailable`（“到虚拟机的数据通道打不开”）；`ProbeFailure` 且 `Unreachable` → 原状态码 + `main_unreachable`；`ErrNoMainSelected` → 422 `no_main_selected`；`ErrStateIncomplete` → 409 `state_incomplete`；其余照旧。
- 路由分类表（验收 d 的源头，放在 `routes.go` 旁边）：`BorrowRoutes = {"main-proxy-apply","main-check","exit-check"}`；`ResidentRoutes = {"enable"}`（由状态机拉起并常驻，不是借用）；其余全部是“不许拉起”。测试按路由表逐条核对（第 4 节）。

### 第 4 步：三个可借用的处理函数

1. **`region_api.go` `handleApplyMainProxy`**
   - `ENABLED`：现有顺序不变（`Probe` → `finishMainProxy`，地区会变时两步确认、`SyncRegion`）。只是在 `Probe` 前过 `withVM` 的就绪检查，不就绪给明确原因。注意这里**不能**持有 `Exclusive`：`SyncRegion` 自己要拿忙标记。
   - 其他状态：`withVM` 里：`pushForProbe` → `borrowStep(checking)` → `Probe` → `release`（在 `withVM` 里）→ `finishMainProxy` 写进 `bufferedResponse`（未启用时它只保存基准、锁定 `region.json`，不确认、不调 `SyncRegion`、不写 Mac 和虚拟机的地区）→ 解锁 → `flushTo(w)`。这样调用方拿到应答时虚拟机已经停了、忙标记已经放了（紧接着启用不会撞上“忙”）。
   - `MainProxyResult` 加 `Borrowed bool`；`Note` 在借用时写“虚拟机是临时启动的，检查完已经关闭”。
2. **`mainproxy_api.go` `handleMainCheck`**：同上，`Probe` 后不保存；借用结束后回 `BaselineSummary`（加 `Borrowed`）。
3. **`m5rest_api.go` `handleExitCheck`**
   - 先判间隔和“正在跑”（`exitChk`）：命中缓存就直接回上一次的结果，不借用；
   - `ENABLED`：不变；
   - 其他状态：`withVM` 里 `pushForProbe`（分流出口的检查也要虚拟机里的网络栈和出站白名单，主代理一起下发，结果才和启用后一致）→ 持 `exitPushMu` 调 `exitLoop.push`（换出站白名单、下发分流出口）→ `checkExitOnce` → 取 `exitStatus` 作为应答 → 把各出口 `SetPushed(false, "没有启用：虚拟机没有运行")`、`exitLoop.pending=true`（启用时照常重新下发）→ `release`。`ExitCheckResult` 加 `Note`（“没有启用：这是临时启动虚拟机检查的结果，启用后会重新检查”）。
   - 这一条请顾问拍板：也可以改成未启用时不检查、回明确原因（“没有启用时不检查分流出口，启用后自动检查”），实现更小。计划默认按前任第 ② 条做借用。
- 共用的 **`pushForProbe(ctx, u vmUse) error`**（`mainproxy_api.go`）：
  - `managedMain()` 为假：借来的虚拟机是新起的、里面没有配置 → `ErrNoMainSelected`（“还没有选主代理节点：请先选好节点（abroad main use …）再应用”）；不是借来的（外部启动、开发工具手动下发的那条路）→ 什么都不做，和现在相同。
  - 否则持 `exitPushMu` 调 `pushManagedLocked(ctx, vm, "应用主代理", persist: !u.borrowed)`。
- **`pushManagedLocked` 加参数 `persist bool`**：为假时只下发，不写“已下发”记录、不删 `pending.json`、不发 `pushed` 事件（虚拟机马上要停，“已下发”是假话；启用时 `ensureMainPushed` 按保存的选择重新下发）。`ensureMainPushed` 传 `true`，行为不变。

### 第 5 步：只读接口和其余改动类接口

不改逻辑，只由测试保证不拉起（第 2 节的清单）。`handleEngines` 已经只读 `vmm.Status()` 和 `Health`，不触发拉起；`recheckFunc` 已经对管理器只读 `Ready()`。

### 第 6 步：启用时主代理的下发顺序（第 0 节发现的缺口）

- `region_api.go` `enableWithMain`：
  - 先看 `d.borrowing`：为真 → 直接回 `state.ErrBusy`（“正在临时启动虚拟机做出口检查”），不碰虚拟机（验收 ④ 要的是“忙”，不能是下发失败）；
  - 只有虚拟机此刻就绪（管理器 `Ready()==nil`，或外部启动）才在 `m.Enable` 之前 `ensureMainPushed`（保留“下发失败时什么都没改”）；不就绪就跳过。
- `daemon.go` 组装状态机处：`d.m.D.VM = mainPushVM{inner: s.VM, d: d}`。`mainPushVM.Start(ctx)` = `inner.Start(ctx)` 之后 `d.ensureMainPushed(ctx)`（按摘要比较，已经是这份就跳过，所以 `SyncRegion` 里的 Start 重复调用无害）；`Stop` 直接转发。`recheckFunc` 和 `startVMManager` 仍然拿原始的 `s.VM`（类型断言 `readyReporter`、`*vmmgr.Manager` 不受包装影响）。
- 这样由管理器管理时，启用第 ④ 步拉起并等到健康后才下发主代理，再到第 ⑤ 步的出口检查；下发失败按第 ④ 步失败回滚（和虚拟机起不来同样处理）。

### 第 7 步：状态和接口类型

- `internal/api/errors.go`：`ErrorCodes` 加 `vm_unavailable`、`main_push_failed`、`main_unreachable`、`no_main_selected`、`state_incomplete`（注释写明状态码和含义）。
- `daemon.go`：`Status` 加 `Borrow *BorrowView \`json:"borrow,omitempty"\``（`op`、`step`、`since`）；`Daemon` 加 `borrowing atomic.Bool`、`borrow atomic.Pointer[BorrowView]`。
- `internal/daemon/vmmgr.go`：只改注释（“回国状态不常驻；借用见 vmborrow.go”）。
- 重新生成应用的接口类型：`go run ./cmd/abroad-apigen`，`-check` 通过。

### 第 8 步：命令行（第 7 节）

`cmd/abroad/commands.go` 的 `cmdMainApply`、`cmdMainCheck`、`cmdExitCheck`；`cmd/abroad/main.go` 的 `codeOf`、新 helper `borrowProgress`。

### 第 9 步：应用（第 7 节）

`app/Sources/AbroadKit/Strings.swift`、`StatusModel.swift`、`MainProxyModel.swift`、`PageModels.swift`；`app/Sources/ClaudeAbroad/MainProxyPage.swift`、`SetupView.swift`、`EnginesPage.swift`、`ExitsPage.swift`。

### 第 10 步：文档

设计文档（第 8 节）、`docs/m6-acceptance.md` 第 9 节那一行（“已修，待用户重装后再做一次应用主代理”）、进度单。

## 2. 清单（验收 d）

### 2.1 可以借用的操作（未启用时在状态机互斥内借用）

| 路由 ID | 处理函数 | 经过的函数 | 为什么要经虚拟机 |
|---|---|---|---|
| `main-proxy-apply` POST `/v1/main-proxy/apply` | `handleApplyMainProxy` | `withVM` → `borrowVM`（`vmmgr.Manager.Borrow`）→ `pushForProbe` → `pushManagedLocked(persist=false)` → `BaselineService.Probe` → `exitcheck.Checker.Check`（`dp.Streams()` → `data.sock`）→ release → `finishMainProxy`（`BaselineService.Save`、`mainProxyTarget`、`applyTarget(enabled=false)` → `Regions.Lock`、`envKick`） | 出口检查经虚拟机里的主代理 |
| `main-check` POST `/v1/main-proxy/check` | `handleMainCheck` | `withVM` → `borrowVM` → `pushForProbe` → `Probe` → release | 同上，只检查不保存 |
| `exit-check` POST `/v1/exits/{tag}/check` | `handleExitCheck` | `exitChk` 间隔判断 → `withVM` → `borrowVM` → `pushForProbe` → `exitLoop.push`（`PushEndpoints`、`PushExits`）→ `checkExitOnce` → `exitLoop.checkOne` → `Sys.CheckExit` → `exitStatus` → `SetPushed(false)` → release | 按出口开流经虚拟机里的分流出口 |

不算借用、由状态机拉起并常驻的：`enable`（`handleEnable` → `enableWithMain` → `Machine.Enable` 第 ④ 步 `mainPushVM.Start`）。

### 2.2 不许拉起的接口（逐个核对过处理函数）

只读（GET）：`status`（`handleStatus` → `Status`，读 `vmm.Status()`）、`restore-preview`、`exits-list`、`exit-get`（读 `exitAvail`）、`rules-get`、`main-get`（`main.view`）、`main-exit`（读基准文件）、`main-sources`、`region-get`、`envcheck-get`、`envcheck-history`、`envcheck-settings-get`、`diagnostics`、`lan-get`、`claude-proxy-get`、`preflight`、`apps`、`setup-get`、`engines`（`handleEngines`：`vmm.Status()`、`MainVM.Health` 只读）、`claude-domains`、`launchguard`、`job-get`、`events`。
未启用时上次的结果：`main-exit` 回上次保存的基准（`at` 标明时间）；`exits-list`/`exit-get` 回上次的可用状态，借用之后标“没有启用：虚拟机没有运行”；`engines` 不可达时如实给 `reachable:false` 和管理器阶段。

改动类、但不经虚拟机或未启用时不经虚拟机：`disable`、`restore`、`restore-alias`、`uninstall`、`exit-put`、`exit-delete`、`rules-put`、`main-select`（未启用时只保存；已启用时 `switchMain` 用常驻的虚拟机）、`main-source-add`、`main-source-refresh`（未启用时要么 `via_host` 经直连拨号模块、要么 `via_host_required`）、`main-source-delete`、`main-lan-confirm`、`region-put`（未启用时只锁定）、`envcheck-run`（未启用时 409 `not_enabled`）、`envcheck-history-clear`、`envcheck-device`、`envcheck-settings-put`、`lan-put`、`claude-proxy-put`、`claude-domains-clear`、`setup-complete`。
另外两条后台路径也确认不拉起：出口复查（`recheckFunc` 对管理器只读 `Ready()`，且只在启用时跑）、出口循环（`exitLoop.run` 只在启用时 `round`）。

## 3. 顾问要求的五点

### ① 未启用时借用的范围

- 只用来让第 2.1 节的三个操作经主代理（或分流出口）做检查。借用期间只做三件事：拉起 `abroad-vm` 子进程、经 `ctl.sock` 下发主代理和分流出口配置、经 `data.sock` 开检查用的流。
- 不放行任何本机流量：闸门的 `state` 原因一直在（回国时“恢复本机网络”或停用时“停用”），借用不碰 `ReasonState`；管理器照常加清的只是 `ReasonVM`，加清它不会让闸门打开。数据面没有 utun、没有接管路由和 DNS，本机应用的流量根本到不了虚拟机。第四道本机代理端口看闸门，也是拒绝。
- 不改宿主机的路由、DNS、IPv6、pf：借用不调用状态机的任何一步（`applyPF`、`rebuild`、`Routes`、`DNS`、`IPv6`、`Tun` 都不碰）。为什么借用期间宿主机网络和 pf 不变：
  - 回国状态下 pf 里只有回国拦截那一组（只拦 Anthropic 的地址段；没打开启动管控的安装连这一组都没有），没有断网规则，也没有放行专用用户的规则——没有什么需要为虚拟机“打开”的；
  - 虚拟机的网卡只有用户态网络（`VZFileHandleNetworkDeviceAttachment` + gvisor 网络栈，设计 4.5，`internal/vmhost` 源码里没有 NAT 和桥接），宿主机上不建网桥、不加 NAT 规则、不改路由；虚拟机的出站就是 `abroad-vm` 这个进程自己的套接字，经宿主机现有的网络出去；
  - 检查的目标（`--exit-host` 的 204 地址、位置查询、DoH）都不在 Anthropic 的地址段里，回国拦截碰不到它们；
  - 虚拟机里的出站白名单只放行主代理第一跳（下发时一起换），不会替本机别的流量转发。
- 不改时区和语言：未启用时 `finishMainProxy` 只锁定 `region.json`（目标），不调用 `SyncRegion`，不写 Mac 和虚拟机的地区。
- 启动管控不受影响：`launchGuardDecision` 看状态和闸门原因，借用不改它们；测试比较借用前、中、后的 `GET /v1/launchguard`。

### ② 用完怎么收

| 情况 | 怎么收 |
|---|---|
| 成功 | `withVM` 第 4 步 `release`：`borrowed=false`，没有被要求常驻 → 管理循环在 `supervise` 收到 `ctl` → `stopProc`（SIGTERM，20 秒后 SIGKILL）→ `idle`；`release` 等到 `idle` 才返回，之后才放忙标记、发应答 |
| 失败（拉不起来、没健康、下发失败、检查不通过） | 同一个 `release`（`withVM` 无条件调用；`Borrow` 自己失败时也先 `release`）。拉起失败时管理器在退避里，`release` 的 `poke` 让它回到循环开头、看到不需要 → `idle` |
| 超时 | `BorrowWait` 到点 → `Borrow` 返回 `KindNotReady` 并 `release`；整体 `BorrowTimeout` 到点 → `fn` 的 ctx 取消，`release` 用脱离取消的 ctx 照样执行 |
| 停不下来 | `release` 返回 `ErrStuck`：阶段 `stuck`，不拉起第二个（现有不变量）；应答照常按操作结果给出，另在 `Note` 和事件里写“虚拟机没能按时停下，正在等它退出” |
| abroadd 崩溃或被强制结束 | `abroad-vm` 带 `--parent-watch`，标准输入的写端随 abroadd 消失，读到 EOF 自行停止（现有，`cmd/abroad-vm` 的 `TestRunParentWatch`）。借用状态只在内存里，不写任何文件；重启后状态文件仍是回国 → `SetWanted(false)` → `idle`；旧进程没退干净时单实例锁让新的拉不起来，也不会有两个 |
| abroadd 正常退出 | `Shutdown` 取消管理循环，循环结束时关掉自己拉起的进程（现有）；`Borrow`/`release` 看到 `closed` 返回 |

交还给 vmmgr：借用结束时管理器回到“按 `wanted` 管”的普通状态；正常流程里借用期间 `wanted` 不可能变（启用被互斥挡住），所以结束时总是停掉。`release` 里的“`wanted` 为真就交还不停”只是防御，有单测。

### ③ 互斥

- 用的是**状态机的忙标记**：`state.Machine.Exclusive`（`lockChange`：CAS `busy`，再过 `Admit`——卸载中或已卸载时拒绝）。它和 `Enable`、`Disable`、`Restore`、`SyncRegion`、`ReapplyRegion`、改规则、改出口、监护的 `Tick` 是同一把。
- 借用期间：
  - 启用：`enableWithMain` 先看 `d.borrowing` → 409 `busy`（不碰虚拟机）；即使刚好错过这一判断，`m.Enable` 的 `lockChange` 也得到 `ErrBusy` → 409 `busy`。异步启用的任务失败、写明“忙”。
  - 回国：`handleRestore` → `exec(d.m.Restore)` → `lock` 得到 `ErrBusy` → 409 `busy`，确认码放回。
  - 卸载：`handleUninstall` 置 `uninstalling` 后 `runUninstall` → `m.Restore` → `ErrBusy` → 409 `busy`，`uninstalling` 复位、确认码放回。
  - 另一个借用、改规则、改出口：409 `busy`。
  - 只读接口照常应答。
- 反过来：启用、回国、卸载进行中发起借用 → `Exclusive` 得到 `ErrBusy` → 409 `busy`；卸载中 `guard` 先拦（`uninstalling`）；已卸载 → `guard` 回 `uninstalled`（这三个路由都不在 `AllowedAfterUninstall` 里）。
- 已启用时三个操作不加锁（和现在相同）：虚拟机常驻，不存在“借用”，也就不存在“借用结束时停掉”和启用、回国的竞争。回国时状态机自己停虚拟机，这时正在跑的检查失败，原因写“虚拟机正在关闭”。

### ④ 失败即断网

- 回国状态下 Claude 的流量本来就不经虚拟机：没有 utun、没有接管路由和 DNS，Claude 走的是本机网络，由回国拦截（pf 只拦 Anthropic 的地址段）和启动管控挡住。借用不改变这一点，也就不存在“借用失败后 Claude 改走直连”的路径——它在借用前、中、后走的是同一条（被拦的）路。
- 闸门全程关着：`ReasonState` 不被碰；测试里起一个采样协程每 5 毫秒读一次 `status`，借用前、中、后（含四种失败）都断言 `gate_open=false` 且原因里有 `state`。
- 已启用时三个操作用的是常驻的虚拟机，失败只影响这次检查的应答，闸门由现有的复查和管理器决定，和现在一样。

### ⑤ 会话里怎么测

- 全部用假的拉起器（`internal/vmmgr` 的 `fakeLauncher` + 假时钟；`internal/daemon` 的 `dmLauncher` + 真实时钟、很短的时长 `dmTiming`），假 pf 执行器、假地区（`FakeVMRegion`、假 `MacRegion`），假主代理一侧。
- 新增两个“跟虚拟机死活绑定”的假实现，让测试复现真机现象：`vmBoundProber`（虚拟机不在时五项都是 `channel_down`，在时通过）、`vmBoundMainVM`（虚拟机不在时下发报错）。修之前用它们跑 `TestBorrowApplyMainRestored` 应当得到 `channel_down`（先写测试、看它失败，再改代码）。
- 不做真起本项目虚拟机的集成测试：真实模式要 root，会话里不能 sudo；`scripts/dev/vm-up.sh` 起的是外部启动的虚拟机，不经过 `vmmgr`，测不到这条路。真机上的验证由用户卸载、重新构建、重装后再做一次“应用主代理”（验收 g）。

## 4. 验收 a–g 对上的测试

### a. 第三节各条

| 条 | 测试（包#名字） | 断言 |
|---|---|---|
| ① | `internal/daemon#TestBorrowApplyMainRestored` | 管理器管理（`dmLauncher`）、状态回国、有待下发的选择：`POST /v1/main-proxy/apply` 200、`borrowed:true`；基准文件存在；`POST /v1/setup/complete` 200；`launches==1`；应答时进程已退出、阶段 `idle`、`Borrowed=false`；采样全程闸门关；假 pf 执行器的调用记录前后相同（监护间隔调大，回国拦截关；另一个子测试打开回国拦截，断言锚点内容不变）；`FakeVMRegion`、假 `MacRegion` 写入 0 次；`state.json` 字节不变；`/v1/launchguard` 前后相同；`vm_borrow` 事件依次是 starting、pushing、checking、stopping、done |
| ② | `internal/daemon#TestBorrowMainCheckRestored`、`#TestBorrowExitCheckRestored` | 同 ① 的“拉起一次、结束时已退出、闸门关、pf 和地区不动”；main-check 不写基准；exit-check 下发了出口、应答里 `up:true` 和 `note`，结束后该出口 `pushed:false`；间隔内再检查回缓存、拉起次数不变 |
| ③ | `internal/daemon#TestBorrowFailures`，子测试 `launch`、`not_ready`、`push`、`probe` | 每条都断言进程已退出或没起来、阶段 `idle`、`Failures==0`、闸门关；`launch`、`not_ready` 代码 `vm_unavailable`，文字分别含“拉起虚拟机失败”和“秒内没有就绪”；`push` 代码 `main_push_failed`；`probe` 代码 `main_unreachable`，文字是中文原因、不含原因码 |
| ③ 补 | `internal/daemon#TestBorrowNoMainSelected` | 借来的虚拟机、没有保存的选择 → 422 `no_main_selected`，虚拟机已停 |
| ④ | `internal/daemon#TestBorrowBusyEnableRestoreUninstall`、`#TestEnableAfterBorrowStaysResident` | 让健康检查卡住使借用停在等待中：同步启用、异步启用、回国（带确认码）、卸载（带确认码）、改规则、再一次应用主代理都得到 409 `busy`，确认码可以再用；借用结束后启用 200，等过 `StopWait+KillWait` 以上虚拟机仍在运行、`launches==2`、阶段 `running` |
| ④ 补 | `internal/daemon#TestEnableFromRestoredManagedPushesAfterStart` | 管理器管理、回国、有待下发的选择、`vmBoundMainVM`：启用成功；下发发生在拉起并健康之后；之前的写法会失败（先写测试看它失败） |
| ⑤ | `internal/daemon#TestBorrowNotNeededWhenEnabled` | 已启用：三个操作都不再拉起（`launches` 不变）、`borrowed` 为假、结束后阶段仍 `running`、闸门仍开 |
| ⑥ | `internal/daemon#TestRoutesNeverLaunchWhenNotEnabled` | 回国状态、管理器管理：按 `RouteTable()` 逐条（`BorrowRoutes` 和 `ResidentRoutes` 之外的全部）用最小请求体调用一次，`launches==0`、阶段一直 `idle` |
| ⑦ | `internal/daemon#TestVMSourceStateMatrix` | 见下面的矩阵设计 |
| ⑧（补） | `internal/daemon#TestBorrowCrashLeavesNothing`；`cmd/abroad-vm#TestRunParentWatch`（现有） | 借用进行中：状态目录里没有任何文件变化（借用状态只在内存）；用同一个状态目录起一个新的 daemon 和新的拉起器：`launches==0`、阶段 `idle`、状态是回国。虚拟机一侧“父进程没了就自己停”由现有测试覆盖 |

`internal/vmmgr` 的单测（新文件 `internal/vmmgr/borrow_test.go`）：`TestBorrowStartsAndStops`、`TestBorrowWhenWantedNoLaunch`、`TestBorrowHandoverWhenWanted`、`TestBorrowLaunchFailsNoRetry`、`TestBorrowNotReadyTimeout`、`TestBorrowStuckOnRelease`（不拉起第二个）、`TestBorrowClosed`、`TestBorrowFailuresNotCounted`（借用失败后再 `Start`，计数从 0 起）、`TestBorrowGateOnlyVMReason`（只出现 `ReasonVM` 的加清，结束后没有残留）。

**矩阵设计（⑦）**

- 三种虚拟机来源：
  - 外部启动：`ExternalVM{Dir: 空临时目录}`（没在运行）；另加一个实现了 `Start/Stop/Probe` 的测试类型模拟“外部启动且在运行”（走的是 `borrowVM` 里同一个“实现 Probe”的分支）；
  - 由管理器管理：`vmmgr.Manager` + `dmLauncher`；
  - 假的：`FakeSystem` 的 `FakeVM` + `WithFakeMainProxy`。
  - `Prober`、`MainVM`、`ExitVM`、`CheckExit` 一律用跟虚拟机死活绑定的假实现。
- 五种状态：`RESTORED`；`DISABLED`（启用后停用）；`ENABLED`；`ENABLING`（启用停在写入 ENABLING 之后的一步：管理器来源让健康检查卡住，其余来源用阻塞的假 IPv6 快照）；`RESTORING`（回国停在写入 RESTORING 之后的一步：阻塞的假路由删除）。另加一行“崩溃留下的 ENABLING/RESTORING 文件、Recover 没走完”（假 pf 让 Recover 失败）。
- 要经虚拟机的接口：`main-proxy-apply`、`main-check`、`exit-check`；以及会间接经虚拟机、要证明不会冒出内部说法的 `main-select`、`main-source-add`（订阅、不带 via_host）、`envcheck-run`。
- 每一格断言：状态码 2xx，或代码是 `busy`，或代码在 {`vm_unavailable`、`main_push_failed`、`main_unreachable`、`no_main_selected`、`state_incomplete`、`not_enabled`、`via_host_required`、`unsupported`} 里，且 `error` 文字里不含任何 `exitcheck.Cause` 的原值、`通道不通`、`数据通道不可用`（只查 `error` 字段：`no_baseline` 同时是一个 `api.ErrorCode`，不能查整段正文）。另外按格写死期望值，例如：管理器×回国×apply = 200；外部（没运行）×回国×apply = 503 `vm_unavailable`；任意来源×ENABLING×apply = 409 `busy`；残留×apply = 409 `state_incomplete`；管理器×停用×apply = 200 且不新拉起（停用不关虚拟机，常驻）。
- 每格结束后断言：管理器来源的阶段回到这一格开始时的阶段（回国时 `idle`），没有多出来的进程。

### b. 改过的原有断言（第 5 节）

### c. 没有残留

`internal/daemon#TestBorrowNoLeftovers`（带 `-race`）：连续做 20 次借用（成功、四种失败交替）后：每个假进程都已 `Done`、阶段 `idle`、`Borrowed=false`、`d.borrowing` 为假、`status.borrow` 为空、`runtime.NumGoroutine()` 在 2 秒内回到开始时的值（容差 2，沿用 `internal/daemon/dataplane_test.go` 的写法）。`internal/vmmgr#TestBorrowStartsAndStops` 里同样检查协程数。

### d. 清单

第 2 节；`internal/daemon#TestBorrowRouteClassification`：路由表里的每一条要么在 `BorrowRoutes`，要么在 `ResidentRoutes`，要么被 ⑥ 覆盖；新增路由没归类就失败。

### e. 内部说法的检查（第 6 节）

### f. 全量验证

两种构建（默认、`-tags abroaddev`）各跑 `go build ./...`、`go vet ./...`；`GOOS=windows GOARCH=amd64 go build ./...` 和 `go vet ./...`；`GOOS=linux GOARCH=arm64 go build ./...`；`ABROAD_FULL_EXHAUSTIVE=1 go test -timeout 30m -race -tags abroaddev ./internal/... ./cmd/... -count=1` 和 `ABROAD_FULL_EXHAUSTIVE=1 go test -timeout 30m ./internal/... ./cmd/... -count=1` 各完整跑一遍、结果写进交验收的材料；`go run ./cmd/abroad-apigen -check`；`scripts/dev/app-test.sh`；`scripts/lib/buildcheck-selftest.sh` 等各份自测；`scripts/dev/privacy-check.sh --commit` 零命中。

### g. 不执行真实模式

会话里只跑上面这些；真机上由用户卸载、重新构建、重装后再做一次 `sudo abroad main apply`，结果交顾问判定，回填验收清单第 9 节。

## 5. 要改的原有测试断言（验收 b）

逐个核对过 `internal/vmmgr`、`internal/daemon`、`internal/state`、`cmd/abroadd`、`app/Tests` 里和“回国状态虚拟机不运行”有关的断言：

| 位置 | 现在 | 改成 | 理由 |
|---|---|---|---|
| `internal/daemon/vmmgr_test.go:194` 注释 | “状态文件是回国（全新安装）时不拉起” | “状态文件是回国（全新安装）时启动不拉起（不常驻）” | 措辞 |
| `internal/daemon/vmmgr_test.go:205` 失败信息 | “回国状态不应拉起虚拟机” | “回国状态启动时虚拟机不应常驻” | 措辞；判断条件（启动后 `launches==0` 且 `idle`）不变 |

其余照旧通过、不改：`internal/state/machine_test.go:104、116`（回国、停用带关虚拟机之后“虚拟机没关”）仍成立——回国仍然关虚拟机；`TestVMManagerGateEventOrder` 的“回国”一步仍是 `idle`；`app/Tests/AbroadKitTests/VMManagerTests.swift` 的阶段名测试加了 `Borrowed` 字段后照常解码（可选字段）。

**不属于这一类、但会被第 1 步影响的一条**（请顾问裁定）：`internal/daemon/errcode_test.go:278`：`failedItems(r)` 不等于 `"mainland（mainland）"` 时要求结果含 `"mainland"`。新格式是“是否在大陆：出口在中国大陆”，不再含英文项名，这条会失败。两个办法：(甲) 把断言改成“含‘是否在大陆’且不含出口 IP”（推荐，列为本次唯一的例外）；(乙) 新格式保留英文项名作括注（“是否在大陆（mainland）：出口在中国大陆”），原断言不动，但英文项名本身也是内部说法。

## 6. 面向用户的“通道不通”怎么换（验收 e）

**原因文字表**（`exitcheck.CauseText`）：

| 原因码 | 给人看的 |
|---|---|
| ok / not_needed | 通过 / 这条规则不需要 |
| mainland | 出口在中国大陆 |
| rule_violation | 出口变了，不符合出口变化规则 |
| unexpected_reply | 收到的应答不对（可能被劫持，或证书不对） |
| timeout | 超时 |
| connect_failed | 连不上 |
| rate_limited | 查询服务暂时限流或出错 |
| bad_format | 查询服务的应答格式不对 |
| channel_down | 到虚拟机的数据通道打不开（虚拟机没有运行或刚刚退出） |
| no_baseline | 没有出口基准 |

**按场景给的原因**（handler 层，优先于逐项列举）：

| 场景 | 代码 | 文字（`usererror`：做什么失败了、原因、下一步） |
|---|---|---|
| 管理器拉不起来 | vm_unavailable | 启动虚拟机失败：拉起虚拟机失败（脱敏后的原因）。下一步：在“引擎与虚拟机”页看最近的错误；本机网络没有改动 |
| 拉起后没就绪 | vm_unavailable | 启动虚拟机失败：虚拟机在 100 秒内没有就绪。下一步同上 |
| 外部启动、没在运行 | vm_unavailable | 虚拟机由外部启动，现在没有在运行：请先启动虚拟机再试 |
| 已启用、常驻的虚拟机没就绪 | vm_unavailable | 虚拟机现在没有就绪（阶段：正在重新拉起 / 已停止重启…）：等它恢复后再试 |
| 检查只有 channel_down | vm_unavailable | 到虚拟机的数据通道打不开……（不再说“通道不通”） |
| 下发失败 | main_push_failed | 把主代理配置下发到虚拟机失败：（中文原因）。下一步：检查节点配置；虚拟机已经关闭，本机网络没有改动 |
| 主代理不通 | main_unreachable | 主代理不通：HTTPS 访问：连不上；出口 IP：超时。下一步：确认节点可用后再试；虚拟机已经关闭 |
| 出口在大陆等明确不通过 | 原有代码 | 这一轮检查没有通过：是否在大陆：出口在中国大陆…… |
| 没选节点 | no_main_selected | 还没有选主代理节点：请先选好节点再应用 |
| 残留的半截状态 | state_incomplete | 上一次启用或回国没有完成：请先执行“恢复本机网络”，再试 |

**检查怎么写**：

1. `internal/exitcheck#TestCauseTextCoversAll`、`#TestItemLabelCoversAll`：判定表里每个原因、每个检查项都有中文，且文字里没有 ASCII 字母组成的原因码。
2. `internal/daemon#TestFailedItemsNoRawCause`：所有原因组合下 `failedItems` 不含任何 `Cause` 原值。
3. `internal/daemon#TestNoInternalCauseWordsInUserOutput`（源码检查，用 `internal/srccheck`）：
   - Go：除 `internal/exitcheck` 自身和测试文件外，禁止出现 `string(<…>.Cause)` 的转换，以及字面量 `"channel_down"`、`"通道不通"`、`"数据通道不可用"`；
   - Swift（`app/Sources/**`）、命令行（`cmd/abroad/**`）、脚本（`scripts/**/*.sh`）：字面量 `channel_down`、`通道不通` 不得出现在给人看的字符串里（扫描范围和允许名单写在测试里）。
4. 矩阵测试（⑦）对每个接口的 `error` 字段做同样的检查，覆盖运行时拼出来的文字。
5. `internal/envcheck` 加 `TestExitItemsNoRawCause`。

## 7. 命令行和应用的显示（前任第 ⑨ 条）

- **命令行**（`cmd/abroad`）：`main apply`、`main check`、`exit check` 发请求前起一个 `borrowProgress`：另开一个 `apiclient.Client` 订阅 `/v1/events`，只打印来源 `vm_borrow` 的事件，写到**标准错误**（`--json` 时标准输出只放最终应答）：
  ```
  正在启动虚拟机（第一次可能要一两分钟）…
  正在下发主代理…
  正在检查出口…
  正在关闭虚拟机…
  已建立出口基准：出口在 US（America/Los_Angeles）
  ```
  应答回来后取消订阅；订阅失败时不打印进度，不影响结果。`codeOf` 加新代码：`vm_unavailable`、`main_push_failed`、`main_unreachable`、`no_main_selected`、`state_incomplete` 打印原文、退出码 `ExitFail`。测试：`cmd/abroad#TestCLIMainApplyProgress`（假后台推几条 `vm_borrow` 事件：标准错误有进度、`--json` 时标准输出只有 JSON）、`#TestCLIBorrowErrorCodes`。
- **应用**：前任写的“已有任务机制”现在只有启用、停用的异步任务，应用里也没有用；而应用主代理有两步确认的 409 流程，改成异步任务改动大。计划用**已有的事件推送和状态**：`StatusModel` 每收到事件就刷新状态（现有行为），状态里的 `borrow.step` 就是进度。`MainProxyModel.applyState` 在 `.checking` 时显示 `L10n.borrowStep(step)`（“正在启动虚拟机”“正在检查出口”“正在关闭虚拟机”），`MainProxyPage`、`SetupView` 的应用按钮下方显示；`EnginesPage` 的管理器一行在 `borrowed` 时显示“临时运行：做出口检查，结束后自动关闭”；`ExitsPage` 显示 `note`；新错误代码在 `Strings.swift` 里有对应的说明。测试：`swift:borrowStepNames`、`swift:applyShowsBorrowProgress`、`swift:borrowErrorCodesHaveText`、`swift:enginesShowsBorrowed`。**请顾问确认用事件和状态代替任务机制。**

## 8. 设计文档要回写的节（v0.69 → **v0.70**）

- 文件头：`草案 v0.70（2026-11-27，借用虚拟机：回国状态下应用主代理等经主代理的检查临时拉起虚拟机，见 7.2、10、11.1、11.2、11.3、16）`。
- 第 10 节“M6-2a 起”一段：“要不要虚拟机”一条改成“`RESTORED` 时不常驻；只在一次互斥的操作内部临时运行（借用），操作结束时已经停掉”，新增“借用”一条（入口、互斥、失败不重试、结束必停、崩溃后不留进程、期间不动 pf/路由/DNS/地区、闸门保持关）；“状态接口”一条加 `borrowed` 和事件来源 `vm_borrow`；补一句启用时主代理在第 ④ 步虚拟机就绪后下发。
- 7.2：“应用主代理”的顺序补上未启用时的借用：拉起 → 按保存的选择下发（不记“已下发”）→ 带位置查询的检查 → 停虚拟机 → 保存基准、锁定目标；已启用时不变。
- 11.1：三个接口的说明（未启用时借用、耗时一两分钟、应答里的 `borrowed`/`note`）；错误代码表加五个代码；`/v1/status` 的 `borrow`；`/v1/engines` 的 `vm_manager.borrowed`；“只读接口不拉起虚拟机”一句。
- 11.2：应用里的进度和引擎页的“临时运行”。11.3：命令行的进度输出写到标准错误。
- 16：v0.70 的修订记录（问题、根因、修法九条的落实、启用下发顺序的顺带修复、原因文字表）。

## 9. 风险和拿不准的地方

1. **借用期间监护被跳过**：`Supervisor.Tick` 拿不到忙标记就跳过这一轮，借用最长约 4 分钟。回国拦截模式下这段时间里锚点被别人清掉不会被立刻重新加载。缓解：借用结束立刻 `d.sup.Kick()`。另一种做法是给借用单独一把锁、在启用/回国/卸载里另查，改动面大、容易漏，不推荐。请顾问判断是否接受。
2. **R1 没有结论**：真机上第一次由 abroadd 以专用用户拉起虚拟机就是这次借用。起不来时用户会看到“启动虚拟机失败：……”和脱敏后的原因（不再是 channel_down），这次的事件和诊断包可以作为 R1 的部分证据。
3. **启用下发顺序的修复**超出“借用”的字面范围（第 1 节第 6 步），但不修的话用户建好基准后启用会失败。改后已有虚拟机就绪时仍在启用前下发；只有虚拟机没就绪时才挪到第 ④ 步之后，那时下发失败会走回滚（短暂断网后恢复），比现在“启用前失败、什么都不改”多一段。
4. `errcode_test.go:278` 的断言（第 5 节），需要裁定。
5. **时长**：最坏约 100+80+25+25 秒，在应用 300 秒、命令行 5 分钟之内，但余量不大；合盖睡眠跨过等就绪的时限会判为没就绪（已知行为，失败即停，偏安全）。
6. 分流出口的借用检查：结果在虚拟机停掉后就“过期”，计划把可用状态标成“没有启用”，只在应答里给这次结果；也可以改成未启用时不检查（第 1 节第 4 步）。
7. 数据通道的会话：虚拟机停掉后 `wire.Dialer` 的会话会关闭，下次 `session()` 看到 `IsClosed` 会重新拨号，`Open` 也有重试，预计没问题；矩阵里连续借用两次覆盖这一点（假的开流器，测不到真实 yamux）。
8. 借用期间管理器照常加清 `ReasonVM`，事件历史里会多出“gate_closed vm：虚拟机启动中”“虚拟机正在关闭”；闸门原来就关着，应用不会因此发“保护断开”的通知（现有 `vmReasonNotifiesOnlyWhenGateWasOpen` 已覆盖这一序列）。
9. 停用状态的虚拟机是常驻的（停用不关虚拟机），所以实际只有回国状态会借用；停用后管理器若已 `failed`，三个操作给“虚拟机已停止重启”的明确原因，不会借用重拉。是否应当在这种情况下重试一次，请顾问决定（计划：不重试，提示用户重新启用）。
10. 虚拟机在回国状态下的出站经宿主机现有网络，宿主机上别的代理软件是否接管这部分流量不由我们决定；借用不改宿主机网络设置，也不碰本机会话依赖的代理。
