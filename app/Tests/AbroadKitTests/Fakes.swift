// 测试用的假传输层和假的等待。
import AbroadAPI
import Foundation
@testable import AbroadKit

final class FakeTransport: Transport, @unchecked Sendable {
    private let lock = NSLock()
    private var _requests: [HTTPRequest] = []
    private var handler: @Sendable (HTTPRequest) throws -> HTTPResponse
    /// 事件流：连上时把回调交给测试，测试决定何时推事件、何时断开。
    private var eventSink: (@Sendable (SSEEvent) -> Void)?
    private var eventEnd: CheckedContinuation<Void, Error>?
    var eventsError: Error?
    private(set) var eventConnects = 0

    init(_ handler: @escaping @Sendable (HTTPRequest) throws -> HTTPResponse) { self.handler = handler }

    func setHandler(_ h: @escaping @Sendable (HTTPRequest) throws -> HTTPResponse) {
        lock.lock(); handler = h; lock.unlock()
    }

    var requests: [HTTPRequest] { lock.lock(); defer { lock.unlock() }; return _requests }

    func send(_ request: HTTPRequest) async throws -> HTTPResponse {
        lock.lock()
        _requests.append(request)
        let h = handler
        lock.unlock()
        return try h(request)
    }

    func events(path: String, onEvent: @escaping @Sendable (SSEEvent) -> Void) async throws {
        lock.lock()
        eventConnects += 1
        if let e = eventsError { lock.unlock(); throw e }
        eventSink = onEvent
        lock.unlock()
        try await withCheckedThrowingContinuation { (c: CheckedContinuation<Void, Error>) in
            lock.lock(); eventEnd = c; lock.unlock()
        }
    }

    var subscribed: Bool { lock.lock(); defer { lock.unlock() }; return eventEnd != nil }

    func push(_ ev: SSEEvent) {
        lock.lock(); let s = eventSink; lock.unlock()
        s?(ev)
    }

    /// 后台断开事件流。
    func dropEvents(with error: Error? = nil) {
        lock.lock(); let c = eventEnd; eventEnd = nil; eventSink = nil; lock.unlock()
        if let error { c?.resume(throwing: error) } else { c?.resume() }
    }
}

struct NoSleep: Sleeper {
    func sleep(seconds: Double) async throws { await Task.yield() }
}

func json(_ status: Int, _ s: String) -> HTTPResponse {
    HTTPResponse(status: status, headers: ["content-type": "application/json"], body: Data(s.utf8))
}

func statusJSON(state: String, gate: Bool, protected: Bool = true, corrupt: Bool = false, security: Bool = true, reasons: [String] = []) -> String {
    let r = reasons.map { "\"\($0)\"" }.joined(separator: ",")
    let sec = security ? ",\"security\":{\"caller_verification\":\"disabled\",\"note\":\"签名校验推迟期间\",\"caller\":\"console\"}" : ""
    return "{\"state\":\"\(state)\",\"corrupt\":\(corrupt),\"gate_open\":\(gate),\"protected\":\(protected),\"gate\":{\"open\":\(gate),\"reasons\":[\(r)],\"lease_left_ms\":0}\(sec)}"
}

let exitJSON = "{\"available\":true,\"ip\":\"203.0.113.7\",\"country\":\"US\",\"city\":\"Exampleville\",\"timezone\":\"America/Los_Angeles\"}"
let confirmJSON = "{\"error\":\"启用需要确认：地区会变。确认后带上确认码再调用一次\",\"confirm\":\"0123456789abcdef0123456789abcdef\",\"expires_in_s\":60,\"preview\":{\"processes\":[{\"pid\":7100,\"app\":\"Claude 桌面版\",\"cmd\":\"/Applications/Claude.app\"}],\"will_kill\":true},\"region\":{\"from\":\"A\",\"to\":\"B\"}}"

/// 等到条件成立（主线程上的视图模型由事件触发的刷新是异步的）。
@MainActor
func eventually(_ f: @MainActor () -> Bool) async -> Bool {
    for _ in 0..<500 {
        if f() { return true }
        try? await Task.sleep(nanoseconds: 2_000_000)
    }
    return f()
}
