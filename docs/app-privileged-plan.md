# B0：应用里直接执行敏感操作（系统密码框运行已安装的 root 命令行）——实施计划（交架构顾问审）

> 版本：**v2**（2026-11-27）。v1 经顾问审过：通过，1 条必须改，改好后直接实现。已实现（偏离见第 7 节）。
>
> **v2：顾问的必须改和三处裁定**
> - **必须改：只读的启用预览 `GET /v1/enable/preview`**：不签发确认码、不改任何状态。内容和启用第一步一样：地区会不会变（从哪到哪）；会变时列出要结束的 Claude 进程、没能确认的进程、在运行的浏览器（回国预览那一套）；写明现在能不能启用（不能时给原因代码，如 `no_baseline`）。应用的启用确认框用它；确认后照旧 root 运行 `abroad --yes --json up`，预览下加“执行时会按当时的情况重新列出”。测试：Go——预览和启用第一步 409 应答内容一致（地区变、不变各一例，`TestEnablePreviewMatchesFirstStep`）；调用预览不签发确认码、状态文件不变；路由级别只读，归入“不许拉起虚拟机”一类（`TestRoutesNeverLaunchWhenNotEnabled` 按路由表自动核对）；另有 `TestEnablePreviewBlockers`、`TestEnablePreviewNoBaseline`。Swift——`enableShowsFixedText` 换成 `enableUsesReadOnlyPreview`。设计 11.1 接口表加一行。下面第 2 节第 5 条、第 5 节风险 5 的“启用没有只读预览”以此为准。
> - **裁定①**：CryptoKit 同意：只在 `InputFiles.swift` import 和使用（读文件、算摘要只在这个文件），源码检查按此写并加反例（别的文件 import CryptoKit 就失败）。
> - **裁定②**：版本 v0.71b（A 是 v0.71、B0 v0.71b、阶段 B v0.71c；回国确认里选第四道 v0.72）。
> - **裁定③**：第四道开关 `claude-proxy on|off` 这次放进白名单：确认框打开时显示三句后果（文字表已有），关闭时显示“关掉之后，回国后靠解析锁和回国拦截。”；白名单 golden 和“不提供”表跟着改（“不提供”只剩 `uninstall`）。
> - 其余计划全部接受（风险 6 按“用法错误”提示“请先升级”）。
> 依据：用户原话（2026-11-27）“添加代理节点又需要执行这个，可以直接执行吗，不需要用户再执行”；顾问 2026-11-27 定的顺序 A → **B0** → B → 回国确认里选第四道，以及 B0 的 10 条必须（下面第 2 节逐条对应）。设计记 v0.72 前的 **v0.71b**（还是 v0.71 的一部分，见第 6 节；版本号请顾问定）。
> 现状核对过的文件：`app/Sources/AbroadKit/{Authorization.swift,ConfirmFlow.swift,InputFiles.swift,StatusModel.swift,MainProxyModel.swift,PageModels.swift}`、`app/Sources/ClaudeAbroad/{Dialogs.swift,HomeView.swift}`、`app/Tests/AbroadKitTests/RepoTests.swift`、`cmd/abroad/{main.go,commands.go}`、`internal/daemon/routes.go`、`internal/api/api.go`、`docs/m6-acceptance.md` 第 1、3、6、7 节、`docs/app-install-plan.md` 第 3 节。
> 会话里的边界：Swift 测试只用假的提权实现，不弹密码框、不提权；不碰 `/Library/Application Support/claude-abroad*`、`/Applications/ClaudeAbroad.app`；不 sudo；不改网络。

---

## 0. 现状和要改成什么

现在：应用里遇到要本人授权的操作（添加来源、添加节点、删除来源、选节点、确认局域网节点、应用主代理、回国、启用（地区会变时）、切换地区、增删出口、改规则放松保护的几项、打开“允许局域网”……），后台回 403 `needs_authorization`，模型调用 `Authorizer.authorize`，现有唯一实现 `TerminalAuthorizer` 返回 `.runInTerminal(command:)`，界面弹 `TerminalSheet`（`Dialogs.swift`）让用户自己去终端执行 `sudo abroad …`（命令由 `SensitiveOperation.terminalCommand` 生成；订阅地址、节点、粘贴的配置先由 `InputFiles.writePrivate` 写进 0600 临时文件，命令里只有文件路径）。

改成：同样的位置弹**应用自己的确认框**（写清要做什么、后果、预览），用户点“执行” → `NSAppleScript` 执行 `do shell script "<命令>" with prompt "<文字>" with administrator privileges` → 以 root 运行**已安装的、属主 root 的** `/Library/Application Support/claude-abroad/bin/abroad --yes --json <子命令>` → 界面显示结果。root 运行的命令行在后台眼里是“完全信任”的调用方，敏感操作照常过两步确认（命令行带 `--yes` 时用新的确认码自动确认，预览照样打印进 `--json` 的结果）。终端退路（“拷贝命令”）保留。

新增一个授权实现 `PrivilegedAuthorizer`（AbroadKit），默认注入它；`TerminalAuthorizer` 保留作退路（提权失败、用户选“在终端执行”时）。

---

## 1. 改哪些文件、按什么顺序

| 顺序 | 文件 | 改什么 |
|---|---|---|
| 1 | `app/Sources/AbroadKit/Authorization.swift` | `SensitiveOperation` 加 `arguments: [String]`（`abroad` 之后的参数数组，不含 `sudo`）；`terminalCommand` 改成由它生成（`"sudo abroad " + arguments.map(quote)`），**只有一份参数生成**（第 2 节第 2 条）；加 `subcommand: PrivilegedSubcommand`（白名单枚举）、`inputFile: URL?`（要经文件传的那个参数）、`options`（`--kill-browsers` 等）。`AuthorizationResult` 加 `.executed(PrivilegedOutcome)`、`.failed(ErrorText)`。 |
| 2 | `app/Sources/AbroadKit/PrivilegedCommand.swift`（新） | 纯函数：`shellQuote`、`appleScriptEscape`、`validate(arg:)`（拒绝换行、NUL、其他控制字符）、`build(op:inputSHA256:) -> String`（第 2 节第 1、4 条的整条 shell 命令）、`PrivilegedSubcommand` 白名单枚举、退出码表。阶段 B 的安装、升级、卸载命令也放这里（**和阶段 B 共用一个实现、一组测试**）。不出现 `NSAppleScript`、`administrator privileges`、`/Library/Application Support`：基础路径由调用方（PrivilegedRunner）传入。 |
| 3 | `app/Sources/AbroadKit/InputFiles.swift` | 加 `sha256(of:) -> String`（读文件算摘要；全应用只有这个文件读文件，规则不放宽）；加 `copyUserFile(_:)`：用户选的本地配置文件（`addSourceFile`、`exitAdd`）先复制成自己临时目录里的 0600 副本，命令里只用副本（见第 2 节第 4 条“为什么先复制一份”）。需要 `import CryptoKit`：系统框架，加进源码检查的允许列表，并限定只在 `InputFiles.swift`。 |
| 4 | `app/Sources/AbroadKit/PrivilegedAuthorizer.swift`（新） | 协议 `PrivilegedRunning { func run(command: String, prompt: String) async -> PrivilegedRunResult }`（`ok(stdout, exit)`、`failed(exit, output)`、`cancelled`、`unavailable(reason)`）；`PrivilegedAuthorizer: Authorizer`：收到 `authorize(op, reason)` 时**不直接提权**，而是返回 `.needsConfirm(PrivilegedRequest)` 交给界面；界面上用户点“执行”后才调 `execute(_ request:)`（第 2 节第 6 条）。解析 `--json` 输出、映射退出码到文字表。 |
| 5 | `app/Sources/ClaudeAbroad/PrivilegedRunner.swift`（新） | 唯一的 `NSAppleScript` 实现，唯一的基础路径常量 `/Library/Application Support`（第 2 节第 9 条）；后台串行队列执行（第 10 条）；错误号 -128 → `cancelled`。 |
| 6 | `app/Sources/AbroadKit/StatusModel.swift`、`MainProxyModel.swift`、`PageModels.swift` | 原来 `case .runInTerminal` 的地方加 `.needsConfirm`：把请求放进 `@Published var privileged: PrivilegedRequest?`；`userTappedExecute()`、`userTappedCopyCommand()`、`userCancelled()` 三个入口；执行完刷新。临时文件在执行结束（成功、失败、取消）后删。 |
| 7 | `app/Sources/ClaudeAbroad/Dialogs.swift`（`PrivilegedSheet`，新视图）、`HomeView.swift`、各页面 | 确认框：标题（做什么）、后果、预览（第 2 节第 5 条）、要传的输入文件的路径（只显示路径，不显示内容）、“执行”“拷贝命令（在终端执行）”“取消”；执行中“正在执行，请输入管理员密码”；结果：成功的文字，失败的文字和退出码、原样的错误。沿用 Palette/Components。 |
| 8 | `app/Sources/AbroadKit/Strings.swift` | 密码框提示（“Claude 出国要以管理员身份执行：<操作名>”）、各操作的确认标题和后果、退出码说明、“输入文件在确认后被改动过，请重新操作”、“执行时会按当时的情况重新列出”、“已安装的命令行不对（不是 root 的或别人可写），没有执行”。 |
| 9 | `app/Tests/AbroadKitTests/PrivilegedTests.swift`（新）、`RepoTests.swift`、`testdata/privileged-subcommands.txt`（新） | 第 2 节各条的测试。 |
| 10 | `cmd/abroad/privileged_test.go`（新，只是测试） | `TestPrivilegedWhitelistMatchesRoutes`：读 `app/Tests/AbroadKitTests/testdata/privileged-subcommands.txt`，和 `commands` 表、路由表交叉核对（第 2 节第 2 条）。 |
| 11 | 文档 | 第 4 节。 |

---

## 2. 顾问的 10 条，逐条对应

### 第 1 条 只执行已安装的 root 程序
- **路径**：`PrivilegedRunner.swift` 里唯一的常量 `static let base = "/Library/Application Support"`；安装目录 `base + "/claude-abroad"`、命令行 `… + "/bin/abroad"`、临时目录 `base + "/claude-abroad-staging"` 都由它拼出，作为参数交给 `PrivilegedCommand.build`。
- **提权 shell 里先检查**（命令的第一段，只用系统程序）：
  ```sh
  A='…/claude-abroad/bin/abroad'
  [ -f "$A" ] && [ ! -L "$A" ] && [ "$(/usr/bin/stat -f %u "$A")" = 0 ] \
    && [ $(( 0$(/usr/bin/stat -f %Lp "$A") & 022 )) = 0 ] || { echo 'RESULT=31'; exit 31; }
  ```
  还检查 `bin/` 和安装目录本身属主 root、组和其他人不可写（否则可以换掉文件）。不满足退出码 31，界面显示“已安装的命令行不对（不是 root 的或别人可写），没有执行”，不执行任何别的东西。
- **测试**：`privilegedCommandGolden`（命令以这段检查开头，路径来自传入的基础路径）；脚本侧 `scripts/m6/install/selftest.sh` 场景 u（把 golden 命令里的路径换成自测目录、`stat` 的属主条件换成当前用户后用 `bash -c` 真跑：文件是符号链接、组可写、目录可写时都停在 31，什么都没执行）。

### 第 2 条 子命令白名单
- `PrivilegedSubcommand`（Swift 枚举）只列这些（命令行命令表里对应敏感或“按本次调用决定、算出来可能敏感”的接口）：`up`、`restore`、`main add-source`、`main add-node`、`main rm-source`、`main use`、`main confirm-lan`、`main apply`、`exit add`、`exit rm`、`app set`、`app rm`、`rules local|pinned|offline|nofake add|rm`、`rules udp443`、`lan on|off`、`region set`、`region auto`。每个 `SensitiveOperation` 映射到其中一项，参数只来自 `arguments`（第 1 节第 1 步；`terminalCommand` 也由它生成，不另写一份）。
- **不提供（写明理由）**：`uninstall`（阶段 B 走已安装的 `uninstall.sh`，不走这里）、`claude-proxy on|off`（v0.72“回国确认里选第四道”时再加）、`down`（普通级别，不需要）、`main refresh`（普通级别）、`domains clear`、`check history clear`（普通级别）。
- **交叉核对**：
  - Swift `privilegedWhitelistFile`：`PrivilegedSubcommand.allCases` 和 `testdata/privileged-subcommands.txt`（每行“命令词 TAB 路由 ID”）一致；每个 `SensitiveOperation` 的 `arguments` 以白名单里某一项的命令词开头。
  - Go `TestPrivilegedWhitelistMatchesRoutes`（`cmd/abroad`）：文件里每一项在 `commands` 表里有、路由 ID 一致、路由级别是 `api.Sensitive` 或 `api.PerCall`；反过来，命令表里路由是 `Sensitive` 或 `PerCall` 的每个命令，要么在文件里，要么在测试里的 `privilegedNotProvided` 表里写了理由（上面那几项）。新增敏感命令而两边都没写 → 失败。
- 反例：`privilegedWhitelistRejectsUnknown`（构造一个不在白名单的子命令，`build` 返回错误，runner 没被调用）。

### 第 3 条 秘密不进命令行
- `do shell script` 是 `/bin/sh -c <命令>`，执行期间 `ps` 能看到整条命令。订阅地址、节点链接、节点密码、粘贴的配置、出口配置一律只经文件：命令里只有临时目录里副本的路径和 SHA-256。来源名字、节点 ID、标签、应用 ID、域名、时区不算秘密，照现在的终端命令一样出现在参数里。
- **测试** `secretsNotInCommand`：每种带输入的操作（订阅地址、单个节点 `vless://…`/`ss://…`、粘贴的配置、选的本地配置文件、WireGuard/OpenVPN/clash 出口配置）用含假秘密（`SECRET-TOKEN-xyz`、`p@ssw0rd-123`、`PrivateKey = AAAA…`）的输入生成命令，断言命令文本（转义前和转义后）里都找不到这些字符串，只有路径和 64 位十六进制摘要。

### 第 4 条 输入文件防调包
- 用户点“执行”那一刻：应用对临时文件算 SHA-256（`InputFiles.sha256`），写进命令。
- 提权 shell 在检查完命令行之后：
  ```sh
  S='…/claude-abroad-staging'; /bin/rm -rf "$S" && /bin/mkdir -m 700 "$S" && /usr/sbin/chown root:wheel "$S" \
    && /bin/cp '<临时文件>' "$S/in" \
    && [ "$(/usr/bin/shasum -a 256 "$S/in" | /usr/bin/cut -c1-64)" = '<摘要>' ] || { /bin/rm -rf "$S"; echo 'RESULT=32'; exit 32; }
  "$A" --yes --json main add-source --url-file "$S/in" --name '…'; rc=$?; /bin/rm -rf "$S"; echo "RESULT=$rc"; exit $rc
  ```
  摘要对不上 → 退出码 32，什么都不改，界面显示“输入文件在确认后被改动过，请重新操作”；用完（成功、失败）都删。临时目录和阶段 B 同一个 `claude-abroad-staging`：所有提权都在同一个串行队列里，一次只有一个在用。
- **为什么先复制一份**：用户选的本地配置文件可能在“桌面”“文稿”“下载”里，那里受隐私保护，root 的 shell 未必读得到（应用经打开面板拿到的是应用自己的权限，不传给子进程）。所以 `addSourceFile`、`exitAdd` 也先由应用把内容复制进自己的临时目录（0700/0600），命令里只用这份副本；顺带让“文件内容”在点击时就固定下来。
- **测试**：`inputHashInCommand`（命令里的摘要等于文件的摘要）；`inputFilesCleanedUp`（执行成功、失败、取消后临时文件都删了）；脚本侧场景 u 里真跑：摘要对 → 把副本路径交给假的 `abroad`（自测里是记录参数的脚本）；确认后改了文件 → 停在 32，假 `abroad` 没被调用，临时目录不在。

### 第 5 条 确认码和预览
- 应用先用只读接口出预览，放在自己的确认框里：
  - 回国：`GET /v1/restore/preview`（已有）；“同时强制退出浏览器”勾选后原样传 `--kill-browsers`。
  - 卸载：阶段 B 加的 `GET /v1/uninstall/preview`（不在 B0 白名单里，见第 2 条）。
  - 启用：**没有只读的启用预览**（地区会不会变由后台在启用第一步算）。确认框只显示固定说明（“启用会接管本机网络；如果出口所在地区和现在的时区、语言不一样，会切换时区和语言，并结束正在运行的 Claude”）加 `GET /v1/region` 里锁定的目标地区（有就显示）。写明这一点。
  - 切换地区：`GET /v1/region` 显示现在和目标。
  - 主代理（添加来源、选节点、应用、确认局域网节点）：`GET /v1/main-proxy`、`/v1/main-proxy/sources` 显示现在的选择和要改成什么。
  - 规则、出口、局域网：显示要改的那一项和后果（文字表里已有的后果说明）。
- 确认后 root 运行 `abroad --yes --json <子命令>`：后台按当时的情况再走一遍两步确认（命令行用新的确认码自动确认），用户在应用里选的选项（`--kill-browsers`，以后的 `--claude-proxy`）原样传。预览下面加一句（文字表）：“执行时会按当时的情况重新列出；之后新启动的 Claude 也会被结束。”（回国、卸载、会结束 Claude 的启用）。
- **测试**：`restoreUsesReadOnlyPreview`（先调只读预览、不发 POST）；`killBrowsersPassedThrough`；`enableShowsFixedText`；`previewHasRecheckNote`。

### 第 6 条 授权缓存
- 同一进程用 `with administrator privileges` 认证一次后，约 5 分钟内再调用不再弹密码框（系统行为，不是我们做的缓存）。前提：每次敏感操作前应用都先弹自己的确认框，写清要做什么和后果，用户点“执行”后才调用提权；**没有任何不经点击就调用的路径**。
- 实现上：`PrivilegedAuthorizer.authorize` 只返回 `.needsConfirm`，真正调用 runner 的只有 `execute(_:)`，而 `execute` 只从各模型的 `userTappedExecute()` 调；`userTappedExecute` 只出现在按钮动作里。
- **测试**：`noRunWithoutTap`（模型 `refresh()`、事件推送、页面出现、状态变化反复触发，假 runner 调用次数为 0；点一次恰好 1 次；进行中再点不重复调用）；源码检查 `privilegedOnlyFromTap`（`userTappedExecute(` 只出现在含 `Button(` 或 `action:` 的行；`execute(` 只在 `PrivilegedAuthorizer.swift` 和三个模型的 `userTappedExecute` 里）。
- 设计里如实写明这 5 分钟（第 4 节）。

### 第 7 条 转义和拒绝
- 参数一律单引号（参数里的单引号写成 `'\''`），整条命令再按 AppleScript 字符串转义（`\` → `\\`、`"` → `\"`）。参数里有换行、NUL、其他控制字符（`U+0000–U+001F`、`U+007F`）时拒绝，不拼命令，提示“参数里有不支持的字符”。
- 和阶段 B 共用 `PrivilegedCommand` 一个实现、一组测试：`shellQuoteCases`（普通、空格、`'`、`"`、`$`、反引号、`\`、中文、`;rm -rf /`）、`appleScriptRoundTrip`（测试里实现最小的 AppleScript 字符串解析和 sh 单引号解析，转义后能还原原参数）、`rejectsControlChars`（runner 没被调用）。
- `SensitiveOperation.quote`（终端命令用的、常见字符不加引号）保留给终端退路；提权命令一律用 `PrivilegedCommand.shellQuote`（总是加引号）。两者都由 `arguments` 生成，参数本身只有一份。

### 第 8 条 输出
- 命令带 `--json`：标准输出最后是命令行的最终应答（JSON），最后一行 `RESULT=<退出码>`。界面按退出码和应答里的 `code` 显示文字表里的说明（成功、需要确认、被拒、忙、连不上后台服务……沿用命令行的退出码 0–7，加我们的 31 命令行不对、32 输入文件被改过、-128 取消），错误原样显示在下面（可选中拷贝）。不按错误文字分支（沿用 `ErrorText.code` 的规则）。
- 不写日志（应用照旧不写磁盘、不输出日志）。
- “拷贝命令”终端退路保留：确认框里有“在终端执行”，点了就是现在的 `TerminalSheet`。
- **测试**：`outcomeMapping`（每个退出码、取消、JSON 里的错误代码对到文字）；`terminalFallbackKept`。

### 第 9 条 只能在一个文件里提权
- 源码检查（`RepoTests` 把 `sourceRules` 的判断抽成 `sourceRuleViolations(files:)`，可以喂反例）：`NSAppleScript`、`administrator privileges`、`do shell script` 只在 `PrivilegedRunner.swift`；`/Library/Application Support` 只在 `PrivilegedRunner.swift` 出现**一次**（基础路径常量），安装目录、临时目录都从它拼出；同文件第二次出现、别的 Swift 文件出现都失败。现有“应用不写磁盘”“`Process(` 禁止”等规则对它照样生效。
- 反例 `privilegedSourceRuleCounterexamples`：`HomeView.swift` 里出现 `NSAppleScript`；`SettingsView.swift` 里出现 `with administrator privileges`；`PrivilegedRunner.swift` 里出现两次 `/Library/Application Support`；`StatusModel.swift` 里出现 `/Library/Application Support/claude-abroad`——每例都被报出来。

### 第 10 条 线程
- `PrivilegedRunner` 在自己的后台串行队列上执行 `NSAppleScript`（不用 `/usr/bin/osascript` 子进程：`Process(` 本来就禁止）；调用前界面已显示“正在执行，请输入管理员密码”，按钮不可点。
- 真机步骤确认：密码框显示应用名（“Claude 出国”）、界面不卡、没有“自动化”权限弹窗。不行就改回主线程执行（先显示进行中提示再调用），在设计里记下。

---

## 3. 会话里怎么测

- `scripts/dev/app-test.sh`（`swift build` + `swift test`，含假系统 abroadd 的集成测试）：上面列的 Swift 测试全部用假的 `PrivilegedRunning`，只验证传了什么命令、什么时候调用、结果怎么显示；不弹密码框、不提权。
- `go test ./cmd/abroad -run TestPrivilegedWhitelistMatchesRoutes`。
- `scripts/m6/install/selftest.sh` 场景 u：把 `app/Tests/AbroadKitTests/testdata/privileged-commands.txt`（Swift golden 生成的标准命令，路径用占位）里的命令换成自测目录、去掉 `chown`、属主条件换成当前用户，用 `bash -c` 真跑（不提权、不碰系统路径），核对第 1、4 条的停止条件和清理。
- 不运行真实的 `PrivilegedRunner`；界面要看时按“会话代测”打开 `app/.build/bundle/` 里的应用、连假系统 abroadd 截图，不点“执行”。

---

## 4. 文档怎么改

- **设计**
  - 11.1（本人授权）：这一版应用里的本人授权 = 系统密码框（`with administrator privileges`）以 root 运行**已安装的、属主 root 的**命令行；后台据 uid 0 认定为完全信任，敏感操作照常过两步确认；“5 分钟内再提权不弹密码框”是系统行为，前提是每次都先有应用自己的确认框。
  - 11.2：确认框（做什么、后果、预览、“执行时会按当时的情况重新列出”）、“执行”“在终端执行”“取消”、进行中和结果的显示。
  - 11.4：提权命令的形状（检查命令行属主和权限、复制输入文件到 root 的临时目录并核对摘要、`--yes --json`、删临时目录）。
  - 15：E1、E2 改成“这一版用系统密码框运行已安装的 root 命令行；E1、E2 不再需要”。
  - 16：修订记录，照录用户原话“添加代理节点又需要执行这个，可以直接执行吗，不需要用户再执行”（2026-11-27）。
- **验收清单** `docs/m6-acceptance.md`
  - 第 1 节第 5 行“三句话”里“应用里目前给出一条要在终端执行的命令、由你自己去执行（系统授权等 M5-3 的 E1、E2 结论）”改成：“应用里点‘执行’后弹系统密码框，以 root 运行已安装的命令行；命令行必须 sudo。控制台用户运行的脚本直接调接口照样被拒。”
  - 第 3 节“敏感操作要本人授权”一行：“应用里目前给出终端命令（系统授权等 M5-3）”改成“应用里经系统密码框以 root 运行已安装的命令行（B0）”；待真机一栏把“M5-3 的 E1/E2”换成“B0 的真机步骤”。
  - 第 6 节 M5-3 一行：B0 验收后标“完成（以系统密码框代替系统授权）”；E1、E2 撤掉。
  - 第 7 节 P8 一行改成“这一版以系统密码框运行 root 命令行代替”；加安全边界：约 5 分钟的授权缓存（同一进程内，系统行为）；SHA-256 核对防的是“确认之后输入文件被调包”，挡不住“确认之前文件就被改了”（应用临时目录属用户，和用户账户本身同一信任程度）；没有签名，应用本身属用户可写，信任程度等于用户账户（和阶段 B 的说法一致）。
- **进度单**：M5-3 在 B0 验收后标完成；“需要用户人工处理”里的 `scripts/m5/probe/run-e1e2.sh`（E1、E2）撤掉，写明理由。
- **`docs/app-install-plan.md`**：第 3 节标出阶段 B 里复用 B0 的部分（本次已在那里加了一段说明）：`PrivilegedRunner`、`PrivilegedCommand`（转义、拒绝控制字符、基础路径常量、临时目录、退出码表、golden 和脚本侧真跑）、源码检查的例外和反例、串行队列、“只从点击调用”的测试，阶段 B 只加安装、升级、卸载三种命令和它们的界面。

---

## 5. 风险和拿不准的地方

1. **root 的 shell 读不到用户受保护目录里的文件**：用复制到应用自己临时目录的办法绕开（第 4 条），但 `$TMPDIR`（`/var/folders/…`）对 root 可读这一点要真机确认。
2. **授权缓存的范围**：系统把认证记在本进程上；5 分钟内应用里任何代码路径都能不弹框提权，所以“只能从点击调用”的测试是这条的唯一防线。用户权限的恶意程序如果能往应用进程里注入代码，在这 5 分钟里能借用——没签名、没加固运行时，这一版防不住，写进第 7 节。
3. **`--json` 下各敏感命令的输出形状**不完全统一（有的是状态、有的是任务结果）。界面先只认退出码和应答里的 `code`、`error`，不解析其余字段；需要逐个看时再加。
4. **`NSAppleScript` 在后台队列执行**：苹果文档对线程的说法不一，真机不行就退回主线程（第 10 条）。
5. **启用没有只读预览**：确认框只能给固定说明和锁定的目标地区；后台第一步算出“地区会变、要结束 Claude”时命令行会直接执行（`--yes`）。要不要加一个只读的启用预览接口，请顾问定（加的话放进 B0）。
6. **已安装的命令行版本比应用旧**：老命令行可能没有某个子命令或参数（例如以后的 `--claude-proxy`）。应用只按白名单发，命令行回“用法错误”（退出码 2）时提示“后台服务的版本比应用旧，请先升级”。阶段 B 的版本比较之后再收紧。

---

## 6. 用户的真机步骤（合成一份：阶段 A 两条 + B0 三条）

阶段 A（已合并，现在就能做）：
1. 已回国、主代理配好时，以自己的身份运行 `scripts/dev/reinstall.sh`。老安装没有版本号，会多问一次“版本不明，继续？”，回车继续，输一次管理员密码。核对：`abroad status` 里 `build` 是新的提交；`abroad main nodes`（或应用里主代理页）里的来源和选中的节点还在，不用重新添加。（以前由安装脚本放进“应用程序”的应用会换成新的，并交给你自己。）
2. 一个终端执行 `sudo abroad main check`（会借用虚拟机几十秒），期间在另一个终端执行 `sudo scripts/m6/install/install.sh --upgrade`。期望看到“正在借用虚拟机（应用主代理或检查主代理），等它完成再升级”，最后一行“什么都没有改动”；`abroad status` 的 `build` 不变。

B0（B0 合并之后）：
3. 在应用里添加一个主代理来源（订阅地址或节点）：确认框写清要做什么 → 点“执行” → 系统密码框的标题里是应用名“Claude 出国”，输密码 → 应用显示成功，来源出现在列表里；期间界面不卡，没有“自动化”权限弹窗。
4. 5 分钟内再做一次敏感操作（例如选一个节点）：点“执行”后不再弹密码框，直接执行成功。
5. 添加来源时，在确认框出现之后、点“执行”之前，把那份输入文件改掉（确认框里会显示临时文件的路径；用文本编辑器改一个字符）：点“执行”后提示“输入文件在确认后被改动过，请重新操作”，来源列表没有变化。

贴回：每一步应用里的提示文字（截图或照抄）、`abroad status` 的输出。

---

## 7. 实现中的偏离和补充（v2）

- **AppleScript 源码在 `PrivilegedRunner.swift` 里拼**（不在 `PrivilegedCommand`）：`do shell script`、`with administrator privileges` 只许出现在这个文件。`PrivilegedCommand` 只提供两层转义的函数和整条 shell 命令，测试对转义做往返还原。
- **命令的每条路都以 0 退出**，结果在最后一行 `RESULT=<退出码>`：这样 `do shell script` 总能拿回标准输出；只有取消密码框（-128）和系统层面的失败才是 AppleScript 的错误。另加 `without altering line endings`（否则输出的换行会被改成 `\r`）。
- **一次确认、多步执行**：主代理页“换节点”在终端退路里原来是两条命令（`main use` 再 `main apply`），提权时合成一条命令依次执行，前一步失败就停。
- **共用的确认框 `PrivilegedSession`**（各页面一个实例，和 `ConfirmFlow` 一样）：`PrivilegedAuthorizer` 返回新的 `.confirmInApp`，模型交给它；“执行”“在终端执行”“取消/关闭”都在它上面。设置窗口里发起的（第四道、允许局域网）在设置窗口里弹（`PrivilegedHost`）。
- **确认框里的“现状”**：回国、启用用只读预览；局域网写“现在：关”；第四道照裁定③；地区、主代理、规则、出口写“要改成什么”和后果（文字表），没有另外再读一次现状接口。
- **用户选的本地文件、出口配置也先存成应用自己的临时文件**（计划第 2 节第 4 条已写），终端退路的命令也随之改用临时文件的路径（两个原有测试按此更新：`exitsAuthorization`、`terminalFallbackTempFile`）。
- **脚本侧真跑（场景 u）**：属主条件换成当前用户才能不提权地跑；“属主是 root”这一条由 Swift golden 和场景 u 的文字核对守着。
- 设置“高级”里卸载那一行的文字改成“应用里的卸载入口下一步提供”（不再说“要等系统授权”）。
