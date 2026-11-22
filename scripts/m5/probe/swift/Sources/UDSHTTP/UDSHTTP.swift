// M5-1 探查 E4：经 unix socket 访问 abroadd 的最小 HTTP/1.1 客户端（URLSession 不支持 unix socket）。
// 传输用 Network.framework 的 NWConnection（NWEndpoint.unix）；报文解析是纯函数（HTTPParser、SSEParser），单测覆盖。
import Foundation
import Network

public struct HTTPResponse: Sendable {
    public var status: Int
    public var headers: [String: String] // 键一律小写
    public var body: Data
}

public enum UDSHTTPError: Error, Equatable {
    case badStatusLine, badHeader, badChunk, closedEarly, tooLarge
}

/// 增量解析 HTTP/1.1 应答：状态行、头部，正文按 Content-Length、分块传输或读到连接关闭。
public struct HTTPParser {
    public private(set) var status = 0
    public private(set) var headers: [String: String] = [:]
    public private(set) var headerDone = false
    private var buf = Data()
    private var chunkLeft: Int? = nil // 分块：当前块还剩多少字节（nil 表示要读块长度那一行）
    public private(set) var finished = false
    public static let maxHeader = 64 * 1024

    public init() {}

    /// 喂一段字节，返回这段里解出的正文字节（分块已经去掉）。
    public mutating func feed(_ data: Data) throws -> Data {
        if finished { return Data() } // 已经收完：后面的字节（例如分块结尾的空行）不再处理
        buf.append(data)
        var out = Data()
        if !headerDone {
            guard let r = buf.range(of: Data("\r\n\r\n".utf8)) else {
                if buf.count > Self.maxHeader { throw UDSHTTPError.tooLarge }
                return out
            }
            let head = String(decoding: buf[buf.startIndex..<r.lowerBound], as: UTF8.self)
            buf = Data(buf[r.upperBound...])
            var lines = head.components(separatedBy: "\r\n")
            let first = lines.removeFirst().split(separator: " ", maxSplits: 2)
            guard first.count >= 2, first[0].hasPrefix("HTTP/1."), let code = Int(first[1]) else { throw UDSHTTPError.badStatusLine }
            status = code
            for l in lines where !l.isEmpty {
                guard let i = l.firstIndex(of: ":") else { throw UDSHTTPError.badHeader }
                headers[l[..<i].trimmingCharacters(in: .whitespaces).lowercased()] = l[l.index(after: i)...].trimmingCharacters(in: .whitespaces)
            }
            headerDone = true
        }
        if chunked {
            while true {
                if chunkLeft == nil {
                    guard let r = buf.range(of: Data("\r\n".utf8)) else { break }
                    let line = String(decoding: buf[buf.startIndex..<r.lowerBound], as: UTF8.self)
                    guard let size = line.split(separator: ";").first,
                          let n = Int(size.trimmingCharacters(in: .whitespaces), radix: 16) else { throw UDSHTTPError.badChunk }
                    buf = Data(buf[r.upperBound...])
                    if n == 0 { finished = true; buf.removeAll(); break }
                    chunkLeft = n
                }
                guard let left = chunkLeft, buf.count >= left + 2 else {
                    // 块还没收全：先交出已经到的部分
                    if let left = chunkLeft, !buf.isEmpty {
                        let take = min(left, buf.count)
                        out.append(buf.prefix(take)); buf = Data(buf.dropFirst(take)); chunkLeft = left - take
                    }
                    break
                }
                out.append(buf.prefix(left))
                buf = Data(buf.dropFirst(left + 2)) // 去掉块后面的 \r\n
                chunkLeft = nil
            }
            // 块的数据交完、但后面的 \r\n 还没到时，chunkLeft 为 0：等下一段
            if chunkLeft == 0, buf.count >= 2 { buf = Data(buf.dropFirst(2)); chunkLeft = nil }
        } else if let cl = contentLength {
            out = buf; buf.removeAll()
            received += out.count
            if received >= cl { finished = true }
        } else {
            out = buf; buf.removeAll()
        }
        return out
    }

    private var received = 0
    public var chunked: Bool { headers["transfer-encoding"]?.lowercased().contains("chunked") ?? false }
    public var contentLength: Int? { headers["content-length"].flatMap { Int($0) } }
}

/// 一条 Server-Sent Event。
public struct SSEEvent: Sendable, Equatable {
    public var id: String?
    public var event: String
    public var data: String
}

/// 逐行解析 SSE（空行结束一条事件；以冒号开头的是注释；data 多行用换行连接）。
public struct SSEParser {
    private var line = Data()
    private var id: String?
    private var event = ""
    private var data: [String] = []
    public init() {}

    public mutating func feed(_ bytes: Data) -> [SSEEvent] {
        var out: [SSEEvent] = []
        for b in bytes {
            if b == UInt8(ascii: "\n") {
                var l = String(decoding: line, as: UTF8.self)
                if l.hasSuffix("\r") { l.removeLast() }
                line.removeAll()
                if l.isEmpty {
                    if !data.isEmpty || !event.isEmpty {
                        out.append(SSEEvent(id: id, event: event.isEmpty ? "message" : event, data: data.joined(separator: "\n")))
                    }
                    event = ""; data = []
                    continue
                }
                if l.hasPrefix(":") { continue }
                let parts = l.split(separator: ":", maxSplits: 1, omittingEmptySubsequences: false)
                let k = String(parts[0])
                var v = parts.count > 1 ? String(parts[1]) : ""
                if v.hasPrefix(" ") { v.removeFirst() }
                switch k {
                case "id": id = v
                case "event": event = v
                case "data": data.append(v)
                default: break
                }
            } else {
                line.append(b)
            }
        }
        return out
    }
}

/// 经 unix socket 发一个 HTTP/1.1 请求（Connection: close）。
public final class UnixHTTPClient: @unchecked Sendable {
    public let path: String
    private let queue = DispatchQueue(label: "udshttp")
    public init(path: String) { self.path = path }

    static func requestBytes(method: String, path: String, body: Data?) -> Data {
        var s = "\(method) \(path) HTTP/1.1\r\nHost: abroadd\r\nConnection: close\r\nAccept: */*\r\n"
        if let body { s += "Content-Type: application/json\r\nContent-Length: \(body.count)\r\n" }
        s += "\r\n"
        var d = Data(s.utf8)
        if let body { d.append(body) }
        return d
    }

    /// 流式：每收到一段正文就回调一次；结束时回调 done。
    public func stream(method: String = "GET", path: String, body: Data? = nil,
                       onHead: @escaping @Sendable (Int, [String: String]) -> Void = { _, _ in },
                       onBody: @escaping @Sendable (Data) -> Void,
                       done: @escaping @Sendable (Error?) -> Void) {
        let conn = NWConnection(to: .unix(path: self.path), using: .tcp)
        var parser = HTTPParser()
        var headSent = false
        func receive() {
            conn.receive(minimumIncompleteLength: 1, maximumLength: 64 * 1024) { data, _, isComplete, err in
                if let err { conn.cancel(); done(err); return }
                if let data, !data.isEmpty {
                    do {
                        let chunk = try parser.feed(data)
                        if parser.headerDone && !headSent { headSent = true; onHead(parser.status, parser.headers) }
                        if !chunk.isEmpty { onBody(chunk) }
                    } catch { conn.cancel(); done(error); return }
                }
                if parser.finished || isComplete {
                    conn.cancel()
                    done(parser.headerDone ? nil : UDSHTTPError.closedEarly)
                    return
                }
                receive()
            }
        }
        conn.stateUpdateHandler = { st in
            switch st {
            case .ready:
                conn.send(content: Self.requestBytes(method: method, path: path, body: body), completion: .contentProcessed { e in
                    if let e { conn.cancel(); done(e) }
                })
                receive()
            case .failed(let e): conn.cancel(); done(e)
            default: break
            }
        }
        conn.start(queue: queue)
    }

    /// 一次性请求：收齐正文再返回。
    public func request(method: String = "GET", path: String, body: Data? = nil) async throws -> HTTPResponse {
        final class Box: @unchecked Sendable { var status = 0; var headers: [String: String] = [:]; var body = Data() }
        let box = Box()
        return try await withCheckedThrowingContinuation { cont in
            stream(method: method, path: path, body: body,
                   onHead: { s, h in box.status = s; box.headers = h },
                   onBody: { box.body.append($0) },
                   done: { err in
                       if let err { cont.resume(throwing: err) } else { cont.resume(returning: HTTPResponse(status: box.status, headers: box.headers, body: box.body)) }
                   })
        }
    }
}
