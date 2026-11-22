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
        // 假系统没有出口基准：如实说明。
        #expect(e.available == false && !(e.note ?? "").isEmpty)
    }

    @MainActor
    @Test func enableDisableWithEvents() async throws {
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
