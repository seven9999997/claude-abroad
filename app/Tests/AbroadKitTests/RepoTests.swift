// 验收 e、g、h：对照仓库里的文件（命令行的帮助、应用的源码、入库的生成类型）。只读文件，不写。
import Foundation
import Testing
@testable import AbroadKit

/// 仓库根目录（这个文件在 app/Tests/AbroadKitTests/ 下）。
let repoRoot = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()

func readRepo(_ rel: String) throws -> String {
    try String(contentsOf: repoRoot.appendingPathComponent(rel), encoding: .utf8)
}

/// abroad 的帮助文本（cmd/abroad/main.go 里的 usage 常量）。
func abroadUsage() throws -> String {
    let src = try readRepo("cmd/abroad/main.go")
    guard let a = src.range(of: "const usage = `"), let b = src.range(of: "`", range: a.upperBound..<src.endIndex) else {
        throw CocoaError(.fileReadCorruptFile)
    }
    return String(src[a.upperBound..<b.lowerBound])
}

/// 验收 e：终端退路给的命令和命令行的实际用法一致。
@Test func terminalCommandsMatchCLIHelp() throws {
    let usage = try abroadUsage()
    let table: [(SensitiveOperation, String, String)] = [
        // 操作、给用户的命令、帮助里必须出现的写法
        (.enable, "sudo abroad up", "up | down"),
        (.restore, "sudo abroad restore", "sudo abroad restore"),
        (.mainApply, "sudo abroad main apply", "sudo abroad main apply"),
        (.regionSet(timezone: "Asia/Tokyo", lang: "ja-JP"), "sudo abroad region set Asia/Tokyo ja-JP", "region show | set <时区> <语言>"),
        (.regionAuto, "sudo abroad region auto", "| auto"),
    ]
    for (op, cmd, inHelp) in table {
        #expect(op.terminalCommand == cmd)
        #expect(usage.contains(inHelp), "帮助里没有“\(inHelp)”")
        #expect(cmd.hasPrefix("sudo abroad "), "要本人授权的操作在终端里都用 sudo")
        #expect(!cmd.contains("--yes"), "终端里照样打印预览、由用户确认")
    }
    // 参数里有特殊字符时加引号（复制到终端不会被 shell 解释）。
    #expect(SensitiveOperation.regionSet(timezone: "A B", lang: "x'y;rm").terminalCommand == "sudo abroad region set 'A B' 'x'\\''y;rm'")
}

/// 应用的源码文件（不含测试）。
func appSources() throws -> [(String, String)] {
    let dir = repoRoot.appendingPathComponent("app/Sources")
    let e = FileManager.default.enumerator(at: dir, includingPropertiesForKeys: nil)
    var out: [(String, String)] = []
    while let u = e?.nextObject() as? URL {
        guard u.pathExtension == "swift", !u.path.contains("/AbroadAPI/") else { continue }
        out.append((u.lastPathComponent, try String(contentsOf: u, encoding: .utf8)))
    }
    return out
}

/// 验收 g：没有第三方包；没有需要宏插件的写法；没有读写状态目录、没有写磁盘缓存。允许的文件访问只有一处：
/// Transport.swift 里看 socket 文件在不在（分辨“后台服务没有运行”）。
@Test func sourceRules() throws {
    let files = try appSources()
    #expect(files.count >= 8)
    let allowedImports: Set<String> = ["Foundation", "Network", "SwiftUI", "Combine", "AppKit", "AbroadAPI", "AbroadKit"]
    let forbidden = [
        "@State ", "@State(", "@Observable", "#Preview", "@Entry", "@Bindable", // 宏
        "UserDefaults", "NSKeyedArchiver", "write(to", "createFile", "createDirectory", "removeItem", "moveItem", "copyItem",
        ".cachesDirectory", ".applicationSupportDirectory", ".documentDirectory", "FileHandle", "fopen", "Process(", "NSTask",
        "/var/db/claude-abroad", "/Library/Application Support", "state-dir", "secrets",
    ]
    for (name, src) in files {
        for line in src.split(separator: "\n") where line.hasPrefix("import ") {
            let m = line.dropFirst("import ".count).trimmingCharacters(in: .whitespaces)
            #expect(allowedImports.contains(m), "\(name) 引入了 \(m)")
        }
        let code = src.split(separator: "\n").filter { !$0.trimmingCharacters(in: .whitespaces).hasPrefix("//") }.joined(separator: "\n")
        for f in forbidden {
            #expect(!code.contains(f), "\(name) 里出现了 \(f)")
        }
        let fm = code.components(separatedBy: "FileManager.default.").count - 1
        let ok = code.components(separatedBy: "FileManager.default.fileExists(atPath:").count - 1
        #expect(fm == ok, "\(name)：FileManager 只允许 fileExists")
        if fm > 0 { #expect(name == "Transport.swift", "\(name) 不该访问文件") }
    }
    // Package.swift 没有外部依赖。
    let pkg = try readRepo("app/Package.swift")
    #expect(!pkg.contains(".package("), "不引入第三方包")
}

/// 验收 h：应用用的就是入库的那一份生成类型（符号链接到 api/swift/APITypes.swift）。
@Test func generatedTypesAreTheCheckedInOnes() throws {
    let link = repoRoot.appendingPathComponent("app/Sources/AbroadAPI/APITypes.swift")
    let dest = try FileManager.default.destinationOfSymbolicLink(atPath: link.path)
    #expect(dest == "../../../api/swift/APITypes.swift")
    #expect(link.resolvingSymlinksInPath().path == repoRoot.appendingPathComponent("api/swift/APITypes.swift").resolvingSymlinksInPath().path)
    let src = try readRepo("api/swift/APITypes.swift")
    #expect(src.contains("public struct DaemonMainExitView") && src.hasPrefix("// 由 cmd/abroad-apigen"))
}
