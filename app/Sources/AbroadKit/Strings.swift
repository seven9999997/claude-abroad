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
    public static let protectionLost = "断网保护没有生效，可能有别的软件改了系统的防火墙规则"
    public static let securityNoteTitle = "调用方签名校验未启用"

    public static func processCount(_ n: Int) -> String { "\(n) 个" }
    public static func regionFromTo(_ from: String, _ to: String) -> String { "\(from) → \(to)" }
}
