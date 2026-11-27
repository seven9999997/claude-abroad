# 借用虚拟机修复：实施计划（交架构顾问审）

> 版本：**v2**（2026-11-27）。只是计划，代码还没改。
> 依据：`docs/PROGRESS.md` 的“2026-11-27 第一条真机结果”和“借用虚拟机修复的验收标准（前任 architect 2026-11-27 原文）”（做法九条、验收 a–g）；设计 v0.69 第 7.2、10 节的“M6-2a 起”一段、11.1。
> 会话里的边界：不改宿主机网络，不碰本机会话依赖的代理（见 `docs/LOCAL.md`），不用 sudo，不运行真实模式，不提交。
>
> **v1 → v2 改动对照**（按顾问 2026-11-27 审 v1 的意见）：
>
> | 意见 | v2 怎么改 | 在哪 |
> |---|---|---|
> | 必须改 1：借用期间回国拦截的巡检不能停 | `state.Machine` 新增 `ExclusiveHold`（同一个忙标记 + 原子标记 `held`）；`Supervisor.Tick` 抢不到忙标记且 `held` 为真时，状态是 RESTORED、处在回国拦截模式就照常 `Patrol.Step`。新增 `internal/state` 单测两条、`TestBorrowApplyMainRestored` 子测试 `return_block_reload` | 1 节第 3 步、3 节③、4 节 a①、9 节风险 1、8 节第 10 节 |
> | 必须改 2：启用时下发挪到第 ④ 步之后，失败路径要测 | 补 `TestEnableManagedPushFailsAtStep4`、`TestMainPushVMStartSkipsSameDigest` | 1 节第 7 步、4 节 a④ |
> | 裁定①：enableWithMain 同类缺口并入 | 第 1 节第 7 步不再标“待确认” | 0 节、1 节第 7 步 |
> | 裁定②：errcode_test.go:278 选甲 | 唯一例外，不保留英文项名括注 | 5 节 |
> | 裁定③：进度用状态 + 事件；事件和结果行不出现 IP、节点服务器、端口 | 加 `TestBorrowEventsNoAddress`（正则断言无 IPv4/IPv6）、命令行最后一行只写国家和时区 | 1 节第 4、9 步、7 节、4 节 a① |
> | 裁定④：分流出口检查未启用时不借用 | exit-check 未启用回 409 `not_enabled`，移到“不许拉起”；`BorrowRoutes` 只剩两条；删 `TestBorrowExitCheckRestored`；矩阵里 exit-check 期望 `not_enabled` | 1 节第 5 步、2 节、4 节、9 节风险 6、8 节第 16 节 |
> | 裁定⑤：时长接受；指出应用里请求时限的代码位置 | 应用两个接口的时限就是 300 秒，代码位置写明；合盖睡眠写进设计第 10 节 | 7 节、8 节、9 节风险 5 |
> | 风险 9：停用状态下管理器已停止重启时不重试 | 定为不重试、提示重新启用 | 9 节风险 9、3 节① |

## 0. 问题和根因（核对代码后）

- `internal/daemon/vmmgr.go` 的 `startVMManager`：状态文件是 `RESTORED` 时调用 `SetWanted(false)`，管理器停在 `idle`。
- `internal/daemon/region_api.go` 的 `handleApplyMainProxy`：直接调 `Sys.Baseline.Probe` → `exitcheck.Checker.Check(ctx,"main",…)`。Checker 的 `Opener` 是 `dp.Streams()`，经 `data.sock` 进虚拟机。虚拟机没运行时 `wire.Dialer` 拨不通，`causeOf` 把错误归成 `CauseChannelDown`，五项都是 `channel_down`。`recheck.go` 的 `failedItems` 把原因码原样拼进 `usererror`（“https（channel_down）；…”），命令行和应用都把这段话显示给用户。
- **同类缺口（裁定①：并入本任务）**：`enableWithMain` 在 `m.Enable` 之前调 `ensureMainPushed`。由 abroadd 管理虚拟机、状态是回国、又有“待下发”的选择（`pending.json`）时，这一步对没运行的虚拟机 `PushMainHops`，必然失败，启用被拒。原因相同：M6-2a 之前的代码都假定虚拟机是外部启动、一直在。假的 `FakeMainVM` 永远成功，所以测试没发现。
- `handleApplyMainProxy` 现在不下发保存的选择（注释写着“主代理配置下发给虚拟机仍由开发工具在此之前完成”）。借来的虚拟机是新起的，里面没有主代理配置，所以借用时必须先按保存的选择下发，再检查。

## 1. 改哪些文件、按什么顺序、每处改什么

按依赖顺序，每一步做完都能编译、单测通过。

### 第 1 步：面向用户的原因文字（不依赖借用，先做）

| 文件 | 函数 | 改什么 |
|---|---|---|
| `internal/exitcheck/text.go`（新） | `CauseText(c Cause) string`、`ItemLabel(name string) string` | 每个 `Cause` 一句中文（第 6 节的表）；每个检查项一个中文名（https→“HTTPS 访问”、ip→“出口 IP”、mainland→“是否在大陆”、rule→“出口变化规则”、doh→“加密 DNS”、geo→“位置”、udp→“UDP”）。表里没有的按“原因不明”，不回显原值 |
| `internal/exitcheck/result.go` | 不改判定表 | `Cause` 常量和 JSON 里的 `cause` 字段不变（给程序看的稳定代码），只是不再拼进给人看的文字 |
| `internal/daemon/recheck.go` | `failedItems` | 改成“HTTPS 访问：连不上；出口 IP：超时”（`ItemLabel` + `CauseText`），不带英文项名 |
| `internal/envcheck/exit_items.go` | 第 88、90 行 | `string(rule.Cause)` 换成 `exitcheck.CauseText(rule.Cause)` |
| `internal/daemon/baseline.go` | `Probe` | 失败时返回带类型的错误 `*ProbeFailure{Report, Unreachable bool, OnlyChannel bool}`（外面仍包 `usererror`，文字用新的 `failedItems`）。`Unreachable`：没通过的项全是网络类原因；`OnlyChannel`：全是 `channel_down`。明确不通过（大陆、证书不对、规则）照旧 |

### 第 2 步：`internal/vmmgr` 加“借用”入口

文件 `internal/vmmgr/manager.go`：

- `Manager` 新字段（都在 `mu` 下）：`borrowed bool`；`failKind`（最近一次失败的类别：`launch`、`notReady`、`exited`）。
- `isWanted()` 改为 `wanted || borrowed`。`Run`、`supervise` 其余逻辑不动：借用期间同样拉起、健康检查、加清 `ReasonVM`、健康时 `Kick`。
- `Run` 在拉起失败处记 `failKind=launch`；`supervise` 返回错误处按情形记 `notReady`/`exited`。
- 新方法 `Borrow(ctx) (release func(context.Context) error, borrowed bool, err error)`：
  1. 锁内：`closed` → `ErrClosed`；`wanted` 为真（常驻）→ 不借用，在 `Timing.BorrowWait` 内等到 `running`（`failed`/`stuck` 立即返回带原因的错误），`release` 是空操作。
  2. 否则 `borrowed=true`、清零 `Failures`，`poke`；在 `BorrowWait` 内等：`running` → 成功；`backoff`（第一次失败就算）/`failed`/`stuck`/`closed` → 立即失败，**不重试**；超时 → 失败。失败时 `Borrow` 先调 `release` 停掉再返回。
  3. 错误类型 `*BorrowError{Kind, Err}`：`KindLaunch`（拉不起来）、`KindNotReady`（等健康超时，含 `BorrowWait` 到点）、`KindStuck`。
- `release(ctx)`：锁内 `borrowed=false`；此刻 `wanted` 为真（只可能是被直接调用 `SetWanted(true)`，正常流程被互斥挡住）→ 交还、不停；否则清零 `Failures`（借用的失败不计入常驻的“连续失败到上限”），`poke`，等到 `idle`（`stuck` → `ErrStuck`；`closed` → nil），时限 `StopWait+KillWait+5s`。可重复调用。
- `Timing` 加 `BorrowWait`（默认 `StartTimeout+10s`=100 秒）；为 0 时按 `StartTimeout+10s`。
- `Status` 加 `Borrowed bool \`json:"borrowed,omitempty"\``。
- 包注释的不变量加一条：“回国状态虚拟机不常驻；只在一次互斥的操作内部被借用，操作结束时已经停掉；借用期间失败不重试”。

### 第 3 步：状态机的借用互斥入口（必须改 1）

文件 `internal/state/machine.go`、`internal/state/supervise.go`：

- `Machine` 加 `held atomic.Bool`。
- 新方法 `func (m *Machine) ExclusiveHold(fn func() error) error`：`lockChange()`（和 `Exclusive`、`Enable`、`Disable`、`Restore`、`SyncRegion` 占同一个忙标记，同样过 `Admit`）→ `held.Store(true)` → `defer { held.Store(false); unlock() }`（先清 `held` 再放锁）→ `fn()`。注释写明：只给借用虚拟机用；持有期间调用方不得改状态文件、不得碰 pf、路由、DNS、地区。
- `Exclusive`、`Enable`、`Disable`、`Restore`、`SyncRegion`、`ReapplyRegion`、`Tick` 自己的持锁都不置 `held`，行为不变。
- `Supervisor.Tick`：`busy.CompareAndSwap` 失败时，原来直接 `return nil`；改为：
  ```go
  if !m.busy.CompareAndSwap(false, true) {
      if m.held.Load() { return s.heldTick(ctx) }
      return nil
  }
  ```
  `heldTick`：读状态文件；状态是 `RESTORED` 且 `s.Patrol` 实现 `RestoredMode()` 且为真 → `s.Patrol.Step(ctx)`；其他情况（读不出、别的状态、不在回国拦截模式）什么都不做。不碰 `Supervisor` 的 `takeoverSet` 等“只在持有忙标记时访问”的字段，不关闸门、不发状态事件。
- 为什么安全：持有 `ExclusiveHold` 期间状态文件不会变（改状态的操作都要忙标记）；借用本身不碰 pf，此刻会碰 pf 的只有巡检。`pf.Patrol.Step` 自带 `p.mu`，现有的 `handleLANPut` 也是不持忙标记直接调 `d.ppf.P.Step`，并发安全已有先例。启用、回国、卸载期间仍整轮跳过（它们自己在动 pf，不能和巡检交错），和现在一样。

### 第 4 步：daemon 的借用框架（新文件 `internal/daemon/vmborrow.go`）

- 类型 `vmUse struct{ borrowed bool; release func(context.Context) error }`。
- `func (d *Daemon) borrowVM(ctx, op string) (vmUse, error)`：
  - `d.vmm != nil` → `d.vmm.Borrow`，错误转成 `*VMUnavailable{Kind}`；
  - 否则 `Sys.VM` 实现 `interface{ Probe(context.Context) error }`（`ExternalVM`）→ 只确认在运行，不拉起也不停；不在 → `VMUnavailable{KindExternalDown}`；
  - 否则（假系统的 `FakeVM`）→ 当作可用。
- `func (d *Daemon) withVM(ctx, op string, fn func(ctx context.Context, u vmUse) error) error`：两个可借用的接口都走这里：
  - 状态是 `ENABLED`：不加锁、不借用；先确认虚拟机就绪（管理器 `Ready()`；外部 `Probe`），不就绪 → `VMUnavailable{KindNotRunning}`（带阶段的中文名），然后调 `fn`。
  - 其他状态：`d.m.ExclusiveHold(...)` 里做：
    1. 锁内重读状态：`ENABLED`（入口之后刚好变了）→ 回“忙，请再试一次”；`ENABLING`/`RESTORING`（崩溃后 Recover 没走完留下的）→ `ErrStateIncomplete`，不借用；停用状态且管理器 `failed` → `VMUnavailable{KindGivenUp}`（“虚拟机已停止重启：请重新启用”），不重试（风险 9）；
    2. `d.borrowing.Store(true)`，`borrowVM`；
    3. `fn(ctx, u)`；
    4. **不论成败** `u.release(context.WithoutCancel(ctx))`；
    5. `d.borrowing.Store(false)`，清进度。
  - 整体时限 `BorrowTimeout = 4*time.Minute`。
- 进度：`func (d *Daemon) borrowStep(op, step, detail string)` 写 `d.borrow atomic.Pointer[BorrowView]` 并发事件（来源 `vm_borrow`）。步骤 `starting` → `pushing` → `checking` → `stopping` → `done`/`failed`；没借用时只有 `checking`。**事件详情只写步骤和中文原因**；`done` 的详情只写“出口在 <国家>（<时区>）”；失败原因先过 `redact.String`，且由 `failedItems`/`CauseText` 组成，不含地址、节点服务器和端口（裁定③）。
- 错误到应答 `writeVMErr(w, status, err)`：`state.ErrBusy` → 409 `busy`；`VMUnavailable` → 503 `vm_unavailable`；下发失败 → 502 `main_push_failed`；`ProbeFailure` 且 `OnlyChannel` → 503 `vm_unavailable`；`ProbeFailure` 且 `Unreachable` → 原状态码 + `main_unreachable`；`ErrNoMainSelected` → 422 `no_main_selected`；`ErrStateIncomplete` → 409 `state_incomplete`；其余照旧。
- 路由分类（验收 d 的源头，放在 `routes.go`）：`BorrowRoutes = {"main-proxy-apply","main-check"}`；`ResidentRoutes = {"enable"}`；其余全部“不许拉起”。

### 第 5 步：两个可借用的处理函数和 exit-check

1. **`region_api.go` `handleApplyMainProxy`**
   - `ENABLED`：现有顺序不变（`Probe` → `finishMainProxy`，地区会变时两步确认、`SyncRegion`），只在 `Probe` 前过 `withVM` 的就绪检查。这里**不能**持锁：`SyncRegion` 要拿忙标记。
   - 其他状态：`withVM` 里 `pushForProbe` → `borrowStep(checking)` → `Probe` → release → `finishMainProxy` 写进 `bufferedResponse`（未启用时只保存基准、锁定 `region.json`，不确认、不调 `SyncRegion`、不写 Mac 和虚拟机的地区）→ 放锁 → `flushTo(w)`。调用方拿到应答时虚拟机已停、忙标记已放。
   - `MainProxyResult` 加 `Borrowed bool`；借用时 `Note`：“虚拟机是临时启动的，检查完已经关闭”。
2. **`mainproxy_api.go` `handleMainCheck`**：同上，不保存；回 `BaselineSummary`（加 `Borrowed`）。
3. **`m5rest_api.go` `handleExitCheck`（裁定④：不借用）**：在 `exits.Has`、`CheckExit` 判断之后加：`d.fileState() != state.Enabled` → 409 `not_enabled`，文字“没有启用时不检查分流出口：启用后会自动检查”。不碰 `exitChk`、不拉起。
- 共用的 **`pushForProbe(ctx, u vmUse) error`**（`mainproxy_api.go`）：
  - `managedMain()` 为假：借来的虚拟机里没有配置 → `ErrNoMainSelected`（“还没有选主代理节点：请先选好节点再应用”）；不是借来的（外部启动、开发工具手动下发）→ 什么都不做，和现在相同。
  - 否则持 `exitPushMu` 调 `pushManagedLocked(ctx, vm, "应用主代理", persist: !u.borrowed)`。
- **`pushManagedLocked` 加参数 `persist bool`**：为假时只下发，不写“已下发”记录、不删 `pending.json`、不发 `pushed` 事件。`ensureMainPushed` 传 `true`。

### 第 6 步：只读接口和其余改动类接口

不改逻辑（exit-check 除外，见上），由测试保证不拉起（第 2.2 节）。

### 第 7 步：启用时主代理的下发顺序（裁定①，必须改 2）

- `region_api.go` `enableWithMain`：
  - 先看 `d.borrowing`：为真 → `state.ErrBusy`（“正在临时启动虚拟机做出口检查”），不碰虚拟机；
  - 虚拟机此刻就绪（管理器 `Ready()==nil`，或外部启动）才在 `m.Enable` 之前 `ensureMainPushed`；不就绪就跳过。
- `daemon.go` 组装状态机处：`d.m.D.VM = mainPushVM{inner: s.VM, d: d}`（新类型放在 `vmborrow.go`）。`mainPushVM.Start(ctx)` = `inner.Start(ctx)` 之后 `d.ensureMainPushed(ctx)`；`ensureMainPushed` → `pushManagedLocked` 已有“虚拟机里的摘要和‘已下发’记录都是这份就跳过”，所以 `SyncRegion` 等重复调用 Start 不会重复下发。`Stop` 直接转发。`recheckFunc`、`startVMManager` 仍拿原始的 `s.VM`。
- 下发失败：错误包成 `MainPushError`，`Machine.Enable` 第 ④ 步失败 → 现有回滚（停虚拟机、清 pf、回 RESTORED）；`handleEnable` 的应答代码映射成 `main_push_failed`（`opErrCode` 加一支 `errors.As(err, &MainPushError{})`），文字是“启用：把主代理配置下发到虚拟机失败（中文原因）。已回到启用前的状态，本机网络已恢复……”。

### 第 8 步：状态和接口类型

- `internal/api/errors.go`：`ErrorCodes` 加 `vm_unavailable`、`main_push_failed`、`main_unreachable`、`no_main_selected`、`state_incomplete`。
- `daemon.go`：`Status` 加 `Borrow *BorrowView \`json:"borrow,omitempty"\``（`op`、`step`、`since`）；`Daemon` 加 `borrowing atomic.Bool`、`borrow atomic.Pointer[BorrowView]`。
- `internal/daemon/vmmgr.go`：只改注释（“回国状态不常驻；借用见 vmborrow.go”）。
- 重新生成应用的接口类型：`go run ./cmd/abroad-apigen`，`-check` 通过。

### 第 9 步：命令行（第 7 节）

`cmd/abroad/commands.go` 的 `cmdMainApply`、`cmdMainCheck`、`cmdExitCheck`（只是新代码的说明）；`cmd/abroad/main.go` 的 `codeOf`、新 helper `borrowProgress`。

### 第 10 步：应用（第 7 节）

`app/Sources/AbroadKit/Strings.swift`、`StatusModel.swift`、`MainProxyModel.swift`；`app/Sources/ClaudeAbroad/MainProxyPage.swift`、`SetupView.swift`、`EnginesPage.swift`、`ExitsPage.swift`（未启用时 exit-check 的提示）。

### 第 11 步：文档

设计文档（第 8 节）、`docs/m6-acceptance.md` 第 9 节那一行（“已修，待用户重装后再做一次应用主代理”）、进度单。

## 2. 清单（验收 d）

### 2.1 可以借用的操作（未启用时在 `ExclusiveHold` 内借用）

| 路由 ID | 处理函数 | 经过的函数 | 为什么要经虚拟机 |
|---|---|---|---|
| `main-proxy-apply` POST `/v1/main-proxy/apply` | `handleApplyMainProxy` | `withVM` → `Machine.ExclusiveHold` → `borrowVM`（`vmmgr.Manager.Borrow`）→ `pushForProbe` → `pushManagedLocked(persist=false)` → `BaselineService.Probe` → `exitcheck.Checker.Check`（`dp.Streams()` → `data.sock`）→ release → `finishMainProxy`（`BaselineService.Save`、`mainProxyTarget`、`applyTarget(enabled=false)` → `Regions.Lock`、`envKick`） | 出口检查经虚拟机里的主代理 |
| `main-check` POST `/v1/main-proxy/check` | `handleMainCheck` | `withVM` → `ExclusiveHold` → `borrowVM` → `pushForProbe` → `Probe` → release | 同上，只检查不保存 |

由状态机拉起并常驻、不算借用：`enable`（`handleEnable` → `enableWithMain` → `Machine.Enable` 第 ④ 步 `mainPushVM.Start`）。

### 2.2 不许拉起的接口（逐个核对过处理函数）

只读（GET）：`status`（`handleStatus` → `Status`，读 `vmm.Status()`）、`restore-preview`、`exits-list`、`exit-get`（读 `exitAvail`）、`rules-get`、`main-get`（`main.view`）、`main-exit`（读基准文件）、`main-sources`、`region-get`、`envcheck-get`、`envcheck-history`、`envcheck-settings-get`、`diagnostics`、`lan-get`、`claude-proxy-get`、`preflight`、`apps`、`setup-get`、`engines`（`handleEngines`：`vmm.Status()`、`MainVM.Health` 只读）、`claude-domains`、`launchguard`、`job-get`、`events`。
未启用时给上次的结果：`main-exit` 回上次保存的基准（`at` 标明时间）；`exits-list`/`exit-get` 回上次的可用状态；`engines` 不可达时如实给 `reachable:false` 和管理器阶段。

改动类、但未启用时不经虚拟机：`exit-check`（未启用 409 `not_enabled`，裁定④）、`disable`、`restore`、`restore-alias`、`uninstall`、`exit-put`、`exit-delete`、`rules-put`、`main-select`（未启用时只保存）、`main-source-add`、`main-source-refresh`（未启用时 `via_host` 经直连拨号模块，或 `via_host_required`）、`main-source-delete`、`main-lan-confirm`、`region-put`（未启用时只锁定）、`envcheck-run`（未启用 409 `not_enabled`）、`envcheck-history-clear`、`envcheck-device`、`envcheck-settings-put`、`lan-put`、`claude-proxy-put`、`claude-domains-clear`、`setup-complete`。
后台路径也不拉起：出口复查（`recheckFunc` 对管理器只读 `Ready()`，只在启用时跑）、出口循环（`exitLoop.run` 只在启用时 `round`）。

## 3. 顾问要求的五点

### ① 未启用时借用的范围

- 只用来让第 2.1 节的两个操作经主代理做检查。借用期间只做三件事：拉起 `abroad-vm` 子进程、经 `ctl.sock` 下发主代理配置、经 `data.sock` 开检查用的流。
- 不放行任何本机流量：闸门的 `state` 原因一直在，借用不碰 `ReasonState`；管理器加清的只是 `ReasonVM`，清掉它不会让闸门打开。没有 utun、没有接管路由和 DNS，本机应用的流量到不了虚拟机；第四道本机代理端口看闸门，也拒绝。
- 不改宿主机的路由、DNS、IPv6、pf：借用不调用状态机的任何一步（`applyPF`、`rebuild`、`Routes`、`DNS`、`IPv6`、`Tun` 都不碰）。原因：
  - 回国状态下 pf 里只有回国拦截那一组（只拦 Anthropic 的地址段；没打开启动管控的安装连这一组都没有），没有断网规则，也没有放行专用用户的规则——没有什么需要为虚拟机“打开”的；借用期间巡检照常重载这一组（第 1 节第 3 步）；
  - 虚拟机的网卡只有用户态网络（`VZFileHandleNetworkDeviceAttachment` + gvisor 网络栈，设计 4.5，`internal/vmhost` 里没有 NAT 和桥接），宿主机上不建网桥、不加 NAT 规则、不改路由；虚拟机的出站就是 `abroad-vm` 进程自己的套接字，经宿主机现有的网络出去；
  - 检查的目标（`--exit-host` 的 204 地址、位置查询、DoH）都不在 Anthropic 的地址段里，回国拦截碰不到；
  - 虚拟机里的出站白名单只放行主代理第一跳，不替本机别的流量转发。
- 不改时区和语言：未启用时 `finishMainProxy` 只锁定 `region.json`（目标），不调 `SyncRegion`。
- 启动管控不受影响：`launchGuardDecision` 看状态和闸门原因，借用不改它们；测试比较借用前、中、后的 `GET /v1/launchguard`。
- 停用状态下管理器已停止重启（`failed`）时不借用、不重试，提示用户重新启用（风险 9）。

### ② 用完怎么收

| 情况 | 怎么收 |
|---|---|
| 成功 | `withVM` 第 4 步 `release`：`borrowed=false`，不要求常驻 → 管理循环在 `supervise` 收到 `ctl` → `stopProc`（SIGTERM，20 秒后 SIGKILL）→ `idle`；`release` 等到 `idle` 才返回，之后才放忙标记、发应答 |
| 失败（拉不起来、没健康、下发失败、检查不通过） | 同一个 `release`（`withVM` 无条件调；`Borrow` 自己失败时也先 `release`）。拉起失败时管理器在退避里，`release` 的 `poke` 让它回到循环开头、看到不需要 → `idle` |
| 超时 | `BorrowWait` 到点 → `Borrow` 返回 `KindNotReady` 并 `release`；整体 `BorrowTimeout` 到点 → `fn` 的 ctx 取消，`release` 用脱离取消的 ctx 照样执行 |
| 停不下来 | `release` 返回 `ErrStuck`：阶段 `stuck`，不拉起第二个（现有不变量）；应答照常按操作结果给出，另在 `Note` 和事件里写“虚拟机没能按时停下，正在等它退出” |
| abroadd 崩溃或被强制结束 | `abroad-vm` 带 `--parent-watch`，标准输入写端随 abroadd 消失，读到 EOF 自行停止（现有 `cmd/abroad-vm#TestRunParentWatch`）。借用状态只在内存里（`borrowed`、`held`、`borrowing` 都不落盘）；重启后状态文件仍是回国 → `SetWanted(false)` → `idle`；旧进程没退干净时单实例锁让新的拉不起来 |
| abroadd 正常退出 | `Shutdown` 取消管理循环，循环结束时关掉自己拉起的进程（现有）；`Borrow`/`release` 看到 `closed` 返回 |

交还给 vmmgr：借用结束时管理器回到“按 `wanted` 管”的普通状态；正常流程里借用期间 `wanted` 不可能变，所以结束时总是停掉。`release` 里“`wanted` 为真就交还”只是防御，有单测。

### ③ 互斥

- 用**状态机的忙标记**，经新入口 `state.Machine.ExclusiveHold`（`lockChange`：CAS `busy`，再过 `Admit`——卸载中或已卸载时拒绝；另置 `held`）。它和 `Enable`、`Disable`、`Restore`、`SyncRegion`、`ReapplyRegion`、`Exclusive`（改规则、改出口）是同一把。
- 和巡检的关系（必须改 1）：`Supervisor.Tick` 抢不到忙标记时看 `held`：为真（借用中）且状态是 `RESTORED`、处在回国拦截模式 → 照常 `Patrol.Step`，锚点被清掉在一轮内重新加载；其他状态什么都不做。别的持锁者不置 `held`，`Tick` 照旧整轮跳过。G9.2 在借用期间不退化。
- 借用期间：
  - 启用：`enableWithMain` 先看 `d.borrowing` → 409 `busy`（不碰虚拟机）；错过这一判断时 `m.Enable` 的 `lockChange` 也得到 `ErrBusy` → 409 `busy`。异步启用的任务失败、写明“忙”。
  - 回国：`handleRestore` → `exec(d.m.Restore)` → `lock` 得到 `ErrBusy` → 409 `busy`，确认码放回。
  - 卸载：`handleUninstall` 置 `uninstalling` 后 `runUninstall` → `m.Restore` → `ErrBusy` → 409 `busy`，`uninstalling` 复位、确认码放回。
  - 另一个借用、改规则、改出口：409 `busy`。只读接口照常应答。
- 反过来：启用、回国、卸载进行中发起借用 → `ErrBusy` → 409 `busy`；卸载中 `guard` 先拦；已卸载 → `guard` 回 `uninstalled`。
- 已启用时两个操作不加锁（和现在相同）：虚拟机常驻，不存在“借用结束时停掉”和启用、回国的竞争。回国时状态机自己停虚拟机，这时正在跑的检查失败，原因写“虚拟机正在关闭”。

### ④ 失败即断网

- 回国状态下 Claude 的流量本来就不经虚拟机：没有 utun、没有接管路由和 DNS，由回国拦截（pf 只拦 Anthropic 的地址段）和启动管控挡住。借用不改变这一点，也就不存在“借用失败后 Claude 改走直连”的路径。
- 闸门全程关：`ReasonState` 不被碰；测试里采样协程每 5 毫秒读一次 `status`，借用前、中、后（含四种失败）都断言 `gate_open=false` 且原因里有 `state`。
- 启用时下发失败（第 1 节第 7 步）走第 ④ 步的回滚，回到 RESTORED，闸门全程关（`TestEnableManagedPushFailsAtStep4`）。
- 已启用时两个操作用常驻的虚拟机，失败只影响这次应答，闸门由现有复查和管理器决定。

### ⑤ 会话里怎么测

- 全部用假的拉起器（`internal/vmmgr` 的 `fakeLauncher` + 假时钟；`internal/daemon` 的 `dmLauncher` + 真实时钟、很短的时长 `dmTiming`），假 pf 执行器、假地区（`FakeVMRegion`、假 `MacRegion`），假主代理一侧。
- 新增两个跟虚拟机死活绑定的假实现：`vmBoundProber`（虚拟机不在时五项都是 `channel_down`）、`vmBoundMainVM`（虚拟机不在时下发报错；可设“起来后也报错”）。先写测试、看它在修之前失败，再改代码。
- 不做真起本项目虚拟机的集成测试：真实模式要 root；`scripts/dev/vm-up.sh` 起的是外部启动的虚拟机，不经过 `vmmgr`。真机验证由用户卸载、重新构建、重装后再做一次“应用主代理”（验收 g）。

## 4. 验收 a–g 对上的测试

### a. 第三节各条

| 条 | 测试（包#名字） | 断言 |
|---|---|---|
| ① | `internal/daemon#TestBorrowApplyMainRestored` | 管理器管理（`dmLauncher`）、状态回国、有待下发的选择：apply 200、`borrowed:true`；基准文件存在；`POST /v1/setup/complete` 200；`launches==1`；应答时进程已退出、阶段 `idle`、`Borrowed=false`；采样全程闸门关；假 pf 执行器的调用记录前后相同（回国拦截关、监护间隔调大）；`FakeVMRegion`、假 `MacRegion` 写入 0 次；`state.json` 字节不变；`/v1/launchguard` 前后相同；`vm_borrow` 事件依次是 starting、pushing、checking、stopping、done |
| ① 子测试 | `TestBorrowApplyMainRestored/return_block_reload`（必须改 1） | 打开回国拦截；让健康检查卡住使借用停在等健康；清掉假 pf 的回国拦截锚点；断言**借用结束前**锚点已被重新加载；放开后借用照常完成 |
| ① 补 | `internal/daemon#TestBorrowEventsNoAddress`（裁定③） | 成功和四种失败各一次后，全部 `vm_borrow` 事件详情、`status.borrow`、命令行最后一行都不匹配 IPv4/IPv6 的正则，不含假节点的服务器名和端口；`done` 的详情只有国家和时区 |
| ② | `internal/daemon#TestBorrowMainCheckRestored` | 同 ① 的“拉起一次、结束时已退出、闸门关、pf 和地区不动”；不写基准 |
| ② 补 | `internal/daemon#TestExitCheckNotEnabled`（裁定④） | 回国、停用状态下 exit-check 409 `not_enabled`、文字是“没有启用时不检查分流出口：启用后会自动检查”，`launches==0` |
| ③ | `internal/daemon#TestBorrowFailures`，子测试 `launch`、`not_ready`、`push`、`probe` | 每条：进程已退出或没起来、阶段 `idle`、`Failures==0`、闸门关；`launch`、`not_ready` 代码 `vm_unavailable`，文字分别含“拉起虚拟机失败”“秒内没有就绪”；`push` 代码 `main_push_failed`；`probe` 代码 `main_unreachable`，中文原因、不含原因码 |
| ③ 补 | `internal/daemon#TestBorrowNoMainSelected` | 借来的虚拟机、没有保存的选择 → 422 `no_main_selected`，虚拟机已停 |
| ③ 补 | `internal/daemon#TestBorrowDisabledGivenUpNoRetry` | 停用状态、管理器已 `failed`：apply、main-check 回 503 `vm_unavailable`“虚拟机已停止重启：请重新启用后再试”，`launches` 不变、阶段仍 `failed` |
| ④ | `internal/daemon#TestBorrowBusyEnableRestoreUninstall`、`#TestEnableAfterBorrowStaysResident` | 借用卡在等健康时：同步启用、异步启用、回国（带确认码）、卸载（带确认码）、改规则、再一次 apply 都得到 409 `busy`，确认码可以再用；借用结束后启用 200，等过 `StopWait+KillWait` 以上虚拟机仍在、`launches==2`、阶段 `running` |
| ④ 补 | `internal/daemon#TestEnableFromRestoredManagedPushesAfterStart` | 管理器管理、回国、待下发的选择、`vmBoundMainVM`：启用成功；下发发生在拉起并健康之后（修之前失败） |
| ④ 补（必须改 2） | `internal/daemon#TestEnableManagedPushFailsAtStep4` | 管理器管理、回国、`vmBoundMainVM` 设成虚拟机起来后下发报错：启用失败、状态回 RESTORED；采样协程每 5 毫秒读一次、全程闸门关；没有“已下发”记录、`pending.json` 还在；虚拟机按回滚停掉（进程已退出、`idle`、没有多余进程）；代码 `main_push_failed`、中文原因 |
| ④ 补（必须改 2） | `internal/daemon#TestMainPushVMStartSkipsSameDigest` | 已启用后状态机层面再调 `mainPushVM.Start`（`SyncRegion` 的“等虚拟机恢复”，以及直接重复调用）：同一份配置按摘要跳过，`PushMainProxy`/`PushMainHops` 调用次数不增加 |
| ⑤ | `internal/daemon#TestBorrowNotNeededWhenEnabled` | 已启用：apply、main-check 不再拉起、`borrowed` 为假、结束后阶段仍 `running`、闸门仍开 |
| ⑥ | `internal/daemon#TestRoutesNeverLaunchWhenNotEnabled` | 回国、管理器管理：按 `RouteTable()` 逐条（`BorrowRoutes`、`ResidentRoutes` 之外的全部，含 exit-check）用最小请求体调用一次，`launches==0`、阶段一直 `idle` |
| ⑦ | `internal/daemon#TestVMSourceStateMatrix` | 见下面的矩阵设计 |
| ⑧（补） | `internal/daemon#TestBorrowCrashLeavesNothing`；`cmd/abroad-vm#TestRunParentWatch`（现有） | 借用进行中状态目录里没有文件变化；用同一个状态目录起新 daemon 和新拉起器：`launches==0`、`idle`、状态是回国 |

`internal/state` 单测（必须改 1，新文件 `internal/state/hold_test.go`）：
- `TestExclusiveHoldPatrolsReturnBlock`：状态 RESTORED、回国拦截模式；持有 `ExclusiveHold` 时清空假 pf 锚点，一轮 `Tick` 内重新加载；`held` 在 `fn` 返回后为假。
- `TestExclusiveSkipsTick`：持有普通 `Exclusive` 时清空锚点，`Tick` 照旧跳过、不重载。
- `TestExclusiveHoldOtherStatesNoop`：持有 `ExclusiveHold`、状态是 DISABLED（或不在回国拦截模式）时 `Tick` 什么都不做。
- `TestExclusiveHoldBusyAndAdmit`：持有期间 `Enable`、`Restore`、`Exclusive` 得到 `ErrBusy`；`Admit` 拒绝时 `ExclusiveHold` 不执行 `fn`、不置 `held`。

`internal/vmmgr` 单测（新文件 `internal/vmmgr/borrow_test.go`）：`TestBorrowStartsAndStops`、`TestBorrowWhenWantedNoLaunch`、`TestBorrowHandoverWhenWanted`、`TestBorrowLaunchFailsNoRetry`、`TestBorrowNotReadyTimeout`、`TestBorrowStuckOnRelease`、`TestBorrowClosed`、`TestBorrowFailuresNotCounted`、`TestBorrowGateOnlyVMReason`。

**矩阵设计（⑦）**

- 三种虚拟机来源：外部启动（`ExternalVM{Dir: 空临时目录}` 没在运行；另加实现 `Start/Stop/Probe` 的测试类型模拟“外部启动且在运行”，走 `borrowVM` 同一分支）；由管理器管理（`vmmgr.Manager` + `dmLauncher`）；假的（`FakeVM` + `WithFakeMainProxy`）。`Prober`、`MainVM`、`ExitVM`、`CheckExit` 一律用跟虚拟机死活绑定的假实现。
- 五种状态：`RESTORED`；`DISABLED`（启用后停用）；`ENABLED`；`ENABLING`（启用停在写入 ENABLING 之后：管理器来源让健康检查卡住，其余来源用阻塞的假 IPv6 快照）；`RESTORING`（回国停在写入 RESTORING 之后：阻塞的假路由删除）。另加一行“崩溃留下的 ENABLING/RESTORING、Recover 没走完”（假 pf 让 Recover 失败）。
- 接口：`main-proxy-apply`、`main-check`、`exit-check`；以及间接经虚拟机、要证明不冒内部说法的 `main-select`、`main-source-add`（订阅、不带 via_host）、`envcheck-run`。
- 每格断言：2xx，或代码 `busy`，或代码在 {`vm_unavailable`、`main_push_failed`、`main_unreachable`、`no_main_selected`、`state_incomplete`、`not_enabled`、`via_host_required`、`unsupported`} 里；`error` 文字不含任何 `exitcheck.Cause` 原值、`通道不通`、`数据通道不可用`（只查 `error` 字段：`no_baseline` 也是一个 `api.ErrorCode`）。写死的期望值举例：管理器×回国×apply = 200；外部（没运行）×回国×apply = 503 `vm_unavailable`；任意来源×ENABLING×apply = 409 `busy`；残留×apply = 409 `state_incomplete`；管理器×停用×apply = 200 且不新拉起；**任意来源×未启用（回国、停用、残留）×exit-check = 409 `not_enabled`**；ENABLING/RESTORING 进行中×exit-check = 409 `not_enabled`（不经锁）；ENABLED×exit-check = 200（外部没运行时为原有的检查结果，不含内部说法）。
- 每格结束后：管理器来源的阶段回到这一格开始时的阶段，没有多出来的进程。

### b. 改过的原有断言（第 5 节）

### c. 没有残留

`internal/daemon#TestBorrowNoLeftovers`（带 `-race`）：连续 20 次借用（成功、四种失败交替）后：每个假进程都已 `Done`、阶段 `idle`、`Borrowed=false`、`d.borrowing` 为假、`m.held` 为假、`status.borrow` 为空、`runtime.NumGoroutine()` 在 2 秒内回到开始时的值（容差 2，沿用 `internal/daemon/dataplane_test.go` 的写法）。`internal/vmmgr#TestBorrowStartsAndStops` 里同样查协程数。

### d. 清单

第 2 节；`internal/daemon#TestBorrowRouteClassification`：路由表每一条要么在 `BorrowRoutes`（两条）、要么在 `ResidentRoutes`、要么被 ⑥ 覆盖；新增路由没归类就失败。

### e. 内部说法的检查（第 6 节）

### f. 全量验证

两种构建（默认、`-tags abroaddev`）各跑 `go build ./...`、`go vet ./...`；`GOOS=windows GOARCH=amd64` 的 build 和 vet；`GOOS=linux GOARCH=arm64 go build ./...`；`ABROAD_FULL_EXHAUSTIVE=1 go test -timeout 30m -race -tags abroaddev ./internal/... ./cmd/... -count=1` 和不带标签的 `ABROAD_FULL_EXHAUSTIVE=1 go test -timeout 30m ./internal/... ./cmd/... -count=1` 各完整跑一遍、写明结果；`go run ./cmd/abroad-apigen -check`；`scripts/dev/app-test.sh`；各份自测（`scripts/lib/buildcheck-selftest.sh` 等）；`scripts/dev/privacy-check.sh --commit` 零命中。

### g. 不执行真实模式

会话里只跑上面这些；真机由用户卸载、重新构建、重装后再做一次 `sudo abroad main apply`，结果交顾问判定，回填验收清单第 9 节。

## 5. 要改的原有测试断言（验收 b）

| 位置 | 现在 | 改成 | 理由 |
|---|---|---|---|
| `internal/daemon/vmmgr_test.go:194` 注释 | “状态文件是回国（全新安装）时不拉起” | “状态文件是回国（全新安装）时启动不拉起（不常驻）” | 措辞 |
| `internal/daemon/vmmgr_test.go:205` 失败信息 | “回国状态不应拉起虚拟机” | “回国状态启动时虚拟机不应常驻” | 措辞；判断条件（启动后 `launches==0` 且 `idle`）不变 |
| **唯一例外**（裁定②选甲）`internal/daemon/errcode_test.go:278` | `failedItems(r)` 不等于 `"mainland（mainland）"` 时要求含 `"mainland"`、不含出口 IP | 要求含“是否在大陆”、不含出口 IP（`203.0.113.7`） | 第 1 步改了 `failedItems` 的格式，不保留英文项名 |

其余照旧通过、不改：`internal/state/machine_test.go:104、116`（回国、停用带关虚拟机之后“虚拟机没关”）仍成立；`TestVMManagerGateEventOrder` 的“回国”一步仍是 `idle`；`internal/state` 里 `Tick` 在忙时跳过的现有测试（普通持锁者）不变；`app/Tests/AbroadKitTests/VMManagerTests.swift` 加了可选字段 `borrowed` 后照常解码。

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

**按场景给的原因**：

| 场景 | 代码 | 文字（做什么失败了、原因、下一步） |
|---|---|---|
| 管理器拉不起来 | vm_unavailable | 启动虚拟机失败：拉起虚拟机失败（脱敏后的原因）。请在“引擎与虚拟机”页看最近的错误；本机网络没有改动 |
| 拉起后没就绪 | vm_unavailable | 启动虚拟机失败：虚拟机在 100 秒内没有就绪。下一步同上 |
| 外部启动、没在运行 | vm_unavailable | 虚拟机由外部启动，现在没有在运行：请先启动虚拟机再试 |
| 已启用、常驻的虚拟机没就绪 | vm_unavailable | 虚拟机现在没有就绪（阶段：正在重新拉起 / 已停止重启…）：等它恢复后再试 |
| 停用状态、管理器已停止重启 | vm_unavailable | 虚拟机已停止重启：请重新启用后再试 |
| 检查只有 channel_down | vm_unavailable | 到虚拟机的数据通道打不开…… |
| 下发失败（借用中或启用第 ④ 步） | main_push_failed | 把主代理配置下发到虚拟机失败：（中文原因）。借用时“虚拟机已经关闭，本机网络没有改动”；启用时“已回到启用前的状态，本机网络已恢复” |
| 主代理不通 | main_unreachable | 主代理不通：HTTPS 访问：连不上；出口 IP：超时。请确认节点可用后再试；虚拟机已经关闭 |
| 明确不通过 | 原有代码 | 这一轮检查没有通过：是否在大陆：出口在中国大陆…… |
| 没选节点 | no_main_selected | 还没有选主代理节点：请先选好节点再应用 |
| 残留的半截状态 | state_incomplete | 上一次启用或回国没有完成：请先执行“恢复本机网络”，再试 |
| 未启用时检查分流出口 | not_enabled | 没有启用时不检查分流出口：启用后会自动检查 |

**检查怎么写**：

1. `internal/exitcheck#TestCauseTextCoversAll`、`#TestItemLabelCoversAll`：判定表里每个原因、每个检查项都有中文，文字里没有 ASCII 字母组成的原因码。
2. `internal/daemon#TestFailedItemsNoRawCause`：所有原因组合下 `failedItems` 不含任何 `Cause` 原值。
3. `internal/daemon#TestNoInternalCauseWordsInUserOutput`（源码检查，用 `internal/srccheck`）：Go 里除 `internal/exitcheck` 自身和测试文件外，禁止 `string(<…>.Cause)` 转换和字面量 `"channel_down"`、`"通道不通"`、`"数据通道不可用"`；Swift（`app/Sources/**`）、`cmd/abroad/**`、`scripts/**/*.sh` 里的给人看的字符串不得出现 `channel_down`、`通道不通`（扫描范围和允许名单写在测试里）。
4. 矩阵测试（⑦）对每个接口的 `error` 字段做同样的检查。
5. `internal/envcheck#TestExitItemsNoRawCause`。

## 7. 命令行和应用的显示（前任第 ⑨ 条，裁定③⑤）

- **命令行**（`cmd/abroad`）：`main apply`、`main check` 发请求前起 `borrowProgress`：另开一个 `apiclient.Client` 订阅 `/v1/events`，只打印来源 `vm_borrow` 的事件，写**标准错误**（`--json` 时标准输出只放最终应答）：
  ```
  正在启动虚拟机（第一次可能要一两分钟）…
  正在下发主代理…
  正在检查出口…
  正在关闭虚拟机…
  已建立出口基准：出口在 US（America/Los_Angeles）
  ```
  最后一行只写国家和时区，不写 IP、节点服务器、端口（`cmdMainApply` 现在就只取 `baseline.country`、`timezone`，保持）。应答回来后取消订阅；订阅失败不打印进度，不影响结果。`codeOf` 加新代码：`vm_unavailable`、`main_push_failed`、`main_unreachable`、`no_main_selected`、`state_incomplete` 打印原文、退出码 `ExitFail`；`exit check` 未启用时的 `not_enabled` 按现有分支打印原文。测试：`cmd/abroad#TestCLIMainApplyProgress`（假后台推几条 `vm_borrow` 事件：标准错误有进度、`--json` 时标准输出只有 JSON、输出无 IPv4/IPv6）、`#TestCLIBorrowErrorCodes`。命令行的请求时限：`internal/apiclient/client.go:35` 的 `Timeout` 为 0 时 5 分钟，够用。
- **应用**：用已有的事件推送和状态（裁定③）。`StatusModel.handleEvent` 每收到事件就 `refresh()`（现有），状态里的 `borrow.step` 就是进度。`MainProxyModel.applyState`、`checkState` 在 `.checking` 时显示 `L10n.borrowStep(step)`（“正在启动虚拟机”“正在下发主代理”“正在检查出口”“正在关闭虚拟机”），`MainProxyPage`、`SetupView` 在按钮下方显示；`EnginesPage` 的管理器一行在 `borrowed` 时显示“临时运行：做出口检查，结束后自动关闭”；`ExitsPage` 未启用时显示 `not_enabled` 的原文；新错误代码在 `Strings.swift` 里有说明。测试：`swift:borrowStepNames`、`swift:applyShowsBorrowProgress`、`swift:borrowErrorCodesHaveText`、`swift:enginesShowsBorrowed`、`swift:exitCheckNotEnabledText`。
- **应用里两个接口的请求时限**（裁定⑤）：`applyMain` 和 `checkMain` 定义在 `app/Sources/AbroadKit/APIClient+Main.swift:35、36`，都走 `APIClient.call`，不带单独的时限；时限来自传输层 `app/Sources/AbroadKit/Transport.swift:47` 的 `UnixTransport.init(path:timeout: Double = 300)`，`run(...)` 在 `Transport.swift:132–136` 按它超时。应用唯一的客户端在 `app/Sources/ClaudeAbroad/App.swift:10`：`APIClient(transport: UnixTransport.fromEnvironment())`，`fromEnvironment`（`Transport.swift:43–45`）用默认值，所以这两个接口的时限就是 **300 秒**，不低于要求，不用改。最坏耗时约 230 秒（100 秒等健康 + 80 秒下发 + 25 秒检查 + 25 秒停止），后台的 `BorrowTimeout` 是 4 分钟。计划加一条 Swift 测试 `swift:transportDefaultTimeoutAtLeast300` 把这个下限固定住，以后谁把默认值调小就会失败。

## 8. 设计文档要回写的节（v0.69 → **v0.70**）

- 文件头：`草案 v0.70（2026-11-27，借用虚拟机：回国状态下应用主代理、主代理检查临时拉起虚拟机；启用时主代理在虚拟机就绪后下发，见 7.2、10、11.1、11.2、11.3、16）`。
- 第 10 节“M6-2a 起”一段：
  - “要不要虚拟机”改成“`RESTORED` 时不常驻；只在一次互斥的操作内部临时运行（借用），操作结束时已经停掉”；
  - 新增“借用”一条：入口（应用主代理、主代理检查）；互斥（状态机忙标记的借用入口 `ExclusiveHold`；期间启用、回国、卸载得到“忙”）；**借用期间回国拦截的巡检照常**（`Tick` 看到借用标记、状态是回国且在回国拦截模式就照常巡检，其他持锁者仍整轮跳过）；失败不重试、结束必停、崩溃后不留进程；期间不动 pf/路由/DNS/地区、闸门保持关；停用状态下已停止重启时不借用、提示重新启用；**合盖睡眠跨过等就绪的时限时判为没就绪，借用失败即停**（偏安全，不处理）；
  - “状态接口”一条加 `borrowed`、`/v1/status` 的 `borrow` 和事件来源 `vm_borrow`（详情不含 IP、节点服务器、端口）；
  - 补一句：启用时主代理在第 ④ 步虚拟机就绪后下发，下发失败按第 ④ 步失败回滚。
- 7.2：“应用主代理”的顺序补上未启用时的借用：拉起 → 按保存的选择下发（不记“已下发”）→ 带位置查询的检查 → 停虚拟机 → 保存基准、锁定目标；已启用时不变。
- 11.1：两个接口的说明（未启用时借用、耗时一两分钟、应答里的 `borrowed`/`note`）；`exit-check` 未启用时 409 `not_enabled`；错误代码表加五个代码；`/v1/status` 的 `borrow`；`/v1/engines` 的 `vm_manager.borrowed`；“只读接口不拉起虚拟机”。
- 11.2：应用里的进度、引擎页的“临时运行”。11.3：命令行的进度写标准错误、最后一行只有国家和时区。
- 16：v0.70 修订记录（问题、根因、修法九条的落实、借用入口和巡检、启用下发顺序的顺带修复、原因文字表），并记“**architect 改判：分流出口未启用时不借用**”。

## 9. 风险和拿不准的地方

1. **借用期间回国拦截的巡检——已解决**（必须改 1）：借用用 `ExclusiveHold`，`Supervisor.Tick` 抢不到忙标记时若 `held` 为真，状态是 RESTORED 且在回国拦截模式就照常 `Patrol.Step`，锚点被清后一轮内重新加载；其他状态什么都不做。安全性：借用期间状态文件不变、借用不碰 pf，`Patrol.Step` 自带互斥（`handleLANPut` 已有不持忙标记调用的先例）。测试：`internal/state#TestExclusiveHoldPatrolsReturnBlock`、`#TestExclusiveSkipsTick`、`internal/daemon#TestBorrowApplyMainRestored/return_block_reload`。
2. **R1 没有结论**：真机上第一次由 abroadd 以专用用户拉起虚拟机就是这次借用。起不来时用户看到“启动虚拟机失败：……”和脱敏后的原因（不再是 channel_down），这次的事件和诊断包可以作为 R1 的部分证据。
3. **启用下发顺序**（已并入）：虚拟机已就绪时仍在启用前下发（失败时什么都不改）；没就绪时挪到第 ④ 步之后，下发失败走回滚（短暂断网后回到 RESTORED），由 `TestEnableManagedPushFailsAtStep4` 守住。
4. （已裁定）`errcode_test.go:278` 选甲，唯一例外。
5. **时长**：最坏约 230 秒，在应用 300 秒（代码位置见第 7 节）、命令行 5 分钟之内；合盖睡眠跨过等就绪的时限时判为没就绪、失败即停（写进设计第 10 节）。
6. （已裁定）分流出口未启用时不借用：回 409 `not_enabled`，启用后由出口循环自动检查。
7. 数据通道的会话：虚拟机停掉后 `wire.Dialer` 的会话关闭，下次 `session()` 看到 `IsClosed` 会重新拨号，`Open` 也有重试；矩阵里连续借用两次覆盖（假开流器，测不到真实 yamux）。
8. 借用期间管理器照常加清 `ReasonVM`，事件历史里会多出“gate_closed vm：虚拟机启动中”“虚拟机正在关闭”；闸门原来就关着，应用不会发“保护断开”的通知（现有 `vmReasonNotifiesOnlyWhenGateWasOpen` 已覆盖）。
9. 停用状态的虚拟机常驻（停用不关虚拟机），实际只有回国状态会借用。停用后管理器已 `failed`：**不借用、不重试**，回 `vm_unavailable`“虚拟机已停止重启：请重新启用后再试”（矩阵和 `TestBorrowFailures` 外另加 `internal/daemon#TestBorrowDisabledGivenUpNoRetry`：`launches` 不变）。
10. 虚拟机在回国状态下的出站经宿主机现有网络，宿主机上别的代理软件是否接管这部分流量不由我们决定；借用不改宿主机网络设置，也不碰本机会话依赖的代理。

## 10. 实现记录（2026-11-27，和 v2 的偏离及理由）

1. `pushForProbe` 只对**借来的**虚拟机下发（v2 写的是不是借来的也按保存的选择下发并记“已下发”）。理由：外部启动、停用时常驻的虚拟机按以前的行为（开发工具手动下发的那条路、原有测试的时间线都不变）；本缺口只需要借来的那台。
2. `mainPushVM.Start` 只在状态文件是 ENABLING（启用第 ④ 步、Recover 接着启用）时下发。理由：地区同步（`SyncRegion`）里也调 Start，而更换主代理的 `switchMain` 在同步地区时正持有 `exitPushMu`，再去拿会死锁；ENABLED 时本来就由 `maintainMain` 维护。`TestMainPushVMStartSkipsSameDigest` 覆盖重复调用和地区同步。
3. 借用进行中 `POST /v1/restore` 回“忙”。理由：回国状态下回国本来是空操作（不拿锁直接回 200），验收 ④ 要求借用期间回国得到“忙”。
4. 裁定④ 让两条原有测试在回国状态下检查分流出口变成 409：`internal/daemon/m5rest_test.go` 的 `TestExitCheckAPI` 先启用；`cmd/abroad/e2e_test.go` 的 `TestEndToEnd` 把 `exit check` 挪到第一次 `up` 之后。两处断言都没改。
5. `vmmgr`：借用期间失败后不进退避，停在 backoff 阶段等 release（`Run` 里 `borrowOnly`）。理由：最初的写法在 `-count=30` 下偶发“拉起 2 次”——Borrow 看到失败、还没来得及 release 时退避计时器先到，管理循环又拉起了一次。这是实现缺陷，不是测试的问题。
6. 新增 `apiclient.(*Client).EventsConnected`：命令行先等订阅建立（最多 2 秒）再发请求，不漏掉开头的进度。订阅期间标准错误经一把锁写（`--json` 时两步确认的预览也写标准错误）。
7. 检查主代理明确不通过（出口在大陆等）时仍按原来的 502 和文字；只有虚拟机相关、主代理不通的情况走新的代码。
8. 源码检查 `MethodCallsOutside` 加在 `internal/srccheck`（反例 `testdata/holdbad`），`internal/daemon#TestExclusiveHoldOnlyInVMBorrow` 用它。
