// 状态的视图模型：菜单栏和首页共用。不创建窗口、不依赖 SwiftUI，可以直接单测（m5-plan v2.5 决定 4）。
// 只用 ObservableObject（命令行工具里没有 @Observable 需要的宏插件）。
import AbroadAPI
import Combine
import Foundation

/// 五种状态（design 11.2：图标颜色表示状态）。
public enum Phase: Sendable, Equatable {
    case abroad // 已启用、闸门开
    case offline // 断网：已启用但闸门关，或停用后保护还在
    case home // 已回国
    case checking // 正在启用、回国，或正在执行操作
    case error // 出错（状态文件损坏、后台不可达）

    public var title: String {
        switch self {
        case .abroad: return L10n.phaseAbroad
        case .offline: return L10n.phaseOffline
        case .home: return L10n.phaseHome
        case .checking: return L10n.phaseChecking
        case .error: return L10n.phaseError
        }
    }

    /// 菜单栏图标（SF Symbols 名字）。
    public var symbol: String {
        switch self {
        case .abroad: return "airplane.circle.fill"
        case .offline: return "wifi.slash"
        case .home: return "house.circle"
        case .checking: return "arrow.triangle.2.circlepath.circle"
        case .error: return "exclamationmark.triangle.fill"
        }
    }

    /// 由 abroadd 的状态推出（state：RESTORED、ENABLING、ENABLED、DISABLED、RESTORING；design 11.2 v0.27）：
    /// - 已回国：只有 RESTORED；
    /// - 已出国：ENABLED、闸门开、保护有效；
    /// - 断网：ENABLED 闸门关、或 DISABLED，且保护有效；
    /// - 检查中：ENABLING、RESTORING；
    /// - 出错：状态文件损坏；ENABLED 或 DISABLED 而保护失效（接管还在、保护没了，最需要提醒）；不认识的状态；后台不可达。
    public static func from(_ st: DaemonStatus) -> Phase {
        if st.corrupt == true { return .error }
        switch st.state {
        case "RESTORED": return .home
        case "ENABLING", "RESTORING": return .checking
        case "ENABLED", "DISABLED":
            if protectionLost(st) { return .error }
            return st.state == "ENABLED" && st.gateOpen ? .abroad : .offline
        default: return .error
        }
    }

    /// 接管还在（ENABLED、DISABLED）而断网保护没有生效。
    public static func protectionLost(_ st: DaemonStatus) -> Bool {
        (st.state == "ENABLED" || st.state == "DISABLED") && !st.protected
    }
}

/// 菜单各项是否可点（只是体验：真正的拒绝靠后台的级别检查）。
public struct MenuAvailability: Sendable, Equatable {
    public var enable: Bool
    public var disable: Bool
    public var restore: Bool

    /// stalled：状态停在 ENABLING 或 RESTORING、而没有操作在进行（上一次操作没有做完）时给出那个状态。
    /// 后台的设计是“再点一次启用或恢复就能接着做”，所以这时启用中给启用和回国、恢复中给回国。
    public static func from(_ phase: Phase, reachable: Bool, busy: Bool, stalled: String? = nil) -> MenuAvailability {
        if !reachable || busy { return MenuAvailability(enable: false, disable: false, restore: false) }
        switch stalled {
        case "ENABLING": return MenuAvailability(enable: true, disable: false, restore: true)
        case "RESTORING": return MenuAvailability(enable: false, disable: false, restore: true)
        default: break
        }
        if phase == .checking { return MenuAvailability(enable: false, disable: false, restore: false) }
        switch phase {
        case .abroad: return MenuAvailability(enable: false, disable: true, restore: true)
        case .offline: return MenuAvailability(enable: true, disable: true, restore: true)
        case .home: return MenuAvailability(enable: true, disable: false, restore: false)
        case .error: return MenuAvailability(enable: false, disable: false, restore: true)
        case .checking: return MenuAvailability(enable: false, disable: false, restore: false)
        }
    }
}

/// 界面上要显示的一条消息（错误按三段原样显示）。
public struct Notice: Sendable, Equatable {
    public enum Kind: Sendable, Equatable { case error, info }
    public var kind: Kind
    public var text: ErrorText
}

/// 主代理出口（只从专用接口取，不进状态接口；界面上不放“复制全部状态”一类入口）。
public struct ExitInfo: Sendable, Equatable {
    public var available: Bool
    public var ip: String
    public var place: String
    public var timezone: String
    public var note: String

    static func from(_ v: DaemonMainExitView) -> ExitInfo {
        let place = [v.country, v.region, v.city].compactMap { $0 }.filter { !$0.isEmpty }.joined(separator: " ")
        return ExitInfo(available: v.available, ip: v.ip ?? "", place: place, timezone: v.timezone ?? "", note: v.note ?? L10n.mainExitNone)
    }
}

@MainActor
public final class StatusModel: ObservableObject {
    @Published public private(set) var phase: Phase = .checking
    @Published public private(set) var status: DaemonStatus?
    @Published public private(set) var reachable = false
    @Published public private(set) var connection: ConnectionError?
    @Published public private(set) var busy = false
    @Published public private(set) var exitInfo: ExitInfo?
    @Published public var pendingConfirm: ConfirmPrompt?
    @Published public var terminalCommand: String?
    @Published public var notice: Notice?

    public let client: APIClient
    public let authorizer: Authorizer
    public let sleeper: Sleeper
    private var loop: Task<Void, Never>?
    private var backoff = ReconnectBackoff()
    /// 等确认的那个操作（确认后照着它走授权等后续）。
    private var confirmOp: SensitiveOperation?

    public init(client: APIClient, authorizer: Authorizer = TerminalAuthorizer(), sleeper: Sleeper = TaskSleeper()) {
        self.client = client
        self.authorizer = authorizer
        self.sleeper = sleeper
    }

    public var menu: MenuAvailability { MenuAvailability.from(phase, reachable: reachable, busy: busy || daemonBusy, stalled: stalledState) }

    /// 后台回过“忙”（有别的操作在进行）时的状态；状态变了就清掉。
    @Published public private(set) var daemonBusy = false
    private var busyAtState: String?

    /// 状态停在 ENABLING 或 RESTORING、本应用没有操作在进行、后台也没说忙：上一次操作没有做完。
    public var stalledState: String? {
        guard reachable, !busy, !daemonBusy, let s = status?.state, s == "ENABLING" || s == "RESTORING" else { return nil }
        return s
    }

    /// 待还原的项（网络已恢复但时区或语言没还原；首页和地区页都显示）。
    public var pendingRestore: [String] { status?.region?.pending ?? [] }

    /// 停在中间状态时的说明。
    public var stalledNote: String? { stalledState == nil ? nil : L10n.interrupted }

    /// 签名校验未启用的说明（status 的 security）；启用了校验时为空。
    public var securityNote: String? {
        guard let s = status?.security, s.callerVerification == "disabled" else { return nil }
        return s.note
    }

    /// 保护失效时的固定说明，加上后台巡检事件里最近一条的说明（处理办法以后台的为准，界面不改写）。
    public var protectionNote: String? {
        guard reachable, let st = status, Phase.protectionLost(st) else { return nil }
        let detail = st.events?.last(where: { $0.source == "patrol" && !($0.detail ?? "").isEmpty })?.detail ?? ""
        return detail.isEmpty ? L10n.protectionLost : L10n.protectionLost + "（" + detail + "）"
    }

    /// 后台不可达时的说明。
    public var connectionText: String? {
        guard !reachable, let c = connection else { return nil }
        switch c {
        case .notRunning, .timeout, .protocolError: return L10n.backendDown
        case .noListener: return L10n.backendNoListener
        case .rejected: return L10n.backendRejected
        }
    }

    // ---- 刷新与事件 ----

    /// 读一次状态和出口信息。
    @discardableResult
    public func refresh() async -> Bool {
        switch await client.status() {
        case .ok(let st):
            apply(st)
            if case .ok(let e) = await client.mainExit() { exitInfo = ExitInfo.from(e) }
            return true
        case .unreachable(let c):
            setUnreachable(c)
            return false
        case let .failed(_, t), let .busy(t), let .invalid(t), let .unsupported(t), let .needsAuthorization(t):
            notice = Notice(kind: .error, text: t)
            return true
        case .needsConfirm:
            return true
        }
    }

    /// 开始：读状态、订阅事件；断线后按退避重连（间隔有上限），恢复后自动回来。
    public func start() {
        guard loop == nil else { return }
        loop = Task { [weak self] in
            while !Task.isCancelled {
                guard let self else { return }
                await self.runOnce()
                if Task.isCancelled { return }
                let d = self.backoff.next()
                do { try await self.sleeper.sleep(seconds: d) } catch { return }
            }
        }
    }

    public func stop() {
        loop?.cancel()
        loop = nil
    }

    /// 一轮：读状态，成功后订阅事件直到断开。事件到达时重新读状态（事件只说“变了”，内容以状态为准）。
    func runOnce() async {
        guard await refresh(), reachable else { return }
        backoff.reset()
        let transport = client.transport
        do {
            try await transport.events(path: "/v1/events") { [weak self] _ in
                Task { @MainActor [weak self] in _ = await self?.refresh() }
            }
            // 后台断开了：下一轮重连。
            setUnreachable(.noListener)
        } catch let c as ConnectionError {
            setUnreachable(c)
        } catch is CancellationError {
        } catch {
            setUnreachable(.protocolError("事件流出错"))
        }
    }

    /// 事件到达（测试和事件流共用）。
    public func handleEvent(_ ev: SSEEvent) async {
        await refresh()
    }

    func apply(_ st: DaemonStatus) {
        if daemonBusy && st.state != busyAtState { daemonBusy = false; busyAtState = nil }
        status = st
        reachable = true
        connection = nil
        phase = Phase.from(st)
    }

    func setUnreachable(_ c: ConnectionError) {
        reachable = false
        connection = c
        phase = .error
    }

    // ---- 操作 ----

    public func enable() async { await perform(.enable) { await self.client.enable() } }
    public func disable() async { await perform(nil) { await self.client.disable() } }
    public func restore() async { await perform(.restore) { await self.client.restore() } }

    /// 用户在确认框里点了确认 / 取消。
    public func answerConfirm(_ accept: Bool) async {
        guard let p = pendingConfirm else { return }
        pendingConfirm = nil
        let op = confirmOp
        confirmOp = nil
        guard accept else { p.discard(); return }
        await perform(op) { await self.client.confirm(p, as: DaemonStatus.self) }
    }

    /// 关掉终端退路的提示后刷新一次（用户可能刚在终端里执行了命令）。
    public func dismissTerminal() {
        terminalCommand = nil
        Task { await refresh() }
    }

    private func perform(_ op: SensitiveOperation?, _ call: @escaping () async -> Outcome<DaemonStatus>) async {
        guard !busy else { return }
        busy = true
        let before = phase
        phase = .checking
        defer { busy = false }
        let out = await call()
        switch out {
        case .ok(let st):
            apply(st)
            notice = nil
        case .needsConfirm(let p):
            phase = before
            confirmOp = op
            pendingConfirm = p
        case .needsAuthorization(let t):
            phase = before
            guard let op else { notice = Notice(kind: .error, text: t); return }
            switch await authorizer.authorize(op, reason: t) {
            case .runInTerminal(let cmd): terminalCommand = cmd
            case .authorized: busy = false; await perform(nil, call) // 系统授权通过后重新发（M5-3）
            case .cancelled: break
            }
        case .busy(let t):
            phase = before
            daemonBusy = t.code == .busy
            busyAtState = status?.state
            notice = Notice(kind: .error, text: t.raw.isEmpty ? ErrorText(raw: L10n.busy) : t)
        case let .invalid(t), let .failed(_, t):
            phase = before
            notice = Notice(kind: .error, text: t)
        case .unsupported(let t):
            phase = before
            notice = Notice(kind: .error, text: ErrorText(raw: L10n.unsupported + "：" + t.raw))
        case .unreachable(let c):
            setUnreachable(c)
        }
        if case .ok = out {} else { await refresh() }
    }
}
