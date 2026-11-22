// M5-8 验收 b–e：安装引导和主代理页面的视图模型（假的传输层；不建窗口）。
import AbroadAPI
import Foundation
import Testing
@testable import AbroadKit

func setupJSON(completed: Bool, mainDone: Bool) -> String {
    "{\"completed\":\(completed),\"steps\":[{\"id\":\"service\",\"done\":true},{\"id\":\"main_proxy\",\"done\":\(mainDone)}]}"
}

// ---- b：安装引导 ----

@MainActor
@Test func setupStepsFollowBackend() async {
    let mainDone = Box(false)
    let completed = Box(false)
    let completeAllowed = Box(false)
    let t = FakeTransport { req in
        switch (req.method, req.path) {
        case ("GET", "/v1/setup"): return json(200, setupJSON(completed: completed.v, mainDone: mainDone.v))
        case ("POST", "/v1/setup/complete"):
            if !completeAllowed.v { return json(422, "{\"error\":\"还不能完成安装引导：还差一步“应用主代理”（选好主代理节点并应用，建立出口基准）\"}") }
            completed.v = true
            return json(200, setupJSON(completed: true, mainDone: true))
        default: return json(404, "{}")
        }
    }
    let m = SetupModel(client: APIClient(transport: t), sleeper: NoSleep())
    await m.refresh()
    #expect(m.step == .welcome && m.canAdvance && m.shouldShow)
    await m.next()
    #expect(m.step == .mainProxy, "后台连上（service 已完成）时直接到配置主代理")
    #expect((m.canAdvance) == false, "主代理没应用：后台说没完成就不能下一步")
    await m.next()
    #expect(m.step == .mainProxy && t.requests.filter { $0.path == "/v1/setup/complete" }.isEmpty)
    // 后台说主代理已应用，但完成仍被后台拒绝（例如基准刚失效）：停在第三步并原样显示原因。
    mainDone.v = true
    await m.recheck()
    #expect(m.canAdvance)
    await m.next()
    #expect(m.step == .mainProxy && m.notice?.text.raw.contains("还差一步“应用主代理”") == true)
    // 后台允许完成。
    completeAllowed.v = true
    await m.next()
    #expect(m.step == .done && m.completed && !m.shouldShow)
    // 之后再读：完成了就不再进入引导。
    let m2 = SetupModel(client: APIClient(transport: t), sleeper: NoSleep())
    await m2.refresh()
    #expect((m2.shouldShow && m2.step == .done) == false)
}

@MainActor
@Test func setupWaitsForService() async {
    let up = Box(false)
    let t = FakeTransport { req in
        guard up.v else { throw ConnectionError.notRunning }
        return json(200, setupJSON(completed: false, mainDone: false))
    }
    let m = SetupModel(client: APIClient(transport: t), sleeper: NoSleep())
    await m.refresh()
    #expect((m.reachable && m.shouldShow) == false)
    await m.next()
    #expect(m.step == .service && !m.canAdvance)
    #expect(m.serviceCommand == L10n.setupServiceCommand && m.serviceCommand.hasPrefix("sudo launchctl "))
    up.v = true
    #expect(await eventually { m.step == .mainProxy && m.reachable })
    m.stop()
}

// ---- c：主代理页面 ----

/// 一个记在内存里的假后台：来源、选择、应用、检查、局域网确认。
final class FakeMain: @unchecked Sendable {
    var sources: [(id: String, name: String, kind: String, nodes: [(String, String, String)])] = [
        ("s1", "本地", "local", [("n-exit", "exit", "ss"), ("n-relay", "relay", "trojan")]),
        ("s2", "订阅", "subscription", [("n-sub", "sub1", "ss"), ("n-lan", "lan", "socks5")]),
    ]
    var enabled = false
    var lanConfirmed = false
    var addResponse: HTTPResponse?
    var selectResponse: HTTPResponse?
    var applyResponse: HTTPResponse?
    var refreshResponse: HTTPResponse?
    var selected: String?

    func handle(_ r: HTTPRequest) -> HTTPResponse {
        let body = r.body.flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] } ?? [:]
        switch (r.method, r.path) {
        case ("GET", "/v1/main-proxy/sources"):
            let arr = sources.map { s in
                ["id": s.id, "name": s.name, "kind": s.kind, "updated": "2026-11-22T00:00:00Z",
                 "nodes": s.nodes.map { ["id": $0.0, "name": $0.1, "type": $0.2] }] as [String: Any]
            }
            return HTTPResponse(status: 200, body: try! JSONSerialization.data(withJSONObject: arr))
        case ("GET", "/v1/main-proxy"):
            let sel = selected.map { "{\"id\":\"\($0)\",\"name\":\"x\",\"type\":\"ss\"}" } ?? "null"
            return json(200, "{\"selected\":\(sel),\"applied\":false,\"needs_reselect\":false,\"lan_pending\":false,\"sources\":\(sources.count)}")
        case ("POST", "/v1/main-proxy/sources"):
            if let a = addResponse { return a }
            sources.append(("s\(sources.count + 1)", body["name"] as? String ?? "", body["kind"] as? String == "subscription" ? "subscription" : "local", [("n-new", "new", "socks5")]))
            return json(200, "{\"id\":\"s\(sources.count)\"}")
        case ("DELETE", let p) where p.hasPrefix("/v1/main-proxy/sources/"):
            let id = String(p.dropFirst("/v1/main-proxy/sources/".count))
            sources.removeAll { $0.id == id }
            return HTTPResponse(status: 200)
        case ("POST", let p) where p.hasSuffix("/refresh"):
            if (body["via_host"] as? Bool) != true && !enabled {
                return json(422, "{\"error\":\"现在没有启用，没有主代理可走：要经本机网络抓取订阅，请明确选择（via_host）；否则先启用再更新\"}")
            }
            return refreshResponse ?? json(200, "{\"id\":\"s2\",\"needs_reselect\":true}")
        case ("PUT", "/v1/main-proxy"):
            if let s = selectResponse { return s }
            let chain = body["chain"] as? [String] ?? []
            if (chain.first ?? body["selected"] as? String) == "n-lan" && !lanConfirmed {
                return json(422, "{\"error\":\"应用主代理失败：主代理的第一跳是局域网节点，还没有确认。请先在应用里确认这个局域网节点（POST /v1/main-proxy/lan-confirm），再应用。\"}")
            }
            selected = body["selected"] as? String
            return json(200, "{\"saved\":true,\"note\":\"现在没有启用：已保存，启用时下发并检查\"}")
        case ("POST", "/v1/main-proxy/apply"):
            return applyResponse ?? json(200, "{\"baseline\":{\"rule\":\"same_country\",\"country\":\"US\",\"timezone\":\"America/Los_Angeles\"},\"synced\":false}")
        case ("POST", "/v1/main-proxy/check"):
            return json(200, "{\"rule\":\"same_country\",\"country\":\"US\",\"timezone\":\"America/Los_Angeles\"}")
        case ("POST", "/v1/main-proxy/lan-confirm"):
            lanConfirmed = (body["node"] as? String) == "n-lan"
            return json(200, "{\"confirmed\":1}")
        default:
            return json(404, "{\"error\":\"没有\"}")
        }
    }
}

func tempDir() -> URL {
    let u = FileManager.default.temporaryDirectory.appendingPathComponent("abroad-test-" + UUID().uuidString, isDirectory: true)
    return u
}

@MainActor
func mainModel(_ f: FakeMain, files: InputFiles = InputFiles(directory: tempDir())) -> (MainProxyModel, FakeTransport) {
    let t = FakeTransport { f.handle($0) }
    return (MainProxyModel(client: APIClient(transport: t), files: files), t)
}

@MainActor
@Test func sourcesListAddDeleteRefresh() async throws {
    let f = FakeMain()
    let (m, t) = mainModel(f)
    await m.refresh()
    #expect(m.sources.map(\.name) == ["本地", "订阅"] && m.sources[1].nodeCount == 2 && m.nodes.count == 4)
    // 订阅地址
    m.addKind = .subscription
    m.formName = "新订阅"
    m.formURL = "https://sub.example.com/x"
    await m.submitAdd()
    var sent = try #require(t.requests.last(where: { $0.method == "POST" && $0.path == "/v1/main-proxy/sources" })?.body)
    var obj = try #require(try JSONSerialization.jsonObject(with: sent) as? [String: Any])
    #expect(obj["kind"] as? String == "subscription" && obj["url"] as? String == "https://sub.example.com/x" && obj["name"] as? String == "新订阅")
    #expect(m.sources.count == 3 && m.formURL.isEmpty)
    // 本地配置文件（读用户选的文件，按 yaml 提交）
    let dir = tempDir()
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: dir) }
    let file = dir.appendingPathComponent("my.yaml")
    try "proxies: []\n".write(to: file, atomically: true, encoding: .utf8)
    m.addKind = .localFile
    m.formFile = file
    await m.submitAdd()
    sent = try #require(t.requests.last(where: { $0.method == "POST" && $0.path == "/v1/main-proxy/sources" })?.body)
    obj = try #require(try JSONSerialization.jsonObject(with: sent) as? [String: Any])
    #expect(obj["kind"] as? String == "yaml" && obj["content"] as? String == "proxies: []\n" && obj["name"] as? String == "my")
    // 手动节点
    m.addKind = .manualNode
    m.nodeServer = "192.168.1.20"
    m.nodePort = "7890"
    m.nodeUser = "u"
    m.nodePassword = "p"
    await m.submitAdd()
    sent = try #require(t.requests.last(where: { $0.method == "POST" && $0.path == "/v1/main-proxy/sources" })?.body)
    obj = try #require(try JSONSerialization.jsonObject(with: sent) as? [String: Any])
    let node = try #require(obj["node"] as? [String: Any])
    #expect(obj["kind"] as? String == "node" && node["server"] as? String == "192.168.1.20" && node["port"] as? Int == 7890 && node["password"] as? String == "p")
    #expect(m.sources.count == 5)
    // 不合法的输入不发请求
    let n = t.requests.count
    m.addKind = .manualNode
    m.nodeServer = "x"
    m.nodePort = "99999"
    await m.submitAdd()
    #expect(t.requests.count == n && m.notice?.text.raw == L10n.addBadNode)
    // 删除
    await m.deleteSource("s1")
    #expect((m.sources.contains { $0.id == "s1" }) == false)
    // 刷新：未启用时被要求显式选择经本机网络
    await m.refreshSource("s2")
    #expect(m.refreshNeedsViaHost == "s2" && m.notice?.text.raw == L10n.viaHostExplain)
    await m.refreshSource("s2", viaHost: true)
    #expect(m.refreshNeedsViaHost == nil && m.needsReselect, "刷新后选中的节点不在了：提示重新选择")
    let last = try #require(t.requests.last(where: { $0.path.hasSuffix("/refresh") })?.body)
    #expect(String(decoding: last, as: UTF8.self) == "{\"via_host\":true}")
}

@MainActor
@Test func addSubscriptionNeedsViaHost() async {
    let f = FakeMain()
    f.addResponse = json(422, "{\"error\":\"现在没有启用，没有主代理可走：要经本机网络抓取订阅，请明确选择（via_host）；否则先启用再更新\"}")
    let (m, _) = mainModel(f)
    m.addKind = .subscription
    m.formURL = "https://sub.example.com/x"
    await m.submitAdd()
    #expect(m.viaHostNeeded && m.notice?.text.raw == L10n.viaHostExplain && m.formURL.isEmpty)
}

@MainActor
@Test func selectChainAndApplySuccess() async throws {
    let f = FakeMain()
    let (m, t) = mainModel(f)
    await m.refresh()
    m.selectedExit = "n-exit"
    m.via = ["n-relay"]
    await m.apply()
    let put = try #require(t.requests.first(where: { $0.method == "PUT" })?.body)
    let obj = try #require(try JSONSerialization.jsonObject(with: put) as? [String: Any])
    #expect(obj["selected"] as? String == "n-exit" && obj["chain"] as? [String] == ["n-relay", "n-exit"])
    #expect(t.requests.contains { $0.path == "/v1/main-proxy/apply" }, "未启用时保存后再建立出口基准")
    #expect(m.applyState == .succeeded(country: "US", timezone: "America/Los_Angeles"))
    await m.check()
    #expect(m.checkState == .ok(rule: "same_country", country: "US", timezone: "America/Los_Angeles"))
}

@MainActor
@Test func applyNeedsConfirm() async throws {
    let f = FakeMain()
    let (m, t) = mainModel(f)
    await m.refresh()
    m.selectedExit = "n-exit"
    f.applyResponse = json(409, confirmJSON)
    await m.apply()
    #expect(m.pendingConfirm?.region?.to == "B")
    f.applyResponse = nil
    await m.answerConfirm(true)
    let last = try #require(t.requests.last(where: { $0.path == "/v1/main-proxy/apply" }))
    #expect(String(decoding: last.body ?? Data(), as: UTF8.self).contains("confirm"))
    #expect(m.applyState == .succeeded(country: "US", timezone: "America/Los_Angeles") && m.pendingConfirm == nil)
}

@MainActor
@Test func applyNeedsAuthorization() async {
    let f = FakeMain()
    f.selectResponse = json(403, "{\"error\":\"需要本人授权：这是敏感操作\"}")
    let (m, _) = mainModel(f)
    await m.refresh()
    m.selectedExit = "n-exit"
    m.via = ["n-relay"]
    await m.apply()
    #expect(m.terminal?.commands == ["sudo abroad main use n-exit --via n-relay", "sudo abroad main apply"])
    #expect(m.applyState == .idle)
}

@MainActor
@Test func applyFailedRollback() async {
    for (text, want) in [
        ("应用主代理失败：引擎没有就绪。已经回到原来的主代理；请检查新节点后再试。", Rollback.done),
        ("应用主代理失败：引擎没有就绪。新节点没有生效；恢复原来的主代理没有完成，稍后会自动重试。", Rollback.incomplete),
    ] {
        let f = FakeMain()
        f.selectResponse = json(502, "{\"error\":\"\(text)\"}")
        let (m, _) = mainModel(f)
        await m.refresh()
        m.selectedExit = "n-exit"
        await m.apply()
        guard case let .failed(t, rb) = m.applyState else { Issue.record("应失败"); continue }
        #expect(rb == want && t.raw == text && t.action == "应用主代理失败" && t.reason == "引擎没有就绪")
    }
}

@MainActor
@Test func lanPendingToConfirmed() async throws {
    let f = FakeMain()
    let (m, t) = mainModel(f)
    await m.refresh()
    m.selectedExit = "n-sub"
    m.via = ["n-lan"]
    await m.apply()
    #expect(m.lanPrompt == LANPrompt(node: "n-lan", name: "lan") && m.applyState == .idle)
    await m.confirmLAN()
    #expect(f.lanConfirmed && m.lanPrompt == nil)
    let lc = try #require(t.requests.last(where: { $0.path == "/v1/main-proxy/lan-confirm" })?.body)
    #expect(String(decoding: lc, as: UTF8.self) == "{\"node\":\"n-lan\"}")
    await m.apply()
    #expect(m.applyState == .succeeded(country: "US", timezone: "America/Los_Angeles"))
}

// ---- d：敏感内容 ----

/// 模型的全部可读状态（dump 递归展开所有存储属性，包括 @Published 的值）。
@MainActor
func dumpState(_ m: MainProxyModel) -> String {
    var s = ""
    dump(m, to: &s, maxDepth: 12)
    return s + m.formURL + m.nodePassword + m.nodeUser + m.nodeServer // 再显式取一次，防止 dump 没展开某个包装
}

@MainActor
@Test func secretsGoneAfterSubmit() async throws {
    for (status, label) in [(200, "成功"), (403, "要本人授权"), (500, "后台出错")] {
        let f = FakeMain()
        f.addResponse = status == 200 ? nil : json(status, status == 403 ? "{\"error\":\"需要本人授权：这是敏感操作\"}" : "{\"error\":\"添加来源失败：内部错误。请稍后再试。\"}")
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let (m, _) = mainModel(f, files: InputFiles(directory: dir))
        // 订阅地址
        m.addKind = .subscription
        m.formURL = "https://sub.example.com/api?token=SUBMARK"
        #expect(dumpState(m).contains("SUBMARK"), "对照：提交前能在模型状态里找到")
        await m.submitAdd()
        #expect((dumpState(m).contains("SUBMARK")) == false, "\(label)：提交后模型里不能有订阅地址")
        let noticeText = m.notice?.text.raw ?? ""
        let cmds = m.terminal?.commands.joined(separator: " ") ?? ""
        #expect((noticeText.contains("SUBMARK") && !cmds.contains("SUBMARK")) == false)
        await m.dismissTerminal()
        // 手动节点的密码
        m.addKind = .manualNode
        m.nodeServer = "10.0.0.9"
        m.nodePort = "1080"
        m.nodeUser = "USERMARK"
        m.nodePassword = "PWDMARK"
        #expect(dumpState(m).contains("PWDMARK"))
        await m.submitAdd()
        let after = dumpState(m)
        for mark in ["PWDMARK", "USERMARK", "10.0.0.9"] {
            #expect((after.contains(mark)) == false, "\(label)：提交后模型里不能有 \(mark)")
        }
        let cmds2 = m.terminal?.commands.joined(separator: " ") ?? ""
        #expect((cmds2.contains("PWDMARK")) == false)
        await m.dismissTerminal()
    }
}

// ---- e：终端退路的临时文件 ----

@MainActor
@Test func terminalFallbackTempFile() async throws {
    let f = FakeMain()
    f.addResponse = json(403, "{\"error\":\"需要本人授权：这是敏感操作\"}")
    let dir = tempDir()
    defer { try? FileManager.default.removeItem(at: dir) }
    let (m, _) = mainModel(f, files: InputFiles(directory: dir))
    m.addKind = .subscription
    m.formName = "我的订阅"
    m.formURL = "https://sub.example.com/api?token=SUBMARK"
    m.viaHost = true
    await m.submitAdd()
    let p = try #require(m.terminal)
    let file = try #require(p.tempFile)
    #expect(file.deletingLastPathComponent().standardizedFileURL == dir.standardizedFileURL, "只在指定的目录里")
    #expect((file.lastPathComponent.contains("SUBMARK") && !file.lastPathComponent.contains("sub.example")) == false, "文件名不含敏感内容")
    let attrs = try FileManager.default.attributesOfItem(atPath: file.path)
    #expect((attrs[.posixPermissions] as? NSNumber)?.intValue == 0o600, "只有本人可读写")
    let dattrs = try FileManager.default.attributesOfItem(atPath: dir.path)
    #expect((dattrs[.posixPermissions] as? NSNumber)?.intValue == 0o700)
    #expect(try String(contentsOf: file, encoding: .utf8) == "https://sub.example.com/api?token=SUBMARK\n")
    #expect(p.commands == ["sudo abroad main add-source --url-file \(file.path) --name '我的订阅' --via-host"])
    #expect((p.commands.joined().contains("SUBMARK")) == false)
    await m.dismissTerminal()
    #expect((FileManager.default.fileExists(atPath: file.path)) == false, "关掉提示后删除")
    // 手动节点：临时文件是 YAML，命令是 add-node --file。
    m.addKind = .manualNode
    m.formName = "家里"
    m.nodeServer = "192.168.1.20"
    m.nodePort = "7890"
    m.nodePassword = "PWDMARK"
    await m.submitAdd()
    let p2 = try #require(m.terminal)
    let f2 = try #require(p2.tempFile)
    let y = try String(contentsOf: f2, encoding: .utf8)
    #expect(y == "name: \"家里\"\ntype: \"socks5\"\nserver: \"192.168.1.20\"\nport: 7890\npassword: \"PWDMARK\"\n")
    #expect(p2.commands == ["sudo abroad main add-node --file \(f2.path) --name '家里'"])
    await m.dismissTerminal()
    #expect((FileManager.default.fileExists(atPath: f2.path)) == false)
    // 本地配置文件：直接用用户自己的路径，不写临时文件。
    let user = dir.appendingPathComponent("mine.yaml")
    try "proxies: []\n".write(to: user, atomically: true, encoding: .utf8)
    m.addKind = .localFile
    m.formFile = user
    m.formName = "本地"
    await m.submitAdd()
    #expect(m.terminal?.tempFile == nil && m.terminal?.commands == ["sudo abroad main add-source --file \(user.path) --name '本地'"])
    await m.dismissTerminal()
    #expect(FileManager.default.fileExists(atPath: user.path), "不删用户自己的文件")
    // 只删指定目录里的文件。
    InputFiles(directory: dir.appendingPathComponent("other")).remove(user)
    #expect(FileManager.default.fileExists(atPath: user.path))
}
