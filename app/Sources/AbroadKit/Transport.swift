// 传输层：协议（视图模型和接口客户端只认它，测试注入假的）和经 unix socket 的实现（Network.framework 的 NWConnection）。
import Foundation
import Network

/// 连不上后台服务的三种情况（和命令行一致：不打印底层的连接错误）。
public enum ConnectionError: Error, Equatable, Sendable {
    case notRunning // socket 不存在
    case noListener // socket 文件在，但没有进程在听
    case rejected // 连上后被断开（不是当前登录的用户或 root）
    case timeout
    case protocolError(String)
}

public protocol Transport: Sendable {
    /// 发一个请求、收齐应答。
    func send(_ request: HTTPRequest) async throws -> HTTPResponse
    /// 订阅事件流：每条事件调用 onEvent；连接断开时返回（正常结束），连不上或应答不是 200 时抛错。
    /// 任务被取消时尽快返回。
    func events(path: String, onEvent: @escaping @Sendable (SSEEvent) -> Void) async throws
}

/// 等待（测试注入，不真的睡）。
public protocol Sleeper: Sendable {
    func sleep(seconds: Double) async throws
}

public struct TaskSleeper: Sleeper {
    public init() {}
    public func sleep(seconds: Double) async throws {
        try await Task.sleep(nanoseconds: UInt64(max(0, seconds) * 1_000_000_000))
    }
}

/// 经 unix socket 的 HTTP/1.1。一次连接一个请求（Connection: close）。
public final class UnixTransport: Transport, @unchecked Sendable {
    public let path: String
    public let timeout: Double
    private let queue = DispatchQueue(label: "claude-abroad.transport")

    /// 真实模式的位置（design 11.1）；开发和测试用环境变量 ABROAD_SOCKET 指定。
    public static let defaultSocket = "/var/run/claude-abroad/api.sock"

    public static func fromEnvironment(_ env: [String: String] = ProcessInfo.processInfo.environment) -> UnixTransport {
        UnixTransport(path: env["ABROAD_SOCKET"].flatMap { $0.isEmpty ? nil : $0 } ?? defaultSocket)
    }

    public init(path: String, timeout: Double = 300) {
        self.path = path
        self.timeout = timeout
    }

    public func send(_ request: HTTPRequest) async throws -> HTTPResponse {
        final class Box: @unchecked Sendable { var status = 0; var headers: [String: String] = [:]; var body = Data() }
        let box = Box()
        let conn = Conn()
        return try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { (cont: CheckedContinuation<HTTPResponse, Error>) in
                run(request, conn: conn, timeout: timeout,
                    onHead: { s, h in box.status = s; box.headers = h },
                    onBody: { box.body.append($0) },
                    done: { err in
                        if let err { cont.resume(throwing: err) } else {
                            cont.resume(returning: HTTPResponse(status: box.status, headers: box.headers, body: box.body))
                        }
                    })
            }
        } onCancel: { conn.cancel() }
    }

    public func events(path: String, onEvent: @escaping @Sendable (SSEEvent) -> Void) async throws {
        final class State: @unchecked Sendable { var parser = SSEParser(); var status = 0; var body = Data() }
        let st = State()
        let conn = Conn()
        try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { (cont: CheckedContinuation<Void, Error>) in
                run(HTTPRequest(method: "GET", path: path), conn: conn, timeout: nil,
                    onHead: { s, _ in st.status = s },
                    onBody: { data in
                        if st.status != 200 { st.body.append(data.prefix(64 * 1024)); return }
                        do {
                            for ev in try st.parser.feed(data) { onEvent(ev) }
                        } catch { conn.cancel() }
                    },
                    done: { err in
                        if let err {
                            cont.resume(throwing: err)
                        } else if st.status != 200 {
                            cont.resume(throwing: APIError.status(st.status, ErrorText.from(body: st.body)))
                        } else {
                            cont.resume() // 后台服务断开了
                        }
                    })
            }
        } onCancel: { conn.cancel() }
    }

    /// 持有 NWConnection，便于取消（取消可能先于连接建立）。
    final class Conn: @unchecked Sendable {
        private let lock = NSLock()
        private var c: NWConnection?
        private var cancelled = false
        func set(_ n: NWConnection) -> Bool {
            lock.lock(); defer { lock.unlock() }
            if cancelled { return false }
            c = n
            return true
        }
        func cancel() {
            lock.lock(); cancelled = true; let n = c; lock.unlock()
            n?.cancel()
        }
        var isCancelled: Bool { lock.lock(); defer { lock.unlock() }; return cancelled }
    }

    private func run(_ request: HTTPRequest, conn holder: Conn, timeout: Double?,
                     onHead: @escaping @Sendable (Int, [String: String]) -> Void,
                     onBody: @escaping @Sendable (Data) -> Void,
                     done: @escaping @Sendable (Error?) -> Void) {
        // done 只回调一次：取消之后 NWConnection 还可能再报一次失败。
        final class Once: @unchecked Sendable {
            let lock = NSLock(); var fired = false
            func fire() -> Bool { lock.lock(); defer { lock.unlock() }; if fired { return false }; fired = true; return true }
        }
        let once = Once()
        let socketPath = path
        let finish: @Sendable (Error?) -> Void = { err in if once.fire() { done(err) } }
        guard FileManager.default.fileExists(atPath: socketPath) else { finish(ConnectionError.notRunning); return }
        let conn = NWConnection(to: .unix(path: socketPath), using: .tcp)
        guard holder.set(conn) else { finish(CancellationError()); return }
        final class P: @unchecked Sendable { var parser = HTTPParser(); var headSent = false; var gotBytes = false }
        let p = P()
        if let timeout {
            queue.asyncAfter(deadline: .now() + timeout) {
                if once.fired { return }
                conn.cancel()
                finish(ConnectionError.timeout)
            }
        }
        @Sendable func classify(_ e: NWError) -> Error {
            if holder.isCancelled { return CancellationError() }
            if case .posix(let code) = e {
                switch code {
                case .ENOENT: return ConnectionError.notRunning
                case .ECONNREFUSED: return ConnectionError.noListener
                // 连上后被断开（M6-3 复审：接受后立刻断开时 Network.framework 报的是 ENETDOWN；只连本机 unix socket，
                // 这几种都只能是后台断开的）。没有进程在听是 ECONNREFUSED，不在这里。
                case .ECONNRESET, .EPIPE, .ENOTCONN, .ECONNABORTED, .ENETDOWN: return ConnectionError.rejected
                default: break
                }
            }
            if !FileManager.default.fileExists(atPath: socketPath) { return ConnectionError.notRunning }
            return ConnectionError.protocolError("连接出错")
        }
        @Sendable func receive() {
            conn.receive(minimumIncompleteLength: 1, maximumLength: 64 * 1024) { data, _, isComplete, err in
                if let err { conn.cancel(); finish(classify(err)); return }
                if let data, !data.isEmpty {
                    p.gotBytes = true
                    do {
                        let chunk = try p.parser.feed(data)
                        if p.parser.headerDone && !p.headSent { p.headSent = true; onHead(p.parser.status, p.parser.headers) }
                        if !chunk.isEmpty { onBody(chunk) }
                    } catch { conn.cancel(); finish(error); return }
                }
                if p.parser.finished { conn.cancel(); finish(nil); return }
                if isComplete {
                    conn.cancel()
                    if !p.gotBytes { finish(holder.isCancelled ? CancellationError() : ConnectionError.rejected); return }
                    do { try p.parser.finishAtEOF(); finish(nil) } catch { finish(error) }
                    return
                }
                receive()
            }
        }
        conn.stateUpdateHandler = { st in
            switch st {
            case .ready:
                conn.send(content: request.bytes, completion: .contentProcessed { e in
                    if let e { conn.cancel(); finish(classify(e)) }
                })
                receive()
            case .waiting(let e), .failed(let e):
                conn.cancel()
                finish(classify(e))
            case .cancelled:
                finish(holder.isCancelled ? CancellationError() : ConnectionError.rejected)
            default: break
            }
        }
        conn.start(queue: queue)
    }
}
