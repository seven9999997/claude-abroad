// 界面文字集中在这里（m5-plan v2.5 决定 6）：视图和视图模型只引用 L10n，不在各处写字面文字，便于以后加别的语言。
import AbroadAPI
import Foundation

public enum L10n {
    public static let appName = "Claude 出国"

    // 五种状态
    public static let phaseAbroad = "已出国"
    public static let phaseOffline = "断网"
    public static let phaseHome = "已回国"
    public static let phaseChecking = "检查中"
    public static let phaseError = "出错"

    // 菜单和按钮
    public static let enable = "启用"
    public static let disable = "停用"
    public static let restore = "一键回国"
    public static let openWindow = "打开主窗口…"
    public static let quit = "退出 Claude 出国"
    public static let confirm = "确认"
    public static let cancel = "取消"
    public static let copyCommand = "拷贝命令"
    public static let close = "关闭"

    // 首页
    public static let homeTitle = "首页"
    public static let mainExit = "主代理出口"
    public static let mainExitNone = "还没有：应用主代理后显示"
    public static let gateReasons = "关闭原因"
    public static let region = "地区"

    // 提示
    public static let backendDown = "后台服务没有运行"
    public static let backendDownDetail = "请确认后台服务已安装并在运行；恢复后这里会自动更新。"
    public static let backendNoListener = "后台服务没有在监听（可能正在重启）"
    public static let backendRejected = "后台服务拒绝了这个连接（只接受当前登录的用户）"
    public static let busy = "有别的操作在进行，请稍后再试"
    public static let unsupported = "后台服务现在的模式不提供这项功能"
    public static let confirmTitle = "需要确认"
    public static let processesWillQuit = "会强制结束的 Claude 进程"
    public static let processesUnverified = "没能确认、不会结束的进程"
    public static let regionChange = "地区变化"
    public static let terminalTitle = "这一步请在终端执行"
    public static let terminalDetail = "这是需要本人授权的操作。请打开“终端”，执行下面的命令，按提示输入密码并确认："
    public static let interrupted = "上一次操作没有做完：可以再点一次继续，或一键回国"
    public static let protectionLost = "断网保护没有生效，可能有别的软件改了系统的防火墙规则"
    public static let securityNoteTitle = "调用方签名校验未启用"

    // 安装引导
    public static let setupTitle = "安装引导"
    public static let setupWelcome = "欢迎"
    public static let setupService = "后台服务"
    public static let setupMainProxy = "配置主代理"
    public static let setupDone = "完成"
    public static let setupWelcomeText = "Claude 出国让 Claude 的流量只经你选的主代理出去；主代理不可用时断网，而不是走本机网络。接下来三步：确认后台服务在运行、配置主代理、完成。"
    public static let setupServiceText = "后台服务还没有连上。这一版没有开发者签名，不提供正式安装包，后台服务要用开发安装脚本（sudo scripts/m6/install/install.sh）注册，或者手动加载。请在“终端”里执行："
    public static let setupServiceOK = "后台服务已经连上。"
    public static let setupServiceCommand = "sudo launchctl bootstrap system /Library/LaunchDaemons/com.claude-abroad.abroadd.plist"
    public static let setupTemporary = "这是临时做法"
    public static let setupMainProxyText = "添加来源、选择节点并“应用主代理”。应用成功后才能完成引导。"
    public static let setupMainProxyDone = "主代理已经应用。"
    public static let setupDoneText = "设置完成。之后可以在菜单栏里启用、停用和一键回国。"
    public static let next = "下一步"
    public static let finish = "完成"
    public static let recheck = "重新检查"
    public static let openMainProxy = "打开主代理页面"

    // 主代理页面
    public static let mainProxyTitle = "主代理"
    public static let sources = "来源"
    public static let noSources = "还没有来源"
    public static let nodes = "节点"
    public static let addSource = "添加来源"
    public static let addSubscription = "订阅地址"
    public static let addLocalFile = "本地配置文件"
    public static let addManualNode = "手动节点"
    public static let sourceName = "名字"
    public static let subscriptionURL = "订阅地址"
    public static let chooseFile = "选择文件…"
    public static let dropFileHint = "或把配置文件拖到这里"
    public static let nodeTypeLabel = "类型"
    public static let nodeServer = "服务器"
    public static let nodePort = "端口"
    public static let nodeUser = "用户名（可不填）"
    public static let nodePassword = "密码（可不填）"
    public static let viaHostToggle = "经本机网络抓取订阅"
    public static let viaHostExplain = "现在没有启用，没有主代理可走。要抓取订阅，只能经本机网络直接连订阅地址（订阅服务方会看到你本机的地址）。确定的话勾选“经本机网络抓取订阅”，请重新填写地址后再提交（刚才的地址提交时已经清掉）；或者先启用再更新。"
    public static let submit = "添加"
    public static let delete = "删除"
    public static let refreshSource = "更新订阅"
    public static let refreshViaHost = "经本机网络更新"
    public static let needsReselect = "订阅更新后选中的节点不在了：请重新选择（正在运行的主代理不受影响）"
    public static let exitNode = "出口节点"
    public static let relays = "中转（按顺序）"
    public static let addRelay = "加一跳中转"
    public static let removeRelay = "去掉"
    public static let none = "（未选）"
    public static let applyMain = "应用主代理"
    public static let applying = "正在应用：切换、下发、检查…"
    public static let applySaved = "已保存选择"
    public static func applySucceeded(_ country: String, _ tz: String) -> String { "主代理已应用：出口在 \(country)" + (tz.isEmpty ? "" : "（\(tz)）") }
    public static let rollbackDone = "已经回到原来的主代理"
    public static let rollbackIncomplete = "恢复原来的主代理没有完成，稍后会自动重试"
    public static let checkMain = "检查主代理"
    public static let checking = "正在检查…"
    public static func checkOK(_ country: String, _ tz: String, _ rule: String) -> String { "主代理可用：出口在 \(country)" + (tz.isEmpty ? "" : "（\(tz)）") + "，规则 \(rule)" }
    public static let lanTitle = "确认局域网节点"
    public static func lanText(_ name: String) -> String { "节点“\(name)”在你的局域网里，确认后虚拟机可以连到这个地址。" }
    public static let addBadURL = "订阅地址要以 http:// 或 https:// 开头"
    public static let addNoFile = "请先选择一个配置文件"
    public static let addReadFailed = "读不了这个文件"
    public static let addBadNode = "请填写服务器和端口（1–65535）"
    public static let tempFileFailed = "没能写临时文件"
    public static let defaultSubscriptionName = "订阅"
    public static let defaultNodeName = "手动节点"
    public static let terminalTempFile = "要提交的内容已经存进一个只有你能读的临时文件（关闭这个提示时会删除）："
    public static let terminalStepWrite = "第一步：内容已存进临时文件"
    public static let terminalStepRun = "第二步：在“终端”里执行"
    public static let terminalDone = "执行完了"
    public static let homeTab = "首页"

    // 其余页面（M5-9）
    public static let loadFailed = "加载失败"
    public static let laterVersion = "在以后的版本提供"
    public static let exitsTitle = "分流出口"
    public static let noExits = "没有分流出口：所有流量走主代理"
    public static let exitTag = "标签"
    public static let exitKind = "类型"
    public static let exitAdd = "导入出口"
    public static let exitCheck = "检查"
    public static let exitUp = "可用"
    public static let exitDown = "不可用"
    public static let exitFormIncomplete = "请填写标签并选择配置文件"
    public static func exitConns(_ tcp: Int64, _ udp: Int64) -> String { "连接 TCP \(tcp) · UDP \(udp)" }
    public static let appsTitle = "应用分流"
    // 补齐一：规则里的四项（说明文字集中在这里）。
    public static let rulesListsTitle = "域名与其他规则"
    public static let rulesLocalTitle = "本地域名名单"
    public static let rulesLocalHelp = "名单里的域名走本机网络，不经代理（公司内网、局域网设备的域名）。添加是放松保护，需要本人授权；Claude 的域名加不进来。"
    public static let rulesPinnedTitle = "置顶域名"
    public static let rulesPinnedHelp = "置顶域名永远走主代理，不管哪个应用发起。内置的几个删不掉；你追加的可以删（删除是放松保护，需要本人授权）。追加的置顶域名不进 Claude 域名解析锁。"
    public static let rulesPinnedBuiltin = "内置（删不掉，含各自的子域名）"
    public static let rulesNoFakeTitle = "不分配假地址的名单"
    public static let rulesNoFakeHelp = "名单里的域名经主代理的加密 DNS 解析出真实地址，之后的连接仍按规则走（默认走主代理）；适合投屏、设备发现一类需要真实地址的域名。内置的几项（时间同步、联网检测、.lan、.local）删不掉。添加是放松保护，需要本人授权；Claude 的域名和置顶域名加不进来。"
    public static let allowLANTitle = "允许局域网"
    public static let allowLANHelp = "关掉后，局域网里的打印机、NAS、投屏、别的电脑的共享都连不上（断网和启用时都一样）。关掉是收紧；从关到开是放松保护，需要本人授权。已回国时只记下，下次启用生效。"
    // 补齐四：环境检测的历史、系统进程单列。
    public static let envHistoryTitle = "历史"
    public static let envHistoryEmpty = "还没有环境检测的历史（启用之后每一轮检测都会记下；结果和上一条相同时只更新时间）"
    public static let envHistoryClear = "清空历史"
    public static func envHistoryLine(_ at: String, _ hard: String, _ until: String?) -> String {
        at + "  硬性项：" + hard + (until.map { "（一直到 " + $0 + "）" } ?? "")
    }
    public static func envHistoryChanged(_ items: [String]) -> String { "变了：" + items.joined(separator: "、") }
    public static let appsSystemSection = "系统进程"
    public static let sharedRuleWarnTitle = "这个进程替很多应用联网"
    public static func sharedRuleWarn(_ note: String) -> String { note + "。改它的规则会同时影响那些应用，确定要改吗？" }
    public static let sharedRuleConfirm = "仍然修改"
    public static let rulesOfflineTitle = "离线放行名单"
    public static let rulesOfflineHelp = "停用或闸门关着时仍可直连的应用（应用 ID）。添加是放松保护，需要本人授权；Claude 加不进来。"
    public static let rulesUDP443Title = "拦截 UDP 443"
    public static let rulesUDP443Help = "打开后拦下 QUIC，浏览器改用 TCP，域名识别更可靠。关掉是放松保护，需要本人授权。"
    public static let rulesAddPlaceholder = "添加"
    public static let rulesNoChange = "没有改动（已经是这样）"
    public static let rulesBuiltinReadonly = "这是内置的置顶域名，删不掉（只能删你追加的）"
    public static let rulesChangedReloaded = "规则刚被别处改过，已刷新，请再确认一次"
    public static let liveApps = "在联网的应用"
    public static let ruleList = "规则"
    public static let actionDirect = "本机网络"
    public static let actionBlock = "阻止"
    public static let actionMain = "主代理"
    public static let actionDefault = "（默认）"
    public static let dropAffected = "断开受影响的连接"
    public static let dropAppHint = "把应用拖到这里添加规则"
    public static let directClaudeDomains = "设成“本机网络”的应用访问 Claude 的域名时仍走主代理（置顶域名，防止从本机地址访问 Claude）。"
    public static let claudeNoDirect = "Claude 的应用不能设成“本机网络”（会从本机地址访问 Claude）。"
    public static let proxyNoDirect = "代理软件不能设成“本机网络”（它转发的别的应用的流量会从本机地址出去）。"
    public static let authorizationReason = "这个改动放松了保护，需要本人授权"
    public static let envTitle = "环境检测"
    public static let envHard = "硬性项"
    public static let envWarn = "警告项"
    public static let envRerun = "立即重新检测"
    public static let envDevice = "设备体检"
    public static let hardItemLocked = "硬性项不能关掉：它们明确不通过时会关闭闸门，保护不能由开关绕过"
    public static let resultPass = "通过"
    public static let resultFail = "不通过"
    public static let resultUnknown = "无法判断"
    public static let resultSkipped = "已跳过"
    public static let regionTitle = "地区"
    public static func regionTarget(_ tz: String, _ lang: String, _ locale: String) -> String { "时区 \(tz)，语言 \(lang)，地区格式 \(locale)" }
    public static let regionSourceExit = "跟随出口"
    public static let regionSourceManual = "手动指定"
    public static let regionSource = "来源"
    public static let regionNone = "还没有锁定目标地区"
    public static let regionTimezone = "时区（例如 America/Los_Angeles）"
    public static let regionLang = "语言（例如 en-US）"
    public static let regionSet = "切换到这个地区"
    public static let regionFollowExit = "跟随出口"
    public static let regionFormIncomplete = "请填写时区和语言"
    public static let regionSwitched = "地区已切换"
    public static let regionPending = "待还原的项（网络已恢复，这些还没还原，后台会自动重试）"
    public static let enginesTitle = "引擎与虚拟机"
    public static let vmUnreachable = "虚拟机不可达"
    public static let vmVersion = "agent 版本"
    public static let vmKernel = "内核"
    public static let vmUptime = "运行时间"
    public static let vmBootChanged = "虚拟机重启过"
    /// 由 abroadd 管理虚拟机时的阶段名称（M6-2a；不认识的原样显示）。
    public static func vmPhaseName(_ code: String) -> String {
        switch code {
        case "idle": return "不需要（未启用或已回国）"
        case "starting": return "正在启动"
        case "running": return "运行中"
        case "unhealthy": return "健康检查不通过"
        case "backoff": return "等待重新启动"
        case "stopping": return "正在关闭"
        case "failed": return "连续启动失败，已停止重启（保持断网）"
        case "stuck": return "关闭不了，没有再启动新的"
        default: return code
        }
    }
    public static func vmManagerLine(_ phase: String, _ restarts: Int64) -> String { "虚拟机：" + vmPhaseName(phase) + "（重新启动 \(restarts) 次）" }
    public static let vmLastError = "最近一次失败"
    /// 借用虚拟机（v0.70）：未启用时应用主代理、检查主代理临时启动虚拟机，检查完自动关闭。
    public static let vmBorrowed = "临时运行：做出口检查，结束后自动关闭"
    /// 借用虚拟机的进度（状态里的 borrow.step；不认识或没有时为 nil）。
    public static func borrowStep(_ step: String?) -> String? {
        switch step {
        case "starting": return "正在启动虚拟机（第一次可能要一两分钟）"
        case "pushing": return "正在下发主代理"
        case "checking": return "正在检查出口"
        case "stopping": return "正在关闭虚拟机"
        default: return nil
        }
    }
    /// 经虚拟机的检查做不成时，按稳定的代码给一句标题（后台的原文照常显示在下面）。
    public static func borrowFailureTitle(_ code: ApiErrorCode?) -> String? {
        switch code {
        case .vmUnavailable: return "虚拟机没起来"
        case .mainPushFailed: return "主代理配置没下发成功"
        case .mainUnreachable: return "主代理不通"
        case .noMainSelected: return "还没有选主代理节点"
        case .stateIncomplete: return "上一次启用或回国没有完成"
        default: return nil
        }
    }
    public static func engineLine(_ name: String, _ state: String, _ restarts: Int64) -> String { "\(name)：\(state)（重启 \(restarts) 次）" }
    public static func seconds(_ s: Int64) -> String { "\(s) 秒" }
    public static let settingsGeneral = "通用"
    public static let settingsEnv = "环境检测"
    public static let settingsClaude = "回国与 Claude"
    public static let settingsAdvanced = "高级"
    public static let signatureDisabled = "调用方签名校验：未启用"
    public static let cliTitle = "命令行工具"
    public static let cliPath = "/usr/local/bin/abroad（开发安装脚本会放到这里；没装时在仓库里 go build ./cmd/abroad）"
    public static let cliUsage = "abroad status 查看状态；sudo abroad restore 一键回国；abroad --help 看全部用法"
    public static let processList = "进程清单"
    public static let processListLater = "要结束的 Claude 进程清单在一键回国的确认框里列出；单独查看的页面在以后的版本提供"
    public static let processListNone = "现在没有要结束的 Claude 进程"
    public static func actionExit(_ tag: String) -> String { "分流出口 " + tag }
    public static let launchControl = "启动管控"
    // M6-3：第一层（Claude 域名解析锁）
    public static let uninstalledError = "操作失败：Claude 出国已经一键卸载。要继续使用请重新安装。"
    public static let claudeDomainsTitle = "Claude 访问过的域名"
    public static let claudeDomainsNone = "还没有记录"
    public static let claudeDomainsClear = "清空"
    public static func claudeDomainLine(_ d: String, _ n: Int) -> String { d + "（\(n) 次）" }
    public static let launchControlOff = "未启用：这次安装没有打开启动管控（开发用的运行方式）"
    public static let resolverLockTitle = "第一层：Claude 域名解析锁"
    public static let resolverLockUnknown = "后台没有报告解析锁的状态"
    public static let resolverLockNotWritten = "还没有写入（首次准备时写入）"
    public static func resolverLockEffective(_ doms: [String]) -> String { "生效中：" + doms.joined(separator: "、") + "（只锁内置的 Claude 域名；你在规则里追加的置顶域名不在锁里）" }
    public static let resolverLockNotEffective = "未生效"
    public static func resolverLockProblem(_ domain: String, _ kind: String) -> String {
        switch kind {
        case "missing": return domain + " 的锁文件不在了"
        case "modified": return domain + " 的锁文件被改过"
        case "permission": return domain + " 的锁文件权限不对"
        default: return domain + " " + kind
        }
    }
    // M6-3b：第四层（Claude Code 的托管设置和本机代理端口）
    // M6-4b：打开 Claude、启动管控
    public static let openClaudeButton = "打开 Claude"
    public static let openClaudeNotEnabled = "已启用之后才能打开 Claude"
    public static let openClaudeEnvNotPassed = "环境检测的硬性项还没有全部通过"
    public static let openClaudeGateClosed = "现在代理流量关着（出口检查或接管有问题）"
    public static let browsersRunning = "正在运行的浏览器（其中打开的 Claude 页面请自行关闭）"
    public static let killBrowsersOption = "同时强制退出下面确认过的浏览器（未保存的表单内容会丢失）"
    public static let launchGuardUnknown = "后台没有报告启动管控的状态"
    public static let launchGuardAllowed = "现在允许启动 Claude"
    public static let launchGuardBlocked = "现在不允许启动 Claude：打开后会被马上结束"
    public static func launchGuardLayer(_ name: String, _ on: Bool) -> String { name + "：" + (on ? "生效" : "未生效") }
    public static func launchGuardCoverage(_ name: String, _ status: String, _ reason: String) -> String {
        let s: String
        switch status {
        case "covered": s = "覆盖"
        case "not_covered": s = "不覆盖"
        case "by_signature": s = "视签名而定"
        default: s = "未知"
        }
        return "· " + name + "：" + s + "。" + reason
    }
    public static func launchGuardRecent(_ n: Int) -> String { "最近拦下 \(n) 次" }
    // M6-11b：回国拦截（回国后 pf 只拦 Anthropic 登记的地址段；设计 10.2 第五道）。
    public static let returnBlockTitle = "回国拦截"
    public static let returnBlockConsequence = "回国后，本机任何程序（包括浏览器）都连不上 Anthropic 登记的地址段；本机另有代理软件接管时拦不住。只有卸载才解除。"
    public static func returnBlockActive(_ nets: [String]) -> String { "生效中：拦 " + nets.joined(separator: "、") }
    public static func returnBlockOn(_ nets: [String]) -> String { "已打开：回国后拦 " + nets.joined(separator: "、") }
    public static let returnBlockTestTag = "测试段（开发构建）：只拦文档保留的地址，不影响真实的 Claude"
    public static let returnBlockOff = "未启用（首次准备没完成，或已经卸载）"
    public static let returnBlockHomeTitle = "已拦截 Anthropic 的地址"
    public static let returnBlockHome = "本机直连 Anthropic 的地址已被拦截（回国拦截，只有卸载才解除）"
    public static let loginItemHint = "如果 Claude 设了“登录时打开”，请在 Claude 的设置里关掉：开机时它会比 Claude 出国先起来，会被拦下"
    public static let claudeCodeTitle = "第四层：Claude Code 的本机代理"
    // M6-7：程序完整性（没有付费开发者账号，用清单校验代替系统签名校验）
    public static let integrityTitle = "程序完整性"
    public static let integrityAlertTitle = "程序文件不完整，不能启用"
    public static func integrityAlert(_ items: [String]) -> String {
        "Claude 出国的程序或虚拟机镜像被改过或损坏（" + items.joined(separator: "、") + "）。现在只保留断网规则、不接管网络，也不能启用。请重新安装 Claude 出国；需要先上网时点“一键回国”。"
    }
    public static let notifyIntegrityFailed = "程序或虚拟机镜像被改过或损坏：只保留断网规则，不能启用。请重新安装；需要先上网时点“一键回国”。"
    public static let integrityUnknown = "后台没有报告完整性校验"
    public static let integrityOK = "按清单校验通过（未使用系统签名校验）"
    public static func integrityNotChecked(_ why: String) -> String { "没有校验：" + why }
    public static func integrityFailed(_ items: [String]) -> String {
        "校验不通过：" + items.joined(separator: "、") + "。只保留断网规则、不能启用，请重新安装。"
    }
    public static func integrityItem(_ file: String, _ kind: String) -> String {
        switch kind {
        case "missing": return file + " 不见了"
        case "mismatch": return file + " 内容不对"
        default: return "完整性清单被改过或读不出"
        }
    }
    // M6-8a：首次准备（写入解析锁；v0.68 起不写托管设置）
    public static let prepareTitle = "首次准备"
    public static let prepareDone = "已完成"
    public static let prepareNotDone = "没有完成（后台下次启动时重试）"
    public static let claudeCodeUnknown = "后台没有报告第四层的状态"
    public static let claudeCodeNotWritten = "托管设置没有写入"
    // v0.68：第四道是可选的开关，默认关（用户 2026-11-27）。关着写“未开启”，和“未生效”区分开。
    public static let claudeCodeOff = "未开启（默认关）：Claude Code 不经本机代理，回国后靠第一层（解析锁）和第五层（回国拦截）"
    public static let claudeCodeTestMode = "测试模式：只监听，不写托管设置（本机真实的 Claude Code 不受影响）"
    public static let claudeCodeLeftover = "托管设置目录里还有 claude-abroad.json（被改过所以没有删，或是上一版写下的）：它会把 Claude Code 指向一个没在听的端口，请自己检查后删掉，或再执行一次下面的关闭命令"
    public static let claudeCodeConsequenceTitle = "打开的后果："
    /// 打开第四道的后果（和后台 daemon.ClaudeProxyConsequences 内容一致；命令行打开前打印同样的三句）。
    public static let claudeCodeConsequences = [
        "这台 Mac 上所有 Claude Code 都会被指向 Claude 出国的本机代理（127.0.0.1:47821）。",
        "没有处于“已启用”时，Claude Code 一律连不上，包括经本机别的代理线路启动的。",
        "关掉开关（sudo abroad claude-proxy off）或卸载才恢复。",
    ]
    public static let claudeCodeOnLabel = "打开（在终端里执行，会先显示后果并要求确认）："
    public static let claudeCodeOffLabel = "关闭（在终端里执行）："
    public static let claudeCodeOnCommand = "sudo abroad claude-proxy on"
    public static let claudeCodeOffCommand = "sudo abroad claude-proxy off"
    public static let claudeCodeEffective = "生效中：Claude Code 只经本机代理端口联网，回国后一律连不上"
    public static let claudeCodeNotEffective = "未生效"
    public static func claudeCodeProblem(_ kind: String) -> String {
        switch kind {
        case "missing": return "托管设置文件不在了"
        case "modified": return "托管设置文件被改过"
        case "unreadable": return "托管设置文件普通用户读不到"
        case "permission": return "托管设置文件权限不对"
        default: return kind
        }
    }
    public static let claudeCodePortDown = "本机代理端口没在听（可能被别的程序占着）"
    public static func claudeCodeConflicts(_ items: [String]) -> String { "别的托管设置文件也定义了代理变量：" + items.joined(separator: "、") }
    public static let claudeCodeUnparsable = "解析不了"
    public static let claudeCodeScope = "只覆盖 Claude Code，不覆盖 Claude 桌面版和浏览器"
    public static func resolverLockConflicts(_ doms: [String]) -> String { "有别的软件写的同名文件：" + doms.joined(separator: "、") + "（这些域名不受锁保护）" }
    public static let uninstall = "卸载"
    public static let uninstallInInstaller = "应用里的卸载入口下一步提供；现在请在终端里执行 sudo abroad uninstall，或开发安装的卸载脚本 sudo scripts/m6/install/uninstall.sh"
    public static let diagnostics = "诊断包"
    // 补齐五：应用里导出诊断包。
    public static let diagnosticsExport = "导出诊断包…"
    public static let diagnosticsIncludeDomains = "包含 Claude 访问过的域名清单"
    public static let diagnosticsHelp = "诊断包是后台整理好的脱敏材料：版本、状态、最近一次环境检测和检测历史、启动管控。不含出口 IP、订阅地址、节点地址和密码；默认不含 Claude 访问过的域名（勾选上面一项才带）。应用打不开时，也可以在终端里执行 abroad diagnostics > 诊断包.json（加 --include-domains 带域名清单）。"
    public static func diagnosticsFileName(_ stamp: String) -> String { "诊断包-" + stamp + ".json" }
    public static func diagnosticsSaved(_ name: String) -> String { "已保存：" + name }
    public static let diagnosticsBackendDown = "导出诊断包失败：后台服务没有运行。请确认后台服务在运行后再试；也可以在终端里执行 abroad diagnostics。"
    public static func diagnosticsBackendError(_ why: String) -> String { "导出诊断包失败：后台没有给出诊断包（" + why + "）。请稍后再试；也可以在终端里执行 abroad diagnostics。" }
    public static func diagnosticsWriteFailed(_ why: String) -> String { "导出诊断包失败：文件没有写成（" + why + "）。请换一个位置再试。" }
    public static let deviceCheckHint = "按需运行：扫描随中文办公软件安装的字体（只提示，不影响放行）"
    public static let deviceCheckRun = "运行设备体检"
    public static let deviceCheckClean = "没有发现随中文办公软件安装的字体"
    public static func deviceCheckFound(_ n: Int) -> String { "发现 \(n) 个随中文办公软件安装的字体：" }
    public static func deviceCheckLine(_ loc: String, _ font: String, _ file: String) -> String { "· " + loc + "：" + font + "（" + file + "）" }
    public static let unreachableTitle = "后台服务没有运行"
    public static let unsupportedTitle = "这个模式不提供"
    public static let retry = "重试"
    public static let listSeparator = "、"

    // 确认框与通知（M5-10）
    public static func confirmRemaining(_ s: Int) -> String { "确认码 \(s) 秒内有效" }
    public static func confirmWait(_ s: Int) -> String { "\(s) 秒后可以确认" }
    public static let confirmExpired = "已过期，请重新发起"
    public static let confirmRefreshed = "确认码已失效，下面是重新取到的内容（可能有变化），请再确认一次"
    public static let notificationsToggle = "发送通知（保护断开和恢复、更换主代理失败、回国完成、地区就绪）"
    public static let openClaude = "打开 Claude"
    public static func notificationTitle(_ k: NotificationKind) -> String {
        switch k {
        case .protectionLost: return "保护断开"
        case .protectionRestored: return "保护已恢复"
        case .switchRolledBack: return "更换主代理失败"
        case .switchRollbackIncomplete: return "更换主代理失败"
        case .restoreDone: return "已回国"
        case .regionReady: return "地区已就绪"
        case .launchBlocked: return "已阻止 Claude 启动"
        case .integrityFailed: return "程序文件不完整"
        case .udpUnavailable: return "主代理的 UDP 不可用"
        case .exitFallback: return "分流出口不可用"
        }
    }
    // 补齐二：UDP 不可用、分流出口回退到主代理（只用出口标签和应用 ID，不出现任何地址）。
    public static let udpUnavailableTitle = "主代理的 UDP 不可用"
    public static let udpUnavailableDetail = "语音、视频通话和部分游戏会不通；换一个支持 UDP 的节点可以恢复。"
    public static func udpLabel(_ state: String?) -> String {
        switch state {
        case "available": return "UDP 可用"
        case "unavailable": return "UDP 不可用"
        default: return "UDP 还不知道"
        }
    }
    public static func fallbackMark(_ exit: String) -> String { "分流出口 " + exit + " 现在不可用，流量改走主代理" }
    public static func exitAffected(_ apps: [String]) -> String { "受影响的规则：" + apps.joined(separator: "、") + "（流量改走主代理）" }
    public static let notifyUDPUnavailable = "主代理的 UDP 不可用：语音、视频通话和部分游戏会不通。换一个支持 UDP 的节点可以恢复。"
    public static func notifyExitFallback(_ exit: String, _ n: Int) -> String { "分流出口 " + exit + " 不可用：" + String(n) + " 条规则的流量改走主代理。" }
    public static func notifyProtectionLost(_ reason: String, _ detail: String) -> String {
        "代理流量已关闭：" + reason + (detail.isEmpty ? "" : "（" + detail + "）")
    }
    /// 闸门原因代码的名称（不认识的代码原样显示）。
    public static func gateReasonName(_ code: String) -> String {
        switch code {
        case "takeover": return "本机接管出了问题"
        case "exit_check": return "主代理的出口检查没有通过"
        case "env_check": return "环境检测的硬性项没有通过"
        case "state": return "没有启用"
        case "main_proxy_switch": return "正在更换主代理"
        case "vm": return "虚拟机没有运行或不健康"
        case "integrity": return "程序文件不完整（要重新安装）"
        default: return code
        }
    }
    public static let confirmNotNeededDone = "这次不需要确认，已完成"
    public static let confirmSubmitting = "正在执行…"
    public static let notifyProtectionRestored = "代理流量已恢复"
    public static let notifySwitchRolledBack = "已经回到原来的主代理"
    public static let notifySwitchRollbackIncomplete = "恢复原来的主代理没有完成，稍后会自动重试"
    public static let notifyRestoreDone = "本机网络已恢复"
    public static func notifyRestorePending(_ items: [String]) -> String { "本机网络已恢复；还没还原：" + items.joined(separator: "、") + "（后台会自动重试）" }
    // M6-4b-1 返工：启动管控拦下 Claude 时的通知（回国状态那一句取自设计 10.2 第三层）。
    public static let notifyLaunchBlockedRestored = "现在是本机网络（已回国），不允许启动 Claude。要使用 Claude，请先启用 Claude 出国；如果不再使用，可以在设置里一键卸载。"
    public static let notifyLaunchBlockedEnv = "环境检测通过前不允许启动 Claude，刚打开的 Claude 已结束。检测通过后再打开。"
    public static func browsersStillOpen(_ names: [String]) -> String {
        "这些浏览器还开着：" + names.joined(separator: "、") + "，其中的 Claude 页面请自行关闭"
    }
    public static let notifyRegionReady = "时区和语言已写入，环境检测通过，可以打开 Claude 了"

    public static func titleLine(_ phase: String) -> String { appName + "：" + phase }
    public static func exitLine(_ ip: String, _ place: String) -> String { mainExit + "：" + ip + "（" + place + "）" }
    public static func exitNoteLine(_ note: String) -> String { mainExit + "：" + note }
    public static func placeLine(_ place: String, _ tz: String) -> String { place + (tz.isEmpty ? "" : "，" + tz) }
    public static func gateReasonsLine(_ r: [String]) -> String { gateReasons + "：" + r.joined(separator: "、") }
    public static func regionLine(_ tz: String, _ lang: String) -> String { region + "：" + tz + "，" + lang }
    public static func sourceLine(kind: String, nodes: Int, updated: String) -> String {
        (kind == "subscription" ? addSubscription : addLocalFile) + " · \(nodes) 个节点 · 更新于 " + updated
    }
    public static func nodeLine(_ name: String, _ type: String, _ source: String) -> String { name + "（" + type + "，" + source + "）" }

    public static func processCount(_ n: Int) -> String { "\(n) 个" }
    public static func regionFromTo(_ from: String, _ to: String) -> String { "\(from) → \(to)" }

    // 按原型改样式（2026-11-27）：侧边栏、首页状态卡片、各页分组的标题。
    public static let navOverview = "总览"
    public static let navGroupNetwork = "网络"
    public static let navGroupEnvironment = "环境"
    public static let navGroupSystem = "系统"
    public static let homeExitIP = "出口 IP"
    public static let homePlace = "位置与时区"
    public static let vmSection = "虚拟机"
    public static let enginesSection = "引擎"
    public static let regionManualSection = "手动指定"
    public static let backendRunning = "后台服务已连上"
}

// ---- B0（v0.71b）：应用里经系统密码框执行敏感操作 ----
extension L10n {
    public static let privExecute = "执行"
    public static let privInTerminal = "在终端执行"
    public static let privClose = "关闭"
    public static let privRunning = "正在执行，请输入管理员密码……"
    public static let privHow = "点“执行”后会弹出系统的管理员密码框，以管理员身份运行已安装的命令行（同一次打开应用后约 5 分钟内再执行不再询问密码）。"
    public static let privRecheck = "执行时会按当时的情况重新列出；之后新启动的 Claude 也会被结束。"
    public static let privInputFile = "要交给命令行的文件（只显示路径；执行时核对内容没有被改过）："
    public static let privSucceeded = "已完成"
    public static let privCancelled = "已取消，什么都没有改动"
    public static let privOutput = "命令行的输出："
    public static let privBadChars = "参数里有不支持的字符（换行或控制字符），没有执行"
    public static let privUnavailable = "没法弹出系统密码框执行，请改在终端执行"
    public static let privEnableBlocked = "现在不能启用："
    public static let privEnableNoChange = "启用会接管本机网络；地区不会变，不结束任何进程。"
    public static let privEnableAlready = "已经是启用状态。"
    public static let privClaudeProxyOff = "关掉之后，回国后靠解析锁和回国拦截。"
    public static let claudeCodeTurnOn = "打开…"
    public static let privScriptFailed = "系统密码框的脚本没能准备好"
    public static func privErrorNumber(_ n: Int) -> String { "系统返回错误 \(n)" }
    public static let claudeCodeTurnOff = "关闭…"
    /// 系统密码框里的提示（标题栏里是应用名）。
    public static func privPrompt(_ what: String) -> String { "Claude 出国要以管理员身份执行：\(what)" }
    /// 退出码（命令行 0–7，加我们的 31、32）对应的说明。
    public static func privExit(_ code: Int) -> String {
        switch code {
        case 0: return "已完成"
        case 1: return "执行失败（后台服务回了错误，见下面的输出）"
        case 2: return "命令行不认识这个操作：后台服务的版本比应用旧，请先升级（scripts/dev/reinstall.sh）"
        case 3: return "被拒绝：权限不够"
        case 4: return "需要确认，没有执行"
        case 5: return "有别的操作在进行，请等它完成后再试"
        case 6: return "连不上后台服务"
        case 7: return "这个操作在以后的版本提供"
        case 31: return "已安装的命令行不对（不是 root 的或别人可写），没有执行"
        case 32: return "输入文件在确认后被改动过，请重新操作"
        default: return "执行失败（退出码 \(code)）"
        }
    }
    /// 确认框的标题（做什么）。
    public static func privTitle(_ op: SensitiveOperation) -> String {
        switch op {
        case .enable: return "启用（接管本机网络）"
        case .restore: return "回国（恢复本机网络）"
        case .mainApply: return "应用主代理"
        case let .regionSet(tz, lang): return "切换地区到 \(tz)、\(lang)"
        case .regionAuto: return "地区改为跟随出口"
        case let .addSourceFile(_, name), let .addSourceURLFile(_, name, _): return "添加主代理来源“\(name)”"
        case let .addNodeFile(_, name): return "添加节点“\(name)”"
        case let .removeSource(id): return "删除主代理来源 \(id)"
        case let .mainUse(node, via): return via.isEmpty ? "主代理换成节点 \(node)" : "主代理换成节点 \(node)（经 \(via.joined(separator: " → "))）"
        case let .lanConfirm(node): return "确认局域网节点 \(node)"
        case let .exitAdd(tag, kind, _): return "添加分流出口 \(tag)（\(kind)）"
        case let .exitRemove(tag): return "删除分流出口 \(tag)"
        case let .appSet(app, action): return "应用 \(app) 改为 \(action)"
        case let .appRemove(app): return "删除应用 \(app) 的规则"
        case let .ruleListAdd(kind, item): return "规则名单（\(kind)）加上 \(item)"
        case let .ruleListRemove(kind, item): return "规则名单（\(kind)）去掉 \(item)"
        case let .udp443(on): return on ? "拦截 UDP 443" : "不再拦截 UDP 443"
        case let .allowLAN(on): return on ? "打开“允许局域网”" : "关掉“允许局域网”"
        case let .claudeProxy(on): return on ? "打开第四道（Claude Code 的本机代理）" : "关掉第四道（Claude Code 的本机代理）"
        }
    }
    /// 确认框里的现状和后果（不含后台给的预览）。
    public static func privConsequences(_ op: SensitiveOperation) -> [String] {
        switch op {
        case .enable: return []
        case .restore: return [returnBlockConsequence]
        case .mainApply: return ["会经虚拟机里的主代理做一次出口检查并建立出口基准；已启用时会先关闸门、换第一跳，失败回到原来的主代理。"]
        case .regionSet, .regionAuto: return ["已启用且地区会变时，会结束正在运行的 Claude 并改 Mac 的时区和语言。"]
        case .addSourceFile, .addSourceURLFile, .addNodeFile: return ["订阅地址、节点和密码只经文件交给命令行，不出现在命令里。"]
        case .removeSource: return ["正在作为主代理的来源不能删。"]
        case .mainUse: return ["已启用时会先关闸门、换第一跳、下发并检查，失败回到原来的主代理。"]
        case .lanConfirm: return ["确认后，主代理的第一跳可以是这个局域网地址。"]
        case .exitAdd: return ["出口配置只经文件交给命令行，不出现在命令里。"]
        case .exitRemove: return ["规则里引用它的应用会回到主代理。"]
        case .appSet, .appRemove, .ruleListAdd, .ruleListRemove, .udp443: return ["改的是分流规则：放松保护的改动要本人授权。"]
        case let .allowLAN(on): return on ? ["现在：关。打开后，局域网里的设备（打印机、NAS、路由器后台）可以直连。"] : ["关掉后更严：局域网地址也一律走代理或拦截。"]
        case let .claudeProxy(on): return on ? claudeCodeConsequences : [privClaudeProxyOff]
        }
    }
}
