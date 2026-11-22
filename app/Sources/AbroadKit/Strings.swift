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
