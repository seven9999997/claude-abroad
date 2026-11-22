import APITypes
import Foundation
import Testing
@testable import UDSHTTP

@Test func contentLengthResponse() throws {
    var p = HTTPParser()
    var body = try p.feed(Data("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 10\r\n\r\n{\"a\":".utf8))
    #expect(p.headerDone && p.status == 200 && !p.finished)
    body.append(try p.feed(Data("true}".utf8)))
    #expect(p.finished)
    #expect(String(decoding: body, as: UTF8.self) == "{\"a\":true}")
}

@Test func chunkedAcrossBoundaries() throws {
    let raw = "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n6\r\n world\r\n0\r\n\r\n"
    // 一个字节一个字节地喂：块长度、数据、结尾都可能被切开
    var p = HTTPParser()
    var body = Data()
    for b in Data(raw.utf8) { body.append(try p.feed(Data([b]))) }
    #expect(p.finished)
    #expect(String(decoding: body, as: UTF8.self) == "hello world")
}

@Test func badStatusLine() {
    var p = HTTPParser()
    #expect(throws: UDSHTTPError.badStatusLine) { _ = try p.feed(Data("SSH-2.0-x\r\n\r\n".utf8)) }
}

@Test func sseParsing() {
    var s = SSEParser()
    var evs = s.feed(Data(": 注释\nid: 1\nevent: state\ndata: {\"seq\":1,\n".utf8))
    #expect(evs.isEmpty)
    evs = s.feed(Data("data: \"kind\":\"state\"}\n\ndata: plain\r\n\r\n".utf8))
    #expect(evs.count == 2)
    #expect(evs[0] == SSEEvent(id: "1", event: "state", data: "{\"seq\":1,\n\"kind\":\"state\"}"))
    #expect(evs[1].event == "message" && evs[1].data == "plain")
}

@Test func requestBytes() {
    let d = UnixHTTPClient.requestBytes(method: "POST", path: "/v1/enable", body: Data("{}".utf8))
    let s = String(decoding: d, as: UTF8.self)
    #expect(s.hasPrefix("POST /v1/enable HTTP/1.1\r\n") && s.contains("Content-Length: 2\r\n") && s.hasSuffix("\r\n\r\n{}"))
}

/// 生成的类型能解码 abroadd 真实的 status（假系统模式下采的一份样本，见 testdata）。
@Test func decodeStatusSample() throws {
    let url = URL(fileURLWithPath: #filePath).deletingLastPathComponent().appendingPathComponent("status-sample.json")
    let d = try Data(contentsOf: url)
    let st = try JSONDecoder().decode(DaemonStatus.self, from: d)
    #expect(!st.state.isEmpty)
}

@Test func trailingBytesAfterFinishIgnored() throws {
    var p = HTTPParser()
    _ = try p.feed(Data("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n".utf8))
    #expect(p.finished)
    #expect(try p.feed(Data("\r\n".utf8)).isEmpty)
}

@Test func badChunkSize() {
    var p = HTTPParser()
    #expect(throws: UDSHTTPError.badChunk) { _ = try p.feed(Data("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\n".utf8)) }
}
