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
        (.addSourceFile(path: "/tmp/a.yaml", name: "本地"), "sudo abroad main add-source --file /tmp/a.yaml --name '本地'", "sudo abroad main add-source --file <配置文件> | --url-file <存订阅地址的文件> | --stdin  [--name <名字>] [--via-host]"),
        (.addSourceURLFile(path: "/tmp/u.txt", name: "sub", viaHost: true), "sudo abroad main add-source --url-file /tmp/u.txt --name sub --via-host", "--url-file <存订阅地址的文件>"),
        (.addNodeFile(path: "/tmp/n.yaml", name: "家里"), "sudo abroad main add-node --file /tmp/n.yaml --name '家里'", "sudo abroad main add-node --file <节点 YAML> [--name <名字>]"),
        (.removeSource(id: "s1"), "sudo abroad main rm-source s1", "sudo abroad main rm-source <来源 ID>"),
        (.mainUse(node: "n1", via: ["n2", "n3"]), "sudo abroad main use n1 --via n2 --via n3", "sudo abroad main use <节点> [--via <中转节点>…]"),
        (.lanConfirm(node: "n9"), "sudo abroad main confirm-lan n9", "sudo abroad main confirm-lan <节点>"),
        (.exitAdd(tag: "wg", kind: "wireguard", path: "/tmp/wg.conf"), "sudo abroad exit add wg --kind wireguard --file /tmp/wg.conf", "sudo abroad exit add <标签> --kind wireguard|openvpn|clash --file <配置文件>"),
        (.exitRemove(tag: "wg"), "sudo abroad exit rm wg", "sudo abroad exit rm <标签>"),
        (.appSet(app: "com.example.a", action: "proxy:main"), "sudo abroad app set com.example.a main", "set <应用> direct|block|main|proxy:<出口>"),
        (.appSet(app: "path:/usr/bin/x", action: "direct"), "sudo abroad app set path:/usr/bin/x direct", "set <应用> direct|block|main|proxy:<出口>"),
        (.appRemove(app: "com.example.a"), "sudo abroad app rm com.example.a", "| rm <应用>"),
        (.appSet(app: "com.example.a", action: "proxy:wg"), "sudo abroad app set com.example.a proxy:wg", "set <应用> direct|block|main|proxy:<出口>"),
    ]
    for (op, cmd, inHelp) in table {
        #expect(op.terminalCommand == cmd)
        #expect(usage.contains(inHelp), "帮助里没有“\(inHelp)”")
        #expect(cmd.hasPrefix("sudo abroad "), "要本人授权的操作在终端里都用 sudo")
        #expect((cmd.contains("--yes")) == false, "终端里照样打印预览、由用户确认")
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

/// 验收 g：没有第三方包；没有需要宏插件的写法；没有读写状态目录、没有写磁盘缓存；没有日志输出。允许的文件访问只有两处：
/// Transport.swift 里看 socket 文件在不在（分辨“后台服务没有运行”）；InputFiles.swift 里终端退路的临时文件和用户选的配置文件
/// （m5-plan v2.6 决定 3：全应用唯一允许写文件的地方）。
@Test func sourceRules() throws {
    let files = try appSources()
    #expect(files.count >= 8)
    let allowedImports: Set<String> = ["Foundation", "Network", "SwiftUI", "Combine", "AppKit", "UniformTypeIdentifiers", "UserNotifications", "AbroadAPI", "AbroadKit"]
    let forbidden = [
        "@State ", "@State(", "@Observable", "#Preview", "@Entry", "@Bindable", // 宏
        "UserDefaults", "NSKeyedArchiver", "write(to", "createFile", "createDirectory", "removeItem", ".moveItem(", ".copyItem(",
        ".cachesDirectory", ".applicationSupportDirectory", ".documentDirectory", "FileHandle", "fopen", "Process(", "NSTask",
        "/var/db/claude-abroad", "/Library/Application Support", "state-dir", "secrets",
        "print(", "NSLog", "os_log", "Logger(", "debugPrint",
    ]
    // 只在 InputFiles.swift 里允许的文件读写。
    let inputOnly = ["createFile", "createDirectory", "removeItem", "setAttributes", "Data(contentsOf"]
    for (name, src) in files {
        for line in src.split(separator: "\n") where line.hasPrefix("import ") {
            let m = line.dropFirst("import ".count).trimmingCharacters(in: .whitespaces)
            #expect(allowedImports.contains(m), "\(name) 引入了 \(m)")
        }
        let code = src.split(separator: "\n").filter { !$0.trimmingCharacters(in: .whitespaces).hasPrefix("//") }.joined(separator: "\n")
        for f in forbidden where !(name == "InputFiles.swift" && inputOnly.contains(f)) && !(name == "Notifications.swift" && f == "UserDefaults") {
            #expect((code.contains(f)) == false, "\(name) 里出现了 \(f)")
        }
        if name == "InputFiles.swift" { continue }
        for f in inputOnly { #expect((code.contains(f)) == false, "\(name) 里出现了 \(f)（只允许在 InputFiles.swift）") }
        let fm = code.components(separatedBy: "FileManager.default.").count - 1
        let ok = code.components(separatedBy: "FileManager.default.fileExists(atPath:").count - 1
        #expect(fm == ok, "\(name)：FileManager 只允许 fileExists")
        if fm > 0 { #expect(name == "Transport.swift", "\(name) 不该访问文件") }
    }
    // 不按错误文字分支（m5-plan v2.7 决定 0）：只看 ErrorText.code。
    for (name, src) in files {
        for bad in [".raw.contains(", ".reason.contains(", ".next.contains(", ".raw.hasPrefix(", ".raw ==", ".action.contains("] {
            #expect((src.contains(bad)) == false, "\(name) 按错误文字分支：\(bad)")
        }
    }
    // Package.swift 没有外部依赖。
    let pkg = try readRepo("app/Package.swift")
    #expect((pkg.contains(".package(")) == false, "不引入第三方包")
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

/// InputFiles.swift 的写文件限定在它自己的目录、权限 0700/0600（源码里写死，测试固定）。
@Test func inputFilesRestricted() throws {
    let src = try readRepo("app/Sources/AbroadKit/InputFiles.swift")
    #expect(src.contains("posixPermissions: 0o700") && src.contains("posixPermissions: 0o600"))
    #expect(src.contains("claude-abroad-input") && src.contains("UUID().uuidString"))
    #expect(src.contains("guard url.deletingLastPathComponent().standardizedFileURL == directory.standardizedFileURL"))
}

/// 验收 g：界面文字集中在 L10n；视图里没有硬编码的中文（注释除外）。允许的例外逐个列在这里（现在没有）。
@Test func noHardcodedChineseInViews() throws {
    let allowed: Set<String> = []
    let dir = repoRoot.appendingPathComponent("app/Sources/ClaudeAbroad")
    let files = try FileManager.default.contentsOfDirectory(atPath: dir.path).filter { $0.hasSuffix(".swift") }
    #expect(files.count >= 6)
    for f in files {
        let src = try String(contentsOf: dir.appendingPathComponent(f), encoding: .utf8)
        for (i, line) in src.split(separator: "\n", omittingEmptySubsequences: false).enumerated() {
            let code = line.components(separatedBy: "//").first ?? ""
            let han = code.unicodeScalars.contains { (0x4E00...0x9FFF).contains($0.value) || (0x3400...0x4DBF).contains($0.value) }
            if han && !allowed.contains("\(f):\(i + 1)") {
                Issue.record("\(f):\(i + 1) 有硬编码的中文：\(line.trimmingCharacters(in: .whitespaces))")
            }
        }
    }
}
