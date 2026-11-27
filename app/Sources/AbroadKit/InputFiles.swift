// 终端退路里用户的输入文件（m5-plan v2.6 决定 3）。这是全应用里唯一读写文件的地方（源码检查单独放开这一个文件）：
// - 读：用户自己选的本地配置文件（经接口提交时要内容）；
// - 写：订阅地址、手动节点要在终端里经 --url-file、--file 交给命令行时，存到只有本人能读的临时文件
//   （目录 0700、文件 0600，文件名是随机的，不含任何输入内容）；命令执行后或用户取消时删除；
// - B0（v0.71b）：点“执行”那一刻算输入文件的 SHA-256（CryptoKit 只在这个文件里 import 和使用，源码检查守着）。
//   用户自己选的本地文件（可能在受隐私保护的目录里，root 的 shell 未必读得到）也先存成这里的临时文件，命令里只用这份。
import CryptoKit
import Foundation

public struct InputFiles: Sendable {
    /// 临时文件所在的目录（测试注入临时目录）。
    public let directory: URL

    public init(directory: URL = FileManager.default.temporaryDirectory.appendingPathComponent("claude-abroad-input", isDirectory: true)) {
        self.directory = directory
    }

    /// 写一个只有本人能读的临时文件，返回路径。
    public func writePrivate(_ content: String, ext: String) throws -> URL {
        let fm = FileManager.default
        try fm.createDirectory(at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        try fm.setAttributes([.posixPermissions: 0o700], ofItemAtPath: directory.path)
        let url = directory.appendingPathComponent(UUID().uuidString.lowercased() + "." + ext)
        guard fm.createFile(atPath: url.path, contents: Data(content.utf8), attributes: [.posixPermissions: 0o600]) else {
            throw CocoaError(.fileWriteUnknown)
        }
        return url
    }

    /// 删除这里写的临时文件（只删这个目录里的）。
    public func remove(_ url: URL) {
        guard url.deletingLastPathComponent().standardizedFileURL == directory.standardizedFileURL else { return }
        try? FileManager.default.removeItem(at: url)
    }

    /// 读用户选的本地配置文件（上限 2 MiB，和后台的请求体上限一致）。
    public static func readUserFile(_ url: URL) throws -> String {
        let d = try Data(contentsOf: url)
        guard d.count <= 2 * 1024 * 1024 else { throw CocoaError(.fileReadTooLarge) }
        return String(decoding: d, as: UTF8.self)
    }

    /// 文件内容的 SHA-256（小写十六进制，64 位）。
    public static func sha256(of url: URL) throws -> String {
        let d = try Data(contentsOf: url)
        return SHA256.hash(data: d).map { String(format: "%02x", $0) }.joined()
    }
}
