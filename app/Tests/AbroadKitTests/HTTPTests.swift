// 验收 b：网络层的纯函数部分。
import Foundation
import Testing
@testable import AbroadKit

/// 一次喂完和逐字节喂入，结果必须一样。
func parseAll(_ raw: String, byteByByte: Bool, eof: Bool = false, maxBody: Int = HTTPParser.defaultMaxBody) throws -> (HTTPParser, String) {
    var p = HTTPParser(maxBody: maxBody)
    var body = Data()
    let bytes = Data(raw.utf8)
    if byteByByte {
        for b in bytes { body.append(try p.feed(Data([b]))) }
    } else {
        body.append(try p.feed(bytes))
    }
    if eof { try p.finishAtEOF() }
    return (p, String(decoding: body, as: UTF8.self))
}

@Test(arguments: [false, true])
func contentLength(byteByByte: Bool) throws {
    let (p, body) = try parseAll("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 10\r\n\r\n{\"a\":true}", byteByByte: byteByByte)
    #expect(p.status == 200 && p.finished && body == "{\"a\":true}")
    #expect(p.headers["content-type"] == "application/json")
}

@Test(arguments: [false, true])
func contentLengthIgnoresExtraBytes(byteByByte: Bool) throws {
    let (p, body) = try parseAll("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nokJUNK", byteByByte: byteByByte)
    #expect(p.finished && body == "ok")
}

@Test(arguments: [false, true])
func chunked(byteByByte: Bool) throws {
    let raw = "HTTP/1.1 409 Conflict\r\nTransfer-Encoding: chunked\r\n\r\n4\r\nWiki\r\n6;ext=1\r\npedia \r\nE\r\nin \r\n\r\nchunks.\r\n0\r\n\r\n"
    let (p, body) = try parseAll(raw, byteByByte: byteByByte)
    #expect(p.status == 409 && p.finished && body == "Wikipedia in \r\n\r\nchunks.")
}

@Test(arguments: [false, true])
func readUntilClose(byteByByte: Bool) throws {
    let (p, body) = try parseAll("HTTP/1.1 200 OK\r\n\r\nhello world", byteByByte: byteByByte, eof: true)
    #expect(p.finished && body == "hello world")
}

@Test func zeroLength() throws {
    let (p, body) = try parseAll("HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n", byteByByte: false)
    #expect(p.finished && body.isEmpty && p.status == 204)
}

@Test func closedEarly() throws {
    var p = HTTPParser()
    _ = try p.feed(Data("HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\nabc".utf8))
    #expect(throws: HTTPError.closedEarly) { try p.finishAtEOF() }
    var q = HTTPParser()
    _ = try q.feed(Data("HTTP/1.1 200".utf8))
    #expect(throws: HTTPError.closedEarly) { try q.finishAtEOF() }
    var c = HTTPParser()
    _ = try c.feed(Data("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nab".utf8))
    #expect(throws: HTTPError.closedEarly) { try c.finishAtEOF() }
}

@Test func headerTooLarge() throws {
    var p = HTTPParser(maxHeader: 1024)
    let big = "HTTP/1.1 200 OK\r\nX-Pad: " + String(repeating: "a", count: 2000)
    #expect(throws: HTTPError.headerTooLarge) { _ = try p.feed(Data(big.utf8)) }
    // 逐字节喂也一样被拒（不会一直攒下去）。
    var q = HTTPParser(maxHeader: 1024)
    #expect(throws: HTTPError.headerTooLarge) {
        for b in Data(big.utf8) { _ = try q.feed(Data([b])) }
    }
}

@Test func bodyTooLarge() throws {
    #expect(throws: HTTPError.bodyTooLarge) {
        _ = try parseAll("HTTP/1.1 200 OK\r\nContent-Length: 2000\r\n\r\n", byteByByte: false, maxBody: 1000)
    }
    #expect(throws: HTTPError.bodyTooLarge) {
        _ = try parseAll("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n7D0\r\n", byteByByte: false, maxBody: 1000)
    }
    #expect(throws: HTTPError.bodyTooLarge) {
        _ = try parseAll("HTTP/1.1 200 OK\r\n\r\n" + String(repeating: "x", count: 1500), byteByByte: true, maxBody: 1000)
    }
}

@Test func malformed() throws {
    #expect(throws: HTTPError.badStatusLine) { _ = try parseAll("FTP/1.0 200 OK\r\n\r\n", byteByByte: false) }
    #expect(throws: HTTPError.badStatusLine) { _ = try parseAll("HTTP/1.1 abc OK\r\n\r\n", byteByByte: false) }
    #expect(throws: HTTPError.badHeader) { _ = try parseAll("HTTP/1.1 200 OK\r\nnocolon\r\n\r\n", byteByByte: false) }
    #expect(throws: HTTPError.badChunk) { _ = try parseAll("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\n", byteByByte: false) }
    #expect(throws: HTTPError.badChunk) { _ = try parseAll("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2\r\nabXY", byteByByte: false) }
}

@Test func requestBytes() {
    let r = HTTPRequest(method: "PUT", path: "/v1/region", body: Data("{}".utf8))
    #expect(String(decoding: r.bytes, as: UTF8.self) == "PUT /v1/region HTTP/1.1\r\nHost: abroadd\r\nConnection: close\r\nAccept: */*\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}")
    #expect(String(decoding: HTTPRequest(method: "GET", path: "/v1/status").bytes, as: UTF8.self).hasSuffix("Accept: */*\r\n\r\n"))
}

// ---- 事件流 ----

let sseRaw = ": connected\n\nid: 1\nevent: state\ndata: {\"kind\":\"enabled\"}\n\n: 心跳\n\nid: 2\nevent: gate\ndata: 第一行\ndata: 第二行\r\n\r\ndata: 没有事件名\n\n"

@Test(arguments: [0, 1, 3, 7])
func sse(chunk: Int) throws {
    var p = SSEParser()
    var evs: [SSEEvent] = []
    let bytes = Data(sseRaw.utf8)
    if chunk == 0 {
        evs = try p.feed(bytes)
    } else {
        var i = 0
        while i < bytes.count {
            evs += try p.feed(bytes.subdata(in: i..<min(i + chunk, bytes.count)))
            i += chunk
        }
    }
    #expect(evs == [
        SSEEvent(id: "1", event: "state", data: "{\"kind\":\"enabled\"}"),
        SSEEvent(id: "2", event: "gate", data: "第一行\n第二行"),
        SSEEvent(id: "2", event: "message", data: "没有事件名"),
    ])
}

@Test func sseLineTooLong() throws {
    var p = SSEParser(maxLine: 100)
    #expect(throws: HTTPError.lineTooLong) { _ = try p.feed(Data(("data: " + String(repeating: "x", count: 200)).utf8)) }
}

// ---- 重连间隔 ----

@Test func backoff() {
    var b = ReconnectBackoff(base: 0.5, cap: 30)
    let seq = (0..<10).map { _ in b.next() }
    #expect(seq == [0.5, 1, 2, 4, 8, 16, 30, 30, 30, 30])
    for _ in 0..<1000 { #expect(b.next() <= 30) }
    b.reset()
    #expect(b.next() == 0.5)
}

@Test func socketFromEnvironment() {
    #expect(UnixTransport.fromEnvironment(["ABROAD_SOCKET": "/tmp/x.sock"]).path == "/tmp/x.sock")
    #expect(UnixTransport.fromEnvironment([:]).path == "/var/run/claude-abroad/api.sock")
    #expect(UnixTransport.fromEnvironment(["ABROAD_SOCKET": ""]).path == UnixTransport.defaultSocket)
}
