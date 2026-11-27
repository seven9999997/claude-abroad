# 回国确认里选择第四道：实施计划（交架构顾问审）

> 版本：**v1**（2026-11-27）。只是计划，代码还没改。
> 依据：用户 2026-11-27 原话“caibao 兜底 回国前提示也提示一下是否需要兜底，不需要就不兜底了”；architect 的判定五条和验收 a–m（原文在任务说明里，本计划逐条对上）；设计 v0.70 的 10.1、10.2 第四层 v0.68 一段、11.1–11.3。
> 会话里的边界：不改宿主机网络，不碰本机会话依赖的代理和隧道（见 `docs/LOCAL.md`），不用 sudo，不运行真实模式，不碰 `/Applications/ClaudeAbroad.app` 和 `/Library/Application Support/claude-abroad`，不提交。下面所有测试都是假系统、临时目录、`127.0.0.1:0` 端口；swift 的集成测试由 `scripts/dev/app-test.sh` 以当前用户起 `--fake-system` 的 abroadd。

## 0. 核对代码后的现状（计划依据）

- `internal/daemon/confirm.go` `handleRestore`：读请求体（`ConfirmRequest`，`DisallowUnknownFields`）→ 借用中回 409 `busy` → 状态已是 RESTORED 回 200 `Status` → 摘要 `digestOf(OpRestore, browserChoice(kill))` → 不带码时预览并签发 → 带码时 `confirms.take` → `d.exec(w, r, d.m.Restore, true)`，返回 `state.ErrBusy` 时 `giveBack`。
- `ConfirmRequest` 被 `enable`、`region-put`、`uninstall` 共用（路由表 `routes.go`）。直接往里加 `claude_proxy` 会让启用、卸载也“接受”这个字段而不理会，所以回国单独用一个请求类型。
- `internal/daemon/daemon.go` `exec`：成功时直接 `writeJSON(200, d.Status())`，没有“成功之后、写应答之前”的钩子。
- `claudeswitch.go`：`claudeProxyOn()`（`cpSwitchMu` 串行；已开时什么都不做；正在卸载或已卸载回 `ErrUninstalledOp`；测试模式只监听；冲突返回 `[]claudecfg.Conflict` + `errCPConflict`；三步任一失败回到关）、`claudeProxyOff()`（返回 notes：删不掉、被改过时带 `ClaudeProxyLeftoverNote`；只有记开关失败才返回 err）、`cpAvailable()`、`cpTestMode()`、`d.cpOn`。`claudecfg.Settings.Conflicts()` 只读。
- `routes.go` 的 `guard`：卸载进行中所有非 GET 回 `busy`；卸载完成后 `restore`/`restore-alias` 仍放行（`AllowedAfterUninstall`），所以回国处理函数里要自己处理“卸载过”。
- 命令行 `cmd/abroad/main.go` `confirmedWord`：第一步 → 打印预览 → （不是 `--yes`）不在终端回 `ExitConfirm`，在终端读确认词 → 带码第二步。读终端用的是每次新建的 `bufio.NewReader(a.stdin)`（`main.go:382`、`claudeproxy.go:99`）。
- 应用：`ConfirmFlow` 已有 `killBrowsers` 和 `reissuer`（勾选和取得确认框时不同就带新选项重新发第一步）；`APIClient.reissue(_:killBrowsers:)`、`requestedKillBrowsers`；`ConfirmPrompt` 从 `DaemonConfirmNeeded` 取字段；`Dialogs.swift` `ConfirmSheet` 浏览器那组里放 `Toggle`。应用作为控制台用户调用回国会先得到 403（敏感级别），走终端退路 `SensitiveOperation.restore` → `sudo abroad restore`；确认框只在调用方够格时出现（测试、将来系统授权）。

## 1. 改哪些文件、按什么顺序、函数级改动

### 第 1 步：后台的类型和小函数（`internal/daemon/confirm.go`、`claudeswitch.go`、`daemon.go`）

1. 新类型 `RestoreRequest`（confirm.go）：
   ```go
   type RestoreRequest struct {
       Confirm      string `json:"confirm,omitempty"`
       KillBrowsers bool   `json:"kill_browsers,omitempty"`
       // ClaudeProxy：回国之后第四道是开还是关（v0.71）。不带 = 保持现状。
       ClaudeProxy *bool `json:"claude_proxy,omitempty"`
   }
   ```
   `routes.go` 里 `restore`、`restore-alias` 两行的 `Req` 改成 `RestoreRequest{}`；`ConfirmRequest` 不动（启用、切换地区、卸载照旧不认这个字段，带了回 400）。
2. `cpChoice(p *bool) string`（confirm.go，挨着 `browserChoice`）：`nil` → `claude_proxy=keep`，`true` → `claude_proxy=on`，`false` → `claude_proxy=off`。**按请求里的字面值绑定，不做“和现状一样就算 keep”的归一**（理由见第 6 节风险 1）。
3. 预览块 `RestoreClaudeProxy`（confirm.go）：`{Current bool "current"; Target bool "target"; TestMode bool "test_mode"}`，三个字段都不省略。`ConfirmNeeded` 加 `ClaudeProxy *RestoreClaudeProxy "claude_proxy,omitempty"`。
4. 回国成功后的结果 `ClaudeProxyChange`（claudeswitch.go）：`{Target bool "target"; OK bool "ok"; Notes []string "notes,omitempty"; Conflicts []claudecfg.Conflict "conflicts,omitempty"}`。`Status` 加 `ClaudeProxyChange *ClaudeProxyChange "claude_proxy_change,omitempty"`（和 `BrowsersLeft` 同样的做法：只在回国这次应答里填，`d.Status()` 本身从不填，所以 `/v1/status` 永远没有它）。这样回国 200 的应答保留 `Status` 原有全部字段，应用照旧解成 `DaemonStatus`。
5. 文字常量（claudeswitch.go，命令行和应用文字表各一份，测试对照）：
   - `ClaudeProxyRestoreOffNote = "回国后 Claude Code 不再被指向本机代理，回国后靠解析锁和回国拦截挡住它（默认状态）"`（开 → 关）；
   - `ClaudeProxyRestoreTestNote = "测试模式：只监听，不写托管设置，本机真实的 Claude Code 不受它管"`；
   - `ClaudeProxyRestoreKeepOn = "第四道保持现状（开）"`、`ClaudeProxyRestoreKeepOff = "第四道保持现状（关）"`（不变时那一行；命令行 `--yes` 时后面接“；要改加 --claude-proxy on|off”）；
   - 关 → 开沿用 `ClaudeProxyConsequences` 三句，不另写。
6. `restoreCPTarget(req *bool) (target *bool, status int, body any)`（confirm.go）——两步都先过这一关，**在 `confirms.take` 之前**（校验不过时确认码不被消耗，也不签发新的）：
   - `req == nil` → 保持，返回 `nil`；
   - `!d.cpAvailable()`：`*req` 为真 → 501 `unsupported`（“回国并打开第四道失败：这次安装没有打开启动管控。第四道只在打开了启动管控的安装里有；不带 --claude-proxy 再回国。”），为假 → 按保持（返回 `nil`）；
   - `d.uninstalled()`：为真 → 409 `uninstalled`（`UninstalledText`），为假 → 按保持；
   - `*req == d.cpOn.Load()` → 按保持（返回 `nil`；摘要仍按字面值，见 2）；
   - 否则返回 `req`。
7. 抽出 `execThen`（daemon.go）：
   ```go
   func (d *Daemon) execThen(w, r, fn, allowCorrupt bool, after func() *ClaudeProxyChange) error
   ```
   与 `exec` 相同，只是 `fn` 成功后先调用 `after()`（可为 nil），再取 `d.Status()`，把 `after` 的结果放进 `st.ClaudeProxyChange`，写 200。`exec` 改成 `return d.execThen(w, r, fn, allowCorrupt, nil)`，其余调用方不变。`fn` 失败的路径一字不改（`after` 不会被调用）。

### 第 2 步：`handleRestore` 的新流程（confirm.go）

顺序（每一步失败都不再往下走）：

1. 读请求体，严格解成 `RestoreRequest`；出错文字改成“只认 confirm、kill_browsers、claude_proxy”。
2. 借用中 → 409 `busy`（不变，在 `take` 之前，所以第二步遇到借用时确认码没被动过，不需要放回）。
3. `target, st, body := d.restoreCPTarget(req.ClaudeProxy)`；有错误直接写（501 / 409），不签发、不消耗。
4. 状态已是 RESTORED：`req.ClaudeProxy != nil && *req.ClaudeProxy != d.cpOn.Load()`（且开关可用、没卸载过——已被第 3 步挡掉或归为保持）→ 422 `invalid`，`field: "claude_proxy"`，文字“已经是回国状态，回国这一步不再执行，第四道也没有改：要切换第四道请用 sudo abroad claude-proxy on”（`off` 时写 off）；否则照旧 200 `Status`。
   - 实现上只要判断 `target != nil` 即可（第 3 步已经把“值和现状一样”归为 nil）。
5. `digest := digestOf(OpRestore, browserChoice(req.KillBrowsers), cpChoice(req.ClaudeProxy))`。
6. 不带确认码（第一步）：
   - 若 `target != nil && *target && !d.cpTestMode() && d.opts.Sys.ClaudeSettings != nil`：调用 `ClaudeSettings.Conflicts()`（只读），有冲突 → 422 `{"code":"invalid","error": errCPConflict{...}.Error() 改写成“回国前检查第四道”的版本,"conflicts":[...]}`，不签发确认码、什么都不改（和 `PUT /v1/settings/claude-proxy` 的冲突应答同一形状）。冲突文字的下一步写“开关仍是关，没有回国。去掉这些变量后再试，或这次不勾选（不带 --claude-proxy）回国。”
   - 预览照旧；`ConfirmNeeded.ClaudeProxy` 在 `d.cpAvailable() && !d.uninstalled()` 时填 `{current: cpOn, target: target 或 current, test_mode: cpTestMode()}`，否则 nil。
   - 日志加一句“第四道：现状开/关，回国后开/关”（不含路径）。
7. 带确认码（第二步）：`take`（摘要不符 → 403 `confirm_invalid`，不变）→ kill_browsers 照旧放进 ctx →
   ```go
   var after func() *ClaudeProxyChange
   if target != nil { t := *target; after = func() *ClaudeProxyChange { return d.restoreSwitchCP(t) } }
   if err := d.execThen(w, r, d.m.Restore, true, after); errors.Is(err, state.ErrBusy) {
       d.confirms.giveBack(req.Confirm, held)
   }
   ```
   注意：第二步不再查冲突（第一步查过；两步之间别人新写的文件由 `claudeProxyOn` 自己的冲突检查兜住，结果写进 `claude_proxy_change`）。

### 第 3 步：回国成功之后切开关（claudeswitch.go 新函数 `restoreSwitchCP`）

```go
func (d *Daemon) restoreSwitchCP(on bool) *ClaudeProxyChange {
    ch := &ClaudeProxyChange{Target: on}
    if on {
        conf, err := d.claudeProxyOn()          // 规则照旧：冲突拒绝、三步回滚、测试模式只监听、卸载中拒绝
        ch.OK, ch.Conflicts = err == nil, conf
        if err != nil { ch.Notes = []string{"回国已完成；第四道没有打开：" + 原因, "处理后执行 sudo abroad claude-proxy on"} }
    } else {
        notes, err := d.claudeProxyOff()
        ch.Notes = notes
        ch.OK = err == nil && len(notes) == 0   // 分片被改过（leftover）也算没完全做到
        if err != nil { ch.Notes = append(ch.Notes, err.Error()) }
        if !ch.OK { ch.Notes = append(ch.Notes, "处理后执行 sudo abroad claude-proxy off") }
    }
    d.events.Add("restore", "claude_proxy_"+onOff(on), okText)   // 事件详情只有“成功/没有完成”，不含路径
    return ch
}
```

- 原因文字：`ErrUninstalledOp` → “正在卸载或已经卸载”；冲突 → 列出“文件 的 变量”（`conflictParts`）；其余用 `claudeProxyOn` 返回的 usererror 原文（已经是“做什么失败了：原因。下一步”，里面已有“开关仍是关，已写的撤掉”）。
- 这一步发生在状态机的忙标记之外（`d.m.Restore` 已经返回），和 `PUT /v1/settings/claude-proxy` 一样只靠 `cpSwitchMu` 串行；回国状态下第四道打开后本机代理端口一律拒绝（现有 `claudeproxy.go` 的规则），所以“先回国、后打开”中间没有放行窗口。
- “先回国、再切开关”的理由（写进第 16 节）：回国失败（结束不了 Claude 中止、状态机出错、忙）时开关一定不动，`TestRestoreLeftovers`“中止什么都不改”保持成立；开关的失败不能让已经完成的回国回滚（回滚就是重新接管网络），所以只报告。

### 第 4 步：接口描述和生成的类型

- `go run ./cmd/abroad-apigen -root .` 重新生成 `api/openapi.json`、`api/swift/APITypes.swift`（新 `DaemonRestoreRequest`、`DaemonRestoreClaudeProxy`、`DaemonClaudeProxyChange`，`DaemonConfirmNeeded.claudeProxy`、`DaemonStatus.claudeProxyChange`）；`go test ./cmd/abroad-apigen/` 的一致性测试通过。
- `routes.go` restore 两行的 `Note` 补“可带 claude_proxy：回国之后第四道开或关（确认码绑定）”。

### 第 5 步：命令行（`cmd/abroad/commands.go`、`main.go`、`claudeproxy.go`）

1. `app` 加一个共用的行读取器 `a.readLine() string`（惰性建一个 `*bufio.Reader` 包住 `a.stdin`，之后都用它）；`main.go:382` 和 `claudeproxy.go:99` 改用它。原因：回国这次要在终端连续读两行（y/N 和确认词），每次新建 `bufio.Reader` 会把第二行吞进第一个缓冲区（测试里的 `strings.Reader` 必然复现）。
2. `cmdRestore`：`parseFlags` 增加带值的 `--claude-proxy`（只认 `on`、`off`，别的值、缺值 → `usageErr`，退出码 2，不发请求）。带了就放进 body `claude_proxy: true|false`。
3. `confirmedWord` 拆出可选的“最后确认前的追问”：新签名 `confirmedAsk(method, path, body, word, ask func(raw []byte, body map[string]any) (map[string]any, bool, int))`，`confirmedWord` 传 nil 保持原行为。流程：
   - 第一步应答是 409 预览：照旧打印；**再打印第四道那一块**（见 4）；
   - `ask != nil`、不是 `--yes`、在终端、没带 `--claude-proxy`、预览里有 `claude_proxy`：问一句——现在关：“回国后开启 Claude Code 兜底（第四道）？[y/N]”；现在开：“回国后保留 Claude Code 兜底（第四道）？[Y/n]”。回车 = 保持；`y/yes/是` = 开；`n/no/否` = 关；其他输入 → “没有确认，什么都没有改动”，`ExitConfirm`（不猜）。
   - 答得和现状相同 → 不改 body，直接到确认词；答得不同 → body 加 `claude_proxy`，**带新选项重新发第一步**（拿新确认码；旧码作废不用，60 秒后自然过期），重新打印预览和第四道那一块（这次是后果），再要求确认词。第二次预览不再追问。
   - `--yes` 或不在终端、没带 `--claude-proxy`、预览里有 `claude_proxy`：不问，打印“第四道保持现状（开/关）；要改加 --claude-proxy on|off”（不在终端又没 `--yes` 时照旧随后 `ExitConfirm`）。
4. 第四道那一块（预览里有 `claude_proxy` 时，写到预览的输出流；`--json` 时是标准错误）：
   - target = current：一行 `ClaudeProxyRestoreKeepOn/Off`；
   - 关 → 开：“回国后打开第四道（Claude Code 的本机代理）的后果：”+ `ClaudeProxyConsequences` 三句；
   - 开 → 关：`ClaudeProxyRestoreOffNote`；
   - `test_mode` 为真时再加 `ClaudeProxyRestoreTestNote`。
5. 回国 200 之后：`printState`、`printBrowsersLeft` 照旧，再 `printCPChange`：有 `claude_proxy_change` 时，ok → 标准输出“第四道已打开”/“第四道已关闭”；不 ok → 标准错误“回国已完成，但第四道没有切换：”+ notes + 冲突（文件 的 变量）。**退出码仍是 0**（回国成功；见第 6 节风险 4）。`--json` 原样输出应答。
6. 已经回国：第一步就是 200（不带参数）或 422（带了不同的值），`confirmedAsk` 不进入预览分支，自然不问；422 按现有错误打印（原文三段），退出码按现有映射（`ExitFail`）。
7. 帮助文本 `main.go:47`：`sudo abroad restore [--kill-browsers] [--claude-proxy on|off]   一键回国（两步确认；--claude-proxy 选回国后第四道开或关，不带就保持现状；后台服务不在时用 sudo abroad-rescue）`。

### 第 6 步：应用（`app/Sources/AbroadKit/*`、`app/Sources/ClaudeAbroad/Dialogs.swift`）

1. `APIClient.swift`：
   - `ConfirmPrompt` 加 `public let claudeProxy: DaemonRestoreClaudeProxy?`（取自 `needed.claudeProxy`）。
   - `reissue(_:killBrowsers:claudeProxy:as:)`：`claudeProxy: Bool?` 为 nil 时去掉 `claude_proxy`，否则写入；原来只带浏览器的调用改为新签名（`claudeProxy` 取当前请求里的值，保持不变）。
   - `static func requestedClaudeProxy(_ p: ConfirmPrompt) -> Bool?`（第一步请求里的值）。
   - `ErrorText` 加 `conflicts: [String]`（从错误应答的 `conflicts` 解出“文件 的 变量”；解析不了的文件写“文件（解析不了）”），`from(body:)` 一并解析。
2. `ConfirmFlow.swift`：
   - `@Published public var claudeProxyOn = false`；私有 `promptCP: Bool?`（这个确认框是按哪个请求值取得的）。
   - `show(p, refreshed:)`：每次都把 `claudeProxyOn` 设为 `p.claudeProxy?.target ?? false`、`promptCP = APIClient.requestedClaudeProxy(p)`（新确认框和重新预览都以后台给的 target 为准）。
   - 派生值 `public var claudeProxyRequest: Bool?`：`prompt.claudeProxy` 为 nil 时 nil；勾选等于 `current` 时 nil（保持，不带字段）；否则勾选值。
   - `confirm()`：`killBrowsers != promptKill || claudeProxyRequest != promptCP` 时走 `reissuer(p, killBrowsers, claudeProxyRequest)`（确认码同时绑定两个选项），其余不变。`reissuer` 闭包签名加一个参数。
   - 记下最后一次提交用的选项 `public private(set) var submittedOptions: (kill: Bool, claudeProxy: Bool?)`，给终端退路用（见 4）。
   - 纯函数 `public static func claudeProxyLines(current:target:testMode:) -> [String]`（放在 `Strings.swift` 的 `L10n` 里，单测覆盖三种目标状态加测试模式）。
3. `Strings.swift`：
   - `restoreClaudeProxyOption = "回国后开启 Claude Code 兜底（第四道）"`；
   - `restoreClaudeProxyOffNote`、`restoreClaudeProxyTestNote`、`restoreClaudeProxyKeepOn/Off` 与后台常量逐字相同；关 → 开用已有的 `claudeCodeConsequences`；
   - `restoreClaudeProxyConflicts(_ items:)`（“回国前检查第四道：别的托管设置文件也定义了代理变量：…。这次没有回国、什么都没有改；去掉这些变量后再试，或取消勾选再回国”）；
   - `restoreClaudeProxyChangeFailed(_ notes:)`、`restoreClaudeProxyChangeOn`、`restoreClaudeProxyChangeOff`（回国成功后首页提示）。
4. 终端命令：`SensitiveOperation` 加 `case restoreWith(killBrowsers: Bool, claudeProxy: Bool?)`，`terminalCommand` = `sudo abroad restore` + （kill 时 ` --kill-browsers`）+ （非 nil 时 ` --claude-proxy on|off`）；`.restore` 保持 `sudo abroad restore`（第一步就被 403 时还没有预览，没有选项可带）。`StatusModel.finish` 在 `.needsAuthorization` 且 `op == .restore` 时，如果确认框刚提交过（`flow.submittedOptions` 有值）就用 `.restoreWith(...)`。因为 `claudeProxyRequest` 只在和现状不同时非 nil，命令只在不同时带 `--claude-proxy`。
5. `StatusModel.finish` `.ok`：`op == .restore` 且 `st.claudeProxyChange` 有值：ok 时首页 info 一句（已打开/已关闭），不 ok 时 error 一句（`restoreClaudeProxyChangeFailed`，notes 原样列出）。`.invalid(t)` 且 `t.conflicts` 非空时，提示文字用 `restoreClaudeProxyConflicts(t.conflicts)`（确认框随 `.failed` 关掉，用户取消勾选后重新点“一键回国”）。
6. `Dialogs.swift` `ConfirmSheet`：在浏览器那组之后，`prompt.claudeProxy != nil` 时加一组 `CardSection(L10n.claudeCodeTitle)`：`CardRow { Toggle(L10n.restoreClaudeProxyOption, isOn: $flow.claudeProxyOn).disabled(flow.submitting) }`，下面按 `L10n.claudeProxyLines(current:, target: flow.claudeProxyOn, testMode:)` 逐行 `ToneNote`（关 → 开用 `.attention` 带 `hand.raised`，其余 `.info`/次要文字）。只用现有的 `CardSection`/`CardRow`/`ToneNote`/`Palette`，不写色值（RestyleTests 的源码检查守着）。

### 第 7 步：文档（v0.71）

- `docs/design.md`：头部状态行加 v0.71；10.1 流程后加“v0.71：回国确认里第四道的选项”一段（字段、默认保持、目标状态的三种文字、测试模式、预览查冲突、已回国 422、没管控 501、卸载过 409）；10.2 第四层 v0.68 一段把“**回国不碰它**”改成“回国流程本身不碰第四道；只有用户在这次回国确认里明确选了另一个状态，才在回国成功之后切换（走 claudeProxyOn/claudeProxyOff，规则照旧）”，测试模式那一条补“回国时打开也一样”，入口那一条补“或在回国确认里选”；11.1 的 v0.71 一段（`claude_proxy` 请求字段、`ConfirmNeeded.claude_proxy`、200 的 `claude_proxy_change`、422/501/409、确认码绑定 `claude_proxy=keep|on|off`）；11.2 回国确认框的勾选和文字；11.3 命令行新参数、交互那句、`--yes` 时的提示、回国后打印切换结果；16 节 v0.71（见第 5 节）。
- `docs/fallback-plan.md` 开头 v0.68 说明下加一行：“v0.71（2026-11-27）：也可以在一键回国的确认里选回国后第四道开或关，见设计 10.2 第四层、16 节 v0.71。”
- `docs/m6-acceptance.md`：G9.1 行补 `go:internal/daemon#TestRestoreCPPreview`、`TestRestoreCPTurnOn`、`TestRestoreCPTurnOff`、`TestRestoreCPOnFails`、`go:cmd/abroad#TestCLIRestoreClaudeProxy`、`swift:restoreClaudeProxyToggle`；G6 行补 `go:internal/daemon#TestRestoreCPBinding`、`TestRestoreLeftovers`（扩充后）；第 4 节第四道那一行“打开（`sudo abroad claude-proxy on`）”后补“也可以在回国确认里选”。
- `docs/m5-walkthrough.md` 回国确认框（L1-41 之后）加 L1-41a–e：勾选（显示后果三句、确认后状态里第四道已开）、不勾（保持）、开着时取消勾选（显示“回国后 Claude Code 不再被指向本机代理…”）、有冲突（假系统里先放一份别的托管设置分片；首页红色提示列出文件和变量、没有回国）、测试模式（写“测试模式：只监听…”）。标“待代测”，只在打开了启动管控的安装上有（没打开时不出现这一组，也走查一次）。
- 端到端：脚本里的回国都是应用里点或 `--yes`、不带新参数，行为不变；`scripts/m6/e2e/selftest.sh` 加两条检查：① `run.sh`、`restore.sh`、`lib.sh`、`commands.txt` 和三段空跑输出里出现的 `restore` 命令都不带 `--claude-proxy`（`grep -E 'abroad.* restore.*--claude-proxy'` 不应命中）；② `claude-proxy` 只以 `--yes claude-proxy on`、`--yes claude-proxy off` 两条命令出现（CC1 照旧）。`commands.txt` 不改。
- `docs/PROGRESS.md` 按工作循环更新（交验收时）。

### 顺序

第 1 → 2 → 3 步（后台，带测试）→ 第 4 步（重新生成类型）→ 第 5 步（命令行）→ 第 6 步（应用）→ 第 7 步（文档、selftest）→ 全量验证（第 4 节 m）。每一步结束跑 `go build ./... && go vet ./... && go test ./internal/daemon/ ./cmd/abroad/`。

## 2. “先回国、再切开关”怎么接到 `d.exec` 上；`claude_proxy_change` 的写法

- `handleRestore` 第二步调用 `d.execThen(w, r, d.m.Restore, true, after)`。`execThen` 里：`fn` 返回错误 → 与现在完全相同地写错误应答并返回错误（`after` 不调用，开关不动）；`fn` 成功 → `chg := after()`（可能为 nil）→ `st := d.Status()`（这时 `claude_code` 已反映切换之后的状态）→ `st.ClaudeProxyChange = chg` → 200。
- 应答例（回国成功、打开失败）：
  ```json
  { "state": "RESTORED", ..., "claude_code": {"enabled": false, ...},
    "claude_proxy_change": { "target": true, "ok": false,
      "notes": ["回国已完成；第四道没有打开：打开第四道（监听本机代理端口）失败：…。开关仍是关，已写的托管设置已撤掉。…", "处理后执行 sudo abroad claude-proxy on"] } }
  ```
  冲突时 `conflicts: [{"file":"corp.json","var":"HTTPS_PROXY"}]`。成功时 `{"target": true, "ok": true}`。这次没要求切换（不带、或值和现状一样）时没有这一项。
- `exec` 的其余调用方行为不变（`exec` 只是 `execThen(..., nil)`）；`Status` 新字段 `omitempty`，`/v1/status` 永远不带。

## 3. 和借用虚拟机的“忙”、卸载、确认码放回的关系

| 情况 | 在哪一步拦下 | 确认码 | 开关 |
|---|---|---|---|
| 借用进行中（`d.borrowing`） | 第 2 步，`take` 之前，409 `busy` | 第二步的码没被取，仍有效 | 不动 |
| 借用刚开始、`borrowing` 检查之后才拿到忙标记 | `d.m.Restore` 返回 `state.ErrBusy` | `giveBack`（与现在一样；摘要含 `claude_proxy`，放回的是同一个绑定） | 不动（`after` 没调用） |
| 卸载进行中 | `guard` 统一回 `busy` | 没到处理函数，码没动 | 不动 |
| 卸载完成后回国 | 要打开 → 409 `uninstalled`（`take` 之前，不签发不消耗）；要关 / 不带 → 按保持、照旧回国 | 正常 | 卸载时已记为关，不再动 |
| 回国成功后、切开关前开始了卸载 | `claudeProxyOn` 的 `ErrUninstalledOp` | 已用掉 | 不打开，`ok=false`，“正在卸载或已经卸载” |
| 回国中止（结束不了 Claude）/ 状态机出错 | `d.m.Restore` 返回错误 | 已用掉（与现在一样，要重新预览） | 不动 |
| 回国成功后和别的 `PUT /v1/settings/claude-proxy` 并发 | `cpSwitchMu` 串行 | — | 后到的那个说了算（与两次 PUT 并发相同） |

借用本身只在 `ExclusiveHold` 里改虚拟机、主代理和基准，不碰第四道；切开关在状态机忙标记之外，不会被借用挡住，也不会挡住借用。

## 4. 验收 a–m 对应的测试

新测试放在新文件 `internal/daemon/restorecp_test.go`（复用 `newCPSEnv`、`putCP`、`assertOff`、`askConfirm`/`withConfirm` 的写法，另加 `restoreCP(t, d, kill bool, cp *bool) (int, []byte)`）。

| 验收 | 测试 | 断言要点 |
|---|---|---|
| a 预览 | `TestRestoreCPPreview` | 启用后不带字段：`claude_proxy == {current:false, target:false, test_mode:false}`；先打开再预览：`{true,true,false}`；带 `true`（现在关）→ `target:true`。没打开管控（`newConfirmEnv`，`cpAvailable` 假）：没有这一块；带 `true` → 501 `unsupported`，没有 `confirm` 字段，`confirms.m` 为空；带 `false` → 照常 409 预览。卸载过：正常卸载后状态总是 RESTORED、也不能再启用，预览走不到，所以分两段测——① 真走一遍卸载：带 `true` → 409 `uninstalled`（先于“已回国”的判断）、不签发；带 `false`/不带 → 200；② 启用后在状态目录直接放 `UninstalledFile` 标记（`uninstalled()` 只看这个文件；模拟“卸载标记在、状态还没回国”的异常情况）：预览没有这一块，带 `true` → 409 |
| b 绑定 | `TestRestoreCPBinding` | 表驱动：第一步 (kill, cp) × 第二步 (kill', cp')，cp ∈ {不带, on, off}、kill ∈ {假, 真}，凡是不同的组合一律 403 `confirm_invalid`；每次之后断言状态仍 ENABLED、网络快照没变（`e.sys.Net.Snapshot()`）、`cpOn` 和分片、开关文件没变；相同组合的一次成功作对照 |
| c 预览查冲突 | `TestRestoreCPPreviewConflict` | 分片目录里放 `corp.json` 定义 `HTTPS_PROXY`；带 `true` → 422 `invalid`、`conflicts` 有 `corp.json`/`HTTPS_PROXY`，应答没有 `confirm`，状态 ENABLED，开关关，没写分片；带 `false`/不带照常预览（冲突与关无关） |
| d 回国 + 打开 | `TestRestoreCPTurnOn` | 启用 → 预览带 `true` → 确认：200，`state=RESTORED`，`claude_proxy_change{target:true, ok:true}`，`cpOn`，分片存在且内容等于 `claudecfg` 的固定内容，端口在听，开关文件 `{"version":1,"on":true}`；经端口发 CONNECT（沿用 `TestClaudeProxyStateTable` 的拨号辅助）→ 403 |
| e 开 → 关 | `TestRestoreCPTurnOff` | 先 `putCP(true)`、启用；带 `false` 回国：分片删、端口不听（拨号被拒）、开关文件记关、`ok:true`。子测试 `modified`：回国前改写分片 → 回国仍 200，`ok:false`，notes 含 `ClaudeProxyLeftoverNote`，开关记关 |
| f 回国失败 | 扩充 `TestRestoreLeftovers`（m4confirm_test.go，现在用的是没打开管控的环境，所以新增同文件的 `TestRestoreLeftoversClaudeProxy`，在 `newCPSEnv` 里重做“结束不了中止”，两个方向各一遍）；`TestRestoreCPStateMachineError`（`sysnet` fake 的 `FailNext` 让 `d.m.Restore` 返回错误，具体用哪个操作写代码时核对 `state.Restore` 哪一步的错误会返回）；`TestRestoreCPBusyGiveBack`（模仿 `TestBorrowBusyEnableRestoreUninstall`：确认码在手时让状态机忙 → 409 `busy` → 忙结束后同一个码成功，开关只在成功那次才切） | 两个方向：开关、分片、端口、开关文件都和回国前一样；没有 `claude_proxy_change` |
| g 回国成功但打开失败 | `TestRestoreCPOnFails`（三个子测试，注入方式同 `TestClaudeProxyTurnOnRollback`：分片目录被普通文件占着、`d.cpListen` 返回错误、`d.writeSwitch` 返回错误；另加端口真被占着一例同 `TestClaudeProxyTurnOnPortBusy`） | 200、`state=RESTORED`、`ok:false`、notes 含原因和“sudo abroad claude-proxy on”，`e.assertOff`（开关关、没有分片和记录、端口不听） |
| h 已是回国状态 | `TestRestoreCPAlreadyRestored` | 回国后：带和现状不同的值 → 422 `invalid`、`field=claude_proxy`、文字含“sudo abroad claude-proxy on”（或 off）；带相同值、不带 → 200，开关没变 |
| i 测试模式 | `TestRestoreCPTestMode`（`e.testOnly = true`） | 预览 `test_mode:true`；带 `true`、分片目录里有冲突文件也不拒绝（测试模式不查托管设置冲突）；回国后端口在听、分片目录里没有 `claude-abroad.json`、`ok:true` |
| — 文字一致 | 扩充 `TestClaudeProxyConsequencesInAppStrings` | 新增的四个后台常量逐字出现在 `Strings.swift` |
| j 命令行 | `TestCLIRestoreClaudeProxy`（cmd/abroad，`newRig(t, rigOpt{cproxy: true})`） | 子测试：`--claude-proxy maybe`、缺值 → 退出码 2、0 个请求；交互（`in{tty:true}`）现在关、输入“\nyes\n” → 问句含“[y/N]”、保持、回国后开关关、只有两次请求；输入“y\nyes\n” → 打印三句后果、共三次请求（预览、新预览、确认）、开关开、输出“第四道已打开”；先打开再交互、问句含“[Y/n]”、输入“n\nyes\n” → 打印开 → 关那一句、开关关；`--yes restore` → 不问、打印“第四道保持现状（关）；要改加 --claude-proxy on|off”；不是终端又没 `--yes` → 同样提示后 `ExitConfirm`；已回国时再 `restore` → 不问；`--yes restore --claude-proxy on` 但有冲突 → 失败退出、文字含文件和变量、没回国；打开失败（注入监听失败）→ 退出码 0、标准错误有“第四道没有切换”。另加 `TestCLIReadLineShared`：两次读取同一个 `strings.Reader` 得到两行 |
| k 应用 | `app/Tests/AbroadKitTests/RestoreClaudeProxyTests.swift`（新文件，Swift Testing）：`restoreClaudeProxyToggle`（预览里没有这一块时 `prompt.claudeProxy == nil`、界面条件为假；有时初始值等于 `target`）、`restoreClaudeProxyReissueBinds`（改勾选 → 发出的第一步请求体带 `claude_proxy`、保留 `kill_browsers`；勾回现状 → 不带；同时改两个选项只重新预览一次；新确认框之后提交的请求体两项都在）、`restoreClaudeProxyLines`（三种目标状态和测试模式的文字来自 `L10n`、关 → 开就是 `claudeCodeConsequences`）、`restoreClaudeProxyTerminalCommand`（`.restoreWith` 只在不同时带 `--claude-proxy`；`RepoTests` 里对照命令行帮助的表加这一项）、`restoreClaudeProxyConflictText`（422 带 conflicts → 首页提示用文字表的句子）、`restoreClaudeProxyChangeNotice`（200 带 `claude_proxy_change` ok/不 ok 的首页提示）。集成测试（`IntegrationTests.swift`，假后台）加一条：带 `claude_proxy` 的第一步应答能解出 `DaemonConfirmNeeded.claudeProxy` | — |
| l 接口一致 | `go test ./cmd/abroad-apigen/`（现有一致性测试）、`swift:` 现有的路由/类型对照测试 | 重新生成后入库文件一致 |
| m 全量 | `go build ./...`、`go vet ./...`、`go test ./internal/... ./cmd/...`、`GOOS=linux GOARCH=arm64 go build ./...`、`scripts/dev/app-test.sh`、`scripts/m6/e2e/selftest.sh`、`scripts/dev/privacy-check.sh --commit` | 全过；设计 16 节照录原话和日期（文档核对，见第 5 节） |

## 5. 第 16 节 v0.71（拟写的内容）

**v0.71（2026-11-27）回国确认里选择第四道（Claude Code 的本机代理）回国后开还是关**

- 用户原话（2026-11-27）：“caibao 兜底 回国前提示也提示一下是否需要兜底，不需要就不兜底了”。
- architect 定的含义：一键回国的请求新增可选字段 `claude_proxy`（true/false）。不带 = 保持开关现状；带了 = “回国之后第四道是开还是关”；值和现状一样按保持。确认框里的勾选默认等于现状，不勾就是关（开着时取消勾选，回国后关掉）。确认框按目标状态显示：关 → 开显示开关已有的三句后果（同 `claude-proxy on`），开 → 关一句“回国后 Claude Code 不再被指向本机代理，回国后靠解析锁和回国拦截挡住它（默认状态）”，不变只写一行现状；测试模式写“只监听，不写托管设置，本机真实的 Claude Code 不受它管”。
- 10.2 第四层原第 6 条“回国不碰它”改为：回国流程本身不碰第四道；只有用户在这次回国确认里明确选了另一个状态，才在回国成功之后切换（`claudeProxyOn`/`claudeProxyOff`，规则照旧）。
- **顺序：先回国、成功后再切开关**。理由：回国失败（结束不了 Claude 中止、状态机出错、忙）时开关一定不动，“中止什么都不改”仍成立；回国已经完成后切开关失败不能回滚回国（那等于重新接管网络），所以只在应答 `claude_proxy_change` 里写明原因和下一步，回国仍是 200。回国状态下第四道一打开就一律拒绝，先回国后打开中间没有放行窗口。
- **预览时就查冲突**：要打开、现在关、不在测试模式时，第一步只读地查托管设置的冲突，有冲突回 422 带 `conflicts`，不签发确认码、什么都不改——不让用户确认之后才发现打不开。
- **确认码绑定这个选项**：摘要 `restore`、`kill_browsers=0|1`、`claude_proxy=keep|on|off`；两步不一致回 `confirm_invalid`，什么都不改；忙时确认码照旧放回。
- **授权**：并进回国这一步，不单独授权。理由：回国本身是敏感级别、只有本人能做；`claude-proxy on|off` 两个方向也是敏感级别，级别相同；选项被确认码绑定，默认保持，脚本 `--yes` 不带参数时不会改变它。
- 边界：已经是回国状态时值和现状不同回 422（提示用 `sudo abroad claude-proxy on|off`）；没打开启动管控时预览没有这一块，要打开回 501；卸载过时没有这一块，要打开回 409 `uninstalled`，要关按保持。
- 10.1、10.2、11.1、11.2、11.3 相应改写；`docs/fallback-plan.md`、`docs/m6-acceptance.md`（G6、G9.1、第 4 节）、`docs/m5-walkthrough.md`（回国确认框）、`scripts/m6/e2e/selftest.sh`（端到端的回国不带 `--claude-proxy`，CC1 照旧用 on、off 两条命令）。计划见 `docs/restore-claudeproxy-plan.md`。

## 6. 风险和拿不准的地方（请顾问裁定的打了 ★）

1. ★ **摘要按字面值还是按归一后的值**。本计划按字面值：现在开、第一步带 `true`、第二步不带 → 摘要不同 → `confirm_invalid`。这样“第一步选开、第二步不带一律无效”（验收 b）不依赖两步之间的现状；代价是现状为开时带 `true` 和不带被当成两种选择（执行效果相同）。应用和命令行只在和现状不同时才带字段，正常使用不会碰到。
2. ★ **命令行追问的输入不是 y/n/回车时**：本计划按“没有确认，什么都没有改动”退出，不按保持继续（不猜用户的意思）。
3. ★ **应用里预览时有冲突**：勾选后重新预览得到 422，确认框关掉，首页用文字表的句子列出冲突，用户取消勾选后重新点“一键回国”。另一种做法是确认框不关、自动按原选项再取一次预览并把勾选复位——更顺，但要在 `ConfirmFlow` 里加一条“重新预览失败后再自动预览”的分支，违背“绝不自动带新确认码”这一类约定的精神。倾向前者。
4. ★ **回国成功、切开关失败时命令行的退出码**：本计划为 0（回国成功，切换失败写标准错误）。如果希望脚本能感知，可以改成单独的退出码，但会让“回国是否成功”的判断变复杂。
5. 两步之间现状被别人改了（例如第一步看到关、带 `true`，确认前另一个终端执行了 `claude-proxy on`）：第二步执行时 `claudeProxyOn` 已开就什么都不做，`ok:true`；带 `false` 时照样关掉——以用户在确认里的明确选择为准。预览里显示的 current 可能过时，影响只是文字。
6. 第一步查过冲突、确认前别人又写了冲突文件：回国照常完成，`claudeProxyOn` 拒绝，`ok:false` 带 `conflicts`（验收 g 的同类）。
7. 打开时端口被占只在回国之后才发现（预览不试监听，试监听要真的占端口）：`ok:false`，开关保持关（验收 g 覆盖）。
8. `ConfirmRequest` 拆出 `RestoreRequest` 会让生成的 Swift 类型多一个 `DaemonRestoreRequest`，应用实际是拼 JSON 字典，不受影响；如果顾问更希望三个两步确认共用一个请求类型，需要另外在启用、卸载、切换地区里显式拒绝 `claude_proxy`。
9. 读终端改用共用的 `bufio.Reader` 会碰到 `claude-proxy on` 和所有两步确认的读取路径；行为应完全不变，现有 `TestCLIClaudeProxy` 和各两步确认的命令行测试守着。
10. 检查顺序是“开关可用 / 卸载过”在“已经是回国状态”之前：卸载后（状态总是 RESTORED）要打开回 409 `uninstalled` 而不是 422，和顾问“卸载过要打开回 409”一致；没管控时要打开回 501 同理。
11. 应用确认框只有调用方够格时才出现；用户现在的日常路径是终端 `sudo abroad restore`（交互时有追问）。走查清单里的确认框几步需要在能弹出确认框的环境（假后台以本人身份）里看，和现有 L1-41 一样。
12. 计划里没有任何改宿主机网络、碰 pf、sudo、真实模式的命令；swift 集成测试由 `scripts/dev/app-test.sh` 以当前用户起假系统的 abroadd，用完停掉。
