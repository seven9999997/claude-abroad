// v0.72（docs/restore-claudeproxy-plan.md v3，验收 k）：回国确认里选第四道。应用的回国确认框是 B0 的 PrivilegedSheet，
// 只读预览 GET /v1/restore/preview 给出第四道的现状、测试模式和冲突；勾选初值等于现状；执行的命令和终端命令都从
// SensitiveOperation.arguments 生成（只在和现状不同时带 --claude-proxy）。全部用假的执行层，不弹密码框、不提权。
import AbroadAPI
import Foundation
import Testing
@testable import AbroadKit

private func previewJSON(current: Bool?, testMode: Bool = false, conflicts: String = "") -> String {
    let cp = current.map { ",\"claude_proxy\":{\"current\":\($0),\"target\":\($0),\"test_mode\":\(testMode)" + (conflicts.isEmpty ? "" : ",\"conflicts\":[\(conflicts)]") + "}" } ?? ""
    return "{\"processes\":[{\"pid\":7100,\"app\":\"Claude 桌面版\",\"cmd\":\"/Applications/Claude.app\"}],\"will_kill\":true,\"browsers\":[{\"pid\":8100,\"app\":\"Safari\",\"cmd\":\"\"}],\"note\":\"x\"" + cp + "}"
}

@MainActor
private func restoreEnv(current: Bool?, testMode: Bool = false, conflicts: String = "", runner: FakeRunner = FakeRunner()) async -> (StatusModel, PrivilegedSession, FakeRunner, FakeTransport) {
    let d = FakePrivDaemon()
    d.restorePreview = previewJSON(current: current, testMode: testMode, conflicts: conflicts)
    let (c, s, r, t, _) = privEnv(d, runner: runner)
    let m = StatusModel(client: c, authorizer: PrivilegedAuthorizer(), sleeper: NoSleep(), flow: fakeFlow(c), privileged: s)
    await m.restore()
    return (m, s, r, t)
}

/// 预览里有这一块才有勾选，初值等于现状；没有这一块时命令不带参数。
@MainActor @Test func restoreClaudeProxyToggle() async throws {
    let (_, s0, r0, _) = await restoreEnv(current: nil)
    #expect(s0.request?.claudeProxy == nil && s0.claudeProxyLines.isEmpty && s0.claudeProxyRequest == nil)
    s0.claudeProxyOn = true // 没有这一块时勾选不起作用
    await s0.userTappedExecute()
    #expect(r0.calls.first?.command.contains("--yes --json 'restore' >\"$S/o1\"") == true)
    for cur in [false, true] {
        let (_, s, _, _) = await restoreEnv(current: cur)
        #expect(s.request?.claudeProxy?.current == cur && s.claudeProxyOn == cur, "初值等于现状")
        #expect(s.claudeProxyRequest == nil)
    }
}

/// 参数只有一份：勾选和现状不同才带 --claude-proxy，--kill-browsers 也从 arguments 来；提权命令和终端命令同一份参数。
@MainActor @Test func restoreClaudeProxyArguments() async throws {
    #expect(SensitiveOperation.restoreWith(killBrowsers: false, claudeProxy: nil).arguments == ["restore"])
    #expect(SensitiveOperation.restoreWith(killBrowsers: true, claudeProxy: true).arguments == ["restore", "--kill-browsers", "--claude-proxy", "on"])
    #expect(SensitiveOperation.restoreWith(killBrowsers: false, claudeProxy: false).terminalCommand == "sudo abroad restore --claude-proxy off")
    for (cur, tick, want) in [(false, true, "'restore' '--kill-browsers' '--claude-proxy' 'on'"), (true, false, "'restore' '--kill-browsers' '--claude-proxy' 'off'"),
                              (false, false, "'restore' '--kill-browsers' >\"$S/o1\""), (true, true, "'restore' '--kill-browsers' >\"$S/o1\"")] {
        let (_, s, r, t) = await restoreEnv(current: cur)
        s.killBrowsers = true
        s.claudeProxyOn = tick
        await s.userTappedExecute()
        let cmd = try #require(r.calls.first?.command)
        #expect(cmd.contains("--yes --json " + want), "现状 \(cur)、勾选 \(tick)：\(cmd)")
        #expect(t.requests.filter { $0.method == "POST" && $0.path == "/v1/restore" }.count == 1, "回国全程只有原来那一次被拒的 POST，预览走只读接口")
    }
}

/// 终端退路：按当前勾选重新生成，和提权命令的参数一致。
@MainActor @Test func restoreClaudeProxyTerminalMatches() async throws {
    let (m, s, r, _) = await restoreEnv(current: false)
    s.claudeProxyOn = true
    s.userTappedTerminal()
    #expect(m.terminalCommand == "sudo abroad restore --claude-proxy on" && r.calls.isEmpty)
    let (m2, s2, _, _) = await restoreEnv(current: true)
    s2.killBrowsers = true
    s2.userTappedTerminal()
    #expect(m2.terminalCommand == "sudo abroad restore --kill-browsers", "和现状一样时不带 --claude-proxy")
}

/// 裁定③之一：勾选打开、只读预览带回冲突：“执行”不可点、列出冲突（文件和变量）；取消勾选恢复可点。
@MainActor @Test func restoreClaudeProxyConflictBlocks() async throws {
    let (_, s, r, _) = await restoreEnv(current: false, conflicts: "{\"file\":\"corp.json\",\"var\":\"HTTPS_PROXY\"},{\"file\":\"bad.json\",\"var\":\"\"}")
    #expect(!s.claudeProxyBlocked, "没勾选时不挡")
    s.claudeProxyOn = true
    #expect(s.claudeProxyBlocked && s.claudeProxyConflicts == ["corp.json 的 HTTPS_PROXY", "bad.json（解析不了）"])
    #expect(L10n.restoreClaudeProxyBlocked(s.claudeProxyConflicts).contains("corp.json 的 HTTPS_PROXY"))
    await s.userTappedExecute()
    #expect(r.calls.isEmpty && s.phase == .confirming, "冲突时点了也不执行")
    s.claudeProxyOn = false
    #expect(!s.claudeProxyBlocked)
    await s.userTappedExecute()
    #expect(r.calls.count == 1)
    // 现在开着时，冲突不挡（打开会撞上的冲突和关掉无关）
    let (_, s2, _, _) = await restoreEnv(current: true, conflicts: "{\"file\":\"corp.json\",\"var\":\"HTTPS_PROXY\"}")
    #expect(!s2.claudeProxyBlocked)
}

/// 裁定③之二：只读预览之后才出现的冲突：root 运行的命令行第一步回 422（退出码 1），应用显示冲突文字，什么都没改。
@MainActor @Test func restoreClaudeProxyLateConflict() async throws {
    let runner = FakeRunner()
    runner.result = .finished(stdout: "{\"code\":\"invalid\",\"field\":\"claude_proxy\",\"error\":\"回国前检查第四道失败：…\",\"conflicts\":[{\"file\":\"corp.json\",\"var\":\"HTTPS_PROXY\"}]}\nRESULT=1\n")
    let (_, s, _, _) = await restoreEnv(current: false, runner: runner)
    s.claudeProxyOn = true
    await s.userTappedExecute()
    guard case let .done(ok, msg, _) = s.phase else { Issue.record("没有结果"); return }
    #expect(!ok && msg == L10n.restoreClaudeProxyConflicts(["corp.json 的 HTTPS_PROXY"]) && msg.contains("什么都没有改"))
    #expect(!s.warning)
}

/// 裁定④：回国成功、切换失败：警告样式，文字“回国已完成，但第四道没有切换”加原因；成功时写已打开/已关闭。
@MainActor @Test func restoreClaudeProxyResult() async throws {
    let runner = FakeRunner()
    runner.result = .finished(stdout: "{\"state\":\"RESTORED\",\"claude_proxy_change\":{\"target\":true,\"ok\":false,\"notes\":[\"第四道没有打开：监听失败\",\"处理后执行 sudo abroad claude-proxy on\"]}}\nRESULT=0\n")
    let (_, s, _, _) = await restoreEnv(current: false, runner: runner)
    s.claudeProxyOn = true
    await s.userTappedExecute()
    guard case let .done(ok, msg, _) = s.phase else { Issue.record("没有结果"); return }
    #expect(ok && s.warning && msg.hasPrefix("回国已完成，但第四道没有切换：") && msg.contains("监听失败") && msg.contains("sudo abroad claude-proxy on"))
    await s.userClosed()
    runner.result = .finished(stdout: "{\"state\":\"RESTORED\",\"claude_proxy_change\":{\"target\":false,\"ok\":true}}\nRESULT=0\n")
    let (_, s2, _, _) = await restoreEnv(current: true, runner: runner)
    s2.claudeProxyOn = false
    await s2.userTappedExecute()
    #expect(s2.phase == .done(ok: true, message: L10n.restoreClaudeProxyChangeOff, output: "{\"state\":\"RESTORED\",\"claude_proxy_change\":{\"target\":false,\"ok\":true}}") && !s2.warning)
    // 没要求切换：照旧“已完成”
    runner.result = .finished(stdout: "{\"state\":\"RESTORED\"}\nRESULT=0\n")
    let (_, s3, _, _) = await restoreEnv(current: false, runner: runner)
    await s3.userTappedExecute()
    #expect(s3.phase == .done(ok: true, message: L10n.privExit(0), output: "{\"state\":\"RESTORED\"}"))
}

/// 文字来自文字表：三种目标状态和测试模式。
@Test func restoreClaudeProxyLines() {
    #expect(L10n.claudeProxyLines(current: false, target: false, testMode: false) == [L10n.restoreClaudeProxyKeepOff])
    #expect(L10n.claudeProxyLines(current: true, target: true, testMode: false) == [L10n.restoreClaudeProxyKeepOn])
    #expect(L10n.claudeProxyLines(current: false, target: true, testMode: false) == [L10n.restoreClaudeProxyOnTitle] + L10n.claudeCodeConsequences)
    #expect(L10n.claudeProxyLines(current: true, target: false, testMode: true) == [L10n.restoreClaudeProxyOffNote, L10n.restoreClaudeProxyTestNote])
    #expect(L10n.restoreClaudeProxyOffNote == "回国后 Claude Code 不再被指向本机代理，回国后靠解析锁和回国拦截挡住它（默认状态）")
}
