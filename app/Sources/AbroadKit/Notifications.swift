// 通知（m5-plan v2.9 决定 3）。发送放在协议后面：系统通知（只有打包的应用才用，在 ClaudeAbroad 里实现）和空实现。
// 触发来源只有后台的事件流和操作的结果；去重：同一原因不重复、重连后补发的当前状态不当作新事件、“无法判断”不通知。
// 通知里不出现出口 IP、订阅地址、节点地址（只用后台事件里给的原因、项目名，以及集中的文字表）。
// 应用不自动打开 Claude：只有用户点了通知上的按钮才打开（ClaudeOpener，也在协议后面）。
import AbroadAPI
import Foundation

public enum NotificationKind: String, Sendable, Equatable, CaseIterable {
    case protectionLost, protectionRestored, switchRolledBack, switchRollbackIncomplete, restoreDone, regionReady
    /// 启动管控的第三层结束了刚打开的 Claude（M6-4b-1 返工）。
    case launchBlocked
    /// 程序完整性校验不通过（M6-7 补审返工；同一组问题只通知一次）。
    case integrityFailed
}

public struct AppNotification: Sendable, Equatable {
    public var kind: NotificationKind
    public var title: String
    public var body: String
    /// 带“打开 Claude”按钮（只有地区写入完成且环境检测通过时）。
    public var offersOpenClaude: Bool
}

public protocol NotificationSender: Sendable {
    func send(_ n: AppNotification)
}

/// 空实现：不是打包的应用（包括测试和命令行运行）时用，什么都不做。
public struct NullNotificationSender: NotificationSender {
    public init() {}
    public func send(_ n: AppNotification) {}
}

/// 打开 Claude（只在用户点了通知上的按钮时调用）。
public protocol ClaudeOpener: Sendable {
    func openClaude()
}

public struct NullClaudeOpener: ClaudeOpener {
    public init() {}
    public func openClaude() {}
}

/// 选哪种发送实现：只有进程是打包的应用（.app 包、有 Bundle ID）才用系统通知；否则空实现
/// （没有应用包的进程调用系统通知中心会直接崩溃）。
public enum NotificationBackend: Sendable, Equatable {
    case system, null

    public static func choose(bundleURL: URL, bundleIdentifier: String?) -> NotificationBackend {
        bundleURL.pathExtension == "app" && !(bundleIdentifier ?? "").isEmpty ? .system : .null
    }
}

/// 通知开关（界面偏好，只影响通知）。测试用内存实现；应用用系统的用户偏好（UserPreferences）。
public protocol NotificationPreference: AnyObject {
    var enabled: Bool { get set }
}

public final class MemoryPreference: NotificationPreference {
    public var enabled: Bool
    public init(enabled: Bool = true) { self.enabled = enabled }
}

/// 系统的用户偏好：只存这一个布尔，键名固定（“不写磁盘”的源码检查为它开的唯一一处例外，不存任何敏感内容）。
public final class UserPreferences: NotificationPreference {
    public static let key = "notificationsEnabled"
    public init() {}
    public var enabled: Bool {
        get { UserDefaults.standard.object(forKey: Self.key) as? Bool ?? true }
        set { UserDefaults.standard.set(newValue, forKey: Self.key) }
    }
}

/// 决定要不要发、发什么（纯逻辑，单测覆盖）。
@MainActor
public final class Notifier {
    public let sender: NotificationSender
    public let opener: ClaudeOpener
    public let preference: NotificationPreference
    /// 发过的（测试和走查用）。
    public private(set) var sent: [AppNotification] = []

    private var lastSeq: Int64 = -1
    private var gateOpen: Bool? // 最近一次已知的闸门状态（重连后由当前状态重新建立，不通知）
    private var lastClosedCode: String?
    /// 这次关闭期间通知过“保护断开”（只有这时，之后的恢复才通知）。
    private var notifiedLost = false

    /// 用户自己引起的闸门关闭（原因代码精确比较）：停用、回国、回滚、退出（state），以及更换主代理（main_proxy_switch）。
    /// 这些关闭不通知，由它们引起的那次恢复也不通知；关闭期间又加了别的原因时照常通知。
    public static let userCodes: Set<String> = ["state", "main_proxy_switch"]
    /// 只在闸门原来开着时才算“保护断开”的原因（M6-2a）：vm。启用、停用、回国时 abroadd 拉起或关闭虚拟机，
    /// 会在闸门已经因为 state 关着时再加 vm；那不是保护断开。已启用、闸门开着时虚拟机崩溃（由开变关）才通知。
    public static let onlyWhenOpenCodes: Set<String> = ["vm"]

    /// 后台 detail 的形式是“原因代码：说明”。
    static func split(_ detail: String) -> (code: String, text: String) {
        guard let r = detail.range(of: "：") else { return (detail, "") }
        return (String(detail[..<r.lowerBound]), String(detail[r.upperBound...]))
    }
    private var regionWritten = false
    /// 已经通知过的完整性问题（同一组不重复）。
    private var integrityNotified: String?
    /// 最近一次读到的状态（RESTORED、ENABLING……），拦下通知按它选文字。
    private var lastState: String?
    /// 最近一次发“拦下”通知的时刻：桌面版一次启动有好几个进程被拦，短时间内的合成一条。
    private var lastBlockedAt: Date?
    /// 合并“拦下”通知的窗口；比这更早的“拦下”事件（补发的旧事件）不通知。
    public static let blockedMergeWindow: TimeInterval = 10
    public static let blockedMaxAge: TimeInterval = 30
    /// 当前时间（测试注入）。
    public var now: () -> Date = { Date() }

    public init(sender: NotificationSender = NullNotificationSender(), opener: ClaudeOpener = NullClaudeOpener(), preference: NotificationPreference = MemoryPreference()) {
        self.sender = sender
        self.opener = opener
        self.preference = preference
    }

    private func emit(_ kind: NotificationKind, _ body: String, openClaude: Bool = false) {
        let n = AppNotification(kind: kind, title: L10n.notificationTitle(kind), body: body, offersOpenClaude: openClaude)
        guard preference.enabled else { return }
        sent.append(n)
        sender.send(n)
    }

    /// 读到状态。reconnect 为真时（刚连上或重连后）只建立基线、不通知（补发的当前状态不当作新事件）。
    public func observe(status st: DaemonStatus, reconnect: Bool) {
        lastState = st.state
        // 完整性校验不通过：同一组问题只通知一次（连上、重连时读到也通知：这是要用户处理的持续状态）
        if let r = st.integrity, !r.ok {
            let key = (r.problems ?? []).map { $0.file + ":" + $0.kind }.joined(separator: ",")
            if key != integrityNotified {
                integrityNotified = key
                emit(.integrityFailed, L10n.notifyIntegrityFailed)
            }
        } else {
            integrityNotified = nil
        }
        if reconnect || gateOpen == nil {
            gateOpen = st.gateOpen
            lastClosedCode = st.gateOpen ? nil : st.gate.reasons?.first
            // 连上时闸门就因为非用户原因关着：不补发“保护断开”，但之后恢复时通知“保护已恢复”。
            notifiedLost = !st.gateOpen && (st.gate.reasons ?? []).contains { !Self.userCodes.contains($0) }
            return
        }
        // 环境检测通过、之前写过地区：地区就绪。
        if regionWritten, st.envcheck?.hard == "pass" {
            regionWritten = false
            emit(.regionReady, L10n.notifyRegionReady, openClaude: true)
        }
    }

    /// 事件流里的一条（data 是 EventRecord 的 JSON：seq、source、kind、detail）。同一个 seq 只处理一次。
    public func observe(event ev: SSEEvent) {
        struct Rec: Decodable { var seq: Int64?; var source: String?; var kind: String?; var time: String?; var detail: String? }
        guard let r = try? JSONDecoder().decode(Rec.self, from: Data(ev.data.utf8)) else { return }
        if let s = r.seq ?? ev.id.flatMap({ Int64($0) }) {
            if s <= lastSeq { return }
            lastSeq = s
        }
        switch (r.source, r.kind) {
        case ("gate", "gate_closed"):
            let (code, text) = Self.split(r.detail ?? "")
            let repeated = gateOpen == false && code == lastClosedCode
            let wasClosed = gateOpen == false
            gateOpen = false
            lastClosedCode = code
            if repeated || Self.userCodes.contains(code) { return } // 同一原因不重复；用户自己引起的不是“保护断开”
            if wasClosed && Self.onlyWhenOpenCodes.contains(code) { return } // 已经关着时又加的 vm：不通知、不置“通知过断开”
            notifiedLost = true
            emit(.protectionLost, L10n.notifyProtectionLost(L10n.gateReasonName(code), text))
        case ("gate", "gate_opened"):
            let notify = gateOpen == false && notifiedLost // 只在通知过“保护断开”之后的恢复才通知
            gateOpen = true
            lastClosedCode = nil
            notifiedLost = false
            if notify { emit(.protectionRestored, L10n.notifyProtectionRestored) }
        case ("state", "region_synced"):
            regionWritten = true
        case ("launchguard", "blocked"):
            let t = now()
            if let s = r.time, let at = Self.parseTime(s), t.timeIntervalSince(at) > Self.blockedMaxAge { return } // 补发的旧事件
            if let last = lastBlockedAt, t.timeIntervalSince(last) < Self.blockedMergeWindow { return } // 合成一条
            lastBlockedAt = t
            let restored = lastState == "RESTORED" || lastState == "RESTORING"
            emit(.launchBlocked, restored ? L10n.notifyLaunchBlockedRestored : L10n.notifyLaunchBlockedEnv)
        default:
            break // 其余（包括出口检查“无法判断”一类）不通知
        }
    }

    static func parseTime(_ s: String) -> Date? {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let d = f.date(from: s) { return d }
        f.formatOptions = [.withInternetDateTime]
        return f.date(from: s)
    }

    /// 更换主代理的结果（操作的结果）。
    public func switchFailed(_ code: ApiErrorCode?) {
        switch code {
        case .switchRolledBack: emit(.switchRolledBack, L10n.notifySwitchRolledBack)
        case .switchRollbackIncomplete: emit(.switchRollbackIncomplete, L10n.notifySwitchRollbackIncomplete)
        default: break
        }
    }

    /// 回国完成（操作的结果）；pending 是没能还原的项目名。
    /// browsers 是选了“同时强制退出浏览器”但还在运行的浏览器（M6-4b-3 补审返工）。
    public func restoreDone(pending: [String], browsers: [String] = []) {
        var body = pending.isEmpty ? L10n.notifyRestoreDone : L10n.notifyRestorePending(pending)
        if !browsers.isEmpty { body += "；" + L10n.browsersStillOpen(browsers) }
        emit(.restoreDone, body)
    }

    /// 用户点了通知上的“打开 Claude”（唯一会打开 Claude 的地方）。
    public func userTappedOpenClaude() {
        opener.openClaude()
    }
}
