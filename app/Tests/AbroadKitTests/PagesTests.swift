// M5-9 验收 b–g：其余页面的视图模型（假的传输层；不建窗口）。
import AbroadAPI
import Foundation
import Testing
@testable import AbroadKit

func errBody(_ status: Int, _ code: String, _ text: String, field: String? = nil) -> HTTPResponse {
    var o = ["error": text, "code": code]
    if let field { o["field"] = field }
    return HTTPResponse(status: status, body: try! JSONSerialization.data(withJSONObject: o))
}

let exitsJSON = "{\"exits\":[{\"tag\":\"wg\",\"kind\":\"wireguard\",\"server\":\"已配置\",\"endpoints\":1,\"avail\":{\"up\":true,\"pushed\":true,\"agent_up\":true,\"check\":\"pass\",\"items\":{\"https\":\"pass\",\"ip\":\"pass\"}}}]}"

// ---- g：每个页面的三种加载状态 ----

@MainActor
@Test func pageLoadStates() async {
    let down = FakeTransport { _ in throw ConnectionError.notRunning }
    let broken = FakeTransport { _ in json(500, "{\"error\":\"读状态目录失败：权限不对。请检查。\",\"code\":\"internal\"}") }
    func ok() -> FakeTransport {
        FakeTransport { req in
            switch req.path {
            case "/v1/exits": return json(200, exitsJSON)
            case "/v1/status": return json(200, statusJSON(state: "ENABLED", gate: true))
            case "/v1/rules?format=json": return json(200, "{\"default\":\"proxy:main\",\"rules\":[],\"offline_allow\":[],\"local_domains\":[],\"pinned_extra\":[],\"block_udp_443\":false}")
            case "/v1/apps": return json(200, "{\"apps\":[]}")
            case "/v1/envcheck/settings": return json(200, "{\"disabled\":[]}")
            case "/v1/envcheck": return json(200, "{\"at\":\"2026-11-22T00:00:00Z\",\"hard\":\"pass\",\"items\":[]}")
            case "/v1/region": return json(200, "{}")
            case "/v1/engines": return json(200, "{\"reachable\":false,\"boot_changed\":false,\"engines\":[]}")
            default: return json(404, "{\"error\":\"没有\",\"code\":\"not_found\"}")
            }
        }
    }
    typealias Loader = (APIClient) async -> LoadState
    let pages: [(String, Loader)] = [
        ("出口", { c in let m = ExitsModel(client: c); await m.load(); return m.state }),
        ("应用", { c in let m = AppsModel(client: c); await m.load(); return m.state }),
        ("环境", { c in let m = EnvModel(client: c); await m.load(); return m.state }),
        ("地区", { c in let m = RegionModel(client: c); await m.load(); return m.state }),
        ("引擎", { c in let m = EnginesModel(client: c); await m.load(); return m.state }),
        ("设置", { c in let m = SettingsModel(client: c); await m.load(); return m.state }),
    ]
    for (name, load) in pages {
        #expect(await load(APIClient(transport: down)) == .unreachable(.notRunning), "\(name)：后台不可达")
        let f = await load(APIClient(transport: broken))
        guard case .failed(let t) = f else { Issue.record("\(name)：应加载失败，得到 \(f)"); continue }
        #expect(t.action == "读状态目录失败", "\(name)：失败原因原样")
        #expect(await load(APIClient(transport: ok())) == .loaded, "\(name)：加载成功")
    }
}

// ---- b：分流出口 ----

@MainActor
@Test func exitsPage() async throws {
    var exits = exitsJSON
    let refsDelete = Box(false)
    let t = FakeTransport { req in
        switch (req.method, req.path) {
        case ("GET", "/v1/exits"): return json(200, exits)
        case ("GET", "/v1/status"): return json(200, "{\"state\":\"ENABLED\",\"gate_open\":true,\"protected\":true,\"gate\":{\"open\":true,\"lease_left_ms\":0},\"opens\":{\"wg\":{\"tcp\":3,\"udp\":1,\"failed\":0}}}")
        case ("PUT", "/v1/exits/wg2"): return json(200, "{\"ok\":true}")
        case ("DELETE", "/v1/exits/wg"):
            if refsDelete.v { return errBody(409, "in_use", "删除出口失败：规则第 1、3 条引用了出口 wg。请先改掉这些规则。") }
            return HTTPResponse(status: 200)
        case ("POST", "/v1/exits/wg/check"):
            return json(200, "{\"tag\":\"wg\",\"cached\":false,\"status\":{\"tag\":\"wg\",\"avail\":{\"up\":false,\"pushed\":true,\"agent_up\":true,\"reason\":\"检查不通过\",\"check\":\"fail\",\"items\":{\"mainland\":\"fail\"}}}}")
        default: return json(404, "{\"error\":\"没有\",\"code\":\"not_found\"}")
        }
    }
    let m = ExitsModel(client: APIClient(transport: t))
    await m.load()
    #expect(m.rows == [ExitRow(tag: "wg", kind: "wireguard", up: true, reason: "", check: "pass", items: ["https": "pass", "ip": "pass"], tcp: 3, udp: 1)])
    #expect(m.emptyText == nil)
    // 单独检查
    await m.check("wg")
    #expect(m.rows[0].up == false && m.rows[0].check == "fail" && m.rows[0].items == ["mainland": "fail"])
    // 添加：配置内容经接口提交，提交后模型里搜不到
    let dir = tempDir()
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: dir) }
    let conf = dir.appendingPathComponent("wg2.conf")
    try "[Interface]\nPrivateKey = PRIVKEYMARK\n".write(to: conf, atomically: true, encoding: .utf8)
    m.formTag = "wg2"
    m.formFile = conf
    await m.add()
    let put = try #require(t.requests.first(where: { $0.method == "PUT" })?.body)
    #expect(String(decoding: put, as: UTF8.self).contains("PRIVKEYMARK"))
    var s = ""
    dump(m, to: &s)
    #expect((s.contains("PRIVKEYMARK")) == false && m.formFile == nil && m.formTag.isEmpty)
    // 删除被引用的出口：原样显示后台的拒绝
    refsDelete.v = true
    await m.remove("wg")
    #expect(m.notice?.text.raw == "删除出口失败：规则第 1、3 条引用了出口 wg。请先改掉这些规则。" && m.notice?.text.code == .inUse)
    // 空状态
    exits = "{\"exits\":[]}"
    let t2 = FakeTransport { req in req.path == "/v1/exits" ? json(200, "{\"exits\":[]}") : json(200, statusJSON(state: "ENABLED", gate: true)) }
    let m2 = ExitsModel(client: APIClient(transport: t2))
    await m2.load()
    #expect(m2.emptyText == L10n.noExits && L10n.noExits.contains("所有流量走主代理"))
}

@MainActor
@Test func exitsAuthorization() async throws {
    let t = FakeTransport { req in
        req.method == "GET" ? json(200, exitsJSON) : errBody(403, "needs_authorization", "需要本人授权：这是敏感操作")
    }
    let m = ExitsModel(client: APIClient(transport: t))
    await m.load()
    let dir = tempDir()
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: dir) }
    let conf = dir.appendingPathComponent("my.ovpn")
    try "client\n".write(to: conf, atomically: true, encoding: .utf8)
    m.formTag = "vpn"
    m.formKind = "openvpn"
    m.formFile = conf
    await m.add()
    // B0：配置存成只有本人能读的临时文件（用户选的文件可能在受保护的目录里），命令里只有临时文件的路径
    let tf = try #require(m.terminal?.tempFile)
    #expect(m.terminal?.commands == ["sudo abroad exit add vpn --kind openvpn --file \(tf.path)"])
    #expect(try String(contentsOf: tf, encoding: .utf8) == "client\n")
    await m.dismissTerminal()
    #expect((FileManager.default.fileExists(atPath: tf.path)) == false)
    await m.remove("wg")
    #expect(m.terminal?.commands == ["sudo abroad exit rm wg"])
}

// ---- c：应用分流 ----

let rulesDocJSON = "{\"default\":\"proxy:main\",\"rules\":[{\"app\":\"com.example.a\",\"action\":\"block\"},{\"app\":\"path:/usr/local/bin/tool\",\"action\":\"direct\"}],\"offline_allow\":[],\"local_domains\":[],\"pinned_extra\":[],\"block_udp_443\":false}"

@MainActor
@Test func appsPage() async throws {
    let putResp = Box<HTTPResponse?>(nil)
    let t = FakeTransport { req in
        switch (req.method, req.path) {
        case ("GET", "/v1/rules?format=json"): return json(200, rulesDocJSON)
        case ("GET", "/v1/apps"): return json(200, "{\"apps\":[{\"app\":\"com.google.chrome\",\"name\":\"com.google.chrome\",\"conns\":4,\"exits\":[\"main\"],\"reason\":\"\"},{\"app\":\"com.example.a\",\"name\":\"com.example.a\",\"conns\":1,\"exits\":[],\"reason\":\"\"}]}")
        case ("PUT", _): return putResp.v ?? json(200, statusJSON(state: "ENABLED", gate: true))
        default: return json(404, "{}")
        }
    }
    let m = AppsModel(client: APIClient(transport: t))
    await m.load()
    #expect(m.rows.map(\.app) == ["com.google.chrome", "com.example.a", "path:/usr/local/bin/tool"])
    #expect(m.rows[0].action == nil && m.rows[1].action == "block" && m.rows[2].name == "tool" && m.rows[0].conns == 4)
    // 收紧：直接成功；默认不断开受影响的连接
    #expect(m.dropAffected == false)
    await m.setRule(app: "com.Google.Chrome", action: "block")
    var put = try #require(t.requests.last(where: { $0.method == "PUT" }))
    #expect(put.path == "/v1/rules?drop_affected=0")
    let doc = try JSONDecoder().decode(RulesDoc.self, from: try #require(put.body))
    #expect(doc.rules.contains(RulesDoc.Rule(app: "com.google.chrome", action: "block")) && m.notice == nil)
    // 勾选后请求里带上
    m.dropAffected = true
    await m.removeRule(app: "com.example.a")
    put = try #require(t.requests.last(where: { $0.method == "PUT" }))
    #expect(put.path == "/v1/rules?drop_affected=1")
    // 放松：需要本人授权和原因
    putResp.v = errBody(403, "needs_authorization", "需要本人授权：改成本机网络会放松保护")
    await m.setRule(app: "com.example.b", action: "direct")
    #expect(m.authorizationReason?.raw == "需要本人授权：改成本机网络会放松保护" && m.terminal?.commands == ["sudo abroad app set com.example.b direct"])
    // Claude 应用 / 代理软件设本机网络：后台校验拒绝，原样显示
    for text in ["读取分流规则失败：rules 第 3 条：Claude 的应用不能走本机网络。请修改规则后重试。", "读取分流规则失败：rules 第 3 条：代理软件不能走本机网络（它转发的别的应用的流量会从本机 IP 出去）。请修改规则后重试。"] {
        putResp.v = errBody(422, "invalid", text, field: "rules[2]")
        await m.setRule(app: "com.anthropic.claudefordesktop", action: "direct")
        #expect(m.notice?.text.raw == text && m.notice?.text.field == "rules[2]")
    }
}

/// 拖入应用包得到的标识与 Go 的归一规则一致（样本由 internal/rules 的 TestAppIDSamples 生成、入库）。
@Test func appIDMatchesGo() throws {
    struct S: Decodable { var raw: String; var normalized: String }
    let url = URL(fileURLWithPath: #filePath).deletingLastPathComponent().appendingPathComponent("appid-samples.json")
    let samples = try JSONDecoder().decode([S].self, from: Data(contentsOf: url))
    #expect(samples.count >= 10)
    for s in samples { #expect(AppIdentity.normalize(s.raw) == s.normalized, "\(s.raw)") }
    // 应用包：有 Bundle ID 时取它（归一后），没有时用可执行文件的路径。
    let dir = tempDir()
    defer { try? FileManager.default.removeItem(at: dir) }
    let app = dir.appendingPathComponent("Foo.app/Contents", isDirectory: true)
    try FileManager.default.createDirectory(at: app.appendingPathComponent("MacOS"), withIntermediateDirectories: true)
    try "<?xml version=\"1.0\"?><plist version=\"1.0\"><dict><key>CFBundleIdentifier</key><string>com.Example.Foo</string><key>CFBundleExecutable</key><string>Foo</string></dict></plist>".write(to: app.appendingPathComponent("Info.plist"), atomically: true, encoding: .utf8)
    #expect(AppIdentity.appID(bundleAt: dir.appendingPathComponent("Foo.app")) == "com.example.foo")
    let bin = dir.appendingPathComponent("tool")
    try "x".write(to: bin, atomically: true, encoding: .utf8)
    #expect(AppIdentity.appID(bundleAt: bin) == "path:" + bin.standardizedFileURL.path)
}

// ---- d：环境检测 ----

let envJSON = "{\"at\":\"2026-11-22T00:00:00Z\",\"hard\":\"fail\",\"items\":[{\"id\":\"01\",\"name\":\"出口 IP\",\"level\":\"hard\",\"result\":\"pass\"},{\"id\":\"06\",\"name\":\"IPv6\",\"level\":\"hard\",\"result\":\"fail\",\"detail\":\"连上了只有 IPv6 的地址\",\"fix\":\"关掉 IPv6\"},{\"id\":\"07\",\"name\":\"DNS\",\"level\":\"hard\",\"result\":\"unknown\"},{\"id\":\"15\",\"name\":\"shell 配置\",\"level\":\"warn\",\"result\":\"skipped\",\"detail\":\"已跳过（在设置里关掉了）\"},{\"id\":\"16\",\"name\":\"时区\",\"level\":\"warn\",\"result\":\"pass\"}]}"

@MainActor
@Test func envPage() async throws {
    let disabled = Box(["15"])
    let ran = Box(false)
    let t = FakeTransport { req in
        switch (req.method, req.path) {
        case ("GET", "/v1/envcheck/settings"): return json(200, "{\"disabled\":[\(disabled.v.map { "\"\($0)\"" }.joined(separator: ","))]}")
        case ("PUT", "/v1/envcheck/settings"):
            let o = try! JSONSerialization.jsonObject(with: req.body!) as! [String: [String]]
            disabled.v = o["disabled"]!
            return json(200, String(decoding: req.body!, as: UTF8.self))
        case ("GET", "/v1/envcheck"): return json(200, envJSON)
        case ("POST", "/v1/envcheck/run"): ran.v = true; return json(200, envJSON.replacingOccurrences(of: "\"fail\",\"detail\"", with: "\"pass\",\"detail\""))
        default: return json(404, "{}")
        }
    }
    let m = EnvModel(client: APIClient(transport: t))
    await m.load()
    #expect(m.hardRows.map(\.id) == ["01", "06", "07"] && m.warnRows.map(\.id) == ["15", "16"])
    #expect(m.hardRows.map { EnvModel.resultTitle($0.result) } == [L10n.resultPass, L10n.resultFail, L10n.resultUnknown])
    #expect(EnvModel.resultTitle(m.warnRows[0].result) == L10n.resultSkipped && m.warnRows[0].enabled == false && m.warnRows[1].enabled)
    #expect(m.hardRows[1].detail == "连上了只有 IPv6 的地址" && m.hardRows[1].fix == "关掉 IPv6")
    // 警告项开关
    await m.setEnabled("16", false)
    #expect(disabled.v == ["15", "16"])
    await m.setEnabled("15", true)
    #expect(disabled.v == ["16"])
    // 硬性项不可操作（不发请求）
    let n = t.requests.count
    await m.setEnabled("06", false)
    #expect(t.requests.count == n && m.notice?.text.raw == L10n.hardItemLocked)
    // 重新检测后结果更新
    await m.rerun()
    #expect(ran.v && m.hardRows[1].result == "pass")
    #expect(m.deviceCheckText == L10n.deviceCheckHint) // M6-6a：没跑过时是提示
}

// ---- e：地区 ----

@MainActor
@Test func regionPage() async throws {
    let mode = Box("same")
    let t = FakeTransport { req in
        switch (req.method, req.path) {
        case ("GET", "/v1/region"): return json(200, "{\"target\":{\"timezone\":\"Asia/Tokyo\",\"lang\":\"ja-JP\",\"locale\":\"ja_JP\",\"source\":\"manual\"},\"pending\":[\"时区\",\"语言\"]}")
        case ("PUT", "/v1/region"):
            let body = String(decoding: req.body ?? Data(), as: UTF8.self)
            switch mode.v {
            case "same": return json(200, statusJSON(state: "ENABLED", gate: true))
            case "change": return body.contains("confirm") ? json(200, statusJSON(state: "ENABLED", gate: true)) : json(409, confirmJSON)
            case "auth": return errBody(403, "needs_authorization", "需要本人授权：地区会变")
            default: return errBody(422, "invalid", "切换地区失败：时区不认识。地区没有改动。", field: "timezone")
            }
        default: return json(404, "{}")
        }
    }
    let rc = APIClient(transport: t)
    let flow = fakeFlow(rc)
    let m = RegionModel(client: rc, flow: flow)
    await m.load()
    #expect(m.targetText == L10n.regionTarget("Asia/Tokyo", "ja-JP", "ja_JP") && m.sourceText == L10n.regionSourceManual && m.pending == ["时区", "语言"])
    m.formTimezone = "Asia/Tokyo"
    m.formLang = "ja-JP"
    await m.setManual()
    #expect(m.switched && flow.prompt == nil)
    // 会变：需要确认
    mode.v = "change"
    m.formTimezone = "Europe/Paris"
    m.formLang = "fr-FR"
    await m.setManual()
    #expect(flow.prompt?.region?.to == "B" && m.switched == false)
    await confirmNow(flow)
    #expect(m.switched)
    // 会变且要本人授权
    mode.v = "auth"
    await m.setManual()
    #expect(m.terminal?.commands == ["sudo abroad region set Europe/Paris fr-FR"])
    await m.followExit()
    #expect(m.terminal?.commands == ["sudo abroad region auto"])
    // 被拒（校验不通过）
    mode.v = "bad"
    await m.setManual()
    #expect(m.notice?.text.reason == "时区不认识" && m.notice?.text.field == "timezone" && m.switched == false)
}

@MainActor
@Test func homeShowsPendingRestore() async {
    let st = "{\"state\":\"RESTORED\",\"gate_open\":false,\"protected\":false,\"gate\":{\"open\":false,\"lease_left_ms\":0},\"region\":{\"pending\":[\"时区\"]}}"
    let t = FakeTransport { req in req.path == "/v1/status" ? json(200, st) : json(200, "{\"available\":false}") }
    let m = StatusModel(client: APIClient(transport: t), sleeper: NoSleep())
    await m.refresh()
    #expect(m.pendingRestore == ["时区"])
}

// ---- f：引擎与虚拟机、设置 ----

@MainActor
@Test func enginesAndSettings() async {
    let reachable = Box(true)
    let t = FakeTransport { req in
        switch req.path {
        case "/v1/engines":
            return reachable.v ? json(200, "{\"reachable\":true,\"version\":\"agent-1.0\",\"kernel\":\"6.12\",\"uptime_s\":42,\"boot_changed\":true,\"engines\":[{\"name\":\"mihomo\",\"state\":\"running\",\"restarts\":1}]}")
                : json(200, "{\"reachable\":false,\"boot_changed\":false,\"engines\":[]}")
        case "/v1/status": return json(200, statusJSON(state: "ENABLED", gate: true, security: true))
        case "/v1/envcheck/settings": return json(200, "{\"disabled\":[]}")
        case "/v1/envcheck": return json(200, envJSON)
        case "/v1/restore/preview": return json(200, "{\"processes\":[{\"pid\":7100,\"app\":\"Claude 桌面版\",\"cmd\":\"/Applications/Claude.app\"}],\"unverified\":[{\"pid\":7300,\"app\":\"claude\",\"cmd\":\"\",\"note\":\"签名查不到\"}],\"will_kill\":true}")
        default: return json(404, "{}")
        }
    }
    let e = EnginesModel(client: APIClient(transport: t))
    await e.load()
    #expect(e.view?.version == "agent-1.0" && e.view?.kernel == "6.12" && e.view?.uptimeS == 42 && e.view?.bootChanged == true && e.view?.engines?.first?.restarts == 1 && e.vmText == nil)
    reachable.v = false
    await e.load()
    #expect(e.state == .loaded && e.vmText == L10n.vmUnreachable, "虚拟机不可达：如实显示、不报错")
    let s = SettingsModel(client: APIClient(transport: t))
    await s.load()
    #expect(s.signatureText == L10n.signatureDisabled + "：签名校验推迟期间")
    // M6-4b 起启动管控显示后台给的状态（这个假后台没有 /v1/launchguard：如实说没报告）
    #expect(s.launchControlText == L10n.launchGuardUnknown && s.uninstallText == L10n.uninstallInInstaller)
    #expect(s.cliUsage == L10n.cliUsage)
    #expect(s.env.warnRows.count == 2)
    // 进程清单（只读预览）
    #expect(s.processes.map(\.pid) == [7100] && s.unverified.map(\.pid) == [7300] && s.processListText == nil)
    #expect(t.requests.contains { $0.method == "GET" && $0.path == "/v1/restore/preview" })
    #expect(t.requests.contains { $0.path == "/v1/restore" } == false, "不调回国的第一步（会签发确认码）")
    // 读不到：显示原因
    let t2 = FakeTransport { req in req.path == "/v1/restore/preview" ? errBody(500, "internal", "列出要结束的 Claude 进程失败：权限不够。请再试一次。") : json(200, statusJSON(state: "ENABLED", gate: true)) }
    let s2 = SettingsModel(client: APIClient(transport: t2))
    await s2.load()
    #expect(s2.processListText == "列出要结束的 Claude 进程失败：权限不够。请再试一次。")
    let t3 = FakeTransport { req in req.path == "/v1/restore/preview" ? json(200, "{\"processes\":[],\"will_kill\":true}") : json(200, statusJSON(state: "ENABLED", gate: true)) }
    let s3 = SettingsModel(client: APIClient(transport: t3))
    await s3.load()
    #expect(s3.processListText == L10n.processListNone)
}

/// M5-9 复审必须改：先读后写。加载之后别人又加了一条规则和一项 local_domains：PUT 里两者都在；
/// 重新读失败时不发 PUT；关掉终端提示后页面的行已更新。
@MainActor
@Test func appsReadBeforeWrite() async throws {
    let doc = Box(rulesDocJSON)
    let readFails = Box(false)
    let t = FakeTransport { req in
        switch (req.method, req.path) {
        case ("GET", "/v1/rules?format=json"):
            return readFails.v ? errBody(500, "internal", "读规则失败：磁盘出错。请再试一次。") : json(200, doc.v)
        case ("GET", "/v1/apps"): return json(200, "{\"apps\":[]}")
        case ("GET", "/v1/exits"): return json(200, exitsJSON)
        case ("PUT", _):
            if String(decoding: req.body ?? Data(), as: UTF8.self).contains("\"direct\"") && String(decoding: req.body ?? Data(), as: UTF8.self).contains("com.example.x") {
                return errBody(403, "needs_authorization", "需要本人授权：放松保护")
            }
            return json(200, statusJSON(state: "ENABLED", gate: true))
        default: return json(404, "{}")
        }
    }
    let m = AppsModel(client: APIClient(transport: t))
    await m.load()
    #expect(m.actions == ["direct", "block", "proxy:main", "proxy:wg"], "动作菜单列出分流出口")
    // 别的客户端在页面打开期间改了规则
    doc.v = "{\"default\":\"proxy:main\",\"rules\":[{\"app\":\"com.example.a\",\"action\":\"block\"},{\"app\":\"com.other.z\",\"action\":\"direct\"}],\"offline_allow\":[],\"local_domains\":[\"intranet.test\"],\"pinned_extra\":[],\"block_udp_443\":false}"
    await m.setRule(app: "com.example.y", action: "proxy:wg")
    let put = try #require(t.requests.last(where: { $0.method == "PUT" }))
    let sent = try JSONDecoder().decode(RulesDoc.self, from: try #require(put.body))
    #expect(sent.rules.contains(RulesDoc.Rule(app: "com.other.z", action: "direct")) && sent.localDomains == ["intranet.test"])
    #expect(sent.rules.contains(RulesDoc.Rule(app: "com.example.y", action: "proxy:wg")))
    // 终端退路：关掉提示后重新加载，页面上有终端里加的规则
    await m.setRule(app: "com.example.x", action: "direct")
    #expect(m.terminal?.commands == ["sudo abroad app set com.example.x direct"])
    doc.v = "{\"default\":\"proxy:main\",\"rules\":[{\"app\":\"com.example.x\",\"action\":\"direct\"}],\"offline_allow\":[],\"local_domains\":[],\"pinned_extra\":[],\"block_udp_443\":false}"
    await m.dismissTerminal()
    #expect(m.terminal == nil && m.rows.contains { $0.app == "com.example.x" && $0.action == "direct" })
    // 指向分流出口的终端命令与命令行的写法一致
    #expect(SensitiveOperation.appSet(app: "com.example.y", action: "proxy:wg").terminalCommand == "sudo abroad app set com.example.y proxy:wg")
    // 重新读失败：不发 PUT
    readFails.v = true
    let n = t.requests.filter { $0.method == "PUT" }.count
    await m.setRule(app: "com.example.q", action: "block")
    #expect(t.requests.filter { $0.method == "PUT" }.count == n)
    guard case .failed = m.state else { Issue.record("重新读失败应按加载失败处理：\(m.state)"); return }
}

/// 环境检测的开关也先读后写。
@MainActor
@Test func envToggleReadBeforeWrite() async throws {
    let disabled = Box(["15"])
    let t = FakeTransport { req in
        switch (req.method, req.path) {
        case ("GET", "/v1/envcheck/settings"): return json(200, "{\"disabled\":[\(disabled.v.map { "\"\($0)\"" }.joined(separator: ","))]}")
        case ("PUT", "/v1/envcheck/settings"): return json(200, String(decoding: req.body!, as: UTF8.self))
        case ("GET", "/v1/envcheck"): return json(200, envJSON)
        default: return json(404, "{}")
        }
    }
    let m = EnvModel(client: APIClient(transport: t))
    await m.load()
    disabled.v = ["15", "17"] // 别处在这期间关掉了 17
    await m.setEnabled("16", false)
    let put = try #require(t.requests.last(where: { $0.method == "PUT" })?.body)
    #expect(String(decoding: put, as: UTF8.self) == "{\"disabled\":[\"15\",\"16\",\"17\"]}")
}

// 2026-11-28：首页环境检测要点里两项警告不能把 07 DNS、08 WebRTC 挤掉。
@Test func homeEnvItemsKeepKeyItems() throws {
    func it(_ id: String, _ r: String) -> EnvcheckItemResult {
        try! JSONDecoder().decode(EnvcheckItemResult.self, from: Data(#"{"id":"\#(id)","name":"项","level":"hard","result":"\#(r)"}"#.utf8))
    }
    let raw = ["01", "02", "03", "04", "05", "06", "07", "08", "09"].map { it($0, "pass") } + [it("16", "unknown"), it("14", "unknown"), it("10", "fail")]
    let got = EnvModel.homeItems(raw).map(\.id)
    #expect(got == ["10", "14", "16", "01", "02", "03", "07", "08", "04"])
    #expect(EnvModel.homeItems([it("07", "fail"), it("01", "pass")]).map(\.id) == ["07", "01"])
}
