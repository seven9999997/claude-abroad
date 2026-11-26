// 界面文字集中在这里（m5-plan v2.5 决定 6）：视图和视图模型只引用 L10n，不在各处写字面文字，便于以后加别的语言。
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
    public static let setupServiceText = "后台服务还没有连上。签名和公证推迟期间，后台服务需要手动加载（临时做法，正式安装包会自动注册）。请在“终端”里执行："
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
    public static func engineLine(_ name: String, _ state: String, _ restarts: Int64) -> String { "\(name)：\(state)（重启 \(restarts) 次）" }
    public static func seconds(_ s: Int64) -> String { "\(s) 秒" }
    public static let settingsGeneral = "通用"
    public static let settingsEnv = "环境检测"
    public static let settingsClaude = "回国与 Claude"
    public static let settingsAdvanced = "高级"
    public static let signatureDisabled = "调用方签名校验：未启用"
    public static let cliTitle = "命令行工具"
    public static let cliPath = "/usr/local/bin/abroad（正式安装包会放到这里；开发时在仓库里 go build ./cmd/abroad）"
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
    // M6-8a：首次准备（写入解析锁和托管设置）
    public static let prepareTitle = "首次准备"
    public static let prepareDone = "已完成"
    public static let prepareNotDone = "没有完成（后台下次启动时重试）"
    public static let claudeCodeUnknown = "后台没有报告第四层的状态"
    public static let claudeCodeNotWritten = "托管设置还没有写入（首次准备时写入）"
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
    public static let uninstallInInstaller = "在正式安装包里提供"
    public static let diagnostics = "诊断包"
    public static let diagnosticsNone = "在终端里执行 abroad diagnostics > 诊断包.json，生成脱敏后的诊断包（不含出口 IP、订阅地址和密码）"
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
        }
    }
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
}
