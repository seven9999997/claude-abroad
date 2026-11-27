// “取得本人授权”的协议（m5-plan v2.5 决定 2）。两个实现：应用里经系统密码框执行（B0，v0.71b：PrivilegedAuthorizer，
// 以 root 运行已安装的命令行）和终端退路（TerminalAuthorizer，拷贝命令到终端执行）。
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
    /// 规则里的名单（补齐一）：kind 是 local、pinned、offline。
    case ruleListAdd(kind: String, item: String)
    case ruleListRemove(kind: String, item: String)
    case udp443(on: Bool)
    case allowLAN(on: Bool)
    /// 第四道（Claude Code 的本机代理）的开关（v0.68；B0 起应用里可以直接执行）。
    case claudeProxy(on: Bool)
    /// 回国，带确认框里选的选项（v0.72）：同时强制退出浏览器；回国之后第四道开或关（nil = 保持现状，只在和现状不同时带）。
    /// .restore 是“刚点一键回国、还没有选项”的场合；执行和终端命令都用这个，参数只从 arguments 来。
    case restoreWith(killBrowsers: Bool, claudeProxy: Bool?)

    /// 是不是回国（.restore 或带选项的 .restoreWith）。
    public var isRestore: Bool {
        switch self {
        case .restore, .restoreWith: return true
        default: return false
        }
    }

    /// 命令行 abroad 之后的参数（不含 sudo）。终端命令和经系统密码框执行的命令都由它生成：参数只有这一份。
    public var arguments: [String] {
        switch self {
        case .enable: return ["up"]
        case .restore: return ["restore"]
        case .mainApply: return ["main", "apply"]
        case let .regionSet(tz, lang): return ["region", "set", tz, lang]
        case .regionAuto: return ["region", "auto"]
        case let .addSourceFile(path, name): return ["main", "add-source", "--file", path, "--name", name]
        case let .addSourceURLFile(path, name, viaHost): return ["main", "add-source", "--url-file", path, "--name", name] + (viaHost ? ["--via-host"] : [])
        case let .addNodeFile(path, name): return ["main", "add-node", "--file", path, "--name", name]
        case let .removeSource(id): return ["main", "rm-source", id]
        case let .mainUse(node, via): return ["main", "use", node] + via.flatMap { ["--via", $0] }
        case let .lanConfirm(node): return ["main", "confirm-lan", node]
        case let .exitAdd(tag, kind, path): return ["exit", "add", tag, "--kind", kind, "--file", path]
        case let .exitRemove(tag): return ["exit", "rm", tag]
        case let .appSet(app, action): return ["app", "set", app, action == "proxy:main" ? "main" : action]
        case let .appRemove(app): return ["app", "rm", app]
        case let .ruleListAdd(kind, item): return ["rules", kind, "add", item]
        case let .ruleListRemove(kind, item): return ["rules", kind, "rm", item]
        case let .udp443(on): return ["rules", "udp443", on ? "on" : "off"]
        case let .allowLAN(on): return ["lan", on ? "on" : "off"]
        case let .claudeProxy(on): return ["claude-proxy", on ? "on" : "off"]
        case let .restoreWith(kill, cp):
            return ["restore"] + (kill ? ["--kill-browsers"] : []) + (cp.map { ["--claude-proxy", $0 ? "on" : "off"] } ?? [])
        }
    }

    /// 经文件传给命令行的那个参数（订阅地址、节点、配置、出口配置；文件里是秘密，命令里只有路径）。
    public var inputFile: String? {
        switch self {
        case let .addSourceFile(path, _), let .addSourceURLFile(path, _, _), let .addNodeFile(path, _), let .exitAdd(_, _, path): return path
        default: return nil
        }
    }

    /// 在终端里执行的完整命令（不带 --yes：终端里照样打印预览、让用户确认）。由 arguments 生成。
    public var terminalCommand: String {
        (["sudo", "abroad"] + arguments.map(Self.quote)).joined(separator: " ")
    }

    /// 命令里的参数只允许常见的时区、语言字符；其他一律加单引号（防止复制到终端时被 shell 解释）。
    static func quote(_ s: String) -> String {
        let ok = CharacterSet(charactersIn: "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789/_-+.:")
        if !s.isEmpty && s.unicodeScalars.allSatisfy({ ok.contains($0) }) { return s }
        return "'" + s.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }
}

public enum AuthorizationResult: Sendable, Equatable {
    case authorized // 保留：系统授权通过后调用方重新发请求（这一版不用）
    case runInTerminal(command: String) // 退路：请用户在终端执行这条命令
    case confirmInApp // B0：在应用自己的确认框里确认，点“执行”后经系统密码框以 root 运行已安装的命令行（PrivilegedSession）
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
