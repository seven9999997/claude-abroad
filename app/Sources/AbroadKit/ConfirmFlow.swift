// 统一的两步确认（m5-plan v2.9 决定 2）：回国、启用、应用主代理、切换地区共用这一个模型。
// - 内容全部来自后台 409 的预览（地区从什么变成什么、要结束的 Claude 进程、没能确认的项），界面不自己推断；
// - “确认”出现 3 秒后才可点（时钟可注入）；显示剩余有效时间，到期后失效、显示“已过期，请重新发起”，不自动重新申请；
// - 取消时丢弃确认码；带确认码的请求被回 confirm_invalid 时重新发起第一步、显示新的预览，要用户再点一次——
//   绝不自动带着新的确认码提交；
// - 同一时间只有一个确认框（各页面共用一个实例；有确认框时其他会触发确认的按钮不可用）。
// 带确认码的提交只在这里（client.confirm 的唯一调用处，源码检查固定）。
import AbroadAPI
import Combine
import Foundation

/// 时钟（测试注入，不真等）。
public protocol NowProvider: Sendable {
    func now() -> Date
}

public struct SystemNow: NowProvider {
    public init() {}
    public func now() -> Date { Date() }
}

/// 重新发起第一步的结果：新的预览；后台这次不再要求确认、操作已经完成；或失败（发起方已经显示了原因）。
public enum RefetchResult: Sendable {
    case prompt(ConfirmPrompt)
    case done
    case failed
}

public enum ConfirmStep: Sendable, Equatable {
    case none
    case waiting // 3 秒内
    case ready
    case expired
}

@MainActor
public final class ConfirmFlow: ObservableObject {
    @Published public private(set) var prompt: ConfirmPrompt?
    @Published public private(set) var shownAt: Date?
    /// 确认码失效后重新取过预览（界面提示“内容有变化，请再确认一次”）。
    @Published public private(set) var refreshed = false
    /// 已过期（确认码作废，只能关掉重新发起）。
    @Published public private(set) var expired = false
    @Published public private(set) var submitting = false
    /// 回国时“同时强制退出浏览器”（M6-4b-3）：默认不勾，每次弹出新的确认框都复位；只有预览里有浏览器时界面才显示。
    @Published public var killBrowsers = false
    /// 刷新界面用的“现在”（视图每秒调一次 tick）。
    @Published public private(set) var now: Date
    /// 重新发起第一步失败时的原因。
    @Published public private(set) var notice: ErrorText?
    /// 重新发起第一步时后台不再要求确认、操作已经完成：给一句说明（确认框关掉后由首页显示）。
    @Published public private(set) var completedNote: String?

    public static let delay: TimeInterval = 3
    public let client: APIClient
    public let clock: NowProvider

    /// 提交：带确认码发请求，返回 true 表示确认码无效（要重新取预览）；否则发起方已经处理完结果。
    private var submitter: ((ConfirmPrompt) async -> Bool)?
    /// 重新发起第一步，取新的预览（后台这次若不再要求确认，发起方照常处理结果，返回 nil）。
    private var refetcher: (() async -> RefetchResult)?
    /// 每次显示新的确认框加一；await 之后核对还是不是当初那一个。
    private var generation = 0

    public init(client: APIClient, clock: NowProvider = SystemNow()) {
        self.client = client
        self.clock = clock
        now = clock.now()
    }

    /// 有确认框时为真（其他会触发确认的按钮不可用）。
    public var isActive: Bool { prompt != nil }

    public func tick() {
        now = clock.now()
        if let p = prompt, !expired, remaining(at: now) <= 0 {
            expired = true
            p.discard() // 到期：确认码作废，不自动重新申请
        }
    }

    /// 剩余有效秒数。
    public func remaining(at t: Date? = nil) -> Int {
        guard let p = prompt, let s = shownAt else { return 0 }
        return max(0, Int(ceil(Double(p.expiresIn) - (t ?? clock.now()).timeIntervalSince(s))))
    }

    public func step(at t: Date? = nil) -> ConfirmStep {
        guard prompt != nil, let s = shownAt else { return .none }
        let t = t ?? clock.now()
        if expired || remaining(at: t) <= 0 { return .expired }
        return t.timeIntervalSince(s) < Self.delay ? .waiting : .ready
    }

    /// 发起方收到 409 时调用。T 是带确认码那次请求的应答类型；onResult 处理除“确认码无效”以外的全部结果。
    public func present<T: Decodable & Sendable>(_ p: ConfirmPrompt, as: T.Type,
                                                 refetch: @escaping () async -> RefetchResult,
                                                 onResult: @escaping (Outcome<T>) async -> Void) -> Bool {
        guard prompt == nil else { p.discard(); return false } // 同一时间只有一个确认框
        let client = self.client
        submitter = { [weak self] p in
            let extra: [String: Bool] = (self?.killBrowsers ?? false) ? ["kill_browsers": true] : [:]
            let o = await client.confirm(p, as: T.self, extra: extra)
            if case .invalid(let t) = o, t.code == .confirmInvalid { return true }
            await onResult(o)
            return false
        }
        refetcher = refetch
        show(p, refreshed: false)
        return true
    }

    private func show(_ p: ConfirmPrompt, refreshed r: Bool) {
        generation += 1
        if !r { completedNote = nil; killBrowsers = false }
        prompt = p
        shownAt = clock.now()
        now = clock.now()
        expired = false
        refreshed = r
        notice = nil
    }

    /// 用户点了确认。3 秒内、过期、已在提交中都不发任何请求。
    /// 提交中（从发出带确认码的请求，一直到重新取预览结束）不可取消、不可重入；每个 await 之后核对确认框还是不是原来那个。
    public func confirm() async {
        tick()
        guard let p = prompt, step() == .ready, !submitting, let submit = submitter else { return }
        let gen = generation
        let refetch = refetcher
        submitting = true
        defer { if generation == gen { submitting = false } }
        let invalid = await submit(p)
        guard generation == gen else { return }
        guard invalid else { finish(); return }
        // 确认码无效：重新发起第一步取新的预览，显示出来，要用户再点一次。
        let r = await refetch?() ?? .failed
        guard generation == gen else { return }
        switch r {
        case .prompt(let np):
            submitting = false
            show(np, refreshed: true)
        case .done:
            finish()
            completedNote = L10n.confirmNotNeededDone // 这次不需要确认，已完成
        case .failed:
            finish()
        }
    }

    /// 用户点了取消（或关掉确认框）：丢弃确认码，后台没有任何改动。提交中不做任何事（请求已经发出，按结果收尾）。
    public func cancel() {
        guard !submitting else { return }
        prompt?.discard()
        close()
    }

    /// 关掉“这次不需要确认，已完成”的说明。
    public func dismissCompletedNote() { completedNote = nil }

    private func finish() {
        submitting = false
        close()
    }

    private func close() {
        generation += 1
        prompt = nil
        shownAt = nil
        submitter = nil
        refetcher = nil
        expired = false
        refreshed = false
    }
}
