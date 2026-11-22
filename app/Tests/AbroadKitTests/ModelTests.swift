// 验收 d：视图模型（不创建窗口、不初始化应用对象）。
import AbroadAPI
import Foundation
import Testing
@testable import AbroadKit

func decodeStatus(_ s: String) -> DaemonStatus { try! JSONDecoder().decode(DaemonStatus.self, from: Data(s.utf8)) }

@Test func phaseMapping() {
    let cases: [(String, Phase)] = [
        (statusJSON(state: "ENABLED", gate: true), .abroad),
        (statusJSON(state: "ENABLED", gate: false, reasons: ["main_exit"]), .offline),
        (statusJSON(state: "DISABLED", gate: false, protected: true), .offline),
        (statusJSON(state: "DISABLED", gate: false, protected: false), .error), // 停用且保护失效：接管还在、保护没了
        (statusJSON(state: "ENABLED", gate: false, protected: false), .error), // 启用且保护失效：不是节点的问题
        (statusJSON(state: "ENABLED", gate: true, protected: false), .error),
        (statusJSON(state: "RESTORED", gate: false, protected: false), .home),
        (statusJSON(state: "ENABLING", gate: false), .checking),
        (statusJSON(state: "RESTORING", gate: false), .checking),
        (statusJSON(state: "ENABLED", gate: true, corrupt: true), .error),
        (statusJSON(state: "WHATEVER", gate: false), .error),
    ]
    for (j, want) in cases {
        let p = Phase.from(decodeStatus(j))
        #expect(p == want, "\(j)")
        #expect(p.symbol == want.symbol && p.title == want.title)
    }
    #expect(Phase.from(decodeStatus(statusJSON(state: "DISABLED", gate: false, protected: true))).title == L10n.phaseOffline)
    // 五种状态的图标各不相同。
    let all: [Phase] = [.abroad, .offline, .home, .checking, .error]
    #expect(Set(all.map(\.symbol)).count == 5 && Set(all.map(\.title)).count == 5)
}

@Test func menuAvailability() {
    typealias M = MenuAvailability
    #expect(M.from(.abroad, reachable: true, busy: false) == M(enable: false, disable: true, restore: true))
    #expect(M.from(.offline, reachable: true, busy: false) == M(enable: true, disable: true, restore: true))
    #expect(M.from(.home, reachable: true, busy: false) == M(enable: true, disable: false, restore: false))
    #expect(M.from(.error, reachable: true, busy: false) == M(enable: false, disable: false, restore: true))
    let none = M(enable: false, disable: false, restore: false)
    #expect(M.from(.checking, reachable: true, busy: false) == none)
    #expect(M.from(.home, reachable: false, busy: false) == none)
    #expect(M.from(.home, reachable: true, busy: true) == none)
}

final class Box<T>: @unchecked Sendable {
    var v: T
    init(_ v: T) { self.v = v }
}

/// 一个会按请求改变状态的假后台。
func fakeBackend(state: Box<String>, enable: @escaping @Sendable (HTTPRequest) -> HTTPResponse) -> FakeTransport {
    FakeTransport { req in
        switch (req.method, req.path) {
        case ("GET", "/v1/status"): return json(200, statusJSON(state: state.v, gate: state.v == "ENABLED"))
        case ("GET", "/v1/main-proxy/exit"): return json(200, exitJSON)
        case ("POST", "/v1/enable"): return enable(req)
        case ("POST", "/v1/disable"): state.v = "DISABLED"; return json(200, statusJSON(state: "DISABLED", gate: false))
        default: return json(404, "{\"error\":\"没有\"}")
        }
    }
}

@MainActor
@Test func enableNeedsConfirmThenOK() async {
    let state = Box("RESTORED")
    let t = fakeBackend(state: state) { req in
        if req.body != nil { state.v = "ENABLED"; return json(200, statusJSON(state: "ENABLED", gate: true)) }
        return json(409, confirmJSON)
    }
    let m = StatusModel(client: APIClient(transport: t), sleeper: NoSleep())
    await m.refresh()
    #expect(m.phase == .home && m.menu.enable && m.exitInfo?.ip == "203.0.113.7")
    await m.enable()
    #expect(m.pendingConfirm != nil && m.phase == .home && !m.busy)
    #expect(m.pendingConfirm?.region?.to == "B")
    await m.answerConfirm(true)
    #expect(m.pendingConfirm == nil && m.phase == .abroad && m.menu.disable)
    await m.disable()
    #expect(m.phase == .offline)
}

@MainActor
@Test func confirmCancelled() async {
    let state = Box("RESTORED")
    let t = fakeBackend(state: state) { _ in json(409, confirmJSON) }
    let m = StatusModel(client: APIClient(transport: t), sleeper: NoSleep())
    await m.refresh()
    await m.enable()
    let p = m.pendingConfirm
    let n = t.requests.count
    await m.answerConfirm(false)
    #expect(m.pendingConfirm == nil && p?.isUsed == true && t.requests.count == n && m.phase == .home)
}

@MainActor
@Test func enableNeedsAuthorization() async {
    let state = Box("RESTORED")
    let t = fakeBackend(state: state) { _ in json(403, "{\"error\":\"需要本人授权：地区会变，这一次按敏感处理\"}") }
    let m = StatusModel(client: APIClient(transport: t), sleeper: NoSleep())
    await m.refresh()
    await m.enable()
    #expect(m.terminalCommand == "sudo abroad up" && m.phase == .home && m.notice == nil)
    m.dismissTerminal()
    #expect(m.terminalCommand == nil)
}

@MainActor
@Test func errorsShownInThreeParts() async {
    let state = Box("RESTORED")
    let t = fakeBackend(state: state) { _ in json(422, "{\"error\":\"启用失败：还没有出口基准。请先应用主代理，再启用。\"}") }
    let m = StatusModel(client: APIClient(transport: t), sleeper: NoSleep())
    await m.refresh()
    await m.enable()
    #expect(m.notice?.text.action == "启用失败" && m.notice?.text.reason == "还没有出口基准" && m.notice?.text.next == "请先应用主代理，再启用。")
    // 忙
    t.setHandler { req in req.path == "/v1/status" ? json(200, statusJSON(state: "RESTORED", gate: false, protected: false)) : json(409, "{\"error\":\"有别的操作在进行\"}") }
    await m.enable()
    #expect(m.notice?.text.raw == "有别的操作在进行" && m.phase == .home)
}

@MainActor
@Test func securityNote() async {
    let t = FakeTransport { req in req.path == "/v1/status" ? json(200, statusJSON(state: "RESTORED", gate: false, security: true)) : json(200, "{\"available\":false,\"note\":\"还没有\"}") }
    let m = StatusModel(client: APIClient(transport: t), sleeper: NoSleep())
    await m.refresh()
    #expect(m.securityNote == "签名校验推迟期间")
    #expect(m.exitInfo?.available == false && m.exitInfo?.note == "还没有")
    let t2 = FakeTransport { req in req.path == "/v1/status" ? json(200, statusJSON(state: "RESTORED", gate: false, security: false)) : json(200, "{\"available\":false}") }
    let m2 = StatusModel(client: APIClient(transport: t2), sleeper: NoSleep())
    await m2.refresh()
    #expect(m2.securityNote == nil)
}

/// 事件到达后状态更新；后台不可达时显示“后台服务没有运行”，恢复后自动回来。
@MainActor
@Test func eventsAndReconnect() async {
    let state = Box("RESTORED")
    let down = Box(false)
    let t = FakeTransport { req in
        if down.v { throw ConnectionError.notRunning }
        switch req.path {
        case "/v1/status": return json(200, statusJSON(state: state.v, gate: state.v == "ENABLED"))
        default: return json(200, exitJSON)
        }
    }
    let m = StatusModel(client: APIClient(transport: t), sleeper: NoSleep())
    m.start()
    #expect(await eventually { m.phase == .home && t.subscribed })
    state.v = "ENABLED"
    t.push(SSEEvent(id: "1", event: "state", data: "{}"))
    #expect(await eventually { m.phase == .abroad })
    // 后台停了：事件流断开、socket 也没了。
    down.v = true
    t.dropEvents()
    #expect(await eventually { !m.reachable && m.connectionText == L10n.backendDown && m.phase == .error && !m.menu.enable })
    // 恢复：自动重连回来。
    state.v = "RESTORED"
    down.v = false
    #expect(await eventually { m.reachable && m.phase == .home && t.subscribed })
    #expect(t.eventConnects >= 2)
    m.stop()
}

/// “已回国”只在 RESTORED 时出现：五种状态 × 闸门开关 × 保护有无 × 是否损坏，全部组合。
@Test func homeOnlyWhenRestored() {
    for state in ["RESTORED", "ENABLING", "ENABLED", "DISABLED", "RESTORING"] {
        for gate in [false, true] {
            for prot in [false, true] {
                for corrupt in [false, true] {
                    let st = decodeStatus(statusJSON(state: state, gate: gate, protected: prot, corrupt: corrupt))
                    let p = Phase.from(st)
                    #expect((p == .home) == (state == "RESTORED" && !corrupt), "\(state) gate=\(gate) protected=\(prot) corrupt=\(corrupt) → \(p)")
                    if corrupt { #expect(p == .error) }
                    if (state == "ENABLED" || state == "DISABLED") && !prot { #expect(p == .error) }
                    if p == .abroad { #expect(state == "ENABLED" && gate && prot && !corrupt) }
                }
            }
        }
    }
}

/// 保护失效时显示固定说明和后台巡检事件里的那句。
@MainActor
@Test func protectionLostNote() async {
    let st = "{\"state\":\"DISABLED\",\"gate_open\":false,\"protected\":false,\"gate\":{\"open\":false,\"lease_left_ms\":0},\"events\":[{\"source\":\"patrol\",\"kind\":\"not_referenced\",\"time\":\"2026-11-22T00:00:00Z\",\"detail\":\"别的软件替换了系统防火墙规则\"}]}"
    let t = FakeTransport { req in req.path == "/v1/status" ? json(200, st) : json(200, "{\"available\":false}") }
    let m = StatusModel(client: APIClient(transport: t), sleeper: NoSleep())
    await m.refresh()
    #expect(m.phase == .error)
    #expect(m.protectionNote == L10n.protectionLost + "（别的软件替换了系统防火墙规则）")
    #expect(m.menu.restore && !m.menu.enable)
    let ok = FakeTransport { req in req.path == "/v1/status" ? json(200, statusJSON(state: "DISABLED", gate: false, protected: true)) : json(200, "{\"available\":false}") }
    let m2 = StatusModel(client: APIClient(transport: ok), sleeper: NoSleep())
    await m2.refresh()
    #expect(m2.phase == .offline && m2.protectionNote == nil)
}
