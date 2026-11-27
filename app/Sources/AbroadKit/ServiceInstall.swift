// 阶段 B（v0.71c，docs/app-install-plan.md 第 3 节）：应用里安装、升级、卸载后台服务。
// - 应用打开时用只读状态接口看后台服务：没装、版本旧、版本不明、状态格式不同、不允许升级的几种状态各显示什么（3.3 的表）；
// - 只有用户点按钮才弹确认框（PrivilegedSession），确认框里再点“执行”才经系统密码框执行（复用 B0 的提权层）；
// - 应用包里的安装文件（payload）的版本由构建写进 Info.plist（PayloadInfo，应用不读文件）。
import AbroadAPI
import Combine
import Foundation

/// 应用包里的安装文件：版本号（来自 Info.plist）和目录（应用包内的固定位置，由 App.swift 用 Bundle 拼出）。
public struct PayloadInfo: Sendable, Equatable {
    public var commit: String
    public var buildTime: String
    public var dirty: Bool
    public var stateFormat: Int
    public var path: String

    public init(commit: String, buildTime: String, dirty: Bool, stateFormat: Int, path: String) {
        self.commit = commit
        self.buildTime = buildTime
        self.dirty = dirty
        self.stateFormat = stateFormat
        self.path = path
    }

    /// 从 Info.plist 的四个键读（没有这些键时是没有 payload 的应用包）。
    public static func from(info: [String: Any], resourcePath: String?) -> PayloadInfo? {
        guard let c = info["ClaudeAbroadPayloadCommit"] as? String, let t = info["ClaudeAbroadPayloadBuildTime"] as? String,
              let sf = (info["ClaudeAbroadPayloadStateFormat"] as? String).flatMap(Int.init), let res = resourcePath else { return nil }
        return PayloadInfo(commit: c, buildTime: t, dirty: (info["ClaudeAbroadPayloadDirty"] as? String) == "true", stateFormat: sf, path: res + "/payload")
    }

    public var label: String { "\(commit)（构建于 \(buildTime)\(dirty ? "，有未提交的改动" : "")）" }
}

/// 后台服务和应用包里的安装文件比，现在是哪种情况（3.3 的表）。
public enum ServiceState: Sendable, Equatable {
    case checking
    case noPayload
    case notInstalled
    case upToDate
    case upgradeAvailable(old: String, new: String)
    case unknownVersion(new: String)
    case formatDiffers(old: Int, new: Int)
    case daemonNewer(old: String, new: String)
    /// 版本更新但现在不允许升级（已启用、启用中、回国中、借用中、忙、卸载中、已卸载、状态文件坏了）。
    case upgradeBlocked(reason: String)
}

@MainActor
public final class ServiceInstallModel: ObservableObject {
    @Published public private(set) var state: ServiceState = .checking

    public let client: APIClient
    public let payload: PayloadInfo?
    public let privileged: PrivilegedSession?
    /// 安装、升级、卸载之后（确认框关掉时）调用：例如让主状态重新连后台。
    public var afterChange: (@MainActor () async -> Void)?

    public init(client: APIClient, payload: PayloadInfo?, privileged: PrivilegedSession?) {
        self.client = client
        self.payload = payload
        self.privileged = privileged
    }

    /// 读一次状态（只读，不调用提权）。
    public func refresh() async {
        let o = await client.status()
        guard let p = payload else {
            state = .noPayload
            return
        }
        switch o {
        case .ok(let st): state = Self.classify(st, payload: p)
        case .unreachable: state = .notInstalled
        default: state = .notInstalled
        }
    }

    static let commitRe = try! NSRegularExpression(pattern: "^[0-9a-f]{7,40}$")
    static let timeRe = try! NSRegularExpression(pattern: "^[0-9]{8}T[0-9]{6}Z$")
    static func matches(_ re: NSRegularExpression, _ s: String) -> Bool { re.firstMatch(in: s, range: NSRange(s.startIndex..., in: s)) != nil }

    /// 3.3 的表（和安装脚本 m6i_upgrade_decide 同一套规则：只比构建时间；完全一样什么都不做；后台更新时不降级）。
    public static func classify(_ st: DaemonStatus, payload p: PayloadInfo) -> ServiceState {
        if let sf = st.stateFormat, sf != 0, Int(sf) != p.stateFormat { return .formatDiffers(old: Int(sf), new: p.stateFormat) }
        let new = p.label
        let b = st.build
        let known = b.map { matches(commitRe, $0.commit) && matches(timeRe, $0.buildTime) } ?? false
        if known, let b {
            let old = "\(b.commit)（构建于 \(b.buildTime)）"
            if b.commit == p.commit && b.buildTime == p.buildTime { return .upToDate }
            if p.buildTime <= b.buildTime { return .daemonNewer(old: old, new: new) }
            if let why = blocked(st) { return .upgradeBlocked(reason: why) }
            return .upgradeAvailable(old: old, new: new)
        }
        if let why = blocked(st) { return .upgradeBlocked(reason: why) }
        return .unknownVersion(new: new)
    }

    static func blocked(_ st: DaemonStatus) -> String? {
        if st.corrupt == true { return L10n.serviceBlockedCorrupt }
        if st.uninstall?.running == true || st.uninstall?.done == true { return L10n.serviceBlockedUninstall }
        switch st.state {
        case "ENABLED": return L10n.serviceBlockedEnabled
        case "ENABLING", "RESTORING": return L10n.serviceBlockedTransition
        default: break
        }
        if st.borrow != nil { return L10n.serviceBlockedBorrow }
        if st.busy == true { return L10n.serviceBlockedBusy }
        return nil
    }

    /// 显示给用户的一句话。
    public var text: String {
        switch state {
        case .checking: return ""
        case .noPayload: return L10n.serviceNoPayload
        case .notInstalled: return L10n.serviceNotInstalled
        case .upToDate: return L10n.serviceUpToDate
        case let .upgradeAvailable(old, new): return L10n.serviceUpgradeAvailable(old, new)
        case let .unknownVersion(new): return L10n.serviceUnknownVersion(new)
        case let .formatDiffers(old, new): return L10n.serviceFormatDiffers(old, new)
        case let .daemonNewer(old, new): return L10n.serviceDaemonNewer(old, new)
        case let .upgradeBlocked(reason): return reason
        }
    }

    public var showsInstall: Bool { state == .notInstalled && payload != nil }
    public var showsUpgrade: Bool {
        switch state {
        case .upgradeAvailable, .unknownVersion, .upgradeBlocked: return true
        default: return false
        }
    }
    /// 升级按钮能不能点（不允许升级的几种状态显示但不能点）。
    public var upgradeEnabled: Bool {
        switch state {
        case .upgradeAvailable, .unknownVersion: return true
        default: return false
        }
    }
    /// 卸载入口：后台服务在（连得上）时才有。
    public var showsUninstall: Bool {
        switch state {
        case .checking, .notInstalled: return false
        default: return true
        }
    }

    // ---- 只有按钮能调用（弹确认框，不执行；执行要在确认框里再点“执行”）----

    public func userTappedInstall(host: PrivilegedHost = .main) async {
        guard showsInstall, let p = payload, let s = privileged else { return }
        await s.presentService(.install, payload: p.path, lines: [L10n.serviceInstallWhat, L10n.serviceBoundary], host: host,
                               terminal: TerminalPrompt(commands: ["sudo scripts/m6/install/install.sh"]), onTerminal: { _ in },
                               onDone: { [weak self] in await self?.done() })
    }

    public func userTappedUpgrade(host: PrivilegedHost = .main) async {
        guard upgradeEnabled, let p = payload, let s = privileged else { return }
        var lines = [L10n.serviceUpgradeKeeps, L10n.serviceUpgradeStops, L10n.serviceBoundary]
        var unknown = false
        if case .unknownVersion = state { unknown = true; lines.insert(L10n.serviceUnknownWarning, at: 0) }
        if case let .upgradeAvailable(old, new) = state { lines.insert(L10n.serviceUpgradeAvailable(old, new), at: 0) }
        await s.presentService(.upgrade(acceptUnknown: unknown), payload: p.path, lines: lines, host: host,
                               terminal: TerminalPrompt(commands: ["sudo scripts/m6/install/install.sh --upgrade"]), onTerminal: { _ in },
                               onDone: { [weak self] in await self?.done() })
    }

    public func userTappedUninstall(host: PrivilegedHost = .settings) async {
        guard showsUninstall, let s = privileged else { return }
        await s.presentService(.uninstall(killBrowsers: false), payload: nil, lines: [L10n.serviceUninstallWhat], host: host,
                               terminal: TerminalPrompt(commands: ["sudo scripts/m6/install/uninstall.sh"]), onTerminal: { _ in },
                               onDone: { [weak self] in await self?.done() })
    }

    private func done() async {
        await refresh()
        await afterChange?()
    }
}
