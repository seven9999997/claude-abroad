// 阶段 B（v0.71c，docs/app-install-plan.md 3.8、3.9）：应用里安装、升级、卸载后台服务。
// 全部用假的执行层（FakeRunner）和假后台，不弹密码框、不提权、不碰系统路径。
import AbroadAPI
import Foundation
import Testing
@testable import AbroadKit

let samplePayload = PayloadInfo(commit: "2222222bbbbb", buildTime: "20261127T080000Z", dirty: false, stateFormat: 1, path: "/@PAYLOAD@")

/// 后台状态的 JSON（只放和升级判断有关的字段）。
func svcStatus(state: String = "RESTORED", build: (String, String)? = ("1111111aaaaa", "20261101T000000Z"), format: Int? = 1, extra: String = "") -> String {
    var s = "{\"state\":\"\(state)\",\"gate_open\":false,\"protected\":false,\"gate\":{\"open\":false,\"lease_left_ms\":0}"
    if let b = build { s += ",\"build\":{\"commit\":\"\(b.0)\",\"build_time\":\"\(b.1)\",\"dirty\":false}" }
    if let f = format { s += ",\"state_format\":\(f)" }
    return s + extra + "}"
}

final class FakeSvcDaemon: @unchecked Sendable {
    var status: String? = svcStatus()
    var uninstallPreview = "{\"processes\":[{\"pid\":7100,\"app\":\"Claude 桌面版\",\"cmd\":\"/Applications/Claude.app\"}],\"will_kill\":true,\"browsers\":[{\"pid\":8100,\"app\":\"Safari\",\"cmd\":\"\"}],\"removes\":[\"恢复本机网络\",\"后台服务、专用用户、日志和凭据\"],\"note\":\"执行时会按当时的情况重新列出；之后新启动的 Claude 也会被结束。\"}"
    func handle(_ r: HTTPRequest) throws -> HTTPResponse {
        switch (r.method, r.path) {
        case ("GET", "/v1/status"):
            guard let s = status else { throw URLError(.cannotConnectToHost) }
            return json(200, s)
        case ("GET", "/v1/uninstall/preview"): return json(200, uninstallPreview)
        default: return errBody(403, "needs_authorization", "需要本人授权")
        }
    }
}

@MainActor
func svcEnv(_ d: FakeSvcDaemon = FakeSvcDaemon(), payload: PayloadInfo? = samplePayload, runner: FakeRunner = FakeRunner()) -> (ServiceInstallModel, PrivilegedSession, FakeRunner, FakeTransport) {
    let t = FakeTransport { try d.handle($0) }
    let c = APIClient(transport: t)
    let s = PrivilegedSession(client: c, runner: runner, files: InputFiles(directory: tempDir()))
    return (ServiceInstallModel(client: c, payload: payload, privileged: s), s, runner, t)
}

// ---- 3.3：应用打开时看要不要显示按钮 ----

@MainActor @Test func serviceStates() async throws {
    let cases: [(String, String?, PayloadInfo?, (ServiceState) -> Bool)] = [
        ("应用包里没有 payload", svcStatus(), nil, { $0 == .noPayload }),
        ("连不上：没装", nil, samplePayload, { $0 == .notInstalled }),
        ("版本旧：可以升级", svcStatus(), samplePayload, { if case .upgradeAvailable = $0 { return true }; return false }),
        ("一样：不显示", svcStatus(build: ("2222222bbbbb", "20261127T080000Z")), samplePayload, { $0 == .upToDate }),
        ("后台更新：不降级", svcStatus(build: ("3333333ccccc", "20261128T000000Z")), samplePayload, { if case .daemonNewer = $0 { return true }; return false }),
        ("没有 build：版本不明", svcStatus(build: nil, format: nil), samplePayload, { if case .unknownVersion = $0 { return true }; return false }),
        ("状态格式不同", svcStatus(format: 2), samplePayload, { $0 == .formatDiffers(old: 2, new: 1) }),
        ("已启用：不允许", svcStatus(state: "ENABLED"), samplePayload, { $0 == .upgradeBlocked(reason: L10n.serviceBlockedEnabled) }),
        ("启用中", svcStatus(state: "ENABLING"), samplePayload, { $0 == .upgradeBlocked(reason: L10n.serviceBlockedTransition) }),
        ("回国中", svcStatus(state: "RESTORING"), samplePayload, { $0 == .upgradeBlocked(reason: L10n.serviceBlockedTransition) }),
        ("借用虚拟机中", svcStatus(extra: ",\"borrow\":{\"op\":\"main-check\",\"step\":\"starting\",\"since\":\"2026-11-27T00:00:00Z\"}"), samplePayload, { $0 == .upgradeBlocked(reason: L10n.serviceBlockedBorrow) }),
        ("忙", svcStatus(extra: ",\"busy\":true"), samplePayload, { $0 == .upgradeBlocked(reason: L10n.serviceBlockedBusy) }),
        ("卸载中", svcStatus(extra: ",\"uninstall\":{\"running\":true,\"done\":false}"), samplePayload, { $0 == .upgradeBlocked(reason: L10n.serviceBlockedUninstall) }),
        ("状态文件坏了", svcStatus(state: "UNKNOWN", extra: ",\"corrupt\":true"), samplePayload, { $0 == .upgradeBlocked(reason: L10n.serviceBlockedCorrupt) }),
        ("版本不明且已启用：不允许", svcStatus(state: "ENABLED", build: nil, format: nil), samplePayload, { $0 == .upgradeBlocked(reason: L10n.serviceBlockedEnabled) }),
    ]
    for (what, st, payload, want) in cases {
        let d = FakeSvcDaemon()
        d.status = st
        let (m, _, r, _) = svcEnv(d, payload: payload)
        await m.refresh()
        #expect(want(m.state), "\(what)：\(m.state)")
        #expect(!m.text.isEmpty, "\(what)：要有一句说明")
        #expect(r.calls.isEmpty)
    }
    // 按钮：没装显示安装；能升级、版本不明可点升级；不允许的显示但不能点；格式不同、一样、更新不显示升级
    let d = FakeSvcDaemon()
    d.status = nil
    var (m, _, _, _) = svcEnv(d)
    await m.refresh()
    #expect(m.showsInstall && !m.showsUpgrade && !m.showsUninstall)
    d.status = svcStatus(state: "ENABLED")
    (m, _, _, _) = svcEnv(d)
    await m.refresh()
    #expect(m.showsUpgrade && !m.upgradeEnabled && m.showsUninstall)
    d.status = svcStatus(format: 2)
    (m, _, _, _) = svcEnv(d)
    await m.refresh()
    #expect(!m.showsUpgrade && m.showsUninstall)
}

@Test func payloadInfoFromPlist() {
    let info: [String: Any] = ["ClaudeAbroadPayloadCommit": "abc1234", "ClaudeAbroadPayloadBuildTime": "20261127T000000Z", "ClaudeAbroadPayloadDirty": "true", "ClaudeAbroadPayloadStateFormat": "1"]
    let p = PayloadInfo.from(info: info, resourcePath: "/Applications/ClaudeAbroad.app/Contents/Resources")
    #expect(p == PayloadInfo(commit: "abc1234", buildTime: "20261127T000000Z", dirty: true, stateFormat: 1, path: "/Applications/ClaudeAbroad.app/Contents/Resources/payload"))
    #expect(PayloadInfo.from(info: [:], resourcePath: "/x") == nil, "没有 payload 的应用包")
}

// ---- 3.4、3.6：三条提权命令（golden；脚本侧场景 u 用 /bin/sh 真跑） ----

func goldenServiceCommands() throws -> String {
    let samples: [(String, ServiceAction)] = [
        ("install", .install), ("upgrade", .upgrade(acceptUnknown: false)), ("upgrade-unknown", .upgrade(acceptUnknown: true)),
        ("uninstall", .uninstall(killBrowsers: false)), ("uninstall-kill", .uninstall(killBrowsers: true)),
    ]
    var out = ""
    for (name, a) in samples {
        let payload: String? = { if case .uninstall = a { return nil }; return "/@PAYLOAD@" }()
        out += "### " + name + "\n" + (try PrivilegedCommand.buildService(a, paths: samplePaths, payload: payload)) + "\n"
    }
    return out
}

@Test func serviceCommandGolden() throws {
    let path = repoRoot.appendingPathComponent("app/Tests/AbroadKitTests/testdata/service-commands.txt")
    let got = try goldenServiceCommands()
    if ProcessInfo.processInfo.environment["ABROAD_UPDATE_PRIV_GOLDEN"] == "1" {
        try got.write(to: path, atomically: true, encoding: .utf8)
    }
    let want = try String(contentsOf: path, encoding: .utf8)
    #expect(want == got, "命令的形状变了：确认后用 ABROAD_UPDATE_PRIV_GOLDEN=1 重新生成，并跑 scripts/m6/install/selftest.sh 的场景 u")
    let ins = try PrivilegedCommand.buildService(.install, paths: samplePaths, payload: "/@PAYLOAD@")
    // 防调包：先只用系统程序复制到 root 的临时目录（解引用符号链接）并核对 payload.sha256，之后一切从副本执行
    #expect(ins.contains("/bin/cp -RL \"$P\" \"$S/p\"") && ins.contains("/usr/bin/shasum -a 256 -s -c payload.sha256") && ins.contains("RESULT=33"))
    #expect(ins.contains("/bin/bash \"$S/p/scripts/m6/install/install.sh\" --from \"$S/p\" --yes 2>&1") && !ins.contains("--upgrade"))
    let up = try PrivilegedCommand.buildService(.upgrade(acceptUnknown: true), paths: samplePaths, payload: "/@PAYLOAD@")
    #expect(up.contains("--from \"$S/p\" --yes --upgrade --accept-unknown-version"))
    // 卸载：已安装的、属主 root 的脚本（不用应用包里的），先复制再执行，带 --yes
    let un = try PrivilegedCommand.buildService(.uninstall(killBrowsers: true), paths: samplePaths, payload: nil)
    #expect(un.contains("[ -d \"$I/scripts\" ] || { echo 'RESULT=30'") && un.contains("/usr/bin/stat -f %u") && un.contains("RESULT=31"))
    #expect(un.contains("/bin/cp -R \"$I/scripts\" \"$S/p/scripts\"") && un.contains("uninstall.sh\" --yes --kill-browsers 2>&1") && !un.contains("@PAYLOAD@"))
}

@Test func serviceCommandQuotingAndRejects() throws {
    let weird = "/Volumes/a b/it's \"x\"/ClaudeAbroad.app/Contents/Resources/payload"
    let c = try PrivilegedCommand.buildService(.install, paths: samplePaths, payload: weird)
    #expect(c.contains("P=" + PrivilegedCommand.shellQuote(weird)))
    #expect(unescapeAppleScript(PrivilegedCommand.appleScriptEscape(c)) == c)
    for bad in ["/x\ny", "/x\u{0}", "/x\ty", "relative/path"] {
        #expect(throws: PrivilegedCommandError.badCharacter) { try PrivilegedCommand.buildService(.upgrade(acceptUnknown: false), paths: samplePaths, payload: bad) }
    }
    #expect(throws: PrivilegedCommandError.badCharacter) { try PrivilegedCommand.buildService(.install, paths: samplePaths, payload: nil) }
}

// ---- 只有点击才调用提权；预览；结果 ----

@MainActor @Test func serviceNoRunWithoutTap() async throws {
    let d = FakeSvcDaemon()
    d.status = nil
    let (m, s, r, _) = svcEnv(d)
    for _ in 0..<3 { await m.refresh() }
    #expect(r.calls.isEmpty && !s.isActive, "刷新、页面出现都不弹确认框、不执行")
    await m.userTappedInstall()
    #expect(s.isActive && r.calls.isEmpty, "点按钮只弹确认框")
    #expect(s.request?.lines.contains(L10n.serviceBoundary) == true, "确认框写明安全边界")
    await s.userTappedExecute()
    #expect(r.calls.count == 1 && r.calls[0].prompt == L10n.servicePrompt(.install))
    #expect(r.calls[0].command.contains("install.sh\" --from \"$S/p\" --yes") && r.calls[0].command.contains("P='/@PAYLOAD@'"))
    await s.userTappedExecute()
    #expect(r.calls.count == 1, "结果出来后再点不重复执行")
}

@MainActor @Test func upgradeUnknownNeedsConfirm() async throws {
    let d = FakeSvcDaemon()
    d.status = svcStatus(build: nil, format: nil)
    let (m, s, r, _) = svcEnv(d)
    await m.refresh()
    await m.userTappedUpgrade()
    #expect(s.request?.lines.first == L10n.serviceUnknownWarning && r.calls.isEmpty)
    await s.userTappedExecute()
    #expect(r.calls.first?.command.contains("--upgrade --accept-unknown-version") == true)
    // 版本已知：不带 --accept-unknown-version；不允许升级时点了也不弹
    let d2 = FakeSvcDaemon()
    let (m2, s2, r2, _) = svcEnv(d2)
    await m2.refresh()
    await m2.userTappedUpgrade()
    await s2.userTappedExecute()
    #expect(r2.calls.first?.command.contains("--yes --upgrade 2>&1") == true)
    d2.status = svcStatus(state: "ENABLED")
    let (m3, s3, _, _) = svcEnv(d2)
    await m3.refresh()
    await m3.userTappedUpgrade()
    #expect(!s3.isActive, "不允许升级时按钮不可点，调用也不弹确认框")
}

@MainActor @Test func uninstallUsesReadOnlyPreview() async throws {
    let (m, s, r, t) = svcEnv()
    await m.refresh()
    await m.userTappedUninstall()
    #expect(t.requests.contains { $0.method == "GET" && $0.path == "/v1/uninstall/preview" })
    #expect(!t.requests.contains { $0.method == "POST" }, "不发改动类请求")
    let req = try #require(s.request)
    #expect(req.removes.count == 2 && req.preview?.processes?.first?.pid == 7100 && req.browsersOption && req.note == L10n.privRecheck && req.host == .settings)
    s.killBrowsers = true
    await s.userTappedExecute()
    #expect(r.calls.first?.command.contains("uninstall.sh\" --yes --kill-browsers") == true)
    #expect(r.calls.first?.prompt == L10n.servicePrompt(.uninstall(killBrowsers: true)))
}

@MainActor @Test func serviceOutcomeMapping() async throws {
    for (a, out, ok, msg) in [
        (ServiceAction.install, "装好了\nRESULT=0\n", true, L10n.serviceExit(0, .install)),
        (.upgrade(acceptUnknown: false), "RESULT=0\n", true, L10n.serviceExit(0, .upgrade(acceptUnknown: false))),
        (.uninstall(killBrowsers: false), "RESULT=0\n", true, L10n.serviceUninstalledTrash),
        (.upgrade(acceptUnknown: false), "升级失败，已退回旧版本：x\nRESULT=20\n", false, L10n.serviceExit(20, .install)),
        (.install, "RESULT=33\n", false, L10n.serviceExit(33, .install)),
        (.uninstall(killBrowsers: false), "RESULT=30\n", false, L10n.serviceExit(30, .install)),
        (.upgrade(acceptUnknown: false), "RESULT=10\n", false, L10n.serviceExit(10, .install)),
    ] {
        let runner = FakeRunner()
        runner.result = .finished(stdout: out)
        let d = FakeSvcDaemon()
        if case .install = a { d.status = nil }
        let (m, s, _, _) = svcEnv(d, runner: runner)
        await m.refresh()
        switch a {
        case .install: await m.userTappedInstall()
        case .upgrade: await m.userTappedUpgrade()
        case .uninstall: await m.userTappedUninstall()
        }
        await s.userTappedExecute()
        guard case let .done(gotOK, gotMsg, _) = s.phase else { Issue.record("没有结果：\(a)"); continue }
        #expect(gotOK == ok && gotMsg == msg, "\(a) \(out)")
    }
    #expect(L10n.serviceUninstalledTrash.contains("可以自己移到废纸篓"))
}

/// 密码框的提示文字（计划 3.5 ③）。
@Test func servicePromptsFromStringTable() {
    #expect(L10n.servicePrompt(.install) == "安装 / 升级 Claude 出国的后台服务（会以管理员身份运行）")
    #expect(L10n.servicePrompt(.upgrade(acceptUnknown: true)) == L10n.servicePrompt(.install))
    #expect(L10n.servicePrompt(.uninstall(killBrowsers: false)) == "卸载 Claude 出国的后台服务并恢复本机网络（会以管理员身份运行）")
}

// ---- B0 建议改⑤：确认框里的“现在：…” ----

@MainActor @Test func currentLineShown() async throws {
    let d = FakePrivDaemon()
    let t = FakeTransport { r in
        switch (r.method, r.path) {
        case ("GET", "/v1/region"): return json(200, "{\"target\":{\"timezone\":\"Asia/Tokyo\",\"lang\":\"ja-JP\",\"locale\":\"ja_JP\",\"source\":\"manual\"}}")
        case ("GET", "/v1/main-proxy"): return json(200, "{\"selected\":{\"id\":\"n1\",\"name\":\"东京 1\",\"type\":\"ss\"},\"applied\":true,\"needs_reselect\":false,\"lan_pending\":false,\"sources\":2}")
        case ("GET", "/v1/rules?format=json"): return json(200, "{\"default\":\"proxy:main\",\"rules\":[{\"app\":\"com.x\",\"action\":\"direct\"}],\"offline_allow\":[],\"local_domains\":[\"intra.example\"],\"pinned_extra\":[],\"block_udp_443\":true}")
        case ("GET", "/v1/exits"): return json(200, "{\"exits\":[]}")
        default: return d.handle(r)
        }
    }
    let c = APIClient(transport: t)
    let s = PrivilegedSession(client: c, runner: FakeRunner())
    for (op, want) in [
        (SensitiveOperation.regionSet(timezone: "Europe/Paris", lang: "fr-FR"), "现在：Asia/Tokyo、ja-JP"),
        (.mainUse(node: "n2", via: []), "现在：主代理选的是 东京 1（已下发），共 2 个来源"),
        (.appSet(app: "com.x", action: "block"), "现在：com.x → direct"),
        (.ruleListAdd(kind: "local", item: "intra.example"), "现在：intra.example 已在名单里（名单共 1 项）"),
        (.udp443(on: false), "现在：开"),
    ] {
        await s.present([op], terminal: TerminalPrompt(commands: []), onTerminal: { _ in }, onDone: {})
        #expect(s.request?.lines.first == want, "\(op)：\(s.request?.lines.first ?? "")")
        await s.userClosed()
    }
}
