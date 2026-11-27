// 2026-11-27 真机：应用里“主代理换成节点 …，然后应用主代理”失败，确认框写“执行失败（后台服务回了错误，见下面的输出）”，
// 下面却是空的——命令行的出错写在标准错误，do shell script 只返回标准输出。标题里是节点 ID 而不是名字。
// 首页“关闭原因：state”——闸门原因代码原样显示。
import AbroadAPI
import Foundation
import Testing
@testable import AbroadKit

// ---- 提权命令的输出格式：标准输出段、---STDERR---、标准错误段、RESULT= ----

@Test func parseOutputSplitsStdoutAndStderr() throws {
    let out = """
    ---STEP main use---
    {"selected":"n1"}
    ---STEP main apply---
    ---STDERR---
    ---STEP main use---
    ---STEP main apply---
    建立出口基准失败：主代理在虚拟机里 30 秒内没有就绪：主代理引擎（mihomo）意外退出，正在等待重启
    RESULT=1

    """
    let o = try #require(PrivilegedCommand.parseOutput(out))
    #expect(o.exit == 1 && o.stdout == "{\"selected\":\"n1\"}" && o.steps == ["main use", "main apply"] && o.failedStep == "main apply")
    #expect(o.stepErrors == [PrivilegedOutput.StepText(step: "main apply", text: "建立出口基准失败：主代理在虚拟机里 30 秒内没有就绪：主代理引擎（mihomo）意外退出，正在等待重启")])
    // JSON 只从标准输出段取（兼容的 parse 给的也是这一段）
    #expect(PrivilegedCommand.parse(out)?.body == "{\"selected\":\"n1\"}")
    // 没有分隔行（安装脚本把标准错误并进了标准输出；或第一步之前就停了）：全部算标准输出
    let plain = try #require(PrivilegedCommand.parseOutput("安装失败：xx\nRESULT=20\n"))
    #expect(plain.stdout == "安装失败：xx" && plain.stepErrors.isEmpty && plain.steps.isEmpty && plain.failedStep == nil)
}

/// 失败时“见下面的输出”下面一定有内容：标准错误里的话；两步时写停在哪一步。
@MainActor @Test func failureShowsStderr() async throws {
    let cases: [(String, Int, [String])] = [
        // 单步：只有标准错误
        ("---STEP main apply---\n---STDERR---\n---STEP main apply---\n后台服务说：节点不可用\nRESULT=1\n", 1, ["后台服务说：节点不可用"]),
        // 两步、第一步失败：第二步没出现，写明停在第一步
        ("---STEP main use---\n---STDERR---\n---STEP main use---\n没有叫 X 的节点\nRESULT=1\n", 2, ["停在第 1 步（abroad main use），后面的步骤没有执行", "main use：没有叫 X 的节点"]),
        // 什么都没输出：也要有一句
        ("---STEP main apply---\n---STDERR---\n---STEP main apply---\nRESULT=1\n", 1, [L10n.privNoOutput]),
    ]
    for (out, steps, want) in cases {
        let runner = FakeRunner()
        runner.result = .finished(stdout: out)
        let (_, s, _, _, _) = privEnv(runner: runner)
        let ops: [SensitiveOperation] = steps == 2 ? [.mainUse(node: "n1", via: []), .mainApply] : [.mainApply]
        await s.present(ops, terminal: TerminalPrompt(commands: []), onTerminal: { _ in }, onDone: {})
        await s.userTappedExecute()
        guard case let .done(ok, msg, output) = s.phase else { Issue.record("没有结果"); continue }
        #expect(!ok && msg == L10n.privExit(1) && !output.isEmpty)
        #expect(output.split(separator: "\n").map(String.init) == want, "\(output)")
        #expect(!output.contains("---"), "标记不应显示给用户：\(output)")
    }
    // 成功：只显示标准输出
    let runner = FakeRunner()
    runner.result = .finished(stdout: "---STEP main apply---\n{\"ok\":true}\n---STDERR---\n---STEP main apply---\nRESULT=0\n")
    let (_, s, _, _, _) = privEnv(runner: runner)
    await s.present([.mainApply], terminal: TerminalPrompt(commands: []), onTerminal: { _ in }, onDone: {})
    await s.userTappedExecute()
    #expect(s.phase == .done(ok: true, message: L10n.privExit(0), output: "{\"ok\":true}"))
}

/// 用 /bin/sh 真跑拼出来的命令（不提权：路径换成临时目录、属主条件换成当前用户、去掉 chown；假的命令行）：
/// 假命令行往标准错误写一段话并以非 0 退出，解析出的错误文字就是那段话；两步命令第一步失败时第二步没执行。
@Test func realShellRoundTrip() throws {
    let dir = tempDir()
    defer { try? FileManager.default.removeItem(at: dir) }
    let inst = dir.appendingPathComponent("inst"), bin = inst.appendingPathComponent("bin")
    try FileManager.default.createDirectory(at: bin, withIntermediateDirectories: true)
    let log = dir.appendingPathComponent("ran")
    let fake = """
    #!/bin/sh
    echo "$3 $4" >>'\(log.path)'
    if [ "$4" = use ]; then echo '我是标准错误里的一段话：没有叫 n1 的节点' >&2; echo '{"partial":1}'; exit 1; fi
    echo '{"ok":true}'
    """
    let abroad = bin.appendingPathComponent("abroad")
    try fake.write(to: abroad, atomically: true, encoding: .utf8)
    for p in [abroad, bin, inst] { try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: p.path) }
    let paths = PrivilegedPaths(installDir: inst.path, staging: dir.appendingPathComponent("stg").path)
    func run(_ steps: [SensitiveOperation]) throws -> String {
        var cmd = try PrivilegedCommand.build(steps.map(PrivilegedStep.init), paths: paths, digests: [:])
        cmd = cmd.replacingOccurrences(of: "\")\" = 0 ]", with: "\")\" = \(getuid()) ]").replacingOccurrences(of: "/usr/sbin/chown root:wheel \"$S\" && ", with: "")
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/bin/sh")
        p.arguments = ["-c", cmd]
        let pipe = Pipe()
        p.standardOutput = pipe
        try p.run()
        let data = pipe.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        #expect(p.terminationStatus == 0, "提权 shell 一律以 0 退出")
        return String(decoding: data, as: UTF8.self)
    }
    // 两步、第一步失败
    let out = try run([.mainUse(node: "n1", via: []), .mainApply])
    let o = try #require(PrivilegedCommand.parseOutput(out), "\(out)")
    #expect(o.exit == 1 && o.steps == ["main use"] && o.failedStep == "main use", "\(out)")
    #expect(o.stderr == "我是标准错误里的一段话：没有叫 n1 的节点" && o.stdout == "{\"partial\":1}")
    #expect((try? String(contentsOf: log, encoding: .utf8)) == "main use\n", "第二步不应执行")
    #expect(!FileManager.default.fileExists(atPath: paths.staging), "临时目录用完删掉")
    // 两步都成功
    try? FileManager.default.removeItem(at: log)
    let ok = try #require(PrivilegedCommand.parseOutput(try run([.mainApply, .mainApply])))
    #expect(ok.exit == 0 && ok.steps == ["main apply", "main apply"] && ok.stderr.isEmpty && ok.stdout == "{\"ok\":true}\n{\"ok\":true}")
    #expect(!FileManager.default.fileExists(atPath: paths.staging))
}

/// 秘密不进命令：新的输出格式里（标记、标准错误的文件名）也只有固定文字。
@Test func newFormatHasNoUserText() throws {
    let cmd = try PrivilegedCommand.build([PrivilegedStep(.addSourceURLFile(path: "/t/secret-url.txt", name: "订阅", viaHost: false)), PrivilegedStep(.mainApply)],
                                          paths: samplePaths, digests: ["/t/secret-url.txt": zeroDigest])
    for l in cmd.split(separator: "\n") where l.contains("---") {
        for m in l.components(separatedBy: "echo '").dropFirst() {
            let marker = m.prefix { $0 != "'" }
            #expect(marker == "---STDERR---" || PrivilegedSubcommand.allCases.contains { "---STEP \($0.rawValue)---" == marker }, "标记只能是固定文字：\(marker)")
        }
    }
    #expect(!cmd.contains("/t/secret-url.txt\" >") && cmd.contains(">\"$S/o1\" 2>\"$S/e1\"") && cmd.contains(">\"$S/o2\" 2>\"$S/e2\""))
}

// ---- 确认框标题用节点名 ----

@MainActor @Test func titleUsesNodeName() async throws {
    final class D: @unchecked Sendable {
        var sources = "[{\"id\":\"s1\",\"name\":\"我的订阅\",\"kind\":\"yaml\",\"updated\":\"2026-11-27\",\"nodes\":[{\"id\":\"16766d0a\",\"name\":\"MY\",\"type\":\"ss\"},{\"id\":\"relay9\",\"name\":\"中转\",\"type\":\"ss\"}]}]"
        func handle(_ r: HTTPRequest) -> HTTPResponse {
            if r.method == "GET" && r.path == "/v1/main-proxy/sources" { return json(200, sources) }
            return FakePrivDaemon().handle(r)
        }
    }
    let d = D()
    let t = FakeTransport { d.handle($0) }
    let c = APIClient(transport: t)
    let runner = FakeRunner()
    let s = PrivilegedSession(client: c, runner: runner, files: InputFiles(directory: tempDir()))
    await s.present([.mainUse(node: "16766d0a", via: ["relay9"]), .mainApply], terminal: TerminalPrompt(commands: []), onTerminal: { _ in }, onDone: {})
    #expect(s.request?.title == "主代理换成节点 MY（经 中转），然后应用主代理")
    await s.userTappedExecute()
    #expect(runner.calls.first?.command.contains("'main' 'use' '16766d0a' '--via' 'relay9'") == true, "交给命令行的仍是 ID")
    await s.userClosed()
    // 找不到名字：退回 ID
    await s.present([.mainUse(node: "gone", via: [])], terminal: TerminalPrompt(commands: []), onTerminal: { _ in }, onDone: {})
    #expect(s.request?.title == "主代理换成节点 gone")
    await s.userClosed()
    #expect(L10n.privTitle(.lanConfirm(node: "16766d0a"), names: ["16766d0a": "MY"]) == "确认局域网节点 MY")
    #expect(L10n.privTitle(.removeSource(id: "s1"), names: ["s1": "我的订阅"]) == "删除主代理来源 我的订阅")
}

// ---- 首页的闸门原因：一律中文，不回显代码 ----

/// internal/gatesm 里全部的闸门原因代码。
func goGateReasons() throws -> [String] {
    let src = try readRepo("internal/gatesm/gate.go")
    let re = try NSRegularExpression(pattern: #"Reason[A-Za-z]*\s+Reason\s*=\s*"([a-z_]+)""#)
    return re.matches(in: src, range: NSRange(src.startIndex..., in: src)).compactMap { Range($0.range(at: 1), in: src).map { String(src[$0]) } }
}

@Test func everyGateReasonHasChinese() throws {
    let codes = try goGateReasons()
    #expect(codes.count >= 7 && codes.contains("state") && codes.contains("main_proxy_switch"), "\(codes)")
    for c in codes {
        let n = L10n.gateReasonName(c)
        #expect(n != L10n.gateReasonOther && !n.contains(c) && n.unicodeScalars.contains { $0.value >= 0x4E00 }, "\(c) 在文字表里没有中文：\(n)")
    }
    #expect(L10n.gateReasonName("something_new") == L10n.gateReasonOther)
}

@Test func homeGateLineNeverShowsCodes() throws {
    #expect(L10n.homeGateLine(reasons: ["state"], state: "RESTORED") == nil, "已回国时不显示这一行")
    #expect(L10n.homeGateLine(reasons: [], state: "ENABLED") == nil)
    #expect(L10n.homeGateLine(reasons: ["state"], state: "DISABLED") == "关闭原因：已停用")
    #expect(L10n.homeGateLine(reasons: ["state", "vm"], state: "ENABLING") == "关闭原因：正在启用、虚拟机没有运行或不健康")
    #expect(L10n.homeGateLine(reasons: ["weird_one", "other_two"], state: "ENABLED") == "关闭原因：其他原因")
    let codes = try goGateReasons() + ["weird_one"]
    for st in ["ENABLED", "DISABLED", "ENABLING", "RESTORING", "CORRUPT"] {
        let line = L10n.homeGateLine(reasons: codes, state: st) ?? ""
        for c in codes { #expect(!line.contains(c), "\(st)：首页出现了原因代码 \(c)：\(line)") }
        #expect(!line.contains("_") && !line.isEmpty)
    }
    // 源码检查：首页只经 homeGateLine 显示原因，不直接拼 reasons
    let home = try readRepo("app/Sources/ClaudeAbroad/HomeView.swift")
    #expect(home.contains("L10n.homeGateLine(") && !home.contains("reasons.joined") && !home.contains("Text(reasons") && !home.contains("gateReasonsLine"))
    let all = try appSources().filter { $0.0 != "Strings.swift" }
    for (name, src) in all where src.contains("gate.reasons") {
        #expect(!src.contains("gate.reasons?.joined") && !src.contains("gate.reasons!.joined"), "\(name) 直接拼了原因代码")
    }
}
