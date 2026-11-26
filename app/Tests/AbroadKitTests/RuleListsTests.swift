// 补齐一（architect 2026-11-27）：“应用分流”页里规则的四项——本地域名名单、置顶域名、离线放行名单、拦截 UDP 443。
// 先读后写、每个规则 PUT 都带 If-Match；放松时显示后台给的原因和具体的终端命令；后台回 rules_changed 时重新加载、提示、
// 不自动再提交；内置的置顶域名只读、删除时在本地拦下。
import AbroadAPI
import Foundation
import Testing
@testable import AbroadKit

/// 一个假后台：规则文档和标识（每次改动标识加一）；可以让下一次 PUT 回冲突、要授权或校验不通过。
final class RulesBackend: @unchecked Sendable {
    let lock = NSLock()
    var doc = #"{"default":"proxy:main","rules":[{"app":"com.example.x","action":"block"}],"offline_allow":[],"local_domains":["*.corp.example"],"pinned_extra":["keep.example"],"block_udp_443":false}"#
    var rev = 1
    var next: HTTPResponse?  // 下一次 PUT 的应答（nil 时照常接受）
    var puts: [HTTPRequest] = []

    func handle(_ r: HTTPRequest) -> HTTPResponse {
        lock.lock(); defer { lock.unlock() }
        switch (r.method, r.path) {
        case ("GET", "/v1/rules?format=json"):
            return HTTPResponse(status: 200, headers: ["content-type": "application/json", "etag": "\"t\(rev)\""], body: Data(doc.utf8))
        case ("GET", "/v1/status"):
            var s = statusJSON(state: "RESTORED", gate: false)
            s.removeLast()
            return json(200, s + #","builtin_pinned":["claude.ai","claude.com","anthropic.com"]}"#)
        case ("PUT", let p) where p.hasPrefix("/v1/rules"):
            puts.append(r)
            if let n = next { next = nil; return n }
            doc = String(decoding: r.body ?? Data(), as: UTF8.self)
            rev += 1
            return json(200, statusJSON(state: "RESTORED", gate: false))
        default:
            return json(200, "{\"apps\":[],\"exits\":[]}")
        }
    }

    var rulesDoc: RulesDoc { lock.lock(); defer { lock.unlock() }; return try! JSONDecoder().decode(RulesDoc.self, from: Data(doc.utf8)) }
}

@MainActor
private func appsModel(_ b: RulesBackend) async -> AppsModel {
    let m = AppsModel(client: APIClient(transport: FakeTransport { b.handle($0) }))
    await m.load()
    return m
}

/// 验收 e：四小节的加载、添加、删除、开关；只改这一处；重复添加和删除不存在的项不提交。
@MainActor
@Test func ruleListsLoadAddRemoveToggle() async {
    let b = RulesBackend()
    let m = await appsModel(b)
    #expect(m.state == .loaded)
    #expect(m.items(.local) == ["*.corp.example"] && m.items(.pinned) == ["keep.example"] && m.items(.offline).isEmpty)
    #expect(m.builtinPinned == ["claude.ai", "claude.com", "anthropic.com"])
    await m.addItem(.local, "intra.example")
    #expect(b.rulesDoc.localDomains == ["*.corp.example", "intra.example"])
    #expect(b.rulesDoc.rules == [.init(app: "com.example.x", action: "block")] && b.rulesDoc.pinnedExtra == ["keep.example"], "别的字段原样保留")
    #expect(m.items(.local).contains("intra.example"), "提交后重新加载")
    let n = b.puts.count
    await m.addItem(.local, "INTRA.example")
    #expect(b.puts.count == n && m.notice?.text.raw == L10n.rulesNoChange, "重复添加不提交")
    await m.addItem(.offline, "com.example.nas")
    #expect(b.rulesDoc.offlineAllow == ["com.example.nas"])
    await m.deleteListItem(.pinned, "keep.example")
    #expect(b.rulesDoc.pinnedExtra.isEmpty)
    let n2 = b.puts.count
    await m.deleteListItem(.local, "nothere.example")
    #expect(b.puts.count == n2, "删除不存在的项不提交")
    await m.setBlockUDP443(true)
    #expect(b.rulesDoc.blockUDP443)
    m.drafts[.pinned] = "added.example"
    await m.addDraft(.pinned)
    #expect(b.rulesDoc.pinnedExtra == ["added.example"] && (m.drafts[.pinned] ?? "") == "")
}

/// 验收 k：每一个规则 PUT 都带读取时的 If-Match（含按应用设规则、删规则）。
@MainActor
@Test func ruleListsPutsCarryIfMatch() async {
    let b = RulesBackend()
    let m = await appsModel(b)
    await m.setRule(app: "com.example.a", action: "block")
    await m.removeRule(app: "com.example.a")
    await m.addItem(.local, "a.example")
    await m.deleteListItem(.local, "a.example")
    await m.addItem(.pinned, "b.example")
    await m.addItem(.offline, "com.example.c")
    await m.setBlockUDP443(true)
    #expect(b.puts.count == 7)
    #expect(b.puts.allSatisfy { ($0.ifMatch ?? "").hasPrefix("\"t") }, "每个规则 PUT 都带 If-Match")
    #expect(b.puts.allSatisfy { String(decoding: $0.bytes, as: UTF8.self).contains("\r\nIf-Match: \"t") }, "请求里真的带了这个头")
}

/// 验收 l：后台回 rules_changed：重新加载、显示提示、没有自动再发 PUT。
@MainActor
@Test func ruleListsConflictReloadsNoRetry() async {
    let b = RulesBackend()
    let m = await appsModel(b)
    b.lock.lock()
    b.next = json(412, #"{"code":"rules_changed","error":"规则在你读取之后被别处改过，这次没有改动。请重新读取后再改。"}"#)
    b.lock.unlock()
    await m.addItem(.local, "intra.example")
    #expect(b.puts.count == 1, "不自动重试")
    #expect(m.notice?.text.raw == L10n.rulesChangedReloaded && m.notice?.text.code == .rulesChanged)
    #expect(!b.rulesDoc.localDomains.contains("intra.example"))
    await m.addItem(.local, "intra.example") // 用户再点一次才提交
    #expect(b.puts.count == 2 && b.rulesDoc.localDomains.contains("intra.example"))
}

/// 验收 e：放松时显示后台给的原因和具体的终端命令（和命令行的帮助对照见 RepoTests）。
@MainActor
@Test func ruleListsRelaxNeedsAuthorization() async {
    let b = RulesBackend()
    let m = await appsModel(b)
    b.lock.lock()
    b.next = json(403, #"{"code":"needs_authorization","error":"改规则失败：放松了保护（local_domains 新增 intra.example）。请用 sudo"}"#)
    b.lock.unlock()
    await m.addItem(.local, "intra.example")
    #expect(m.authorizationReason?.raw.contains("local_domains") == true)
    #expect(m.terminal?.commands == ["sudo abroad rules local add intra.example"])
}

/// 验收 d：后台拒绝的原样显示；删除内置置顶域名在本地拦下、不发请求；内置的只读。
@MainActor
@Test func ruleListsRejectsShownAndBuiltinReadonly() async {
    let b = RulesBackend()
    let m = await appsModel(b)
    let reason = "local_domains 第 2 项：与置顶域名 claude.ai 有重叠，Claude 相关的域名不能走本机网络"
    b.lock.lock()
    b.next = json(422, "{\"code\":\"invalid\",\"error\":\"\(reason)\"}")
    b.lock.unlock()
    await m.addItem(.local, "claude.ai")
    #expect(m.notice?.kind == .error && m.notice?.text.raw == reason)
    let n = b.puts.count
    await m.deleteListItem(.pinned, "claude.ai")
    await m.deleteListItem(.pinned, "*.anthropic.com")
    #expect(b.puts.count == n && m.notice?.text.raw == L10n.rulesBuiltinReadonly)
    #expect(!m.items(.pinned).contains("claude.ai"), "内置的不在可删的名单里")
}

/// 验收 e：后台不可达、加载失败、加载成功三种状态；说明文字来自文字表。
@MainActor
@Test func ruleListsLoadStatesAndTexts() async {
    let down = AppsModel(client: APIClient(transport: FakeTransport { _ in throw ConnectionError.notRunning }))
    await down.load()
    if case .unreachable = down.state {} else { Issue.record("后台不可达：\(down.state)") }
    let bad = AppsModel(client: APIClient(transport: FakeTransport { _ in json(500, #"{"code":"internal","error":"读规则失败"}"#) }))
    await bad.load()
    if case .failed = bad.state {} else { Issue.record("加载失败：\(bad.state)") }
    #expect(AppsModel.help(.local) == L10n.rulesLocalHelp && AppsModel.help(.pinned) == L10n.rulesPinnedHelp && AppsModel.help(.offline) == L10n.rulesOfflineHelp)
    #expect(L10n.rulesLocalHelp.contains("走本机网络") && L10n.rulesPinnedHelp.contains("永远走主代理") && L10n.rulesPinnedHelp.contains("不进 Claude 域名解析锁"))
}
