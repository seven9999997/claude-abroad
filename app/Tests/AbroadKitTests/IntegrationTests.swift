// 验收 f：对开发模式的假系统 abroadd 的集成测试。由 scripts/dev/app-test.sh 启动 abroadd 并经 ABROAD_SOCKET 给出 socket；
// 没有这个环境变量时跳过（单独跑 swift test 时）。
import AbroadAPI
import Foundation
import Testing
@testable import AbroadKit

let integrationSocket = ProcessInfo.processInfo.environment["ABROAD_SOCKET"] ?? ""

@Suite(.serialized, .enabled(if: !integrationSocket.isEmpty, "要先由 scripts/dev/app-test.sh 启动开发模式的 abroadd"))
struct IntegrationTests {
    let client = APIClient(transport: UnixTransport(path: integrationSocket, timeout: 30))

    @Test func readStatusAndExit() async throws {
        guard case .ok(let st) = await client.status() else { Issue.record("读状态失败"); return }
        #expect(["RESTORED", "DISABLED", "ENABLED"].contains(st.state))
        #expect(st.security?.callerVerification == "disabled")
        guard case .ok(let e) = await client.mainExit() else { Issue.record("读出口信息失败"); return }
        // 应用主代理之前没有基准（如实说明）；之后是假检查给的文档地址。
        if e.available {
            #expect(e.ip == "203.0.113.7" && e.country == "US")
        } else {
            let note = e.note ?? ""
            #expect(note.isEmpty == false)
        }
    }

    /// M5-8 验收 f：走和界面相同的视图模型（不经过任何视图）：引导的进度 → 添加本地配置文件形式的来源并列出 →
    /// 选择节点 → 应用主代理（开发模式下属主就是完全信任的调用方）→ 完成引导 → 启用。
    /// 假系统里没有的环节如实跳过：订阅的真实抓取（假系统不抓取，不在这里测）。
    @MainActor
    @Test func setupFlowThroughModels() async throws {
        let setup = SetupModel(client: client, sleeper: TaskSleeper())
        await setup.refresh()
        #expect(setup.reachable)
        if setup.completed { return } // 同一个 abroadd 上已经走过（测试顺序不保证）
        await setup.next()
        #expect(setup.step == .mainProxy)
        let mainDone = setup.progress?.steps?.first(where: { $0.id == "main_proxy" })?.done ?? false
        let main = MainProxyModel(client: client, flow: fakeFlow(client))
        if mainDone == false {
            try await IntegrationTests.applyMainProxy(main)
        }
        await setup.recheck()
        #expect(setup.canAdvance, "应用主代理之后后台说第三步完成")
        await setup.next()
        #expect(setup.step == .done && setup.completed && setup.shouldShow == false)
        // 启用。
        let st = StatusModel(client: client, sleeper: TaskSleeper(), flow: fakeFlow(client))
        await st.refresh()
        if st.phase != .abroad {
            await st.enable()
            if st.flow.isActive { await confirmNow(st.flow) }
        }
        #expect(st.phase == .abroad, "启用后应是已出国：\(String(describing: st.notice))")
        await st.disable()
    }

    /// 添加一个本地配置文件形式的来源、选节点、应用主代理（有基准之后其他测试才能启用）。
    @MainActor
    static func applyMainProxy(_ main: MainProxyModel) async throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("abroad-it-" + UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        let file = dir.appendingPathComponent("nodes.yaml")
        try "proxies:\n  - {name: it-exit, type: ss, server: 8.8.4.20, port: 8388, cipher: aes-128-gcm, password: itpass}\n".write(to: file, atomically: true, encoding: .utf8)
        await main.refresh()
        main.addKind = .localFile
        main.formFile = file
        main.formName = "集成测试"
        await main.submitAdd()
        #expect(main.notice == nil, "添加来源：\(String(describing: main.notice))")
        #expect(main.sources.contains { $0.name == "集成测试" && $0.nodeCount == 1 })
        let node = try #require(main.nodes.first { $0.name == "it-exit" })
        main.selectedExit = node.id
        await main.apply()
        if main.flow.isActive { await confirmNow(main.flow) }
        guard case .succeeded(let country, _) = main.applyState else {
            Issue.record("应用主代理：\(main.applyState)"); return
        }
        #expect(country == "US")
    }

    @MainActor
    @Test func enableDisableWithEvents() async throws {
        // 启用要有出口基准：还没应用过主代理时先应用（测试顺序不保证）。
        if case .ok(let sv) = await client.setup(), sv.steps?.first(where: { $0.id == "main_proxy" })?.done != true {
            try await IntegrationTests.applyMainProxy(MainProxyModel(client: client, flow: fakeFlow(client)))
        }
        let m = StatusModel(client: client, sleeper: TaskSleeper())
        m.start()
        defer { m.stop() }
        #expect(await eventually { m.reachable })
        // 先回到停用状态。
        if m.phase == .abroad { await m.disable() }
        let before = m.status?.state
        // 另起一个客户端直接启用：视图模型应经事件流看到变化。
        let other = APIClient(transport: UnixTransport(path: integrationSocket, timeout: 30))
        var out: Outcome<DaemonStatus> = await other.enable()
        if case .needsConfirm(let p) = out { out = await other.confirm(p, as: DaemonStatus.self) }
        guard case .ok(let st) = out else { Issue.record("启用失败：\(out)"); return }
        #expect(st.state == "ENABLED")
        #expect(await eventually(timeoutMs: 5000) { m.phase == .abroad }, "事件流应带来状态变化（之前 \(before ?? "")）")
        await m.disable()
        #expect(m.phase == .offline || m.phase == .home)
        #expect(m.status?.state == "DISABLED")
    }

    @Test func eventsStream() async throws {
        let t = UnixTransport(path: integrationSocket, timeout: 30)
        final class Got: @unchecked Sendable { var n = 0; let lock = NSLock() }
        let got = Got()
        let task = Task {
            try await t.events(path: "/v1/events") { _ in got.lock.lock(); got.n += 1; got.lock.unlock() }
        }
        if case .ok(let sv) = await client.setup(), sv.steps?.first(where: { $0.id == "main_proxy" })?.done != true {
            try await IntegrationTests.applyMainProxy(MainProxyModel(client: client, flow: fakeFlow(client)))
        }
        try await Task.sleep(nanoseconds: 300_000_000)
        var out: Outcome<DaemonStatus> = await client.enable()
        if case .needsConfirm(let p) = out { out = await client.confirm(p, as: DaemonStatus.self) }
        _ = await client.disable()
        for _ in 0..<250 {
            got.lock.lock(); let n = got.n; got.lock.unlock()
            if n > 0 { break }
            try await Task.sleep(nanoseconds: 20_000_000)
        }
        task.cancel()
        got.lock.lock(); let n = got.n; got.lock.unlock()
        #expect(n > 0, "状态变化时应收到事件")
    }

    /// M5-9 验收 h：其余页面的视图模型对假系统 abroadd。假系统没有环境检测：页面是“这个模式不提供”。
    @MainActor
    @Test func otherPages() async throws {
        let apps = AppsModel(client: client)
        await apps.load()
        #expect(apps.state == .loaded)
        await apps.setRule(app: "com.example.integration", action: "block") // 收紧：直接提交
        // 失败时打出当时的提示（代码和全文），任何一次失败都能直接看到原因（architect 判 M6-3）。
        #expect(apps.notice == nil, "收紧的规则：代码 \(String(describing: apps.notice?.text.code))，提示 \(apps.notice?.text.raw ?? "")")
        let again = AppsModel(client: client)
        await again.load()
        #expect(again.rules?.rules.contains(RulesDoc.Rule(app: "com.example.integration", action: "block")) == true,
                "读回规则：\(String(describing: again.rules?.rules))；读取状态 \(again.state)；提示 \(again.notice?.text.raw ?? "")")
        await again.removeRule(app: "com.example.integration")

        let env = EnvModel(client: client)
        await env.load()
        guard case .unsupported(let t) = env.state else { Issue.record("假系统没有环境检测，应是“这个模式不提供”：\(env.state)"); return }
        #expect(t.code == .unsupported)

        let region = RegionModel(client: client)
        await region.load()
        #expect(region.state == .loaded)

        let engines = EnginesModel(client: client)
        await engines.load()
        #expect(engines.state == .loaded && engines.view?.reachable == true, "假虚拟机（--fake-main-proxy）答得上")

        let exits = ExitsModel(client: client)
        await exits.load()
        #expect(exits.state == .loaded && exits.emptyText == L10n.noExits)
    }

    @Test func unreachableKinds() async throws {
        let c = APIClient(transport: UnixTransport(path: integrationSocket + ".nope", timeout: 5))
        guard case .unreachable(.notRunning) = await c.status() else { Issue.record("不存在的 socket 应是 notRunning"); return }
    }
}

@MainActor
func eventually(timeoutMs: Int, _ f: @MainActor () -> Bool) async -> Bool {
    for _ in 0..<(timeoutMs / 5) {
        if f() { return true }
        try? await Task.sleep(nanoseconds: 5_000_000)
    }
    return f()
}
