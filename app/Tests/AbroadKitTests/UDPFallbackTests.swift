// 补齐二（architect 2026-11-27）：“UDP 不可用”和“分流出口回退到主代理”的提示与通知。信息都由后台给，界面不推断；
// 文字来自文字表，里面没有任何地址。
import AbroadAPI
import Foundation
import Testing
@testable import AbroadKit

private func status(_ state: String, udp: String?, exits: String? = nil) -> DaemonStatus {
    var j = statusJSON(state: state, gate: state == "ENABLED")
    j.removeLast()
    if let udp { j += ",\"main_udp\":\"\(udp)\"" }
    if let exits { j += ",\"exits\":" + exits }
    return decodeStatus(j + "}")
}

private let hkDown = #"[{"tag":"hk","avail":{"up":false,"pushed":true,"agent_up":true,"reason":"最近一次检查没有通过"},"affected_count":1}]"#
private let hkUp = #"[{"tag":"hk","avail":{"up":true,"pushed":true,"agent_up":true},"affected_count":1}]"#
private let hkDownList = #"[{"tag":"hk","avail":{"up":false,"pushed":true,"agent_up":true,"reason":"最近一次检查没有通过"},"affected":["com.example.a"]}]"#
private let jpDownNoRules = #"[{"tag":"jp","avail":{"up":false,"pushed":true,"agent_up":true,"reason":"x"}}]"#

/// 验收 g：首页和菜单栏的说明只在已启用且 UDP 不可用时出现；旧后台不给这个字段时不显示。
@MainActor
@Test func udpNoticeOnlyWhenEnabledAndUnavailable() async {
    func notice(_ body: String) async -> String? {
        let t = FakeTransport { req in req.path == "/v1/status" ? json(200, body) : json(200, "{\"available\":false}") }
        let m = StatusModel(client: APIClient(transport: t))
        _ = await m.refresh()
        return m.udpNotice
    }
    var on = statusJSON(state: "ENABLED", gate: true); on.removeLast(); on += ",\"main_udp\":\"unavailable\"}"
    #expect(await notice(on) == L10n.udpUnavailableDetail)
    var ok = statusJSON(state: "ENABLED", gate: true); ok.removeLast(); ok += ",\"main_udp\":\"available\"}"
    #expect(await notice(ok) == nil)
    var unk = statusJSON(state: "ENABLED", gate: true); unk.removeLast(); unk += ",\"main_udp\":\"unknown\"}"
    #expect(await notice(unk) == nil)
    var off = statusJSON(state: "DISABLED", gate: false); off.removeLast(); off += ",\"main_udp\":\"unavailable\"}"
    #expect(await notice(off) == nil)
    #expect(await notice(statusJSON(state: "ENABLED", gate: true)) == nil, "旧后台：不显示、不报错")
}

/// 验收 g：节点列表的三种标记；旧后台不给时不显示。
@MainActor
@Test func nodeUDPLabels() async {
    let body = #"[{"id":"s1","name":"本地","kind":"local","updated":"2026-11-27T00:00:00Z","nodes":[{"id":"n1","name":"东京","type":"trojan","udp":"available"},{"id":"n2","name":"大阪","type":"ss","udp":"unavailable"},{"id":"n3","name":"首尔","type":"ss","udp":"unknown"},{"id":"n4","name":"旧","type":"ss"}]}]"#
    let t = FakeTransport { req in
        switch req.path {
        case "/v1/main-proxy/sources": return json(200, body)
        case "/v1/main-proxy": return json(200, #"{"applied":false,"needs_reselect":false,"lan_pending":false,"sources":1}"#)
        default: return json(200, statusJSON(state: "DISABLED", gate: false))
        }
    }
    let c = APIClient(transport: t)
    let m = MainProxyModel(client: c, flow: fakeFlow(c))
    await m.refresh()
    #expect(m.nodes.map(\.udpLabel) == [L10n.udpLabel("available"), L10n.udpLabel("unavailable"), L10n.udpLabel("unknown"), nil])
    #expect(L10n.udpLabel("available") != L10n.udpLabel("unavailable") && L10n.udpLabel("unknown") != L10n.udpLabel("available"))
}

/// 验收 g：分流页的回退标记、出口页的受影响规则；旧后台不给时不显示。
@MainActor
@Test func fallbackMarksInPages() async {
    let t = FakeTransport { req in
        switch req.path {
        case "/v1/rules?format=json":
            return HTTPResponse(status: 200, headers: ["etag": "\"t1\""], body: Data(#"{"default":"proxy:main","rules":[{"app":"com.example.a","action":"proxy:hk"},{"app":"com.example.m","action":"proxy:main"}]}"#.utf8))
        case "/v1/apps": return json(200, #"{"apps":[],"fallbacks":[{"app":"com.example.a","exit":"hk","reason":"最近一次检查没有通过"}]}"#)
        case "/v1/exits": return json(200, "{\"exits\":" + hkDownList + "}")
        case "/v1/status": return json(200, statusJSON(state: "ENABLED", gate: true))
        default: return json(200, "{\"exits\":[]}")
        }
    }
    let apps = AppsModel(client: APIClient(transport: t))
    await apps.load()
    #expect(apps.rows.first(where: { $0.app == "com.example.a" })?.fallback == L10n.fallbackMark("hk"))
    #expect(apps.rows.first(where: { $0.app == "com.example.m" })?.fallback == nil, "指向主代理的规则没有标记")
    let exits = ExitsModel(client: APIClient(transport: t))
    await exits.load()
    #expect(exits.rows.first?.affectedText == L10n.exitAffected(["com.example.a"]))
    // 旧后台：没有这些字段
    let old = FakeTransport { req in
        switch req.path {
        case "/v1/rules?format=json": return json(200, #"{"default":"proxy:main","rules":[{"app":"com.example.a","action":"proxy:hk"}]}"#)
        case "/v1/apps": return json(200, #"{"apps":[]}"#)
        case "/v1/exits": return json(200, #"{"exits":[{"tag":"hk","avail":{"up":false}}]}"#)
        default: return json(200, statusJSON(state: "ENABLED", gate: true))
        }
    }
    let a2 = AppsModel(client: APIClient(transport: old))
    await a2.load()
    #expect(a2.rows.allSatisfy { $0.fallback == nil })
    let e2 = ExitsModel(client: APIClient(transport: old))
    await e2.load()
    #expect(e2.rows.first?.affectedText == nil)
}

/// 验收 e：两种新通知——UDP 由可用变不可用一条、一直不可用不重复、还不知道不发；有规则指向的出口变不可用一条、没有规则
/// 指向的不发；重连后的当前状态不发；恢复不发；开关关掉不发。
@MainActor
@Test func udpAndExitNotifications() {
    let s = FakeSender()
    let n = Notifier(sender: s)
    n.observe(status: status("ENABLED", udp: "available", exits: hkUp), reconnect: true) // 基线
    n.observe(status: status("ENABLED", udp: "unknown", exits: hkUp), reconnect: false)
    #expect(s.got.isEmpty, "还不知道不发")
    n.observe(status: status("ENABLED", udp: "unavailable", exits: hkUp), reconnect: false)
    n.observe(status: status("ENABLED", udp: "unavailable", exits: hkUp), reconnect: false)
    #expect(s.got.map(\.kind) == [.udpUnavailable], "由可用变不可用一条、一直不可用不重复")
    n.observe(status: status("ENABLED", udp: "unavailable", exits: hkDown), reconnect: false)
    n.observe(status: status("ENABLED", udp: "unavailable", exits: hkDown), reconnect: false)
    #expect(s.got.map(\.kind) == [.udpUnavailable, .exitFallback])
    #expect(s.got.last?.body == L10n.notifyExitFallback("hk", 1))
    n.observe(status: status("ENABLED", udp: "available", exits: hkUp), reconnect: false) // 恢复：不发
    n.observe(status: status("ENABLED", udp: "available", exits: jpDownNoRules), reconnect: false) // 没有规则指向：不发
    #expect(s.got.count == 2)
    // 重连后的当前状态：只建立基线
    let s2 = FakeSender()
    let n2 = Notifier(sender: s2)
    n2.observe(status: status("ENABLED", udp: "unavailable", exits: hkDown), reconnect: true)
    n2.observe(status: status("ENABLED", udp: "unavailable", exits: hkDown), reconnect: false)
    #expect(s2.got.isEmpty)
    // 开关关掉：不发
    let s3 = FakeSender()
    let n3 = Notifier(sender: s3, preference: MemoryPreference(enabled: false))
    n3.observe(status: status("ENABLED", udp: "available", exits: hkUp), reconnect: true)
    n3.observe(status: status("ENABLED", udp: "unavailable", exits: hkDown), reconnect: false)
    #expect(s3.got.isEmpty)
}

/// 验收 f：通知和界面的文字里没有地址；文字来自文字表。
@Test func udpFallbackTextsHaveNoAddresses() {
    let texts = [L10n.notifyUDPUnavailable, L10n.notifyExitFallback("hk", 2), L10n.udpUnavailableDetail, L10n.fallbackMark("hk"),
                 L10n.exitAffected(["com.example.a"]), L10n.udpLabel("available"), L10n.udpLabel("unavailable"), L10n.udpLabel(nil)]
    let addr = try! NSRegularExpression(pattern: #"\d+\.\d+\.\d+\.\d+|[0-9a-f]{1,4}:[0-9a-f:]+|\.example\.com|:\d{2,5}\b"#)
    for t in texts {
        #expect(addr.firstMatch(in: t, range: NSRange(t.startIndex..., in: t)) == nil, "文字里有地址：\(t)")
    }
}
