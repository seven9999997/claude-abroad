// 补齐四：环境检测历史；系统进程单列和共用进程的提醒。
import AbroadAPI
import Foundation
import Testing
@testable import AbroadKit

/// 验收 e：倒序、每条标出和更早一条相比变了的项、空的时候有说明；不可达、失败、成功三种状态。
@MainActor
@Test func envHistoryRows() async {
    let body = #"{"entries":[{"at":"2026-11-27T12:02:00Z","last_seen":"2026-11-27T12:05:00Z","hard":"fail","items":[{"id":"06","result":"fail"},{"id":"01","result":"pass"}]},{"at":"2026-11-27T12:00:00Z","last_seen":"2026-11-27T12:00:00Z","hard":"pass","items":[{"id":"06","result":"pass"},{"id":"01","result":"pass"}]}]}"#
    func model(_ handler: @escaping @Sendable (HTTPRequest) throws -> HTTPResponse) async -> EnvModel {
        let m = EnvModel(client: APIClient(transport: FakeTransport(handler)))
        await m.loadHistory()
        return m
    }
    let m = await model { _ in json(200, body) }
    #expect(m.historyState == .loaded && m.history.count == 2)
    #expect(m.history[0].text == L10n.envHistoryLine("2026-11-27T12:02:00Z", "fail", "2026-11-27T12:05:00Z"))
    #expect(m.history[0].changed == L10n.envHistoryChanged(["06 pass→fail"]))
    #expect(m.history[1].changed == nil && m.history[1].text == L10n.envHistoryLine("2026-11-27T12:00:00Z", "pass", nil))
    #expect(m.historyEmptyText == nil)
    let empty = await model { _ in json(200, #"{"entries":[]}"#) }
    #expect(empty.historyEmptyText == L10n.envHistoryEmpty)
    let down = await model { _ in throw ConnectionError.notRunning }
    if case .unreachable = down.historyState {} else { Issue.record("不可达：\(down.historyState)") }
    let bad = await model { _ in json(500, #"{"code":"internal","error":"读不出"}"#) }
    if case .failed = bad.historyState {} else { Issue.record("失败：\(bad.historyState)") }
}

/// 验收 h：系统进程单列；共用的显示说明；给共用的进程设规则前有提醒，确认才提交，取消不发请求；旧后台按原来的样子显示。
@MainActor
@Test func systemProcessesAndSharedPrompt() async {
    let b = RulesBackend()
    let apps = #"{"apps":[{"app":"com.example.a","name":"A","conns":1,"exits":[],"reason":"","kind":"app"},{"app":"system:apsd","name":"apsd","conns":2,"exits":[],"reason":"","kind":"system","shared":"推送服务：所有应用的推送通知都经它"},{"app":"system:foo","name":"foo","conns":1,"exits":[],"reason":"","kind":"system"}]}"#
    let t = FakeTransport { r in r.path == "/v1/apps" ? json(200, apps) : b.handle(r) }
    let m = AppsModel(client: APIClient(transport: t))
    await m.load()
    #expect(m.appRows.map(\.app).contains("com.example.a") && !m.appRows.contains { $0.isSystem })
    #expect(Set(m.systemRows.map(\.app)) == ["system:apsd", "system:foo"])
    #expect(m.systemRows.first { $0.app == "system:apsd" }?.shared == "推送服务：所有应用的推送通知都经它")
    let n = b.puts.count
    await m.requestRule(app: "system:apsd", action: "block")
    #expect(m.sharedPrompt?.text == L10n.sharedRuleWarn("推送服务：所有应用的推送通知都经它") && b.puts.count == n, "提交前先提醒")
    m.cancelShared()
    #expect(m.sharedPrompt == nil && b.puts.count == n, "取消不发请求")
    await m.requestRule(app: "system:apsd", action: "block")
    await m.confirmShared()
    #expect(b.puts.count == n + 1 && b.rulesDoc.rules.contains(.init(app: "system:apsd", action: "block")), "确认后才提交")
    await m.requestRule(app: "system:foo", action: "block") // 不是共用的：直接提交
    #expect(m.sharedPrompt == nil && b.puts.count == n + 2)
    // 旧后台：没有种类字段，全部按原来的样子列在一起
    let old = FakeTransport { r in r.path == "/v1/apps" ? json(200, #"{"apps":[{"app":"system:apsd","name":"apsd","conns":2,"exits":[],"reason":""}]}"#) : b.handle(r) }
    let m2 = AppsModel(client: APIClient(transport: old))
    await m2.load()
    #expect(m2.systemRows.isEmpty && m2.appRows.contains { $0.app == "system:apsd" })
    #expect(L10n.sharedRuleWarn("x").contains("同时影响那些应用"))
}
