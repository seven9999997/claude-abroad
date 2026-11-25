// M6-3 复审必须改 2：后台在不同时机断开连接时，应用的传输层一律给出 rejected（和命令行一致）。
// 用真的 unix socket（POSIX）起一个只会断开的假后台：接受后立刻断、读到一半断、读完请求不应答就断。
import Foundation
import Testing
@testable import AbroadKit

/// 只会断开连接的假后台。mode：0 接受后立刻断；1 读几个字节就断；2 读完请求头再断。
final class DisconnectServer: @unchecked Sendable {
    let path: String
    private let fd: Int32
    private let mode: Int

    init(mode: Int) throws {
        self.mode = mode
        path = NSTemporaryDirectory() + "dc-\(UUID().uuidString.prefix(8)).sock"
        fd = socket(AF_UNIX, SOCK_STREAM, 0)
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        let bytes = Array(path.utf8)
        withUnsafeMutableBytes(of: &addr.sun_path) { buf in
            for (i, b) in bytes.enumerated() where i < buf.count - 1 { buf[i] = b }
        }
        let ok = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
        }
        guard ok == 0, listen(fd, 64) == 0 else { throw ConnectionError.protocolError("bind/listen 失败") }
        let fd = self.fd, mode = self.mode
        Thread.detachNewThread {
            while true {
                let c = accept(fd, nil, nil)
                if c < 0 { return }
                var buf = [UInt8](repeating: 0, count: 64 * 1024)
                switch mode {
                case 1: _ = read(c, &buf, 8)
                case 2:
                    var got = Data()
                    while !got.contains(Data("\r\n\r\n".utf8)) {
                        let n = read(c, &buf, buf.count)
                        if n <= 0 { break }
                        got.append(contentsOf: buf[0..<n])
                    }
                default: break
                }
                close(c)
            }
        }
    }

    func stop() {
        shutdown(fd, SHUT_RDWR)
        close(fd)
        unlink(path)
    }

    /// 关掉监听但留下套接字文件（模拟“后台刚退出”）。
    func stopKeepingFile() {
        shutdown(fd, SHUT_RDWR)
        close(fd)
    }
}

@Test(arguments: [0, 1, 2])
func disconnectAlwaysRejected(mode: Int) async throws {
    let s = try DisconnectServer(mode: mode)
    defer { s.stop() }
    let t = UnixTransport(path: s.path, timeout: 5)
    let big = Data(repeating: 0x61, count: 256 * 1024) // 大请求体：让“还没写完就被断开”的时机也出现
    for i in 0..<40 {
        let req = HTTPRequest(method: "PUT", path: "/v1/rules", body: i % 2 == 1 ? big : nil)
        do {
            _ = try await t.send(req)
            Issue.record("模式 \(mode) 第 \(i) 次：应失败")
        } catch let e as ConnectionError {
            #expect(e == .rejected, "模式 \(mode) 第 \(i) 次：\(e)")
        } catch {
            Issue.record("模式 \(mode) 第 \(i) 次：\(error)")
        }
    }
}

/// 只有套接字文件、没有进程在听：仍是 noListener（不能被归成 rejected）。
@Test func staleSocketIsNoListener() async throws {
    let s = try DisconnectServer(mode: 0)
    // 关掉监听但保留文件
    let path = s.path
    s.stopKeepingFile()
    let t = UnixTransport(path: path, timeout: 5)
    for _ in 0..<10 {
        do {
            _ = try await t.send(HTTPRequest(method: "GET", path: "/v1/status"))
            Issue.record("应失败")
        } catch let e as ConnectionError {
            #expect(e == .noListener, "\(e)")
        }
    }
    unlink(path)
}
