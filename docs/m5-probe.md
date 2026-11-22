# M5-1 探查结论

> 状态：2026-11-22，交 architect 验收。E3–E6 在会话里有结论；**E1、E2 的核心结果等用户执行 `scripts/m5/probe/run-e1e2.sh` 后补上**（下文标着【等用户】）。
> 依据：`docs/m5-plan.md` v2 第 5 节和表后“授权凭证的语义”。
> 会话里没有用 sudo，没有弹窗口，没有启动界面程序，没有改授权数据库，没有改宿主机网络。

## 总表

| # | 问题 | 结论 | 状态 |
|---|---|---|---|
| E1 | abroadd（root）能否核验应用交来的系统授权 | 机制和两种核验程序（Swift、Go）都已写好并编过；Go 一侧用一个只链接系统 Security.framework 的极小 cgo 文件，不引入第三方依赖。不用 cgo 的路子（purego）要新增第三方依赖，会话里没有引入 | 【等用户】以 root 核验的实际结果 |
| E2 | 未签名程序能否登记和申请自定义权限项；规则不共享；授权框显示什么 | 登记规则写好（本人认证、`shared` 为假、不缓存，带我们自己的提示文字）；验证脚本齐全 | 【等用户】 |
| E3 | 控制台用户的取法；socket 属组 | 用 `/dev/console` 的属主（一次 stat，纯 Go，不起外部命令）；单测加一次只读核对通过。属组：推荐“目录 0755 + socket 0666 + 只按对端 uid 判级别”，不依赖 admin 组 | 会话里完成 |
| E4 | Swift 一侧访问 unix socket | Network.framework 的 `NWConnection(.unix)` 加约 200 行的 HTTP/1.1 客户端（含分块传输、SSE）可行；对假系统模式的 abroadd 读到 status，对演示服务读到 3 个事件 | 会话里完成 |
| E5 | 接口描述到 Swift 类型的生成 | 候选二（Go 直接从类型生成 Swift Codable，同时生成 OpenAPI 描述）跑通，选定；候选一（swift-openapi-generator）要从网上拉第三方包，会话里被拒绝下载，只做书面比较 | 会话里完成（候选一未实测） |
| E6 | 只用命令行工具编界面程序 | 能编：SwiftPM + Command Line Tools（Swift 6.4、SDK 27.2）编出了带 `MenuBarExtra` 的 SwiftUI 程序，swift-testing 单测能跑。**限制：没有 SwiftUI 的宏插件**，新 SDK 里 `@State` 是宏，用不了，要改用 `ObservableObject` + `@StateObject`。`.app` 包的组装和实际运行没做（会话里不启动） | 会话里完成（运行交用户走查） |

## E1 授权凭证的核验方式

- **验证了什么**：应用申请到 `com.claude-abroad.sensitive` 之后，把系统授权的外部形式（32 字节）交给以 root 运行的进程，后者能否不带交互地确认这个授权确实拥有该权限项。
- **怎么验证的**：
  - `scripts/m5/probe/swift` 的 `AuthzProbe`：`request` 申请（会弹授权框）、`verify` 核验；
  - `scripts/m5/probe/goverify`：Go 程序，一个极小的 cgo 片段调用 `AuthorizationCreateFromExternalForm` 和 `AuthorizationCopyRights`（只带 `ExtendRights`，不带交互），只链接系统框架；
  - `scripts/m5/probe/run-e1e2.sh` 依次做：登记、申请、以 root 用两种程序核验（应通过）、核验没登记的权限项（应不通过）、以普通用户核验（对照）、授权会话结束后再核验（记录是否失效）、删除。
- **会话里的结果**：两个程序都编过；`run-e1e2.sh` 语法检查和空跑通过。**以 root 核验的实际结果【等用户】**。
- **对计划的影响**：
  - 不管 E1 结果如何，凭证语义已按 v2 定下：abroadd 核验系统授权后签发自己的短期凭证。平台无关的签发和校验已经实现：`internal/authz`，单测覆盖一次性、过期、绑定操作和参数摘要、并发只通过一次、上限，能交叉编译到 Windows。
  - **“不用 cgo”这一条需要 architect 定**。abroadd 现在是纯 Go，可以有三种做法：
    - (a) 在 abroadd 里加这个只链接系统框架的极小 cgo 文件，只在 darwin 编译，Windows 不受影响；
    - (b) 引入 purego（第三方依赖），动态调用；
    - (c) abroadd 调用一个随应用打包的小核验程序。
  - 我倾向 (a)。它没有第三方依赖，代码量最小；abroad-vm 已经在用 cgo（VZ），构建链本来就有。

## E2 未签名程序申请自定义权限项

- **验证了什么**：
  - ad-hoc 签名的程序能否登记和申请自定义权限项；
  - 规则设成不共享后，别的进程用自己的授权会话申请同一权限项会不会再次要求认证；
  - 授权框上显示的申请者名字，以及提示文字能否由我们自定义。
- **怎么验证的**：
  - `AuthzProbe register` 用 `AuthorizationRightSet` 登记规则 `{class: user, authenticate-user: true, session-owner: true, shared: false, timeout: 0}`，描述文字是我们自己的一句话；
  - `show` 读回规则；
  - 第二个 `AuthzProbe request` 进程测“不共享”；
  - 脚本在两处停下来问用户看到了什么。
- **结果【等用户】**。
- **对计划的影响**：
  - 不共享不成立（第二个进程没弹框就拿到了）：改用“每次敏感操作都由 abroadd 签发一次性凭证 + 系统授权只在换凭证时用一次”，并缩短凭证有效期；同时评估备选 A。
  - 授权框上显示不出可辨认的名字：依靠我们登记的提示文字说明是谁、要做什么（提示文字能自定义的话）；不能自定义时，在结论里评估“用户知道自己在授权什么”这一条是否还成立，必要时改用备选 B。

## E1、E2 都行不通时的两个备选（评估）

| 备选 | 做法 | 优点 | 缺点 |
|---|---|---|---|
| A | 敏感操作在应用里只“发起”，abroadd 以 root 用 `launchctl asuser <控制台用户 uid>` 在用户会话里运行一个只做认证的小程序（申请系统授权框），认证成功后 abroadd 自己执行 | 授权框仍由系统弹出，用户体验接近原设计；凭证不经过应用，应用被冒充也拿不到授权 | 实现复杂：要处理“控制台用户切换、锁屏、没人登录”，小程序要随应用打包；同样依赖自定义权限项（E2）或退回 `system.privilege.admin`（要管理员密码，不能只用本人密码） |
| B | 应用里的敏感操作一律提示“请在终端执行 `sudo abroad …`” | 最简单，不依赖签名，也不依赖授权服务，命令行的 sudo 路径 M5-3 本来就要做 | 体验差，违背“所有操作都能在应用里完成”（设计 14 的 M5 完成标准），需要用户同意改标准 |

## E3 控制台用户与 socket 属组

- **验证了什么**：
  - 不用 cgo、不起外部命令能否取到“当前控制台用户”；
  - socket 的属组怎么定。
- **怎么验证的**：
  - `internal/console`：`UID()` 读 `/dev/console` 的属主，属主是 root（登录窗口）时视为没人登录；
  - 单测 `TestFromOwner`；
  - 只读核对 `TestUIDReadOnly`：取到了，等于运行测试的用户，输出不写用户名；
  - 只读查了：当前用户在 admin 组；admin 组有 2 个成员。
- **结果**：取法可行。备选的取法是用 `scutil` 读动态存储里的控制台用户键，要起子进程、解析文本，不采用。非 Mac 平台返回“尚未支持”（`platform.Unsupported`），Windows 的“当前交互会话用户”由 W 系列另做。
- **对计划的影响（socket 属组）**：设计写的是“socket 0660、属组 admin”。问题是控制台用户不一定在 admin 组：标准账户不在。这时只读一级就连不上，而设计里只读一级的要求是“当前控制台用户或 root”，与 admin 组无关。推荐改为：**目录 0755、socket 0666，级别只按对端 uid 判**——控制台用户或 root 才放行，其余连接直接断开，沿用现在的 peerListener 写法。
  - 好处：标准账户也能用，判定只有一处。
  - 代价：任何本机用户都能连上 socket，但会立刻被断开，连 HTTP 都不解析。
  - 以 root 建 socket、改属组的实测放进 M5-2 的 `integration` 测试（会话里不跑）。

## E4 Swift 访问 unix socket

- **验证了什么**：Swift 一侧能否不靠 URLSession，经 unix socket 发 HTTP 请求、读 JSON 应答和 SSE 事件流。
- **怎么验证的**：
  - `scripts/m5/probe/swift/Sources/UDSHTTP`：`NWConnection(to: .unix(path:), using: .tcp)` 加纯函数的 `HTTPParser`（Content-Length、分块传输、读到关闭）和 `SSEParser`；
  - `UDSClient status` 对假系统模式的 abroadd（开发模式，当前用户，状态目录在临时目录，用完即删）；
  - `UDSClient events` 对 `scripts/m5/probe/ssedemo`：abroadd 的事件推送 M5-2 才做，这个演示服务先发 3 个事件，Go 的 HTTP 服务对 SSE 用分块传输。
- **结果**：
  - `HTTP 200；状态 RESTORED；闸门 关；事件 0 条；用生成的类型解码成功`；
  - 事件流 `HTTP 200；Content-Type text/event-stream；分块传输 chunked`，3 个事件逐条读到；
  - Swift 单测 8 个全过，包括逐字节喂入的分块解析、结束后多出的字节、坏的块长度、坏的状态行、SSE 多行 data 和注释、用真实 status 样本解码。
  - 单测发现了一个解析器的 bug（分块结尾之后还有字节时越界），已修。
- **对计划的影响**：M5-7 的网络层就用这套。SSE 的重连（`Last-Event-ID`）和超时在 M5-2、M5-7 里补。

## E5 类型生成

- **验证了什么**：从 Go 的接口类型生成 Swift 类型，两种候选的可行性和代价。
- **怎么验证的**：
  - 候选二：`scripts/m5/probe/apigen` 用反射读 `daemon.Status`、`ConfirmNeeded`、`RegionRequest` 和一个事件原型，生成 20 个 Swift `Codable` 结构体，同时生成 OpenAPI 3.1 的 `components/schemas`（`scripts/m5/probe/openapi.json`）。
  - 候选一：准备了一个依赖 `swift-openapi-generator` 和 `swift-openapi-runtime` 的试验包，喂候选二生成的 openapi.json。**下载第三方包这一步被会话的权限拒绝**，没有实测。
- **结果**：
  - 候选二编过，生成的类型解码了真实的 status（E4）。
  - 规则已写进生成器：json 标签决定字段名；omitempty、指针、切片、map 一律可选，因为 Go 的 nil 切片编成 null；实现了 JSON 或文本序列化的类型当字符串；“包名 + 类型名”避免同名冲突；Swift 关键字加反引号。
  - 候选一的书面比较：它要拉 3 个以上的 SwiftPM 包，构建时还要跑生成插件；离线和没有网络权限时不能重新生成；生成的是“请求方法 + 类型”的全套客户端，我们的传输是自写的 unix socket，用不上它的传输层。
- **对计划的影响**：选定候选二（v2 已拍板）。M5-2 把生成器的输入从“手写的类型列表”改成路由表；OpenAPI 描述照样生成，给人看、给以后的客户端用。

## E6 只用命令行工具编界面程序

- **验证了什么**：这台 Mac 没有 Xcode，只有 Command Line Tools（Swift 6.4、macOS SDK 27.2），能否编出 SwiftUI 的菜单栏程序、能否跑单测。
- **怎么验证的**：SwiftPM 包 `scripts/m5/probe/swift` 的 `MenuBarDemo`（`MenuBarExtra` 加一个 `Window`）和测试目标 `UDSHTTPTests`（swift-testing）。
- **结果**：
  - 第一次编译失败：`external macro implementation type 'SwiftUIMacros.StateMacro' could not be found`。命令行工具里没有 SwiftUI 的宏插件，新 SDK 里的 `@State` 是宏。
  - 改用 `ObservableObject` + `@StateObject`（属性包装器，不是宏）后编过。`@Observable` 来自 ObservationMacros，命令行工具里有这个插件，M5-7 再确认。
  - `swift test` 能跑，Command Line Tools 自带 swift-testing。
  - **没有做的**：组装成 `.app` 包（Info.plist、`LSUIElement`）、ad-hoc 签名、实际运行看菜单栏图标。会话里不启动界面程序，运行写进走查清单交用户。
- **对计划的影响**：
  - M5-7 起界面用 SwiftPM，不依赖 Xcode；代码约定写进 m5-plan：不用 SwiftUI 的宏（`@State`、`@Entry`、`#Preview` 等），状态用 `ObservableObject`。
  - `.app` 的组装写成脚本（M5-7）。
  - 会话里编和测的命令写进 m5-plan 的 M5-7 一节，后面每个界面任务的验收都用同一套。
  - 如果用户以后装了 Xcode，可以改回 `@State`，不影响结构。

## 产出清单

- `internal/authz`：签发和核对我们自己的短期凭证（`Issuer`）、`SystemVerifier` 接口和 `Unsupported` 实现、外部形式解析、参数摘要；单测。
- `internal/console`：控制台用户（darwin 读 `/dev/console` 的属主，其他平台“尚未支持”）；单测。
- `scripts/m5/probe/`：
  - `apigen`：E5 生成器原型；
  - `ssedemo`：E4 事件流演示服务；
  - `goverify`：E1 的 Go 核验程序，cgo，只链接系统框架；
  - `swift/`：SwiftPM 包，含 UDSHTTP、UDSClient、MenuBarDemo、AuthzProbe、APITypes 和单测；
  - `openapi.json`；
  - `run-e1e2.sh` 和 `README.md`：给用户。
