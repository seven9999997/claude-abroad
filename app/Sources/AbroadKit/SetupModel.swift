// 安装引导的视图模型（m5-plan v2.6 决定 1）。四步按原型：欢迎与说明、后台服务、配置主代理、完成。
// “能不能下一步”全部来自后台的进度接口（GET /v1/setup）；界面不自己判断主代理是否已经应用。
import AbroadAPI
import Combine
import Foundation

public enum SetupStepID: Int, Sendable, CaseIterable, Comparable {
    case welcome, service, mainProxy, done
    public static func < (a: SetupStepID, b: SetupStepID) -> Bool { a.rawValue < b.rawValue }

    public var title: String {
        switch self {
        case .welcome: return L10n.setupWelcome
        case .service: return L10n.setupService
        case .mainProxy: return L10n.setupMainProxy
        case .done: return L10n.setupDone
        }
    }
}

@MainActor
public final class SetupModel: ObservableObject {
    @Published public private(set) var step: SetupStepID = .welcome
    @Published public private(set) var progress: DaemonSetupView?
    @Published public private(set) var reachable = false
    @Published public private(set) var connection: ConnectionError?
    @Published public private(set) var notice: Notice?
    @Published public private(set) var busy = false

    public let client: APIClient
    public let sleeper: Sleeper
    private var backoff = ReconnectBackoff()
    private var waitTask: Task<Void, Never>?

    public init(client: APIClient, sleeper: Sleeper = TaskSleeper()) {
        self.client = client
        self.sleeper = sleeper
    }

    /// 后台说引导已经完成：不再进入引导。
    public var completed: Bool { progress?.completed == true }
    /// 要不要显示引导（后台不可达时不知道，先显示，停在“后台服务”一步）。
    public var shouldShow: Bool { !completed }

    func done(_ id: String) -> Bool { progress?.steps?.first(where: { $0.id == id })?.done == true }

    /// 当前一步能不能点“下一步”（完全由后台的进度决定）。
    public var canAdvance: Bool {
        switch step {
        case .welcome: return true
        case .service: return reachable && done("service")
        case .mainProxy: return reachable && done("main_proxy")
        case .done: return false
        }
    }

    /// “后台服务”一步要用户执行的命令（付费项推迟期间的临时做法：手动加载）。
    public var serviceCommand: String { L10n.setupServiceCommand }

    /// 读一次进度。
    @discardableResult
    public func refresh() async -> Bool {
        switch await client.setup() {
        case .ok(let v):
            progress = v
            reachable = true
            connection = nil
            if v.completed { step = .done }
            return true
        case .unreachable(let c):
            reachable = false
            connection = c
            return false
        case let .failed(_, t), let .busy(t), let .invalid(t), let .unsupported(t), let .needsAuthorization(t):
            notice = Notice(kind: .error, text: t)
            return true
        case .needsConfirm:
            return true
        }
    }

    /// 下一步。离开“欢迎”进入“后台服务”时开始等后台：连上后自动前进到“配置主代理”。
    public func next() async {
        guard canAdvance else { return }
        switch step {
        case .welcome:
            step = .service
            await refresh()
            if canAdvance { step = .mainProxy } else { startWaiting() }
        case .service:
            step = .mainProxy
        case .mainProxy:
            await complete()
        case .done:
            break
        }
    }

    /// 在“配置主代理”一步里，用户在主代理页面应用之后回来时调用：重新读进度。
    public func recheck() async { await refresh() }

    /// 完成：交给后台；后台拒绝（例如还没有出口基准）时停在第三步并原样显示原因。
    public func complete() async {
        guard !busy else { return }
        busy = true
        defer { busy = false }
        switch await client.setupComplete() {
        case .ok(let v):
            progress = v
            notice = nil
            step = v.completed ? .done : .mainProxy
        case let .invalid(t), let .failed(_, t), let .busy(t), let .needsAuthorization(t), let .unsupported(t):
            notice = Notice(kind: .error, text: t)
            step = .mainProxy
        case .unreachable(let c):
            reachable = false
            connection = c
            step = .service
            startWaiting()
        case .needsConfirm:
            break
        }
    }

    /// 后台不可达时按退避重试，连上后从“后台服务”自动前进。
    func startWaiting() {
        guard waitTask == nil else { return }
        backoff.reset()
        waitTask = Task { [weak self] in
            while !Task.isCancelled {
                guard let self else { return }
                if await self.refresh(), self.reachable {
                    if self.step == .service && self.done("service") { self.step = .mainProxy }
                    self.waitTask = nil
                    return
                }
                let d = self.backoff.next()
                do { try await self.sleeper.sleep(seconds: d) } catch { return }
            }
        }
    }

    public func stop() {
        waitTask?.cancel()
        waitTask = nil
    }
}
