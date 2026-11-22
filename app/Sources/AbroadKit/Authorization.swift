// “取得本人授权”的协议（m5-plan v2.5 决定 2）。两个实现：系统授权（等 M5-3 的 E1、E2）和现在就能用的终端退路。
// 授权只是“让后台相信这是本人”的一种方式；能不能做仍由 abroadd 按级别判断。
import Foundation

/// 需要本人授权的操作。命令和命令行（cmd/abroad）的用法一致，测试对照 abroad 的帮助文本。
public enum SensitiveOperation: Sendable, Equatable {
    case enable
    case restore
    case mainApply
    case regionSet(timezone: String, lang: String)
    case regionAuto
    case addSourceFile(path: String, name: String) // 本地配置文件，或存了内容的临时文件
    case addSourceURLFile(path: String, name: String, viaHost: Bool) // 存了订阅地址的临时文件
    case addNodeFile(path: String, name: String) // 存了单个节点的临时文件
    case removeSource(id: String)
    case mainUse(node: String, via: [String]) // 节点 ID
    case lanConfirm(node: String)
    case exitAdd(tag: String, kind: String, path: String) // 配置文件用用户自己的路径
    case exitRemove(tag: String)
    case appSet(app: String, action: String)
    case appRemove(app: String)

    /// 在终端里执行的完整命令（不带 --yes：终端里照样打印预览、让用户确认）。
    public var terminalCommand: String {
        switch self {
        case .enable: return "sudo abroad up"
        case .restore: return "sudo abroad restore"
        case .mainApply: return "sudo abroad main apply"
        case let .regionSet(tz, lang): return "sudo abroad region set \(Self.quote(tz)) \(Self.quote(lang))"
        case .regionAuto: return "sudo abroad region auto"
        case let .addSourceFile(path, name): return "sudo abroad main add-source --file \(Self.quote(path)) --name \(Self.quote(name))"
        case let .addSourceURLFile(path, name, viaHost):
            return "sudo abroad main add-source --url-file \(Self.quote(path)) --name \(Self.quote(name))" + (viaHost ? " --via-host" : "")
        case let .addNodeFile(path, name): return "sudo abroad main add-node --file \(Self.quote(path)) --name \(Self.quote(name))"
        case let .removeSource(id): return "sudo abroad main rm-source \(Self.quote(id))"
        case let .mainUse(node, via): return (["sudo abroad main use", Self.quote(node)] + via.map { "--via " + Self.quote($0) }).joined(separator: " ")
        case let .lanConfirm(node): return "sudo abroad main confirm-lan \(Self.quote(node))"
        case let .exitAdd(tag, kind, path): return "sudo abroad exit add \(Self.quote(tag)) --kind \(Self.quote(kind)) --file \(Self.quote(path))"
        case let .exitRemove(tag): return "sudo abroad exit rm \(Self.quote(tag))"
        case let .appSet(app, action): return "sudo abroad app set \(Self.quote(app)) \(Self.quote(action == "proxy:main" ? "main" : action))"
        case let .appRemove(app): return "sudo abroad app rm \(Self.quote(app))"
        }
    }

    /// 命令里的参数只允许常见的时区、语言字符；其他一律加单引号（防止复制到终端时被 shell 解释）。
    static func quote(_ s: String) -> String {
        let ok = CharacterSet(charactersIn: "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789/_-+.:")
        if !s.isEmpty && s.unicodeScalars.allSatisfy({ ok.contains($0) }) { return s }
        return "'" + s.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }
}

public enum AuthorizationResult: Sendable, Equatable {
    case authorized // 系统授权通过（M5-3 之后），调用方重新发请求
    case runInTerminal(command: String) // 退路：请用户在终端执行这条命令
    case cancelled
}

public protocol Authorizer: Sendable {
    func authorize(_ op: SensitiveOperation, reason: ErrorText) async -> AuthorizationResult
}

/// 终端退路：告诉用户“这一步请在终端执行”，给出完整命令。
public struct TerminalAuthorizer: Authorizer {
    public init() {}
    public func authorize(_ op: SensitiveOperation, reason: ErrorText) async -> AuthorizationResult {
        .runInTerminal(command: op.terminalCommand)
    }
}

/// 终端退路给用户看的内容：一条或几条命令；用到临时文件时附上路径（命令执行后或取消时删除）。
public struct TerminalPrompt: Sendable, Equatable {
    public var commands: [String]
    public var tempFile: URL?

    public init(commands: [String], tempFile: URL? = nil) {
        self.commands = commands
        self.tempFile = tempFile
    }
}
