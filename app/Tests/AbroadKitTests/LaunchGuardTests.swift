// M6-4b：“打开 Claude”按钮的条件和动作（超过 10 分钟先补跑检测）、设置页的启动管控文字。从不自动打开。
import AbroadAPI
import Foundation
import Testing
@testable import AbroadKit

final class CountOpener: ClaudeOpener, @unchecked Sendable {
    var calls = 0
    func openClaude() { calls += 1 }
}

private func statusEnv(state: String, gate: Bool, hard: String, at: String) -> String {
    var j = statusJSON(state: state, gate: gate)
    j.removeLast()
    j += ",\"envcheck\":{\"at\":\"\(at)\",\"hard\":\"\(hard)\",\"items\":[]}}"
    return j
}

@MainActor
private func model(_ status: @escaping @Sendable () -> String, runs: Counter, opener: CountOpener) async -> StatusModel {
    let t = FakeTransport { req in
        if req.method == "POST" && req.path == "/v1/envcheck/run" { runs.inc(); return json(200, "{\"at\":\"2026-11-25T12:00:00Z\",\"hard\":\"pass\",\"items\":[]}") }
        return json(200, status())
    }
    let m = StatusModel(client: APIClient(transport: t), sleeper: NoSleep(), notifier: Notifier(opener: opener))
    _ = await m.refresh()
    return m
}

final class Counter: @unchecked Sendable {
    private let lock = NSLock()
    private var n = 0
    func inc() { lock.lock(); n += 1; lock.unlock() }
    var value: Int { lock.lock(); defer { lock.unlock() }; return n }
}

@MainActor
@Test func openClaudeConditions() async {
    let runs = Counter()
    for (state, gate, hard, can) in [("ENABLED", true, "pass", true), ("ENABLED", false, "pass", false), ("ENABLED", true, "fail", false), ("DISABLED", false, "pass", false), ("RESTORED", false, "pass", false)] {
        let o = CountOpener()
        let s = statusEnv(state: state, gate: gate, hard: hard, at: "2026-11-25T12:00:00Z")
        let m = await model({ s }, runs: runs, opener: o)
        #expect(m.canOpenClaude == can, "\(state) \(gate) \(hard)")
        #expect((m.openClaudeHint == nil) == can)
        await m.openClaude(now: ISO8601DateFormatter().date(from: "2026-11-25T12:01:00Z")!)
        #expect(o.calls == (can ? 1 : 0), "不可用时不打开：\(state)")
    }
    #expect(runs.value == 0, "检测在 10 分钟内：不补跑")
}

@MainActor
@Test func openClaudeRechecksAfterTenMinutes() async {
    let runs = Counter()
    let o = CountOpener()
    let s = statusEnv(state: "ENABLED", gate: true, hard: "pass", at: "2026-11-25T12:00:00Z")
    let m = await model({ s }, runs: runs, opener: o)
    await m.openClaude(now: ISO8601DateFormatter().date(from: "2026-11-25T12:20:00Z")!)
    #expect(runs.value == 1, "超过 10 分钟先补跑一轮检测")
    #expect(o.calls == 1)
}

@MainActor
@Test func openClaudeRecheckFails() async {
    let runs = Counter()
    let o = CountOpener()
    final class Box: @unchecked Sendable { var after = false }
    let box = Box()
    let m = await model({ box.after ? statusEnv(state: "ENABLED", gate: true, hard: "fail", at: "2026-11-25T12:20:00Z")
                                    : statusEnv(state: "ENABLED", gate: true, hard: "pass", at: "2026-11-25T12:00:00Z") },
                        runs: runs, opener: o)
    box.after = true
    await m.openClaude(now: ISO8601DateFormatter().date(from: "2026-11-25T12:20:00Z")!)
    #expect(o.calls == 0, "补跑没通过：不打开")
    #expect(m.notice?.text.raw == L10n.openClaudeEnvNotPassed)
}

@MainActor
@Test func settingsLaunchGuardText() async {
    let lg = #"{"allowed":false,"since":"2026-11-25T12:00:00Z","cover_all":true,"layers":[{"layer":1,"name":"Claude 域名解析锁","effective":true},{"layer":2,"name":"启动拦截（系统扩展）","effective":false,"note":"暂不提供"},{"layer":3,"name":"发现即结束","effective":true},{"layer":4,"name":"Claude Code 的本机代理","effective":false,"note":"只覆盖 Claude Code"}],"coverage":[{"kind":"npm","name":"Claude Code（npm 安装）","covered":false,"reason":"签名确认不了"}],"recent":[{"time":"2026-11-25T12:00:01Z","pid":7,"kind":"native","name":"Claude Code"}]}"#
    let t = FakeTransport { req in
        switch req.path {
        case "/v1/launchguard": return json(200, lg)
        case "/v1/restore/preview": return json(200, "{\"processes\":[],\"will_kill\":true}")
        default: return json(200, statusJSON(state: "RESTORED", gate: false))
        }
    }
    let m = SettingsModel(client: APIClient(transport: t))
    await m.load()
    let text = m.launchControlText
    #expect(text.hasPrefix(L10n.launchGuardBlocked))
    #expect(text.contains(L10n.launchGuardLayer("Claude 域名解析锁", true)))
    #expect(text.contains(L10n.launchGuardCoverage("Claude Code（npm 安装）", "签名确认不了")))
    #expect(text.contains(L10n.launchGuardRecent(1)))
    #expect(text.hasSuffix(L10n.loginItemHint))
}

// M6-4b-3：回国确认框里“同时强制退出浏览器”默认不勾；勾了，带确认码的请求里才有 kill_browsers；每次新的确认框复位。
@MainActor
@Test func killBrowsersOption() async {
    for tick in [false, true] {
        let b = ConfirmBackend()
        b.previews = ["{\"code\":\"needs_confirm\",\"error\":\"恢复本机网络需要确认\",\"confirm\":\"0123456789abcdef0123456789abcdef\",\"expires_in_s\":60,\"preview\":{\"processes\":[],\"will_kill\":true,\"browsers\":[{\"pid\":500,\"app\":\"Google Chrome\",\"cmd\":\"\"}],\"browser_note\":\"请自行关闭\"}}"]
        let (m, flow, _, t) = confirmRig(b)
        _ = await m.refresh()
        await m.restore()
        #expect(flow.killBrowsers == false, "默认不勾")
        #expect(flow.prompt?.preview.browsers?.first?.app == "Google Chrome")
        flow.killBrowsers = tick
        await confirmNow(flow)
        let body = t.requests.last(where: { $0.path == "/v1/restore" })?.body.map { String(decoding: $0, as: UTF8.self) } ?? ""
        #expect(body.contains("\"confirm\""))
        #expect(body.contains("kill_browsers") == tick, "勾了才带：\(body)")
        // 新的确认框：复位成不勾
        await m.restore()
        #expect(flow.killBrowsers == false)
        flow.cancel()
    }
}
