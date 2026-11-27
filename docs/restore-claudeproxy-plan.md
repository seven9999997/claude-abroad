# 回国确认里选择第四道：实施计划（交架构顾问审）

> 版本：**v2，对应设计 v0.72**（2026-11-27）。只是计划，代码还没改。
> v1（3cbcf17，设计号原定 v0.71）写在 B0 之前。之后 v0.71 给了阶段 A，v0.71b 给了 B0（应用里经系统密码框以 root 运行已安装的 `abroad --yes --json <子命令>`），v0.71c 给了阶段 B；这一条顺延为 **v0.72**（`docs/app-privileged-plan.md` v2 裁定②）。v2 按顾问的修订要求①–⑦接上 B0 之后的现状，对照表见第 0 节末尾。
> 依据：用户原话（2026-11-27）“caibao 兜底 回国前提示也提示一下是否需要兜底，不需要就不兜底了”；architect 原判定五条和验收 a–m；顾问的修订要求①–⑦；设计 v0.71c 的 10.1、10.2 第四层 v0.68 一段、10.3、11.1–11.4、16 节 v0.71–v0.71c；`docs/app-privileged-plan.md` v2。
> 会话里的边界：不改宿主机网络，不碰本机会话依赖的代理和隧道（见 `docs/LOCAL.md`），不用 sudo，不运行真实模式，不运行真实的 `PrivilegedRunner`（不弹密码框、不提权），不碰 `/Applications/ClaudeAbroad.app` 和 `/Library/Application Support/claude-abroad*`，不提交。测试全是假系统、临时目录、`127.0.0.1:0` 端口、假的 `PrivilegedRunning`；swift 集成测试由 `scripts/dev/app-test.sh` 以当前用户起 `--fake-system` 的 abroadd，用完停掉。

## 0. 核对代码后的现状（HEAD 9e83d70）

后台（3cbcf17 之后没有改过 `confirm.go`、`claudeswitch.go`）：
- `handleRestore`（`internal/daemon/confirm.go`）：严格解码 `ConfirmRequest` → 借用中回 409 `busy` → 已是 RESTORED 回 200 `Status` → 摘要 `digestOf(OpRestore, browserChoice(kill))` → 不带码时预览并签发 → 带码时 `confirms.take` → `d.exec(w, r, d.m.Restore, true)`，遇到 `state.ErrBusy` 时 `giveBack`。
- `ConfirmRequest` 由 `enable`、`region-put`、`uninstall` 共用。
- `handleRestorePreview`（`GET /v1/restore/preview`，只读）：只返回 `claudeproc.Preview`。应用设置页的进程清单（`PageModels.swift:688`）和 B0 回国确认框（`PrivilegedAuthorizer.swift:97`）都用它。
- B0 之后加了 `GET /v1/enable/preview`（`enable_preview.go`）和 `GET /v1/uninstall/preview`（`UninstallPreviewView` 嵌入 `UninstallPreview`，带 `?kill_browsers=true`），可以照着做。
- `exec` 成功时直接写 `d.Status()`，没有“成功之后”的钩子。`Status` 在 v0.71 多了 `build`、`state_format`、`busy`、`uninstall`。
- `claudeProxyOn()`、`claudeProxyOff()`、`cpAvailable()`、`cpTestMode()`、`cpSwitchMu`、`Conflicts()`（只读）的行为和 v1 计划核对时一样。

命令行：
- `cmdRestore` 只认 `--force`、`--kill-browsers`。
- `confirmedWord` 的流程：第一步 → 打印预览 → （带 `--yes` 时）直接用新码做第二步。所以应用经 root 执行 `abroad --yes --json restore …` 时，**两步都在这一次命令行进程里发生，两次请求体相同**。
- 读终端用的是每次新建的 `bufio.NewReader(a.stdin)`，在 `main.go` 和 `claudeproxy.go` 两处。

应用（B0、阶段 B 之后）：
- 回国的路径：`StatusModel.restore()` → `perform(.restore)` 发 `POST /v1/restore`。控制台用户在 `guard` 就被回 403，到不了处理函数，不签发确认码，也不产生预览。之后 → `PrivilegedAuthorizer.authorize` 回 `.confirmInApp` → `PrivilegedSession.present([.restore])` 读只读预览 `client.restorePreview()` → 应用自己的确认框（`Dialogs.swift` 的 `PrivilegedSheet`，有 `session.killBrowsers` 勾选）→ 点“执行” → `PrivilegedCommand.build`，命令是 `"$A" --yes --json 'restore' '--kill-browsers'`。
- `ConfirmFlow`（POST 409 的确认框）只对完全信任的调用方出现，应用现在不是这种调用方。
- **`--kill-browsers` 现在不在 `SensitiveOperation.arguments` 里**，而是由 `PrivilegedSession.userTappedExecute` 当作 `PrivilegedStep.options` 另加的。终端退路用的是 `present` 时按 `.restore` 生成的 `sudo abroad restore`，不带勾选。这和顾问要求的“参数只有一份 arguments”不一致，这次一并收口（第 1 节第 6 步）。
- 白名单：`PrivilegedSubcommand`（Swift）、`app/Tests/AbroadKitTests/testdata/privileged-subcommands.txt`（命令词 TAB 路由 ID）、Go `TestPrivilegedWhitelistMatchesRoutes`。现在只核对子命令，不核对参数（flag）。命令形状的 golden 是 `testdata/privileged-commands.txt`，已有 `restore-kill` 一例。
- 退出码说明 `L10n.privExit`：2 = “后台服务的版本比应用旧，请先升级”。

**v1 → v2 对照**

| 顾问要求 | v2 怎么做 | 在哪 |
|---|---|---|
| ① 含义不变 | 字段、keep/on/off 绑定、先回国后切换、失败处理照 v1 | 1 节第 1–3 步 |
| ② 拒绝不变 | 422 冲突 / 422 已回国 / 501 / 409 / 测试模式只监听，照 v1 | 1 节第 2 步 |
| ③ 只读预览补第四道 | `GET /v1/restore/preview` 改回 `RestorePreviewView`（嵌入 `claudeproc.Preview`，加 `claude_proxy`，里面有现状、目标、测试模式、冲突）；应用不再走 POST 409 的确认框（v1 的 `ConfirmFlow` 改动全部删掉） | 1 节第 4 步、第 6 步 |
| ④ 应用勾选、arguments 一份、改白名单 golden | 新增 `.restoreWith(killBrowsers:claudeProxy:)`，`arguments` 生成两个 flag；去掉 `options` 这条旁路；白名单 golden 加第三列“允许的参数”，Go/Swift 两边核对；命令 golden 加两例 | 1 节第 6 步 |
| ⑤ 命令行交互 | 照原判定 | 1 节第 5 步 |
| ⑥ 忙、卸载、确认码放回 | 第 3 节的表，加上“root 命令行内部两步”的情况 | 3 节 |
| ⑦ 16 节照录原话和日期 | 第 5 节 | 5 节 |
| 确认码绑定在哪一步 | 在 root 运行的那一次 `abroad --yes --json restore [--kill-browsers] [--claude-proxy on|off]` 里：命令行第一步 POST 和第二步 POST 带同一个请求体，后台每一步都重新算一遍摘要；和 `--kill-browsers` 同一种做法 | 2 节 |

## 1. 改哪些文件、按什么顺序、函数级改动

### 第 1 步：后台的类型和小函数（`confirm.go`、`claudeswitch.go`、`daemon.go`、`routes.go`）

1. 新的请求类型 `RestoreRequest`：`Confirm string`、`KillBrowsers bool`、`ClaudeProxy *bool "claude_proxy,omitempty"`。路由表里 `restore`、`restore-alias` 两行的 `Req` 改用它。`ConfirmRequest` 不动：启用、切换地区、卸载照旧不认 `claude_proxy`，带了回 400。
2. `cpChoice(p *bool) string`：`nil` → `claude_proxy=keep`，`true` → `claude_proxy=on`，`false` → `claude_proxy=off`。**按字面值绑定**，理由见风险 1。
3. `RestoreClaudeProxy`，POST 的 409 和只读预览共用：
   ```go
   type RestoreClaudeProxy struct {
       Current   bool                 `json:"current"`
       Target    bool                 `json:"target"`
       TestMode  bool                 `json:"test_mode"`
       Conflicts []claudecfg.Conflict `json:"conflicts,omitempty"` // 只读预览：现在关、不是测试模式时，打开会撞上的冲突（只读）
   }
   ```
   `ConfirmNeeded` 加 `ClaudeProxy *RestoreClaudeProxy "claude_proxy,omitempty"`。
4. `ClaudeProxyChange`：`{Target bool; OK bool; Notes []string omitempty; Conflicts []claudecfg.Conflict omitempty}`。`Status` 加 `ClaudeProxyChange *ClaudeProxyChange "claude_proxy_change,omitempty"`。和 `BrowsersLeft` 同样的做法：只在回国那次应答里填，`d.Status()` 本身从不填，所以 `/v1/status` 永远没有这一项。
5. 文字常量。命令行和应用的文字表各一份，由测试对照：
   - `ClaudeProxyRestoreOffNote`：“回国后 Claude Code 不再被指向本机代理，回国后靠解析锁和回国拦截挡住它（默认状态）”；
   - `ClaudeProxyRestoreTestNote`：“测试模式：只监听，不写托管设置，本机真实的 Claude Code 不受它管”；
   - `ClaudeProxyRestoreKeepOn`：“第四道保持现状（开）”；`ClaudeProxyRestoreKeepOff`：“第四道保持现状（关）”；
   - 关 → 开沿用 `ClaudeProxyConsequences` 的三句。
6. `restoreCPBlock()`：开关可用并且没卸载过时，返回 `{Current: cpOn, Target: cpOn, TestMode: cpTestMode()}`，否则返回 nil。只读预览和 POST 的 409 都用它。
7. `restoreCPTarget(req *bool) (target *bool, errStatus int, errBody any)`，**两步都在 `confirms.take` 之前过这一关**（不过就不签发、也不消耗确认码）。按顺序判断：
   - `nil` → 保持；
   - 开关不可用：要打开回 501 `unsupported`，要关按保持；
   - 已卸载：要打开回 409 `uninstalled`，要关按保持；
   - 值和现状相同 → 保持；
   - 否则返回 `req`。
8. `execThen(w, r, fn, allowCorrupt, after func() *ClaudeProxyChange) error`（`daemon.go`）：`fn` 成功后先调 `after()`，再取 `d.Status()`，填 `ClaudeProxyChange`，写 200。`exec` 改成 `execThen(…, nil)`，`fn` 失败的路径一字不改。

### 第 2 步：`handleRestore`（`POST /v1/restore`）

按顺序，哪一步不过就停：
1. 严格解码 `RestoreRequest`。出错文字改成“只认 confirm、kill_browsers、claude_proxy”。
2. 借用中 → 409 `busy`（在 `take` 之前）。
3. `restoreCPTarget`：回 501 或 409，不签发、不消耗。
4. 已是 RESTORED：`target != nil`（也就是值和现状不同）→ 422 `invalid`，`field: "claude_proxy"`，文字“已经是回国状态，回国这一步不再执行，第四道也没有改：要切换第四道请用 sudo abroad claude-proxy on|off”（按请求的值写 on 或 off）。否则照旧 200。
5. `digest := digestOf(OpRestore, browserChoice(kill), cpChoice(req.ClaudeProxy))`。
6. 第一步（不带确认码）：
   - `target` 是“打开”、不是测试模式、`ClaudeSettings != nil` 时，只读地调一次 `Conflicts()`。有冲突 → 422 `{"code":"invalid","error":…,"conflicts":[…]}`，和 `PUT /v1/settings/claude-proxy` 冲突时的应答同一个形状，不签发、什么都不改。文字的下一步写“开关仍是关，没有回国。去掉这些变量后再试，或这次不选打开（不带 --claude-proxy）回国。”
   - 预览照旧，`ConfirmNeeded.ClaudeProxy = restoreCPBlock()`，目标改成 `target`（有的话）。
7. 第二步：`take`（摘要不符 → 403 `confirm_invalid`）→ 把 kill_browsers 照旧放进 ctx → `execThen(w, r, d.m.Restore, true, after)`，其中 `after` 只在 `target != nil` 时是 `func() *ClaudeProxyChange { return d.restoreSwitchCP(*target) }` → 遇到 `ErrBusy` 时 `giveBack`。

### 第 3 步：回国成功后切开关（`claudeswitch.go` 的 `restoreSwitchCP(on bool) *ClaudeProxyChange`）

- **打开**：`conf, err := d.claudeProxyOn()`，规则照旧：冲突就拒绝、三步回滚、测试模式只监听、卸载中拒绝。`OK = err == nil`，`Conflicts = conf`。失败时 notes 写两句：“回国已完成；第四道没有打开：<原因>”和“处理后执行 sudo abroad claude-proxy on”。
- **关掉**：`notes, err := d.claudeProxyOff()`；`OK = err == nil && len(notes) == 0`（分片被改过、留下 leftover 也算没完全做到）；不 OK 时补“处理后执行 sudo abroad claude-proxy off”。
- 事件 `restore:claude_proxy_on|off`，详情只写“成功”或“没有完成”，不带路径。
- 这一步在状态机的忙标记之外，只靠 `cpSwitchMu` 串行（和 PUT 一样）。回国状态下第四道一打开，本机代理端口就一律拒绝，所以“先回国、后打开”中间没有放行窗口。

### 第 4 步：只读预览 `GET /v1/restore/preview`（顾问要求③）

- 应答类型改成 `RestorePreviewView{ claudeproc.Preview（嵌入）; ClaudeProxy *RestoreClaudeProxy "claude_proxy,omitempty"; Note string "note" }`，路由表的 `Resp` 跟着改。嵌入后 JSON 平铺，原有字段不变，所以设置页按 `ClaudeprocPreview` 解码照样能用（多出来的键会被忽略）。
- 照 `uninstall/preview` 的做法加查询参数：`?kill_browsers=true` 走 `WithBrowserChoice`；`?claude_proxy=on|off` 把目标设成请求的值，别的值回 400。只读，**不按目标拒绝**：501、409、422 只在 POST 里出现，预览只如实报告。
- `claude_proxy` 那一块由 `restoreCPBlock()` 生成：开关不可用或已卸载时没有这一块。**现在关、不是测试模式、`ClaudeSettings != nil` 时总是带上 `Conflicts()`（只读）**，这样应用读一次预览就能在用户勾选时提示冲突，不用另发请求。
- `Note` 取 `RestorePreviewRecheckNote`：“执行时会按当时的情况重新列出；第四道的冲突也会再查一次”。
- 只读、不签发、不拉起虚拟机：`TestRoutesNeverLaunchWhenNotEnabled` 按路由表自动覆盖这一点。
- 重新生成 `api/openapi.json`、`api/swift/APITypes.swift`（命令 `go run ./cmd/abroad-apigen -root .`）。新增 `DaemonRestoreRequest`、`DaemonRestoreClaudeProxy`、`DaemonClaudeProxyChange`、`DaemonRestorePreviewView`；`DaemonConfirmNeeded.claudeProxy`、`DaemonStatus.claudeProxyChange`。`go test ./cmd/abroad-apigen/` 核对一致。
- 状态目录没有新增落盘文件（开关文件 v0.68 就有），**`TestStateFormatPinned` 的 golden 不变**；如果它变了，说明改错了。

### 第 5 步：命令行（`cmd/abroad/commands.go`、`main.go`、`claudeproxy.go`）（顾问要求⑤，照原判定）

1. `app` 加共用的 `a.readLine()`：惰性建一个 `*bufio.Reader`，`main.go` 和 `claudeproxy.go` 两处读终端都改用它。不改的话，连续读两行时第二行会被第一个缓冲区吞掉。
2. `cmdRestore` 加带值的 `--claude-proxy`，只认 `on` 和 `off`。别的值或缺值 → `usageErr`，退出码 2，不发请求。
3. `confirmedWord` 拆出一个可选的追问钩子，签名 `confirmedAsk(…, ask)`；原来的 `confirmedWord` 传 nil，行为不变。
   - 预览打印完后，再打印第四道那一块：保持时一行；关 → 开打印“后果：”加三句；开 → 关打印 `ClaudeProxyRestoreOffNote`；测试模式时再加一行。
   - **同时满足**交互运行（在终端、没有 `--yes`）、没带 `--claude-proxy`、预览里有 `claude_proxy` 时，问一句：现在关时问“回国后开启 Claude Code 兜底（第四道）？[y/N]”，现在开时问“回国后保留 Claude Code 兜底（第四道）？[Y/n]”。
     - 回车 = 保持；`y`/`yes`/`是` = 开；`n`/`no`/`否` = 关；别的输入 → “没有确认，什么都没有改动”，退出码 `ExitConfirm`（风险 2）。
     - 答得和现状不同：带上明确的值重新发第一步，拿新确认码，重新打印预览和后果，再要求确认词。第二次预览不再追问。
   - 带 `--yes`、或不在终端：不问，打印“第四道保持现状（开/关）；要改加 --claude-proxy on|off”。不在终端又没有 `--yes` 时，打印后照旧 `ExitConfirm`。
4. 回国 200 之后先 `printState`、`printBrowsersLeft`，再 `printCPChange`：ok 时在标准输出写“第四道已打开”或“第四道已关闭”；不 ok 时在标准错误写“回国已完成，但第四道没有切换：”加 notes 和冲突。**退出码 0**（风险 4）。`--json` 时原样输出应答，里面就有 `claude_proxy_change`。
5. 已经是回国状态时：第一步就回 200 或 422，进不了预览分支，自然不问。
6. 帮助文本：`sudo abroad restore [--kill-browsers] [--claude-proxy on|off]   一键回国（两步确认；--claude-proxy 选回国后第四道开或关，不带就保持现状；…）`。

### 第 6 步：应用（顾问要求③④）

v1 里改 `ConfirmFlow` 和 `reissue` 的部分**全部删掉**：应用的回国确认框是 B0 的 `PrivilegedSheet`，用只读预览，不发 POST 拿预览。`ConfirmFlow` 不动；它只对完全信任的调用方出现，那时请求里不带 `claude_proxy`，就是保持。

1. **`SensitiveOperation`**（`Authorization.swift`）：
   - 新增 `case restoreWith(killBrowsers: Bool, claudeProxy: Bool?)`。`arguments` = `["restore"]` + （kill 时加 `["--kill-browsers"]`）+ （claudeProxy 不为 nil 时加 `["--claude-proxy", on ? "on" : "off"]`）。
   - `.restore` 保持 `["restore"]`，用于“刚点一键回国、还没有选项”的场合（`perform(.restore)`、`authorize`）。
   - 终端命令和提权命令都由 `arguments` 生成，参数只有这一份。
2. **去掉旁路**（`PrivilegedAuthorizer.swift`）：
   - `userTappedExecute` 不再给 `.restore` 加 `options: ["--kill-browsers"]`。改成把 `steps` 里的 `.restore` 换成 `.restoreWith(killBrowsers: killBrowsers, claudeProxy: claudeProxyRequest)` 再 `build`。
   - `PrivilegedStep.options` 改完后如果没有别的用处就删掉（写代码时 grep 核对）。
   - `browsersOption` 的判断改为“是回国（`.restore` 或 `.restoreWith`）”。
3. **`PrivilegedSession`**（`PrivilegedAuthorizer.swift`）：
   - 新增 `@Published public var claudeProxyOn = false` 和 `public var claudeProxyRequest: Bool?`。后者在 `request.claudeProxy` 为 nil、或勾选等于 `current` 时为 nil，否则是勾选的值。
   - `present` 里遇到回国时读 `client.restorePreview()`，现在返回 `DaemonRestorePreviewView`：`r.preview` 照阶段 B 卸载预览的做法转成 `ClaudeprocPreview`；`r.claudeProxy = pv.claudeProxy`；`claudeProxyOn = pv.claudeProxy?.current ?? false`，即**初值等于现状**。
   - 派生值 `claudeProxyBlocked: Bool`：勾选成“开”、现在关、预览里有冲突时为真。这时“执行”不可点，并显示冲突文字。后台第一步照样会再查一次（风险 6）。
   - `userTappedTerminal` 对回国**按当前勾选重新生成**终端命令，即 `.restoreWith(kill, cpRequest).terminalCommand`，不再用 `present` 时生成的那条。这样终端命令和提权命令一致，只在和现状不同时带 `--claude-proxy`。
   - `finish`：回国且退出码 0 时，从输出的 JSON（`PrivilegedCommand.parse` 给的 body）里解出 `claude_proxy_change`。`ok == false` 时结果显示 `L10n.restoreClaudeProxyChangeFailed(notes)`，`phase` 记为完成但带提示（回国本身成功）；`ok == true` 时追加“第四道已打开/已关闭”。退出码 1 且 body 里有 `conflicts` 时，显示 `L10n.restoreClaudeProxyConflicts(...)`（第一步的 422：确认后才出现的冲突）。退出码 2 时照旧显示“后台服务的版本比应用旧，请先升级”：老版本的命令行不认 `--claude-proxy`，而只有勾选和现状不同才会带这个参数。
4. **`APIClient+Pages.swift`**：`restorePreview()` 的返回类型改为 `DaemonRestorePreviewView`。设置页（`PageModels.swift:688`）改读它的进程字段：嵌入后字段平铺，生成的 Swift 类型里有同名字段，写代码时按生成结果核对。
5. **`Strings.swift`**：
   - `restoreClaudeProxyOption`：“回国后开启 Claude Code 兜底（第四道）”；
   - `restoreClaudeProxyOffNote`、`restoreClaudeProxyTestNote`、`restoreClaudeProxyKeepOn`、`restoreClaudeProxyKeepOff` 和后台常量逐字相同；
   - `claudeProxyLines(current:target:testMode:) -> [String]`：关 → 开用已有的 `claudeCodeConsequences`；
   - `restoreClaudeProxyConflicts(_:)`、`restoreClaudeProxyChangeFailed(_:)`、`restoreClaudeProxyChangeOn`、`restoreClaudeProxyChangeOff`。
6. **`Dialogs.swift`（`PrivilegedSheet`）**：浏览器那组之后，`r.claudeProxy != nil` 时加一组 `CardSection(L10n.claudeCodeTitle)`：
   - `Toggle(L10n.restoreClaudeProxyOption, isOn: $session.claudeProxyOn)`，`phase != .confirming` 时不可点；
   - 下面逐行显示 `L10n.claudeProxyLines(...)`：关 → 开用 `.attention`，其余用次要文字；
   - 冲突时显示 `.failure` 的 `ToneNote`；
   - “执行”按钮的可点条件加上 `!session.claudeProxyBlocked`；
   - 只用现有的组件和 `Palette`，`RestyleTests` 的源码检查守着。
7. **白名单 golden**：
   - `privileged-subcommands.txt` 改成三列：命令词 TAB 路由 ID TAB 允许的参数（空格分隔，可以为空）。`restore` 一行写 `--kill-browsers --claude-proxy=on|off`，其余各行第三列为空：它们的参数都是位置参数，或已经写死在 `arguments` 里（如 `--via`、`--file`、`--url-file`、`--name`、`--via-host`、`--kind`）。为了不改语义，那些 flag 也照实列进第三列，第三列就成了“允许的 flag 全集”。
   - Swift 的 `PrivilegedSubcommand` 加 `allowedFlags`。`PrivilegedCommand.build` 拒绝不在这个集合里的 `--xxx`，`--claude-proxy` 的值只许 `on` 或 `off`。
   - Swift 测试 `privilegedWhitelistFile` 核对第三列和 `allowedFlags` 一致。
   - Go `TestPrivilegedWhitelistMatchesRoutes` 加一段：每个 flag 必须出现在命令行帮助文本里那个子命令的用法行里（`usage` 常量，`main.go`）。`restore` 一行必须同时有 `--kill-browsers` 和 `--claude-proxy`。
   - `privileged-commands.txt` 新增 `restore-cp-on`（`'restore' '--claude-proxy' 'on'`）和 `restore-kill-cp-off`（`'restore' '--kill-browsers' '--claude-proxy' 'off'`）两例；`restore-kill` 一例文字不变（证明参数从 `arguments` 生成后形状没变）。
   - `RepoTests` 里对照命令行帮助的终端命令表加 `.restoreWith` 的两例。

### 第 7 步：文档（v0.72）

- **`docs/design.md`**：
  - 头部状态行加 v0.72；
  - 10.1 加“v0.72：回国确认里第四道的选项”一段，写字段、默认保持、三种目标状态各显示什么文字、测试模式、预览时查冲突、已回国 422、没管控 501、卸载过 409；
  - 10.2 第四层把“**回国不碰它**”改成原判定的写法；测试模式补“回国时打开也一样”；入口补“或在回国确认里选（命令行 `--claude-proxy on|off`、应用确认框里的勾选）”；
  - 11.1：`POST /v1/restore` 的 `claude_proxy`、`ConfirmNeeded.claude_proxy`、200 的 `claude_proxy_change`、422/501/409、确认码绑定；`GET /v1/restore/preview` 改为 `RestorePreviewView`（`claude_proxy` 带 `conflicts`、`?claude_proxy=`、`?kill_browsers=`、`note`）；
  - 11.2：B0 回国确认框里的勾选、文字、冲突时不可执行、结果里的切换说明；
  - 11.3：命令行的新参数、交互那句、`--yes` 时的提示、回国后打印切换结果；
  - 11.4：提权白名单加“允许的参数”一列；
  - 16 节 v0.72（见第 5 节）。
- **`docs/fallback-plan.md`**：开头 v0.68 说明下加一行，指向设计 v0.72。
- **`docs/m6-acceptance.md`**：
  - G9.1 补测试名：`TestRestoreCPPreview`、`TestRestorePreviewClaudeProxy`、`TestRestoreCPTurnOn`、`TestRestoreCPTurnOff`、`TestRestoreCPOnFails`、`TestCLIRestoreClaudeProxy`、`swift:restoreClaudeProxyToggle`；
  - G6 补 `TestRestoreCPBinding`、`TestRestoreLeftoversClaudeProxy`；
  - 第 4 节第四道那一行补“也可以在回国确认里选”；
  - 第 1、3 节里 B0 的白名单说法补“含允许的参数”。
- **`docs/m5-walkthrough.md`**：回国确认框（B0 的确认框）加 L1-41a–f，标“待代测”。没打开启动管控的安装上没有这一组，也走查一次。
  - a：勾选后显示三句后果，执行后状态里第四道已开；
  - b：不勾就保持；
  - c：开着时取消勾选，显示开 → 关那一句；
  - d：有冲突（假系统里先放一份别的分片）时显示冲突文字，“执行”不可点；
  - e：测试模式的文字；
  - f：“在终端执行”拷出的命令只在和现状不同时带 `--claude-proxy`。
- **端到端**：脚本里的回国是应用里点、或 `--yes` 不带新参数，行为不变。`scripts/m6/e2e/selftest.sh` 加两条：① `run.sh`、`restore.sh`、`lib.sh`、`commands.txt` 和三段空跑输出里，`restore` 命令都不带 `--claude-proxy`；② `claude-proxy` 只以 `--yes claude-proxy on` 和 `--yes claude-proxy off` 两条命令出现（CC1 照旧）。`commands.txt` 不改。
- **`docs/app-privileged-plan.md`**：第 2 节第 2 条“不提供”里关于 `claude-proxy` 的那半句（v0.72 再加）已经过时，加一句注：“v0.72：白名单加允许的参数一列；回国的选项从 `arguments` 生成，见 `docs/restore-claudeproxy-plan.md`”。
- **`docs/PROGRESS.md`**：按工作循环更新（交验收时）。

### 顺序

第 1–3 步（后台）→ 第 4 步（只读预览，重新生成类型）→ 第 5 步（命令行）→ 第 6 步（应用和白名单）→ 第 7 步（文档、selftest）→ 全量验证。每步结束跑一次：`go build ./... && go vet ./... && go test ./internal/daemon/ ./cmd/abroad/ ./cmd/abroad-apigen/`。

## 2. 确认码绑定在哪一步；“先回国、再切开关”怎么接上 `exec`

- **改用只读预览之后，确认码不在应用里**：应用只读 `GET /v1/restore/preview`，不发 POST 拿预览，也不持有确认码。用户点“执行”后，root 运行 `abroad --yes --json restore [--kill-browsers] [--claude-proxy on|off]`，命令行在这一次进程里：
  1. `POST /v1/restore {kill_browsers?, claude_proxy?}`：后台过 `restoreCPTarget`，查冲突，按 `digestOf(restore, kill_browsers=…, claude_proxy=keep|on|off)` 签发确认码；
  2. 同一个请求体加上 `confirm` 再 POST 一次：后台**按第二次请求重新算一遍摘要**，和签发时记下的比较。
  
  和现在 `--kill-browsers` 完全同一种做法。应用的勾选通过 `arguments` 变成命令行参数，命令行把它原样放进两次请求，所以“预览时选的”和“执行时用的”在 root 的那一次里由确认码保证一致。应用的只读预览和执行之间如果情况变了（新的冲突、新的 Claude 进程），以执行时第一步的结果为准，确认框上已经写了“执行时会按当时的情况重新列出”。
- **终端里的交互运行**：第一步和第二步之间有人的确认，确认码保证两步的选项一致；追问后改了选项会重新发第一步、拿新码。
- **接上 `exec`**：`handleRestore` 的第二步调用 `d.execThen(w, r, d.m.Restore, true, after)`。
  - `fn` 返回错误：和现在完全一样，写错误应答，`after` 不调用，开关不动。
  - `fn` 成功：`chg := after()` → `st := d.Status()`（这时 `claude_code` 已经是切换之后的）→ `st.ClaudeProxyChange = chg` → 200。
- 应答例：
  ```json
  {"state":"RESTORED", …, "claude_proxy_change":{"target":true,"ok":false,
   "notes":["回国已完成；第四道没有打开：打开第四道（监听本机代理端口）失败：…","处理后执行 sudo abroad claude-proxy on"]}}
  ```
  冲突时多 `conflicts`。成功时是 `{"target":true,"ok":true}`。这次没要求切换时没有这一项。

## 3. 和借用虚拟机的“忙”、卸载、确认码放回的关系（顾问要求⑥）

| 情况 | 在哪里拦下 | 确认码 | 开关 |
|---|---|---|---|
| 借用进行中 | `handleRestore` 第 2 步，在 `take` 之前，回 409 `busy` | 第一步：不签发；第二步：码没被取，仍有效 | 不动 |
| 借用在检查之后才拿到忙标记 | `d.m.Restore` 回 `ErrBusy` | `giveBack`，放回的仍是同一个含 `claude_proxy` 的绑定 | 不动（`after` 没调用） |
| 经 root 命令行（`--yes`）遇到忙 | 同上两行。命令行退出码 5，应用显示“有别的操作在进行，请等它完成后再试” | 放回的码随命令行进程结束而不再使用，60 秒后过期 | 不动；用户再点一次“执行”就是全新的两步 |
| 卸载进行中 | `guard` 统一回 `busy` | 码没动 | 不动 |
| 卸载完成后回国 | 要打开 → 409 `uninstalled`，在 `take` 之前；要关或不带 → 按保持、照旧 200。只读预览这时没有 `claude_proxy` 这一块，应用也就没有勾选 | 正常 | 卸载时已经记为关 |
| 回国成功后、切开关之前开始了卸载 | `claudeProxyOn` 回 `ErrUninstalledOp` | 已用掉 | 不打开，`ok=false`，写“正在卸载或已经卸载” |
| 回国中止（结束不了 Claude）或状态机出错 | `d.m.Restore` 回错误 | 已用掉，和现在一样 | 不动 |
| 和别的 `PUT /v1/settings/claude-proxy` 同时进行 | `cpSwitchMu` 串行 | — | 后到的说了算 |
| 只读预览 | 借用、卸载、忙的时候照常返回 | 不签发 | 不动 |

借用只在 `ExclusiveHold` 里改虚拟机、主代理和基准，不碰第四道。切开关在状态机的忙标记之外，借用挡不住它，它也不会挡住借用。应用的执行都在 `PrivilegedRunner` 的串行队列里，同一时间只有一个提权命令。

## 4. 验收 a–m 对应的测试

后台新测试放在新文件 `internal/daemon/restorecp_test.go`，复用 `newCPSEnv`、`putCP`、`assertOff`、`askConfirm`、`withConfirm` 的写法，另写一个辅助函数 `restoreCP(t, d, kill bool, cp *bool)`。

| 验收 | 测试 | 断言要点 |
|---|---|---|
| a 预览 | `TestRestoreCPPreview`（POST 的 409）、`TestRestorePreviewClaudeProxy`（只读预览，新增） | 不带字段：`{current, target=current, test_mode}`，先打开再预览时也对。没打开管控：没有这一块；POST 带 `true` → 501、不签发（`confirms.m` 为空）。卸载过：真走一遍卸载后，带 `true` → 409、带 `false` 或不带 → 200；另在启用后直接放 `UninstalledFile` 标记，预览没有这一块，带 `true` → 409。只读预览：内容和 POST 第一步一致；`?claude_proxy=on` 改变目标；非法值 400；现在关时带上 `conflicts`；不签发、状态文件不变 |
| b 绑定 | `TestRestoreCPBinding` | 表驱动：(kill, cp) × (kill', cp')，cp ∈ {不带, on, off}，凡两步不同一律 403 `confirm_invalid`；之后状态仍是 ENABLED，网络快照、`cpOn`、分片、开关文件都没变。相同组合成功一次作对照 |
| c 预览查冲突 | `TestRestoreCPPreviewConflict` | 放一份 `corp.json` 定义 `HTTPS_PROXY`：POST 带 `true` → 422 加 `conflicts`、没有 `confirm`、什么都没变；带 `false` 或不带照常预览；只读预览带出同样的 `conflicts` |
| d 回国 + 打开 | `TestRestoreCPTurnOn` | 200，RESTORED，`claude_proxy_change{target:true, ok:true}`；分片内容逐字对，端口在听，开关文件记开；经端口发 CONNECT 回 403（沿用 `TestClaudeProxyStateTable` 的拨号辅助） |
| e 开 → 关 | `TestRestoreCPTurnOff`（含子测试 `modified`） | 分片删了，端口不听，开关文件记关，`ok:true`。分片被改过时：仍回 200，`ok:false`，notes 里有 `ClaudeProxyLeftoverNote`，开关记关 |
| f 回国失败 | 新增 `TestRestoreLeftoversClaudeProxy`（在 `newCPSEnv` 里重做“结束不了就中止”，开和关两个方向）；`TestRestoreCPStateMachineError`（用 `sysnet` 假实现的 `FailNext` 让 `d.m.Restore` 回错误，哪个操作写代码时核对）；`TestRestoreCPBusyGiveBack`；原有 `TestRestoreLeftovers` 不改、照样通过 | 开关、分片、端口、开关文件都和回国前一样，没有 `claude_proxy_change`；忙时同一个码之后能成功，开关只在成功那次切换 |
| g 回国成功、打开失败 | `TestRestoreCPOnFails`，四个子测试：分片目录被普通文件占着、`cpListen` 注入失败、`writeSwitch` 注入失败、端口真被占 | 200、RESTORED、`ok:false`，notes 有原因和“sudo abroad claude-proxy on”，`assertOff` |
| h 已是回国状态 | `TestRestoreCPAlreadyRestored` | 值不同 → 422、`field=claude_proxy`、文字里有命令；值相同或不带 → 200，开关不变 |
| i 测试模式 | `TestRestoreCPTestMode` | 预览里 `test_mode:true`；有冲突文件也不拒绝；回国后端口在听、没有写分片、`ok:true` |
| — 文字一致 | 扩充 `TestClaudeProxyConsequencesInAppStrings` | 新增的四个后台常量逐字出现在 `Strings.swift` |
| j 命令行 | `TestCLIRestoreClaudeProxy`、`TestCLIReadLineShared` | 非法值和缺值 → 退出码 2、0 个请求；交互时现在关输入 “\nyes\n” → 问句里有 `[y/N]`、保持、2 次请求；输入 “y\nyes\n” → 打印三句后果、3 次请求、开关开、输出“第四道已打开”；现在开输入 “n\nyes\n” → 问句里有 `[Y/n]`、打印开 → 关那一句、开关关；`--yes` 时不问并打印提示；不在终端 → 提示后 `ExitConfirm`；已回国时不问；`--yes --claude-proxy on` 遇到冲突 → 退出码 1、文字里有文件和变量、没有回国；打开失败 → 退出码 0、标准错误里有“第四道没有切换”；`--json` 输出里有 `claude_proxy_change`。另一个测试：两次读同一个输入流各读到一行 |
| k 应用 | 新文件 `app/Tests/AbroadKitTests/RestoreClaudeProxyTests.swift`：`restoreClaudeProxyToggle`、`restoreClaudeProxyArguments`、`restoreClaudeProxyCommandGolden`、`restoreClaudeProxyTerminalMatches`、`restoreClaudeProxyConflictBlocks`、`restoreClaudeProxyLines`、`restoreClaudeProxyResult`、`restoreUsesReadOnlyPreviewOnly`；扩充 `privilegedWhitelistFile`、`privilegedWhitelistRejectsUnknown`、`RepoTests` 的终端命令表；`IntegrationTests.swift` 加一条 | 预览里没有这一块时没有勾选、命令不带参数；有时初值等于 `current`。`arguments` 只在和现状不同时带 `--claude-proxy`，`--kill-browsers` 也从 `arguments` 来。golden 的 `restore-cp-on`、`restore-kill-cp-off` 逐字一致，`restore-kill` 不变。改勾选后“在终端执行”的命令等于 `.restoreWith(...).terminalCommand`，参数和提权命令一致。勾选打开且有冲突时“执行”不可点、假 runner 调用 0 次。文字来自 `L10n`。输出 JSON 里 `ok:false`、`ok:true`、退出码 1 带 conflicts、退出码 2 各显示对应文字。回国全程不发 `POST /v1/restore`（假传输记录请求）。白名单：第三列和 `allowedFlags` 一致；未列出的 flag（如 `--force`）和 `--claude-proxy maybe` 被 `build` 拒绝，runner 没被调用。集成测试：假后台的只读预览能解出 `claudeProxy` |
| — Go 白名单 | 扩充 `TestPrivilegedWhitelistMatchesRoutes` | 第三列每个 flag 出现在帮助文本对应用法行里；`restore` 有两个 flag；加一个反例：第三列里写不存在的 flag 时测试报错（用临时文件喂给抽出来的核对函数） |
| l 接口一致 | `go test ./cmd/abroad-apigen/`（`-check`）、Swift 现有的类型对照 | 重新生成的文件和入库的一致 |
| m 全量 | `go build ./...`、`go vet ./...`、`go test ./internal/... ./cmd/...`、`GOOS=linux GOARCH=arm64 go build ./...`、`scripts/dev/app-test.sh`、`scripts/m6/e2e/selftest.sh`、`scripts/m6/install/selftest.sh`（场景 u 用到命令 golden）、`scripts/dev/privacy-check.sh --commit`；设计 16 节照录原话和日期 | 全部通过 |

## 5. 第 16 节 v0.72（拟写）

**v0.72（2026-11-27）回国确认里选择第四道（Claude Code 的本机代理）回国后开还是关**

- 用户原话（2026-11-27）：“caibao 兜底 回国前提示也提示一下是否需要兜底，不需要就不兜底了”。
- architect 定的含义：
  - 一键回国的请求新增可选字段 `claude_proxy`（true/false）。不带就保持开关现状；带了表示“回国之后第四道是开还是关”；值和现状一样时按保持。
  - 确认框里的勾选默认等于现状；不勾就是关，开着时取消勾选，回国后就关掉。
  - 确认框按目标状态显示：关 → 开显示开关已有的三句后果；开 → 关显示“回国后 Claude Code 不再被指向本机代理，回国后靠解析锁和回国拦截挡住它（默认状态）”；不变只写一行现状；测试模式写“只监听，不写托管设置，本机真实的 Claude Code 不受它管”。
- 10.2 第四层原来的“回国不碰它”改为：回国流程本身不碰第四道；只有用户在这次回国确认里明确选了另一个状态，才在回国成功之后切换（`claudeProxyOn`、`claudeProxyOff`，规则照旧）。
- **先回国、成功后再切开关**。理由：
  - 回国失败（结束不了 Claude 而中止、状态机出错、忙）时开关一定不动，“中止时什么都不改”仍然成立；
  - 回国完成后切开关失败，不能把回国回滚（那等于重新接管网络），所以只在 `claude_proxy_change` 里写明原因和下一步，回国照样回 200；
  - 回国状态下第四道一打开就一律拒绝，中间没有放行的窗口。
- **预览时就查冲突**：POST 的第一步和只读预览 `GET /v1/restore/preview` 都只读地查托管设置的冲突。POST 有冲突回 422，不签发确认码、什么都不改；应用确认框里显示冲突，勾选打开时“执行”不可点。
- **确认码绑定这个选项**：摘要由 `restore`、`kill_browsers=0|1`、`claude_proxy=keep|on|off` 组成，两步不一致回 `confirm_invalid`。应用里的确认用只读预览，确认码在 root 运行的那一次 `abroad --yes --json restore …` 里由命令行的两步请求产生和核对，和 `--kill-browsers` 同一种做法。忙时确认码照旧放回。
- **授权**：并进回国这一步，不单独授权。理由：
  - 回国和 `claude-proxy on|off` 都是敏感级别，在应用里都是经系统密码框以 root 运行已安装的命令行（v0.71b）；
  - 选项由确认码绑定，默认保持；
  - 脚本里 `--yes` 不带参数时不会改变它。
- 边界：已经是回国状态时，值和现状不同回 422，提示用 `sudo abroad claude-proxy on|off`；没打开启动管控时没有这一块，要打开回 501；卸载过时没有这一块，要打开回 409 `uninstalled`，要关按保持。
- 11.1：`RestoreRequest`、`ConfirmNeeded.claude_proxy`、`Status.claude_proxy_change`、`GET /v1/restore/preview` 改为 `RestorePreviewView`（`claude_proxy` 带冲突，`?claude_proxy=`、`?kill_browsers=`）。
- 11.2：B0 回国确认框的勾选。11.3：`restore --claude-proxy on|off` 和交互时的追问。
- 11.4：提权白名单加“允许的参数”一列；回国的 `--kill-browsers`、`--claude-proxy` 都从 `SensitiveOperation.arguments` 生成，终端命令和提权命令同一份。
- 文档：`docs/fallback-plan.md`、`docs/m6-acceptance.md`（G6、G9.1、第 4 节）、`docs/m5-walkthrough.md`、`docs/app-privileged-plan.md` 的注；`scripts/m6/e2e/selftest.sh` 检查端到端的回国不带 `--claude-proxy`、CC1 照旧。计划见 `docs/restore-claudeproxy-plan.md` v2。

## 6. 风险和拿不准的地方（★ 是请顾问裁定的）

1. ★ **摘要按字面值，还是先按现状归一**。本计划按字面值：现在开时，第一步带 `true`、第二步不带，算不一致。这样验收 b 不依赖两步之间的现状。应用和命令行只在值和现状不同时才带这个字段，正常使用碰不到。
2. ★ 命令行追问时输入不是 y、n 或回车：本计划按“没有确认，什么都没有改动”退出，不猜。
3. ★ **应用里预览有冲突时**：勾选打开就让“执行”不可点，并显示冲突（预览已经带了 `conflicts`，不用另发请求）。只读预览和执行之间才出现的冲突，由 root 命令行的第一步回 422，应用显示冲突文字，什么都没改。
4. ★ 回国成功、切开关失败时，命令行退出码定为 0（回国成功，失败写到标准错误，`--json` 里有 `claude_proxy_change`）；应用按 JSON 显示“回国已完成，但第四道没有切换”。
5. 两步之间现状被别人改了：带了明确值时以用户的选择为准；带 keep 时以执行时的现状为准。只读预览显示的现状可能过时，影响只是文字。
6. 只读预览时没有冲突，执行前别人写了冲突文件：命令行第一步回 422，什么都不改（验收 c 的同类）。如果这份文件是在命令行两步之间、回国之后才出现的，`claudeProxyOn` 会拒绝，`ok:false`（验收 g 的同类）。
7. 端口被占只有在回国之后才发现（预览不试着监听）：`ok:false`，开关保持关（验收 g）。
8. **已安装的命令行比应用旧**：老命令行不认 `--claude-proxy`，回退出码 2，应用照旧提示“请先升级”。只有勾选和现状不同才会带这个参数，不勾、不改的默认路径对老命令行不受影响。
9. `restorePreview()` 的返回类型改了：设置页的进程清单跟着改读法。嵌入后字段平铺，旧客户端按 `ClaudeprocPreview` 解码仍然能用。
10. 白名单加第三列会改动 B0 刚审过的 golden 和两边的核对测试。如果顾问更希望只核对子命令、flag 由 `arguments` 的单测守着，可以不加第三列，只加命令 golden 的两例。
11. 读终端改为共用一个缓冲读取器，会碰到 `claude-proxy on` 和所有两步确认的读取路径；行为应该不变，由现有的命令行测试守着。
12. 检查顺序是“开关不可用、已卸载”在“已经是回国状态”之前：卸载后状态总是 RESTORED，要打开时回 409 而不是 422，和原判定一致。
13. 计划里没有任何改宿主机网络、碰 pf、sudo、运行真实模式或真实提权的命令。`scripts/m6/install/selftest.sh` 场景 u 是以当前用户在自测目录里用 `bash -c` 跑 golden 命令，不提权、不碰系统路径（B0 已审过的做法）。
