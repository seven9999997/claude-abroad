// B0（v0.71b，docs/app-privileged-plan.md v2 第 2 节的 10 条）：应用里经系统密码框执行敏感操作。
// 全部用假的执行层（只记下传了什么命令、什么时候调用），不弹密码框、不提权、不碰系统路径。
import AbroadAPI
import Foundation
import Testing
@testable import AbroadKit

/// 假的执行层：记下命令和提示，按设定返回。
final class FakeRunner: PrivilegedRunning, @unchecked Sendable {
    private let lock = NSLock()
    private var _calls: [(command: String, prompt: String)] = []
    var result: PrivilegedRunResult = .finished(stdout: "{}\nRESULT=0\n")
    let paths = PrivilegedPaths(installDir: "@INST@", staging: "@STG@")

    var calls: [(command: String, prompt: String)] { lock.lock(); defer { lock.unlock() }; return _calls }

    func run(command: String, prompt: String) async -> PrivilegedRunResult {
        lock.lock(); _calls.append((command, prompt)); let r = result; lock.unlock()
        return r
    }
}

/// 一个假的后台：敏感操作一律 403 要本人授权；只读预览按设定返回。
final class FakePrivDaemon: @unchecked Sendable {
    var enablePreview = "{\"can_enable\":true,\"note\":\"执行时会按当时的情况重新列出；之后新启动的 Claude 也会被结束。\"}"
    var restorePreview = "{\"processes\":[{\"pid\":7100,\"app\":\"Claude 桌面版\",\"cmd\":\"/Applications/Claude.app\"}],\"will_kill\":true,\"browsers\":[{\"pid\":8100,\"app\":\"Safari\",\"cmd\":\"\"}]}"
    func handle(_ r: HTTPRequest) -> HTTPResponse {
        switch (r.method, r.path) {
        case ("GET", "/v1/status"): return json(200, statusJSON(state: "RESTORED", gate: false))
        case ("GET", "/v1/enable/preview"): return json(200, enablePreview)
        case ("GET", "/v1/restore/preview"): return json(200, restorePreview)
        case ("GET", "/v1/main-proxy/sources"): return json(200, "[]")
        case ("GET", "/v1/main-proxy"): return json(200, "{}")
        case ("GET", "/v1/exits"): return json(200, "{\"exits\":[]}")
        case ("GET", _): return json(200, "{}")
        default: return errBody(403, "needs_authorization", "需要本人授权：这是敏感操作")
        }
    }
}

@MainActor
func privEnv(_ d: FakePrivDaemon = FakePrivDaemon(), runner: FakeRunner = FakeRunner(), dir: URL = tempDir()) -> (APIClient, PrivilegedSession, FakeRunner, FakeTransport, InputFiles) {
    let t = FakeTransport { d.handle($0) }
    let c = APIClient(transport: t)
    let files = InputFiles(directory: dir)
    return (c, PrivilegedSession(client: c, runner: runner, files: files), runner, t, files)
}

let samplePaths = PrivilegedPaths(installDir: "@INST@", staging: "@STG@")
let zeroDigest = String(repeating: "a", count: 64)

// ---- 第 7 条：转义和拒绝（和阶段 B 共用一套） ----

/// sh 单引号的最小解析（测试用）：'…' 里原样，引号外只认 \' 。
func unquoteShell(_ s: String) -> String? {
    var out = "", inQ = false, i = s.startIndex
    while i < s.endIndex {
        let c = s[i]
        if inQ { if c == "'" { inQ = false } else { out.append(c) } } else if c == "'" { inQ = true } else if c == "\\" {
            i = s.index(after: i); guard i < s.endIndex else { return nil }; out.append(s[i])
        } else { return nil }
        i = s.index(after: i)
    }
    return inQ ? nil : out
}

/// AppleScript 字符串字面量里的最小反转义（测试用）。
func unescapeAppleScript(_ s: String) -> String? {
    var out = "", i = s.startIndex
    while i < s.endIndex {
        let c = s[i]
        if c == "\\" { i = s.index(after: i); guard i < s.endIndex else { return nil }; out.append(s[i]) } else if c == "\"" { return nil } else { out.append(c) }
        i = s.index(after: i)
    }
    return out
}

@Test func shellQuoteCases() {
    for s in ["plain", "with space", "it's", "\"dq\"", "$HOME", "`id`", "back\\slash", "中文 名字", ";rm -rf /", "a'b'c", ""] {
        let q = PrivilegedCommand.shellQuote(s)
        #expect(q.hasPrefix("'"))
        #expect(unquoteShell(q) == s, "单引号后应能还原：\(s)")
    }
}

@Test func appleScriptRoundTrip() {
    for s in ["echo 'a b'", "x \"y\" z", "c:\\path\\file", "'it'\\''s'", "RESULT=$rc; exit 0"] {
        let e = PrivilegedCommand.appleScriptEscape(s)
        #expect(unescapeAppleScript(e) == s)
        #expect(unescapeAppleScript(e).flatMap(unquoteShell(_:)) == unquoteShell(s) || unquoteShell(s) == nil)
    }
    // 整条命令：两层转义后还原，参数原样
    let op = SensitiveOperation.regionSet(timezone: "Asia/Tok'yo", lang: "x\"y\\z")
    let cmd = try! PrivilegedCommand.build([PrivilegedStep(op)], paths: samplePaths, digests: [:])
    #expect(unescapeAppleScript(PrivilegedCommand.appleScriptEscape(cmd)) == cmd)
    #expect(cmd.contains(PrivilegedCommand.shellQuote("Asia/Tok'yo")) && cmd.contains(PrivilegedCommand.shellQuote("x\"y\\z")))
}

@MainActor @Test func rejectsControlChars() async throws {
    for bad in ["a\nb", "a\u{0}b", "a\tb", "a\u{7F}b", "a\rb"] {
        #expect(throws: PrivilegedCommandError.badCharacter) {
            try PrivilegedCommand.build([PrivilegedStep(.ruleListAdd(kind: "local", item: bad))], paths: samplePaths, digests: [:])
        }
    }
    // 经确认框：拼不出命令时不调用执行层
    let (_, s, r, _, _) = privEnv()
    await s.present([.ruleListAdd(kind: "local", item: "x\ny")], terminal: TerminalPrompt(commands: []), onTerminal: { _ in }, onDone: {})
    await s.userTappedExecute()
    #expect(r.calls.isEmpty)
    #expect(s.phase == .done(ok: false, message: L10n.privBadChars, output: ""))
}

// ---- 第 1、4 条：命令的形状（golden，脚本侧 scripts/m6/install/selftest.sh 场景 u 真跑这几条） ----

func goldenCommands() throws -> String {
    let samples: [(String, [PrivilegedStep])] = [
        ("addsource", [PrivilegedStep(.addSourceURLFile(path: "@IN@", name: "订阅", viaHost: false))]),
        ("restore-kill", [PrivilegedStep(.restoreWith(killBrowsers: true, claudeProxy: nil))]),
        ("mainuse-apply", [PrivilegedStep(.mainUse(node: "n1", via: ["n2"])), PrivilegedStep(.mainApply)]),
        // v0.72：回国确认里选第四道（参数从 arguments 来）
        ("restore-cp-on", [PrivilegedStep(.restoreWith(killBrowsers: false, claudeProxy: true))]),
        ("restore-kill-cp-off", [PrivilegedStep(.restoreWith(killBrowsers: true, claudeProxy: false))]),
    ]
    var out = ""
    for (name, steps) in samples {
        out += "### " + name + "\n" + (try PrivilegedCommand.build(steps, paths: samplePaths, digests: ["@IN@": zeroDigest])) + "\n"
    }
    return out
}

@Test func privilegedCommandGolden() throws {
    let path = repoRoot.appendingPathComponent("app/Tests/AbroadKitTests/testdata/privileged-commands.txt")
    let got = try goldenCommands()
    if ProcessInfo.processInfo.environment["ABROAD_UPDATE_PRIV_GOLDEN"] == "1" {
        try got.write(to: path, atomically: true, encoding: .utf8)
    }
    let want = try String(contentsOf: path, encoding: .utf8)
    #expect(want == got, "命令的形状变了：确认后用 ABROAD_UPDATE_PRIV_GOLDEN=1 重新生成，并跑 scripts/m6/install/selftest.sh 的场景 u")
    // 第 1 条：先检查已安装的命令行、bin、安装目录（属主 root、组和其他人不可写、不是符号链接），不对就退出码 31
    let c = try PrivilegedCommand.build([PrivilegedStep(.mainApply)], paths: samplePaths, digests: [:])
    let lines = c.split(separator: "\n").map(String.init)
    #expect(lines[1] == "A='@INST@/bin/abroad'; S='@STG@'")
    #expect(lines[2].hasPrefix("for p in \"$A\" '@INST@/bin' '@INST@'; do") && lines[2].contains("/usr/bin/stat -f %u") && lines[2].contains("& 022") && lines[2].contains("RESULT=31"))
    #expect(lines[3] == "\"$A\" --yes --json 'main' 'apply'; rc=$?; [ $rc = 0 ] || { /bin/rm -rf \"$S\"; echo \"RESULT=$rc\"; exit 0; }")
    // 第 4 条：输入文件先复制到 root 的临时目录、核对摘要，对不上退出码 32；交给命令行的是副本
    let a = try PrivilegedCommand.build([PrivilegedStep(.addNodeFile(path: "/tmp/n.yaml", name: "家里"))], paths: samplePaths, digests: ["/tmp/n.yaml": zeroDigest])
    #expect(a.contains("/bin/mkdir -m 700 \"$S\" && /usr/sbin/chown root:wheel \"$S\" && /bin/cp '/tmp/n.yaml' '@STG@/in1'"))
    #expect(a.contains("/usr/bin/shasum -a 256 '@STG@/in1'") && a.contains("= '\(zeroDigest)' ]") && a.contains("RESULT=32"))
    #expect(a.contains("--yes --json 'main' 'add-node' '--file' '@STG@/in1' '--name' '家里'") && !a.contains("--file '/tmp/n.yaml'"))
    // 没有摘要不拼命令
    #expect(throws: PrivilegedCommandError.missingDigest) {
        try PrivilegedCommand.build([PrivilegedStep(.addNodeFile(path: "/tmp/n.yaml", name: "x"))], paths: samplePaths, digests: [:])
    }
}

// ---- 第 2 条：白名单 ----

let sampleOps: [SensitiveOperation] = [
    .enable, .restore, .mainApply, .regionSet(timezone: "Asia/Tokyo", lang: "ja-JP"), .regionAuto,
    .addSourceFile(path: "/t/a", name: "n"), .addSourceURLFile(path: "/t/u", name: "n", viaHost: true), .addNodeFile(path: "/t/n", name: "n"),
    .removeSource(id: "s1"), .mainUse(node: "n1", via: ["n2"]), .lanConfirm(node: "n9"), .exitAdd(tag: "wg", kind: "wireguard", path: "/t/w"),
    .exitRemove(tag: "wg"), .appSet(app: "com.x", action: "direct"), .appRemove(app: "com.x"),
    .ruleListAdd(kind: "local", item: "a"), .ruleListRemove(kind: "local", item: "a"), .ruleListAdd(kind: "pinned", item: "a"), .ruleListRemove(kind: "pinned", item: "a"),
    .ruleListAdd(kind: "offline", item: "a"), .ruleListRemove(kind: "offline", item: "a"), .ruleListAdd(kind: "nofake", item: "a"), .ruleListRemove(kind: "nofake", item: "a"),
    .udp443(on: true), .allowLAN(on: true), .allowLAN(on: false), .claudeProxy(on: true), .claudeProxy(on: false),
    .restoreWith(killBrowsers: true, claudeProxy: nil), .restoreWith(killBrowsers: false, claudeProxy: true), .restoreWith(killBrowsers: true, claudeProxy: false),
]

@Test func privilegedWhitelistFile() throws {
    let txt = try readRepo("app/Tests/AbroadKitTests/testdata/privileged-subcommands.txt")
    let rows = txt.split(separator: "\n").filter { !$0.hasPrefix("#") && !$0.isEmpty }.map { $0.split(separator: "\t", omittingEmptySubsequences: false).map(String.init) }
    let words = rows.map { $0[0] }
    #expect(words == PrivilegedSubcommand.allCases.map(\.rawValue), "白名单和 testdata/privileged-subcommands.txt 不一致")
    // v0.72：第三列“允许的参数”和 allowedFlags 一致
    for (row, sub) in zip(rows, PrivilegedSubcommand.allCases) {
        #expect(row.count == 3, "每行三列：\(row)")
        let flags = row.count == 3 ? row[2].split(separator: " ").map(String.init) : []
        #expect(flags == sub.allowedFlags, "\(sub.rawValue) 的允许参数和文件里不一致")
    }
    #expect(PrivilegedSubcommand.restore.allowedFlags == ["--kill-browsers", "--claude-proxy=on|off"])
    var used = Set<PrivilegedSubcommand>()
    for op in sampleOps {
        let m = try #require(PrivilegedSubcommand.matching(op.arguments), "\(op) 不在白名单里")
        used.insert(m)
        // 参数只有一份：终端命令就是 sudo abroad 加同一份参数
        #expect(op.terminalCommand == (["sudo", "abroad"] + op.arguments.map(SensitiveOperation.quote)).joined(separator: " "))
    }
    #expect(used == Set(PrivilegedSubcommand.allCases), "每个白名单项都有对应的操作")
}

@Test func privilegedWhitelistRejectsUnknown() {
    for args in [["uninstall"], ["down"], ["main", "refresh", "s1"], ["domains", "clear"], ["check", "history", "clear"], ["status"]] {
        #expect(PrivilegedSubcommand.matching(args) == nil, "\(args) 不应在白名单里")
    }
    // v0.72 反例：子命令在白名单里，但参数不在允许的参数里（或值不对）时拒绝，不拼命令
    for args in [["restore", "--force"], ["restore", "--claude-proxy", "maybe"], ["restore", "--claude-proxy"], ["lan", "on", "--yes"]] {
        #expect(throws: PrivilegedCommandError.self, "\(args) 应被拒绝") {
            try PrivilegedCommand.checkFlags(PrivilegedSubcommand.matching(args)!, Array(args.dropFirst(PrivilegedSubcommand.matching(args)!.words.count)))
        }
    }
    for args in [["restore", "--kill-browsers", "--claude-proxy", "on"], ["restore", "--claude-proxy", "off"], ["main", "use", "n1", "--via", "n2"]] {
        #expect(throws: Never.self) {
            try PrivilegedCommand.checkFlags(PrivilegedSubcommand.matching(args)!, Array(args.dropFirst(PrivilegedSubcommand.matching(args)!.words.count)))
        }
    }
}

// ---- 第 3、4 条：秘密只经文件、摘要在点击时算 ----

@MainActor @Test func secretsNotInCommand() async throws {
    let dir = tempDir()
    defer { try? FileManager.default.removeItem(at: dir) }
    let secretURL = "https://sub.example/api?token=SECRET-TOKEN-xyz"
    let pass = "p@ssw0rd-123"
    let key = "PrivateKey = AAAAsecretKEY="
    // 订阅地址、手动节点：主代理页
    for kind in [AddKind.subscription, .manualNode, .localFile] {
        let d = FakePrivDaemon()
        let (c, s, r, _, files) = privEnv(d, dir: dir)
        let m = MainProxyModel(client: c, authorizer: PrivilegedAuthorizer(), files: files, flow: fakeFlow(c), privileged: s)
        m.addKind = kind
        m.formName = "订阅"
        switch kind {
        case .subscription: m.formURL = secretURL
        case .manualNode: m.nodeServer = "192.0.2.10"; m.nodePort = "8080"; m.nodeType = "socks5"; m.nodeUser = "u"; m.nodePassword = pass
        case .localFile:
            let f = dir.appendingPathComponent("mine-\(UUID().uuidString).yaml")
            try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
            try "password: \(pass)\n".write(to: f, atomically: true, encoding: .utf8)
            m.formFile = f
        }
        await m.submitAdd()
        #expect(s.isActive && r.calls.isEmpty, "弹确认框，不执行")
        await s.userTappedExecute()
        let cmd = try #require(r.calls.first?.command)
        let wrapped = PrivilegedCommand.appleScriptEscape(cmd)
        for x in [secretURL, "SECRET-TOKEN-xyz", pass, "192.0.2.10"] {
            #expect(!cmd.contains(x) && !wrapped.contains(x), "命令里出现了秘密：\(x)")
        }
        let file = try #require(s.request?.inputFiles.first)
        let sum = try InputFiles.sha256(of: URL(fileURLWithPath: file))
        #expect(cmd.contains(PrivilegedCommand.shellQuote(file)) && cmd.contains(sum))
        await s.userClosed()
    }
    // 出口配置：分流出口页
    let conf = dir.appendingPathComponent("wg.conf")
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    try "[Interface]\n\(key)\n".write(to: conf, atomically: true, encoding: .utf8)
    let (c, s, r, _, _) = privEnv(dir: dir)
    let e = ExitsModel(client: c, authorizer: PrivilegedAuthorizer(), flow: fakeFlow(c), privileged: s)
    e.formTag = "wg"
    e.formKind = "wireguard"
    e.formFile = conf
    await e.add()
    await s.userTappedExecute()
    let cmd = try #require(r.calls.first?.command)
    #expect(!cmd.contains("AAAAsecretKEY") && !cmd.contains(conf.path), "出口配置只经临时文件，命令里也没有用户自己的路径")
    await s.userClosed()
}

@MainActor @Test func inputHashInCommand() async throws {
    let dir = tempDir()
    defer { try? FileManager.default.removeItem(at: dir) }
    let (_, s, r, _, files) = privEnv(dir: dir)
    let f = try files.writePrivate("https://sub.example/x\n", ext: "txt")
    await s.present([.addSourceURLFile(path: f.path, name: "x", viaHost: false)], tempFiles: [f], terminal: TerminalPrompt(commands: []), onTerminal: { _ in }, onDone: {})
    // 确认框出现之后、点“执行”之前改了文件：命令里是点击那一刻的摘要（提权 shell 里核对的是这个）
    try "https://evil.example/\n".write(to: f, atomically: true, encoding: .utf8)
    let now = try InputFiles.sha256(of: f)
    await s.userTappedExecute()
    let cmd = try #require(r.calls.first?.command)
    #expect(cmd.contains("= '\(now)' ]") && now.count == 64)
}

@MainActor @Test func inputFilesCleanedUp() async throws {
    let dir = tempDir()
    defer { try? FileManager.default.removeItem(at: dir) }
    for path in ["done", "cancel", "fail"] {
        let runner = FakeRunner()
        if path == "fail" { runner.result = .finished(stdout: "RESULT=32\n") }
        let (_, s, _, _, files) = privEnv(runner: runner, dir: dir)
        let f = try files.writePrivate("x\n", ext: "txt")
        var refreshed = 0
        await s.present([.addNodeFile(path: f.path, name: "n")], tempFiles: [f], terminal: TerminalPrompt(commands: []), onTerminal: { _ in }, onDone: { refreshed += 1 })
        if path != "cancel" { await s.userTappedExecute() }
        await s.userClosed()
        #expect(!FileManager.default.fileExists(atPath: f.path), "\(path)：临时文件应删掉")
        #expect(refreshed == 1 && !s.isActive)
    }
}

// ---- 第 5 条：预览 ----

@MainActor @Test func restoreUsesReadOnlyPreview() async throws {
    let (c, s, r, t, _) = privEnv()
    let m = StatusModel(client: c, authorizer: PrivilegedAuthorizer(), sleeper: NoSleep(), flow: fakeFlow(c), privileged: s)
    await m.restore()
    #expect(t.requests.contains { $0.method == "GET" && $0.path == "/v1/restore/preview" })
    #expect(t.requests.filter { $0.method == "POST" && $0.path == "/v1/restore" }.count == 1, "只有原来那一次（被拒的）请求，预览走只读接口")
    let req = try #require(s.request)
    #expect(req.preview?.processes?.first?.pid == 7100 && req.browsersOption && req.note == L10n.privRecheck)
    #expect(r.calls.isEmpty)
    s.killBrowsers = true
    await s.userTappedExecute()
    let cmd = try #require(r.calls.first?.command)
    #expect(cmd.contains("--yes --json 'restore' '--kill-browsers'"))
}

@MainActor @Test func enableUsesReadOnlyPreview() async throws {
    let d = FakePrivDaemon()
    d.enablePreview = "{\"can_enable\":true,\"region\":{\"from\":\"Europe/Berlin\",\"to\":\"Asia/Tokyo\"},\"preview\":{\"processes\":[{\"pid\":7200,\"app\":\"Claude Code\",\"cmd\":\"claude\"}],\"will_kill\":true},\"note\":\"执行时会按当时的情况重新列出；之后新启动的 Claude 也会被结束。\"}"
    let (c, s, r, t, _) = privEnv(d)
    let m = StatusModel(client: c, authorizer: PrivilegedAuthorizer(), sleeper: NoSleep(), flow: fakeFlow(c), privileged: s)
    await m.enable()
    #expect(t.requests.contains { $0.method == "GET" && $0.path == "/v1/enable/preview" })
    let req = try #require(s.request)
    #expect(req.region?.to == "Asia/Tokyo" && req.preview?.processes?.first?.pid == 7200 && req.note == L10n.privRecheck && req.blocked == nil)
    await s.userTappedExecute()
    #expect(r.calls.first?.command.contains("--yes --json 'up'") == true)
    await s.userClosed()
    // 现在不能启用：确认框写出原因，“执行”不可点（界面），点了也不执行之外的东西由后台判断
    d.enablePreview = "{\"can_enable\":false,\"blocker\":\"no_baseline\",\"reason\":\"还没有出口基准：先选好主代理节点并应用\",\"note\":\"x\"}"
    await m.enable()
    #expect(s.request?.blocked == L10n.privEnableBlocked + "还没有出口基准：先选好主代理节点并应用")
    await s.userClosed()
}

@MainActor @Test func previewHasRecheckNote() {
    #expect(L10n.privRecheck == "执行时会按当时的情况重新列出；之后新启动的 Claude 也会被结束。")
}

/// 第四道：打开时显示三句后果，关闭时显示“关掉之后，回国后靠解析锁和回国拦截”；在设置窗口里显示。
@MainActor @Test func claudeProxyConfirmShowsConsequences() async throws {
    let (c, s, r, _, _) = privEnv()
    let m = SettingsModel(client: c, authorizer: PrivilegedAuthorizer(), privileged: s)
    await m.userTappedPrivileged(.claudeProxy(on: true))
    #expect(s.request?.lines == L10n.claudeCodeConsequences && s.request?.host == .settings && r.calls.isEmpty)
    await s.userTappedExecute()
    #expect(r.calls.first?.command.contains("--yes --json 'claude-proxy' 'on'") == true)
    await s.userClosed()
    await m.userTappedPrivileged(.claudeProxy(on: false))
    #expect(s.request?.lines == [L10n.privClaudeProxyOff])
    await s.userClosed()
}

// ---- 第 6 条：只有点击才执行 ----

@MainActor @Test func noRunWithoutTap() async throws {
    let (c, s, r, _, _) = privEnv()
    let m = StatusModel(client: c, authorizer: PrivilegedAuthorizer(), sleeper: NoSleep(), flow: fakeFlow(c), privileged: s)
    for _ in 0..<3 { _ = await m.refresh() }
    await m.restore()
    for _ in 0..<3 { _ = await m.refresh() }
    await m.restore() // 已经有确认框：不叠第二个
    #expect(r.calls.isEmpty, "刷新、再次发起都不应调用执行层")
    await s.userTappedExecute()
    await s.userTappedExecute() // 结果已出：再点不重复执行
    #expect(r.calls.count == 1)
}

// ---- 第 8 条：输出 ----

@MainActor @Test func outcomeMapping() async throws {
    for (res, ok, msg) in [
        (PrivilegedRunResult.finished(stdout: "{\"state\":\"ENABLED\"}\nRESULT=0\n"), true, L10n.privExit(0)),
        (.finished(stdout: "命令行：后台服务回了错误\nRESULT=1\n"), false, L10n.privExit(1)),
        (.finished(stdout: "用法\nRESULT=2\n"), false, L10n.privExit(2)),
        (.finished(stdout: "RESULT=5\n"), false, L10n.privExit(5)),
        (.finished(stdout: "RESULT=31\n"), false, L10n.privExit(31)),
        (.finished(stdout: "RESULT=32\n"), false, L10n.privExit(32)),
        (.cancelled, false, L10n.privCancelled),
        (.failed("系统错误"), false, L10n.privUnavailable),
        (.finished(stdout: "没有结果行"), false, L10n.privUnavailable),
    ] {
        let runner = FakeRunner()
        runner.result = res
        let (_, s, _, _, _) = privEnv(runner: runner)
        await s.present([.mainApply], terminal: TerminalPrompt(commands: []), onTerminal: { _ in }, onDone: {})
        await s.userTappedExecute()
        guard case let .done(gotOK, gotMsg, _) = s.phase else { Issue.record("没有结果：\(res)"); continue }
        #expect(gotOK == ok && gotMsg == msg, "\(res)")
    }
    #expect(L10n.privExit(2).contains("请先升级"), "风险 6：命令行不认识这个操作时提示先升级")
    #expect(L10n.privExit(32) == "输入文件在确认后被改动过，请重新操作")
}

@MainActor @Test func terminalFallbackKept() async throws {
    let (c, s, r, _, _) = privEnv()
    let m = StatusModel(client: c, authorizer: PrivilegedAuthorizer(), sleeper: NoSleep(), flow: fakeFlow(c), privileged: s)
    await m.restore()
    s.userTappedTerminal()
    #expect(m.terminalCommand == "sudo abroad restore" && !s.isActive && r.calls.isEmpty)
    // 没有执行层时（例如测试里）直接走终端退路
    let m2 = StatusModel(client: c, authorizer: TerminalAuthorizer(), sleeper: NoSleep(), flow: fakeFlow(c))
    await m2.restore()
    #expect(m2.terminalCommand == "sudo abroad restore")
}

// ---- 第 9 条和“只能由点击触发”：源码检查（带反例） ----

/// 提权相关的源码规则；返回违反的条目（空就是通过）。
func privilegedSourceRuleViolations(_ files: [(String, String)]) -> [String] {
    var out: [String] = []
    for (name, src) in files {
        let code = src.split(separator: "\n").filter { !$0.trimmingCharacters(in: .whitespaces).hasPrefix("//") }
        let joined = code.joined(separator: "\n")
        for w in ["NSAppleScript", "administrator privileges", "do shell script"] where joined.contains(w) && name != "PrivilegedRunner.swift" {
            out.append("\(name) 里出现了 \(w)（只允许在 PrivilegedRunner.swift）")
        }
        let base = joined.components(separatedBy: "/Library/Application Support").count - 1
        if name == "PrivilegedRunner.swift" { if base != 1 { out.append("PrivilegedRunner.swift 里基础路径应恰好出现一次（实际 \(base) 次）") } } else if base > 0 { out.append("\(name) 里出现了 /Library/Application Support") }
        if name != "InputFiles.swift" && (joined.contains("import CryptoKit") || joined.contains("SHA256.")) { out.append("\(name) 里用了 CryptoKit（只允许在 InputFiles.swift）") }
        if name != "App.swift" && joined.contains("PrivilegedRunner(") { out.append("\(name) 里构造了 PrivilegedRunner（只允许在 App.swift）") }
        for l in code where l.contains("userTappedExecute(") {
            let ok = (name == "PrivilegedAuthorizer.swift" && l.contains("func userTappedExecute(")) || l.contains("Button(") || l.contains("action:")
            if !ok { out.append("\(name) 里不经按钮调用了 userTappedExecute：\(l.trimmingCharacters(in: .whitespaces))") }
        }
        for l in code where l.contains(".run(command:") && name != "PrivilegedAuthorizer.swift" {
            out.append("\(name) 里直接调用了执行层：\(l.trimmingCharacters(in: .whitespaces))")
        }
    }
    if files.filter({ $0.1.contains("PrivilegedRunner(") && $0.0 == "App.swift" }).flatMap({ $0.1.components(separatedBy: "PrivilegedRunner(").dropFirst() }).count > 1 {
        out.append("App.swift 里 PrivilegedRunner 只能构造一次")
    }
    return out
}

@Test func privilegedSourceRules() throws {
    let files = try appSources()
    #expect(files.contains { $0.0 == "PrivilegedRunner.swift" })
    let v = privilegedSourceRuleViolations(files)
    #expect(v.isEmpty, "\(v)")
}

@Test func privilegedSourceRuleCounterexamples() {
    let runner = ("PrivilegedRunner.swift", "let base = \"/Library/Application Support\"\nlet s = NSAppleScript(source: \"do shell script \\\"x\\\" with administrator privileges\")")
    #expect(privilegedSourceRuleViolations([runner]).isEmpty)
    let bad: [(String, [(String, String)])] = [
        ("别的文件出现 NSAppleScript", [runner, ("HomeView.swift", "let s = NSAppleScript(source: \"x\")")]),
        ("别的文件出现 with administrator privileges", [runner, ("SettingsView.swift", "let s = \"with administrator privileges\"")]),
        ("基础路径出现两次", [("PrivilegedRunner.swift", runner.1 + "\nlet x = \"/Library/Application Support/claude-abroad\"")]),
        ("别的文件出现基础路径", [runner, ("StatusModel.swift", "let p = \"/Library/Application Support/claude-abroad\"")]),
        ("别的文件 import CryptoKit", [runner, ("MainProxyModel.swift", "import CryptoKit")]),
        ("页面出现时调用执行", [runner, ("HomeView.swift", ".onAppear { Task { await session.userTappedExecute() } }")]),
        ("模型刷新里调用执行", [runner, ("StatusModel.swift", "func refresh() async { await privileged?.userTappedExecute() }")]),
        ("别处直接调用执行层", [runner, ("StatusModel.swift", "let x = await runner.run(command: c, prompt: p)")]),
        ("别处构造 PrivilegedRunner", [runner, ("HomeView.swift", "let r = PrivilegedRunner()")]),
    ]
    for (what, fs) in bad {
        #expect(!privilegedSourceRuleViolations(fs).isEmpty, "反例没被拦下：\(what)")
    }
}
