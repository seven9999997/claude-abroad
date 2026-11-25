// 系统服务的实现（m5-plan v2.9 决定 3）：系统通知和“打开 Claude”。会话和测试里不用它们（测试用 AbroadKit 里的空实现和假实现）。
// - 系统通知只有进程是打包的应用时才用（没有应用包的进程调用通知中心会直接崩溃）；
// - 打开 Claude 只在用户点了按钮时调用：通知上的按钮，或首页、菜单的“打开 Claude”（M6-4b；条件由状态给出）。
//   这里是打开应用的系统调用的唯一位置（源码检查固定）。
import AbroadKit
import AppKit
import UserNotifications

enum SystemServices {
    @MainActor static func makeNotifier() -> Notifier {
        let opener = WorkspaceClaudeOpener()
        let sender: NotificationSender
        switch NotificationBackend.choose(bundleURL: Bundle.main.bundleURL, bundleIdentifier: Bundle.main.bundleIdentifier) {
        case .system: sender = UserNotificationSender(opener: opener)
        case .null: sender = NullNotificationSender()
        }
        return Notifier(sender: sender, opener: opener, preference: UserPreferences())
    }
}

/// 打开 Claude 桌面版（只在用户点了按钮时调用）。
struct WorkspaceClaudeOpener: ClaudeOpener {
    static let bundleID = "com.anthropic.claudefordesktop"
    func openClaude() {
        guard let url = NSWorkspace.shared.urlForApplication(withBundleIdentifier: Self.bundleID) else { return }
        NSWorkspace.shared.openApplication(at: url, configuration: NSWorkspace.OpenConfiguration())
    }
}

/// 系统通知（UserNotifications）。第一次发送时才申请权限；点了“打开 Claude”按钮时调用 opener。
final class UserNotificationSender: NSObject, NotificationSender, UNUserNotificationCenterDelegate, @unchecked Sendable {
    static let category = "claude-abroad.region-ready"
    static let openAction = "open-claude"
    let opener: ClaudeOpener
    private var asked = false

    init(opener: ClaudeOpener) {
        self.opener = opener
        super.init()
        let center = UNUserNotificationCenter.current()
        center.delegate = self
        let open = UNNotificationAction(identifier: Self.openAction, title: L10n.openClaude, options: [.foreground])
        center.setNotificationCategories([UNNotificationCategory(identifier: Self.category, actions: [open], intentIdentifiers: [])])
    }

    func send(_ n: AppNotification) {
        let center = UNUserNotificationCenter.current()
        let content = UNMutableNotificationContent()
        content.title = n.title
        content.body = n.body
        if n.offersOpenClaude { content.categoryIdentifier = Self.category }
        let req = UNNotificationRequest(identifier: UUID().uuidString, content: content, trigger: nil)
        if !asked {
            asked = true
            center.requestAuthorization(options: [.alert, .sound]) { ok, _ in if ok { center.add(req) } }
        } else {
            center.add(req)
        }
    }

    func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse, withCompletionHandler done: @escaping () -> Void) {
        if response.actionIdentifier == Self.openAction { opener.openClaude() }
        done()
    }
}
