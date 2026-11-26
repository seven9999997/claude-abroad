// M5-10 验收 a–i：统一的确认模型（假时钟）和通知（假的发送器、脚本化的事件流）。不建窗口、不调系统通知中心。
import AbroadAPI
import Foundation
import Testing
@testable import AbroadKit

/// 一个会要两步确认的假后台（回国）。confirmInvalid 为真时第一次带确认码的请求回 confirm_invalid；
/// previews 依次给出每次第一步的预览。
final class ConfirmBackend: @unchecked Sendable {
    var confirmInvalidOnce = false
    var previews: [String] = [confirmJSON]
    var firstCalls = 0
    var confirmCalls = 0
    var afterConfirm: HTTPResponse = json(200, statusJSON(state: "RESTORED", gate: false, protected: false))

    func handle(_ r: HTTPRequest) -> HTTPResponse {
        switch r.path {
        case "/v1/status": return json(200, statusJSON(state: "ENABLED", gate: true))
        case "/v1/main-proxy/exit": return json(200, "{\"available\":false}")
        case "/v1/restore":
            if String(decoding: r.body ?? Data(), as: UTF8.self).contains("confirm") {
                confirmCalls += 1
                if confirmInvalidOnce {
                    confirmInvalidOnce = false
                    return errBody(403, "confirm_invalid", "恢复本机网络失败：确认码无效、已过期或已经用过。本机网络没有改动。请重新发起恢复，按新的预览确认。")
                }
                return afterConfirm
            }
            let p = previews[min(firstCalls, previews.count - 1)]
            firstCalls += 1
            return json(409, p)
        default: return json(404, "{}")
        }
    }
}

@MainActor
func confirmRig(_ b: ConfirmBackend, notifier: Notifier? = nil) -> (StatusModel, ConfirmFlow, FakeNow, FakeTransport) {
    let t = FakeTransport { b.handle($0) }
    let c = APIClient(transport: t)
    let clock = FakeNow()
    let flow = ConfirmFlow(client: c, clock: clock)
    return (StatusModel(client: c, sleeper: NoSleep(), flow: flow, notifier: notifier), flow, clock, t)
}

// ---- a：3 秒、过期、取消、只有一个确认框 ----

@MainActor
@Test func confirmDelayExpiryCancel() async {
    let b = ConfirmBackend()
    let (m, flow, clock, _) = confirmRig(b)
    await m.refresh()
    await m.restore()
    #expect(flow.isActive && flow.step() == .waiting)
    await flow.confirm() // 3 秒内：无效
    clock.advance(2.9)
    await flow.confirm()
    #expect(b.confirmCalls == 0 && flow.isActive)
    clock.advance(0.2)
    #expect(flow.step() == .ready && flow.remaining() == 57)
    await flow.confirm()
    #expect(b.confirmCalls == 1 && flow.isActive == false && m.phase == .home)
    // 过期：确认无效、显示过期、不发请求
    await m.restore()
    clock.advance(61)
    flow.tick()
    #expect(flow.step() == .expired && flow.expired && flow.prompt?.isUsed == true)
    await flow.confirm()
    #expect(b.confirmCalls == 1, "过期后不发任何请求")
    flow.cancel()
    // 取消：不发带确认码的请求
    await m.restore()
    clock.advance(5)
    flow.cancel()
    #expect(b.confirmCalls == 1 && flow.isActive == false)
    // 同时只有一个确认框：有框时再发起不会再弹（第二个预览的确认码作废）
    await m.restore()
    let first = flow.prompt
    let p2 = ConfirmPrompt.testMake(confirmJSON)
    #expect(flow.present(p2, as: Empty.self, refetch: { .failed }, onResult: { _ in }) == false)
    #expect(flow.prompt === first && p2.isUsed)
    let before = b.firstCalls
    await m.restore() // 有确认框时，其他会触发确认的按钮不可用（不发请求）
    #expect(b.firstCalls == before && m.menu.restore == false)
    flow.cancel()
}

// ---- b：确认码无效 → 重新取预览、显示新的内容、要再点一次 ----

@MainActor
@Test func confirmInvalidRefetches() async {
    let b = ConfirmBackend()
    b.confirmInvalidOnce = true
    let newPreview = confirmJSON.replacingOccurrences(of: "\"pid\":7100", with: "\"pid\":7999")
    b.previews = [confirmJSON, newPreview]
    let (m, flow, clock, _) = confirmRig(b)
    await m.refresh()
    await m.restore()
    #expect(flow.prompt?.preview.processes?.first?.pid == 7100)
    await confirmNow(flow)
    #expect(b.confirmCalls == 1 && b.firstCalls == 2, "确认码无效后重新发起了第一步")
    #expect(flow.isActive && flow.refreshed && flow.prompt?.preview.processes?.first?.pid == 7999, "显示的是新的预览")
    #expect(flow.step() == .waiting, "要用户再点一次（又是 3 秒）")
    clock.advance(10)
    #expect(b.confirmCalls == 1, "用户再次确认之前，带确认码的请求次数不变")
    await flow.confirm()
    #expect(b.confirmCalls == 2 && flow.isActive == false && m.phase == .home)
}

// ---- d：确认之后被要求本人授权 ----

@MainActor
@Test func confirmThenAuthorization() async {
    let b = ConfirmBackend()
    b.afterConfirm = errBody(403, "needs_authorization", "需要本人授权：这是敏感操作")
    let (m, flow, _, t) = confirmRig(b)
    await m.refresh()
    await m.restore()
    await confirmNow(flow)
    #expect(m.terminalCommand == "sudo abroad restore" && flow.isActive == false)
    let n = t.requests.filter { $0.path == "/v1/restore" }.count
    #expect(n == 2, "第一步和带确认码的一次，之后界面不再发请求")
    m.dismissTerminal()
    #expect(await eventually { t.requests.filter { $0.path == "/v1/status" }.count >= 2 }, "关掉终端提示后重新读状态")
}

// ---- c：四种操作都走同一个确认模型；源码里带确认码的提交只在 ConfirmFlow ----

@MainActor
@Test func allFourUseTheFlow() async throws {
    // 启用、回国（StatusModel）
    let b = ConfirmBackend()
    let (m, flow, _, _) = confirmRig(b)
    await m.restore()
    #expect(flow.isActive)
    flow.cancel()
    // 应用主代理（MainProxyModel）
    let f = FakeMain()
    f.applyResponse = json(409, confirmJSON)
    let tm = FakeTransport { f.handle($0) }
    let cm = APIClient(transport: tm)
    let mflow = fakeFlow(cm)
    let mm = MainProxyModel(client: cm, flow: mflow)
    await mm.refresh()
    mm.selectedExit = "n-exit"
    await mm.apply()
    #expect(mflow.isActive && mflow.prompt?.preview.processes?.count == 1 && mflow.prompt?.region != nil)
    mflow.cancel()
    // 切换地区（RegionModel）
    let tr = FakeTransport { r in r.method == "PUT" ? json(409, confirmJSON) : json(200, "{}") }
    let cr = APIClient(transport: tr)
    let rflow = fakeFlow(cr)
    let rm = RegionModel(client: cr, flow: rflow)
    rm.formTimezone = "Europe/Paris"
    rm.formLang = "fr-FR"
    await rm.setManual()
    #expect(rflow.isActive)
    rflow.cancel()
    // 源码检查：client.confirm 只在 ConfirmFlow.swift 里调用。
    let dir = repoRoot.appendingPathComponent("app/Sources")
    let e = FileManager.default.enumerator(at: dir, includingPropertiesForKeys: nil)
    while let u = e?.nextObject() as? URL {
        guard u.pathExtension == "swift", u.lastPathComponent != "ConfirmFlow.swift", u.lastPathComponent != "APIClient.swift" else { continue }
        let src = try String(contentsOf: u, encoding: .utf8)
        #expect((src.contains(".confirm(") && !src.contains("flow.confirm(")) == false, "\(u.lastPathComponent) 直接提交了确认码")
        #expect((src.contains("client.confirm(")) == false, "\(u.lastPathComponent) 直接提交了确认码")
    }
}

// ---- e、f：通知的触发和去重 ----

final class FakeSender: NotificationSender, @unchecked Sendable {
    var got: [AppNotification] = []
    func send(_ n: AppNotification) { got.append(n) }
}

final class FakeOpener: ClaudeOpener, @unchecked Sendable {
    var calls = 0
    func openClaude() { calls += 1 }
}

func ev(_ seq: Int, _ source: String, _ kind: String, _ detail: String = "") -> SSEEvent {
    SSEEvent(id: "\(seq)", event: source, data: "{\"seq\":\(seq),\"source\":\"\(source)\",\"kind\":\"\(kind)\",\"time\":\"2026-11-22T00:00:00Z\",\"detail\":\"\(detail)\"}")
}

func st(gate: Bool, env: String? = nil, reasons: [String] = []) -> DaemonStatus {
    var j = statusJSON(state: "ENABLED", gate: gate, reasons: reasons)
    if let env { j.removeLast(); j += ",\"envcheck\":{\"at\":\"2026-11-22T00:00:00Z\",\"hard\":\"\(env)\",\"items\":[]}}" }
    return decodeStatus(j)
}

@MainActor
@Test func notificationsTriggerAndDedupe() {
    let s = FakeSender()
    let o = FakeOpener()
    let pref = MemoryPreference()
    let n = Notifier(sender: s, opener: o, preference: pref)
    n.observe(status: st(gate: false, reasons: ["main_exit"]), reconnect: true) // 连上时的当前状态：只建立基线
    #expect(s.got.isEmpty)
    n.observe(event: ev(1, "gate", "gate_opened"))
    #expect(s.got.map(\.kind) == [.protectionRestored])
    // 保护断开（写原因）；同一原因的重复事件不重复；同一个 seq 只处理一次
    n.observe(event: ev(2, "gate", "gate_closed", "main_exit：出口检查不通过"))
    n.observe(event: ev(3, "gate", "gate_closed", "main_exit：出口检查不通过"))
    n.observe(event: ev(2, "gate", "gate_closed", "main_exit：出口检查不通过"))
    #expect(s.got.map(\.kind) == [.protectionRestored, .protectionLost])
    #expect(s.got[1].body.contains("出口检查不通过"))
    // 用户自己停用（原因 state）不算保护断开，之后的恢复也不通知
    n.observe(event: ev(4, "gate", "gate_opened"))
    n.observe(event: ev(5, "gate", "gate_closed", "state：停用"))
    n.observe(event: ev(6, "gate", "gate_opened"))
    #expect(s.got.map(\.kind) == [.protectionRestored, .protectionLost, .protectionRestored])
    // “无法判断”一类事件不通知
    n.observe(event: ev(7, "exits", "exit_down", "检查无法判断"))
    n.observe(event: ev(8, "baseline", "baseline_failed"))
    #expect(s.got.count == 3)
    // 重连后补发的当前状态不当作新事件
    n.observe(status: st(gate: false, reasons: ["main_exit"]), reconnect: true)
    #expect(s.got.count == 3)
    // 更换主代理失败（已回退 / 回退未完成）、回国完成（有待还原的项）
    n.switchFailed(.switchRolledBack)
    n.switchFailed(.switchRollbackIncomplete)
    n.switchFailed(.internal)
    n.restoreDone(pending: ["时区"])
    #expect(s.got.suffix(3).map(\.kind) == [.switchRolledBack, .switchRollbackIncomplete, .restoreDone])
    #expect(s.got.last?.body.contains("时区") == true)
    // 地区写入完成且环境检测通过：带“打开 Claude”按钮；没有点击时不打开
    n.observe(event: ev(9, "state", "region_synced", "时区 America/Los_Angeles"))
    n.observe(status: st(gate: true, env: "unknown"), reconnect: false)
    #expect(s.got.last?.kind != .regionReady, "环境检测没通过时不发")
    n.observe(status: st(gate: true, env: "pass"), reconnect: false)
    #expect(s.got.last?.kind == .regionReady && s.got.last?.offersOpenClaude == true)
    #expect(o.calls == 0, "没有点击时不打开 Claude")
    n.userTappedOpenClaude()
    #expect(o.calls == 1)
    // 六种都发过（保护断开、恢复、两种更换失败、回国完成、地区就绪；拦下见 launchBlockedNotifications；UDP、出口回退见 udpAndExitNotifications）
    #expect(Set(s.got.map(\.kind)) == Set(NotificationKind.allCases).subtracting([.launchBlocked, .integrityFailed, .udpUnavailable, .exitFallback]))
    // 开关关掉后不发
    let count = s.got.count
    pref.enabled = false
    n.restoreDone(pending: [])
    n.observe(event: ev(10, "gate", "gate_closed", "main_exit：x"))
    #expect(s.got.count == count)
}

/// M6-4b-1 返工：拦下 Claude 时发通知；文字按状态区分；短时间内的多条合成一条；补发的旧事件不通知；开关关掉不发。
@MainActor
@Test func launchBlockedNotifications() {
    let s = FakeSender()
    let pref = MemoryPreference()
    let n = Notifier(sender: s, preference: pref)
    let base = ISO8601DateFormatter().date(from: "2026-11-22T00:00:00Z")!
    var clock = base
    n.now = { clock }
    var restored = decodeStatus(statusJSON(state: "RESTORED", gate: false))
    n.observe(status: restored, reconnect: true)
    // 桌面版一次启动：好几个进程被拦，合成一条
    n.observe(event: ev(1, "launchguard", "blocked", "现在不允许启动 Claude，已结束：Claude 桌面版"))
    n.observe(event: ev(2, "launchguard", "blocked", "现在不允许启动 Claude，已结束：Claude 桌面版"))
    clock = base.addingTimeInterval(5)
    n.observe(event: ev(3, "launchguard", "blocked", "x"))
    #expect(s.got.map(\.kind) == [.launchBlocked])
    #expect(s.got[0].body == L10n.notifyLaunchBlockedRestored && s.got[0].title == L10n.notificationTitle(.launchBlocked))
    // 窗口过后再打开：又一条；启用中（环境检测没通过）用另一句
    clock = base.addingTimeInterval(20)
    restored = decodeStatus(statusJSON(state: "ENABLING", gate: false))
    n.observe(status: restored, reconnect: false)
    n.observe(event: SSEEvent(id: "4", event: "launchguard", data: "{\"seq\":4,\"source\":\"launchguard\",\"kind\":\"blocked\",\"time\":\"2026-11-22T00:00:19Z\",\"detail\":\"x\"}"))
    #expect(s.got.count == 2 && s.got[1].body == L10n.notifyLaunchBlockedEnv)
    // 补发的旧事件（事件时间早于 30 秒）不通知
    clock = base.addingTimeInterval(200)
    n.observe(event: ev(5, "launchguard", "blocked", "x"))
    #expect(s.got.count == 2)
    // 开关关掉不发
    pref.enabled = false
    clock = base.addingTimeInterval(400)
    n.observe(event: SSEEvent(id: "6", event: "launchguard", data: "{\"seq\":6,\"source\":\"launchguard\",\"kind\":\"blocked\",\"time\":\"2026-11-22T00:06:40Z\",\"detail\":\"x\"}"))
    #expect(s.got.count == 2)
}

/// 通知的内容里搜不到出口 IP、订阅地址、节点地址（只用后台的原因和集中的文字表）。
@MainActor
@Test func notificationsCarryNoAddresses() {
    let s = FakeSender()
    let n = Notifier(sender: s)
    n.observe(status: st(gate: true), reconnect: true)
    n.observe(event: ev(1, "gate", "gate_closed", "main_exit：出口在大陆"))
    n.observe(event: ev(2, "gate", "gate_opened"))
    n.switchFailed(.switchRolledBack)
    n.restoreDone(pending: ["语言"])
    n.observe(event: ev(3, "state", "region_synced"))
    n.observe(status: st(gate: true, env: "pass"), reconnect: false)
    let all = s.got.map { $0.title + $0.body }.joined()
    for mark in ["203.0.113.7", "sub.example.com", "SUBMARK", "8.8.4.20", "srvmark"] {
        #expect((all.contains(mark)) == false, "通知里出现了 \(mark)")
    }
    #expect(s.got.count == 5)
}

/// StatusModel 把事件和操作结果交给通知者：回国完成发通知，重连的当前状态不发。
@MainActor
@Test func statusModelFeedsNotifier() async {
    let s = FakeSender()
    let b = ConfirmBackend()
    let (m, flow, _, _) = confirmRig(b, notifier: Notifier(sender: s))
    await m.refresh()
    await m.restore()
    await confirmNow(flow)
    #expect(s.got.map(\.kind) == [.restoreDone])
    await m.handleEvent(ev(1, "gate", "gate_closed", "main_exit：x"))
    #expect(s.got.last?.kind == .protectionLost)
}

// ---- h：发送实现的选择 ----

@Test func notificationBackendChoice() {
    #expect(NotificationBackend.choose(bundleURL: URL(fileURLWithPath: "/Applications/Claude 出国.app"), bundleIdentifier: "com.claude-abroad.app") == .system)
    #expect(NotificationBackend.choose(bundleURL: URL(fileURLWithPath: "/tmp/build/debug"), bundleIdentifier: nil) == .null)
    #expect(NotificationBackend.choose(bundleURL: URL(fileURLWithPath: "/tmp/x.app"), bundleIdentifier: "") == .null)
    // 测试进程本身不是打包的应用：选空实现
    #expect(NotificationBackend.choose(bundleURL: Bundle.main.bundleURL, bundleIdentifier: Bundle.main.bundleIdentifier) == .null || Bundle.main.bundleURL.pathExtension == "app")
}

// ---- g、h、i：源码检查 ----

@Test func systemServicesConfined() throws {
    func files(_ sub: String) throws -> [(String, String)] {
        let dir = repoRoot.appendingPathComponent(sub)
        return try FileManager.default.contentsOfDirectory(atPath: dir.path).filter { $0.hasSuffix(".swift") }.map { ($0, try String(contentsOf: dir.appendingPathComponent($0), encoding: .utf8)) }
    }
    for (name, src) in try files("app/Sources/ClaudeAbroad") + files("app/Sources/AbroadKit") {
        let inServices = name == "SystemServices.swift"
        for api in ["openApplication(", "NSWorkspace.shared.open(", "UNUserNotificationCenter", "requestAuthorization"] {
            #expect(src.contains(api) == false || inServices, "\(name) 里出现了 \(api)：只允许在 SystemServices.swift")
        }
        let defaults = src.components(separatedBy: "UserDefaults").count - 1
        #expect(defaults == 0 || name == "Notifications.swift", "\(name) 用了 UserDefaults")
    }
    // 通知开关：键名固定、值只是布尔
    let n = try String(contentsOf: repoRoot.appendingPathComponent("app/Sources/AbroadKit/Notifications.swift"), encoding: .utf8)
    #expect(n.contains("public static let key = \"notificationsEnabled\""))
    #expect(n.contains("UserDefaults.standard.object(forKey: Self.key) as? Bool") && n.contains("UserDefaults.standard.set(newValue, forKey: Self.key)"))
    #expect(n.components(separatedBy: "UserDefaults.standard").count - 1 == 2)
}

extension ConfirmPrompt {
    static func testMake(_ j: String) -> ConfirmPrompt {
        let c = try! JSONDecoder().decode(DaemonConfirmNeeded.self, from: Data(j.utf8))
        return ConfirmPrompt(needed: c, request: HTTPRequest(method: "POST", path: "/v1/restore"))
    }
}

// ---- M5-10 复审必须改 1：提交中不可取消、不可重入；await 之后核对确认框 ----

/// 能把指定请求挂住的传输层（测试提交、重新取预览进行中的情况）。
final class HoldTransport: Transport, @unchecked Sendable {
    let inner: FakeTransport
    private let lock = NSLock()
    private var holdIf: @Sendable (HTTPRequest) -> Bool = { _ in false }
    private var waiters: [CheckedContinuation<Void, Never>] = []
    init(_ inner: FakeTransport) { self.inner = inner }
    func hold(_ f: @escaping @Sendable (HTTPRequest) -> Bool) { lock.lock(); holdIf = f; lock.unlock() }
    var held: Int { lock.lock(); defer { lock.unlock() }; return waiters.count }
    func release() {
        lock.lock(); let ws = waiters; waiters = []; holdIf = { _ in false }; lock.unlock()
        ws.forEach { $0.resume() }
    }
    func send(_ r: HTTPRequest) async throws -> HTTPResponse {
        lock.lock(); let h = holdIf(r); lock.unlock()
        if h { await withCheckedContinuation { c in lock.lock(); waiters.append(c); lock.unlock() } }
        return try await inner.send(r)
    }
    func events(path: String, onEvent: @escaping @Sendable (SSEEvent) -> Void) async throws { try await inner.events(path: path, onEvent: onEvent) }
}

@MainActor
func holdRig(_ b: ConfirmBackend) -> (StatusModel, ConfirmFlow, HoldTransport) {
    let h = HoldTransport(FakeTransport { b.handle($0) })
    let c = APIClient(transport: h)
    let flow = ConfirmFlow(client: c, clock: FakeNow())
    return (StatusModel(client: c, sleeper: NoSleep(), flow: flow), flow, h)
}

let isConfirmed: @Sendable (HTTPRequest) -> Bool = { $0.path == "/v1/restore" && String(decoding: $0.body ?? Data(), as: UTF8.self).contains("confirm") }
let isFirstStep: @Sendable (HTTPRequest) -> Bool = { $0.path == "/v1/restore" && !String(decoding: $0.body ?? Data(), as: UTF8.self).contains("confirm") }

@MainActor
@Test func cancelWhileSubmittingIsIgnored() async {
    let b = ConfirmBackend()
    let (m, flow, h) = holdRig(b)
    await m.restore()
    (flow.clock as! FakeNow).advance(3.1)
    h.hold(isConfirmed)
    let task = Task { await flow.confirm() }
    #expect(await eventually { h.held == 1 })
    #expect(flow.submitting)
    flow.cancel() // 请求已经发出：取消不做任何事
    #expect(flow.isActive && flow.prompt?.isUsed == true)
    h.release()
    await task.value
    #expect(flow.isActive == false && m.phase == .home && b.confirmCalls == 1, "请求返回后按结果正常收尾")
}

@MainActor
@Test func cancelWhileSubmittingInvalidAddsNoFirstStep() async {
    let b = ConfirmBackend()
    b.confirmInvalidOnce = true
    let (m, flow, h) = holdRig(b)
    await m.restore()
    #expect(b.firstCalls == 1)
    (flow.clock as! FakeNow).advance(3.1)
    h.hold(isConfirmed)
    let task = Task { await flow.confirm() }
    #expect(await eventually { h.held == 1 })
    flow.cancel()
    flow.cancel()
    h.release()
    await task.value
    #expect(b.firstCalls == 2, "只有确认码无效后的那一次重新取预览，取消没有多出第一步的请求")
    #expect(flow.isActive && flow.refreshed)
}

@MainActor
@Test func confirmDuringRefetchNotReentered() async {
    let b = ConfirmBackend()
    b.confirmInvalidOnce = true
    let (m, flow, h) = holdRig(b)
    await m.restore()
    (flow.clock as! FakeNow).advance(3.1)
    h.hold(isFirstStep) // 挂住重新取预览
    let task = Task { await flow.confirm() }
    #expect(await eventually { h.held == 1 })
    #expect(flow.submitting, "提交状态一直保持到重新取预览结束")
    (flow.clock as! FakeNow).advance(5)
    await flow.confirm()
    await flow.confirm()
    flow.cancel()
    #expect(b.confirmCalls == 1, "重新取预览期间再点确认：带确认码的请求次数不变")
    h.release()
    await task.value
    #expect(flow.isActive && flow.refreshed && flow.submitting == false)
}

/// 确认码无效后重新发起第一步，后台这次不再要求确认、操作直接完成：确认框关掉并给出说明。
@MainActor
@Test func refetchDoneShowsNote() async {
    let b = ConfirmBackend()
    b.confirmInvalidOnce = true
    let (m, flow, _, _) = confirmRig(b)
    await m.restore()
    b.previews = [] // 之后的第一步：不再要求确认（下面改成直接成功）
    let t2 = FakeTransport { r in
        if r.path == "/v1/restore" && String(decoding: r.body ?? Data(), as: UTF8.self).contains("confirm") {
            return errBody(403, "confirm_invalid", "确认码无效")
        }
        if r.path == "/v1/restore" { return json(200, statusJSON(state: "RESTORED", gate: false, protected: false)) }
        return b.handle(r)
    }
    let c2 = APIClient(transport: t2)
    let flow2 = ConfirmFlow(client: c2, clock: FakeNow())
    let m2 = StatusModel(client: c2, sleeper: NoSleep(), flow: flow2)
    _ = m
    _ = flow
    // 第一次第一步要确认：直接放一个确认框
    #expect(flow2.present(ConfirmPrompt.testMake(confirmJSON), as: DaemonStatus.self, refetch: {
        let o = await c2.restore()
        if case .needsConfirm(let np) = o { return .prompt(np) }
        if case .ok = o { return .done }
        return .failed
    }, onResult: { _ in }))
    await confirmNow(flow2)
    #expect(flow2.isActive == false && flow2.completedNote == L10n.confirmNotNeededDone)
    _ = m2
}

// ---- M5-10 复审必须改 2：按原因代码精确过滤用户自己引起的关闭 ----

@MainActor
@Test func notificationsIgnoreUserCausedClosures() {
    let s = FakeSender()
    let n = Notifier(sender: s)
    n.observe(status: st(gate: true), reconnect: true)
    // 更换主代理成功：关闭 main_proxy_switch → 打开：0 条
    n.observe(event: ev(1, "gate", "gate_closed", "main_proxy_switch：正在更换主代理"))
    n.observe(event: ev(2, "gate", "gate_opened"))
    #expect(s.got.isEmpty)
    // 更换中途又加了 exit_check：1 条断开；恢复后 1 条恢复
    n.observe(event: ev(3, "gate", "gate_closed", "main_proxy_switch：正在更换主代理"))
    n.observe(event: ev(4, "gate", "gate_closed", "exit_check：出口检查明确不通过"))
    n.observe(event: ev(5, "gate", "gate_opened"))
    #expect(s.got.map(\.kind) == [.protectionLost, .protectionRestored])
    #expect(s.got[0].body == L10n.notifyProtectionLost(L10n.gateReasonName("exit_check"), "出口检查明确不通过"))
    #expect((s.got[0].body.contains("exit_check")) == false, "原因代码映射成文字表里的名称")
    // 假想的 state_other：不被过滤（精确比较，不是前缀）
    n.observe(event: ev(6, "gate", "gate_closed", "state_other：别的原因"))
    #expect(s.got.last?.kind == .protectionLost && s.got.count == 3)
    // 停用（state）不通知
    n.observe(event: ev(7, "gate", "gate_opened"))
    n.observe(event: ev(8, "gate", "gate_closed", "state：停用"))
    n.observe(event: ev(9, "gate", "gate_opened"))
    #expect(s.got.count == 4 && s.got.last?.kind == .protectionRestored)
}

/// M6-7 补审返工：完整性校验不通过时通知一次（同一组问题不重复，问题变了再通知）；首页有醒目说明。
@MainActor
@Test func integrityFailedNotifyAndAlert() async {
    let s = FakeSender()
    let n = Notifier(sender: s)
    func st(_ problems: String?) -> DaemonStatus {
        var j = statusJSON(state: "ENABLED", gate: false, reasons: ["integrity"])
        if let problems {
            j.removeLast()
            j += ",\"integrity\":{\"checked\":true,\"ok\":false,\"problems\":[\(problems)]}}"
        }
        return decodeStatus(j)
    }
    n.observe(status: st(#"{"file":"bin/abroad-vm","kind":"mismatch"}"#), reconnect: true)
    n.observe(status: st(#"{"file":"bin/abroad-vm","kind":"mismatch"}"#), reconnect: false)
    #expect(s.got.map(\.kind) == [.integrityFailed])
    n.observe(status: st(#"{"file":"image/rootfs.img","kind":"missing"}"#), reconnect: false)
    #expect(s.got.count == 2, "问题变了再通知一次")
    #expect(s.got.allSatisfy { $0.body == L10n.notifyIntegrityFailed })
    // 首页说明
    let t = FakeTransport { req in
        req.path == "/v1/status" ? json(200, statusJSON(state: "ENABLED", gate: false, reasons: ["integrity"]).dropLast() + ",\"integrity\":{\"checked\":true,\"ok\":false,\"problems\":[{\"file\":\"bin/abroad-vm\",\"kind\":\"mismatch\"}]}}") : json(200, "{\"available\":false}")
    }
    let m = StatusModel(client: APIClient(transport: t))
    _ = await m.refresh()
    #expect(m.integrityAlert == L10n.integrityAlert(["bin/abroad-vm 内容不对"]))
}

/// M6-4b-3 补审返工：回国完成时还有浏览器开着（选了结束浏览器、没能结束或确认不了）：通知和首页提示都写明；没有时不提。
@MainActor
@Test func restoreDoneBrowsersLeft() async {
    let s = FakeSender()
    let n = Notifier(sender: s)
    n.restoreDone(pending: [], browsers: ["Firefox", "Safari"])
    n.restoreDone(pending: [])
    #expect(s.got[0].body == L10n.notifyRestoreDone + "；" + L10n.browsersStillOpen(["Firefox", "Safari"]))
    #expect(s.got[1].body == L10n.notifyRestoreDone)
    // 首页：回国应答里带 browsers_left 时有提示
    let b = ConfirmBackend()
    var after = statusJSON(state: "RESTORED", gate: false, protected: false)
    after.removeLast()
    b.afterConfirm = json(200, after + ",\"browsers_left\":[\"Firefox\"]}")
    let (m, flow, _, _) = confirmRig(b)
    _ = await m.refresh()
    await m.restore()
    await confirmNow(flow)
    #expect(m.notice?.text.raw == L10n.browsersStillOpen(["Firefox"]))
}
