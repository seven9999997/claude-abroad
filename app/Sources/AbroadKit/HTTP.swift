// 经 unix socket 访问 abroadd 的最小 HTTP/1.1（URLSession 不支持 unix socket；M5-1 的 E4）。
// 这里是纯函数部分：请求的字节、应答的增量解析、事件流的逐条解析，全部可以逐字节喂入单测。
import Foundation

public struct HTTPRequest: Sendable, Equatable {
    public var method: String
    public var path: String
    public var body: Data?
    public var contentType: String
    /// 条件写入（规则，补齐一）：非空时带 If-Match 头。
    public var ifMatch: String?

    public init(method: String, path: String, body: Data? = nil, contentType: String = "application/json", ifMatch: String? = nil) {
        self.method = method
        self.path = path
        self.body = body
        self.contentType = contentType
        self.ifMatch = ifMatch
    }

    /// 请求的字节（Connection: close：一次连接一个请求）。
    public var bytes: Data {
        var s = "\(method) \(path) HTTP/1.1\r\nHost: abroadd\r\nConnection: close\r\nAccept: */*\r\n"
        if let ifMatch, !ifMatch.isEmpty, !ifMatch.contains("\r"), !ifMatch.contains("\n") { s += "If-Match: \(ifMatch)\r\n" }
        if let body { s += "Content-Type: \(contentType)\r\nContent-Length: \(body.count)\r\n" }
        s += "\r\n"
        var d = Data(s.utf8)
        if let body { d.append(body) }
        return d
    }
}

public struct HTTPResponse: Sendable, Equatable {
    public var status: Int
    public var headers: [String: String] // 键一律小写
    public var body: Data

    public init(status: Int, headers: [String: String] = [:], body: Data = Data()) {
        self.status = status
        self.headers = headers
        self.body = body
    }
}

public enum HTTPError: Error, Equatable {
    case badStatusLine, badHeader, badChunk, closedEarly
    case headerTooLarge, bodyTooLarge, lineTooLong
}

/// 增量解析 HTTP/1.1 应答：状态行、头部，正文按 Content-Length、分块传输或读到连接关闭。
/// 头部超过 maxHeader、正文超过 maxBody 时报错（防止一个出错的对端把应用的内存吃满）。
public struct HTTPParser {
    public private(set) var status = 0
    public private(set) var headers: [String: String] = [:]
    public private(set) var headerDone = false
    public private(set) var finished = false
    public let maxHeader: Int
    public let maxBody: Int

    private var buf = Data()
    private var chunkLeft: Int? // 分块：当前块还剩多少字节（nil 表示要读块长度那一行）
    private var chunkTrailer = false // 一个块的数据读完了，等它后面的 \r\n
    private var received = 0

    public static let defaultMaxHeader = 64 * 1024
    public static let defaultMaxBody = 16 * 1024 * 1024

    public init(maxHeader: Int = HTTPParser.defaultMaxHeader, maxBody: Int = HTTPParser.defaultMaxBody) {
        self.maxHeader = maxHeader
        self.maxBody = maxBody
    }

    public var chunked: Bool { headers["transfer-encoding"]?.lowercased().contains("chunked") ?? false }
    public var contentLength: Int? { headers["content-length"].flatMap { Int($0) } }

    /// 喂一段字节，返回这段里解出的正文字节（分块已经去掉）。
    public mutating func feed(_ data: Data) throws -> Data {
        if finished { return Data() }
        buf.append(data)
        if !headerDone {
            guard let r = buf.range(of: Data("\r\n\r\n".utf8)) else {
                if buf.count > maxHeader { throw HTTPError.headerTooLarge }
                return Data()
            }
            if r.lowerBound - buf.startIndex > maxHeader { throw HTTPError.headerTooLarge }
            let head = String(decoding: buf[buf.startIndex..<r.lowerBound], as: UTF8.self)
            buf = Data(buf[r.upperBound...])
            var lines = head.components(separatedBy: "\r\n")
            let first = lines.removeFirst().split(separator: " ", maxSplits: 2)
            guard first.count >= 2, first[0].hasPrefix("HTTP/1."), let code = Int(first[1]), (100...599).contains(code) else {
                throw HTTPError.badStatusLine
            }
            status = code
            for l in lines where !l.isEmpty {
                guard let i = l.firstIndex(of: ":") else { throw HTTPError.badHeader }
                headers[l[..<i].trimmingCharacters(in: .whitespaces).lowercased()] = l[l.index(after: i)...].trimmingCharacters(in: .whitespaces)
            }
            headerDone = true
            if let cl = contentLength {
                if cl > maxBody { throw HTTPError.bodyTooLarge }
                if cl == 0 && !chunked { finished = true; return Data() }
            }
        }
        var out = Data()
        if chunked {
            try feedChunked(&out)
        } else if let cl = contentLength {
            let take = min(buf.count, cl - received)
            out = Data(buf.prefix(take))
            buf.removeAll()
            received += take
            if received >= cl { finished = true }
        } else {
            out = buf
            buf.removeAll()
            received += out.count
        }
        if received > maxBody { throw HTTPError.bodyTooLarge }
        return out
    }

    private mutating func feedChunked(_ out: inout Data) throws {
        while !finished {
            if chunkTrailer {
                guard buf.count >= 2 else { return }
                guard buf.prefix(2) == Data("\r\n".utf8) else { throw HTTPError.badChunk }
                buf = Data(buf.dropFirst(2))
                chunkTrailer = false
            }
            if chunkLeft == nil {
                guard let r = buf.range(of: Data("\r\n".utf8)) else {
                    if buf.count > 1024 { throw HTTPError.badChunk }
                    return
                }
                let line = String(decoding: buf[buf.startIndex..<r.lowerBound], as: UTF8.self)
                guard let size = line.split(separator: ";").first,
                      let n = Int(size.trimmingCharacters(in: .whitespaces), radix: 16), n >= 0 else { throw HTTPError.badChunk }
                buf = Data(buf[r.upperBound...])
                if n == 0 { finished = true; buf.removeAll(); return }
                if received + n > maxBody { throw HTTPError.bodyTooLarge }
                chunkLeft = n
            }
            guard let left = chunkLeft else { return }
            if buf.isEmpty { return }
            let take = min(left, buf.count)
            out.append(buf.prefix(take))
            buf = Data(buf.dropFirst(take))
            received += take
            if take == left {
                chunkLeft = nil
                chunkTrailer = true
            } else {
                chunkLeft = left - take
                return
            }
        }
    }

    /// 连接关闭时调用：读到关闭的正文到这里结束；定长或分块没收全则是提前关闭。
    public mutating func finishAtEOF() throws {
        if finished { return }
        guard headerDone else { throw HTTPError.closedEarly }
        if chunked || contentLength != nil { throw HTTPError.closedEarly }
        finished = true
    }
}

/// 一条 Server-Sent Event。
public struct SSEEvent: Sendable, Equatable {
    public var id: String?
    public var event: String
    public var data: String

    public init(id: String? = nil, event: String, data: String) {
        self.id = id
        self.event = event
        self.data = data
    }
}

/// 逐行解析事件流（空行结束一条事件；以冒号开头的是注释；data 多行用换行连接）。一行超过 maxLine 时报错。
public struct SSEParser {
    private var line = Data()
    private var id: String?
    private var event = ""
    private var data: [String] = []
    private var hasData = false
    public let maxLine: Int

    public init(maxLine: Int = 1024 * 1024) { self.maxLine = maxLine }

    public mutating func feed(_ bytes: Data) throws -> [SSEEvent] {
        var out: [SSEEvent] = []
        for b in bytes {
            if b != UInt8(ascii: "\n") {
                line.append(b)
                if line.count > maxLine { throw HTTPError.lineTooLong }
                continue
            }
            var l = String(decoding: line, as: UTF8.self)
            if l.hasSuffix("\r") { l.removeLast() }
            line.removeAll(keepingCapacity: true)
            if l.isEmpty {
                if hasData || !event.isEmpty {
                    out.append(SSEEvent(id: id, event: event.isEmpty ? "message" : event, data: data.joined(separator: "\n")))
                }
                event = ""
                data = []
                hasData = false
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
            case "data": data.append(v); hasData = true
            default: break
            }
        }
        return out
    }
}

/// 断线重连的间隔：从 base 起每次翻倍，不超过 cap；连上并收到数据后 reset。
public struct ReconnectBackoff: Sendable, Equatable {
    public let base: Double
    public let cap: Double
    public private(set) var attempt = 0

    public init(base: Double = 0.5, cap: Double = 30) {
        self.base = base
        self.cap = cap
    }

    public mutating func next() -> Double {
        let d = min(cap, base * pow(2, Double(min(attempt, 30))))
        attempt += 1
        return d
    }

    public mutating func reset() { attempt = 0 }
}
