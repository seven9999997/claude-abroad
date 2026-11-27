// 应用里直接执行敏感操作（B0，设计 11.1、11.4 v0.71b；docs/app-privileged-plan.md v2）：拼交给系统密码框执行的那条 shell 命令。
// 这里只有纯函数：白名单、转义、拒绝、命令的形状、结果的解析。真正执行（NSAppleScript）只在 PrivilegedRunner.swift，
// 安装目录、命令行、临时目录的路径也由它（唯一的基础路径常量）拼好传进来。阶段 B 的安装、升级、卸载命令也放在这里（共用一套转义和测试）。
//
// 命令依次做：
// 1. 检查已安装的命令行、bin、安装目录：存在、不是符号链接、属主 root、组和其他人不可写；不满足退出码 31，什么都不执行；
// 2. 有输入文件时：复制到属主 root、0700 的临时目录，核对点“执行”那一刻算出的 SHA-256；对不上退出码 32，什么都不改；
// 3. 以 root 运行 abroad --yes --json <子命令>（多步时前一步失败就停）；删临时目录；最后一行 RESULT=<退出码>。
// 每条路都以 0 退出（结果在 RESULT 行里），这样标准输出总能拿回来；只有取消密码框、系统层面的失败才是 AppleScript 的错误。
import Foundation

/// 白名单：只有这些子命令能经系统密码框以 root 运行（命令行命令表里对应敏感或“按本次调用决定”的接口）。
/// 和 testdata/privileged-subcommands.txt 一致；cmd/abroad 的 TestPrivilegedWhitelistMatchesRoutes 双向核对路由级别。
/// 不提供（理由写在那份 Go 测试里）：uninstall（阶段 B 走已安装的 uninstall.sh）、down、main refresh、domains clear、check history clear（普通级别）。
public enum PrivilegedSubcommand: String, CaseIterable, Sendable {
    case up = "up"
    case restore = "restore"
    case mainAddSource = "main add-source"
    case mainAddNode = "main add-node"
    case mainRmSource = "main rm-source"
    case mainUse = "main use"
    case mainConfirmLAN = "main confirm-lan"
    case mainApply = "main apply"
    case exitAdd = "exit add"
    case exitRm = "exit rm"
    case appSet = "app set"
    case appRm = "app rm"
    case rulesLocalAdd = "rules local add"
    case rulesLocalRm = "rules local rm"
    case rulesPinnedAdd = "rules pinned add"
    case rulesPinnedRm = "rules pinned rm"
    case rulesOfflineAdd = "rules offline add"
    case rulesOfflineRm = "rules offline rm"
    case rulesNofakeAdd = "rules nofake add"
    case rulesNofakeRm = "rules nofake rm"
    case rulesUDP443 = "rules udp443"
    case lanOn = "lan on"
    case lanOff = "lan off"
    case regionSet = "region set"
    case regionAuto = "region auto"
    case claudeProxyOn = "claude-proxy on"
    case claudeProxyOff = "claude-proxy off"

    public var words: [String] { rawValue.split(separator: " ").map(String.init) }

    /// 允许的参数（v0.72，白名单 golden 的第三列）：“--x”是开关，“--x=a|b”是带值的、值只许列出的几种；
    /// 不带值的位置参数不在这里（由 arguments 生成）。
    public var allowedFlags: [String] {
        switch self {
        case .restore: return ["--kill-browsers", "--claude-proxy=on|off"]
        case .mainAddSource: return ["--file=*", "--url-file=*", "--name=*", "--via-host"]
        case .mainAddNode: return ["--file=*", "--name=*"]
        case .mainUse: return ["--via=*"]
        case .exitAdd: return ["--kind=*", "--file=*"]
        default: return []
        }
    }

    public static func matching(_ args: [String]) -> PrivilegedSubcommand? {
        allCases.filter { args.starts(with: $0.words) }.max { $0.words.count < $1.words.count }
    }
}

/// 交给系统密码框执行的命令里要用的路径（由 PrivilegedRunner 从唯一的基础路径常量拼出）。
public struct PrivilegedPaths: Sendable, Equatable {
    public var installDir: String
    public var abroad: String { installDir + "/bin/abroad" }
    public var binDir: String { installDir + "/bin" }
    public var staging: String

    public init(installDir: String, staging: String) {
        self.installDir = installDir
        self.staging = staging
    }
}

/// 一步：一个敏感操作。参数只来自 op.arguments（v0.72：确认框里的选项也编进 op，例如 .restoreWith，不另加）。
public struct PrivilegedStep: Sendable, Equatable {
    public var op: SensitiveOperation

    public init(_ op: SensitiveOperation) {
        self.op = op
    }
}

/// 阶段 B（v0.71c）：应用里安装、升级、卸载后台服务。
public enum ServiceAction: Sendable, Equatable {
    case install
    case upgrade(acceptUnknown: Bool)
    case uninstall(killBrowsers: Bool)
}

public enum PrivilegedCommandError: Error, Equatable {
    case notWhitelisted(String)
    case flagNotAllowed(String)
    case badCharacter
    case missingDigest
}

public enum PrivilegedCommand {
    /// 我们自己的退出码（命令行的是 0–7）。
    public static let exitBadInstall = 31
    public static let exitInputChanged = 32
    /// 交给命令行时输入文件在临时目录里的名字。
    public static let stagedInputName = "in"

    /// 单引号（总是加；参数里的单引号写成 '\''）。
    public static func shellQuote(_ s: String) -> String {
        "'" + s.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }

    /// AppleScript 字符串里的转义（PrivilegedRunner 把整条命令和提示文字放进 AppleScript 的字符串字面量时用）。
    public static func appleScriptEscape(_ s: String) -> String {
        s.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\"", with: "\\\"")
    }

    /// 子命令之后的参数：以 -- 开头的必须在 allowedFlags 里；带值的取下一个参数，值只许列出的几种（* 表示任意）。
    static func checkFlags(_ sub: PrivilegedSubcommand, _ rest: [String]) throws {
        var allowed: [String: [String]?] = [:] // 开关 → nil；带值 → 允许的值（["*"] 表示任意）
        for f in sub.allowedFlags {
            if let eq = f.firstIndex(of: "=") {
                allowed[String(f[..<eq])] = .some(f[f.index(after: eq)...].split(separator: "|").map(String.init))
            } else {
                allowed[f] = .some(nil)
            }
        }
        var i = 0
        while i < rest.count {
            let a = rest[i]
            if a.hasPrefix("--") {
                guard let spec = allowed[a] else { throw PrivilegedCommandError.flagNotAllowed(a) }
                if let vals = spec {
                    guard i + 1 < rest.count, vals == ["*"] || vals.contains(rest[i + 1]) else { throw PrivilegedCommandError.flagNotAllowed(a) }
                    i += 1
                }
            }
            i += 1
        }
    }

    /// 参数里有换行、NUL 或其他控制字符时拒绝。
    public static func acceptable(_ s: String) -> Bool {
        !s.unicodeScalars.contains { $0.value < 0x20 || $0.value == 0x7F }
    }

    /// 整条 shell 命令。steps 依次执行（前一步失败就停）；有输入文件的一步要给摘要（点“执行”那一刻算的）。
    public static func build(_ steps: [PrivilegedStep], paths: PrivilegedPaths, digests: [String: String]) throws -> String {
        var lines: [String] = ["umask 077", "A=" + shellQuote(paths.abroad) + "; S=" + shellQuote(paths.staging)]
        for p in [paths.installDir, paths.binDir, paths.abroad] + steps.flatMap({ $0.op.arguments }) {
            guard acceptable(p) else { throw PrivilegedCommandError.badCharacter }
        }
        lines.append(checkLine(paths))
        var staged = 0
        var runs: [String] = []
        for s in steps {
            guard let sub = PrivilegedSubcommand.matching(s.op.arguments) else {
                throw PrivilegedCommandError.notWhitelisted(s.op.arguments.joined(separator: " "))
            }
            try checkFlags(sub, Array(s.op.arguments.dropFirst(sub.words.count)))
            var args = s.op.arguments
            if let src = s.op.inputFile {
                guard let sum = digests[src], sum.count == 64, sum.allSatisfy({ $0.isHexDigit }) else { throw PrivilegedCommandError.missingDigest }
                staged += 1
                let dst = paths.staging + "/" + stagedInputName + "\(staged)"
                lines.append(stageLine(src: src, dst: dst, sha: sum.lowercased(), first: staged == 1))
                args = args.map { $0 == src ? dst : $0 }
            }
            runs.append((["\"$A\"", "--yes", "--json"] + args.map(shellQuote)).joined(separator: " "))
        }
        for r in runs {
            lines.append(r + "; rc=$?; [ $rc = 0 ] || { /bin/rm -rf \"$S\"; echo \"RESULT=$rc\"; exit 0; }")
        }
        lines.append("/bin/rm -rf \"$S\"; echo 'RESULT=0'; exit 0")
        return lines.joined(separator: "\n")
    }

    /// 第一段：已安装的命令行、bin、安装目录都要存在、不是符号链接、属主 root、组和其他人不可写。
    static func checkLine(_ p: PrivilegedPaths) -> String {
        "for p in \"$A\" " + shellQuote(p.binDir) + " " + shellQuote(p.installDir) + "; do " +
            "[ -e \"$p\" ] && [ ! -L \"$p\" ] && [ \"$(/usr/bin/stat -f %u \"$p\")\" = 0 ] && [ $(( 0$(/usr/bin/stat -f %Lp \"$p\") & 022 )) = 0 ] " +
            "|| { echo 'RESULT=\(exitBadInstall)'; exit 0; }; done; [ -f \"$A\" ] || { echo 'RESULT=\(exitBadInstall)'; exit 0; }"
    }

    /// 复制输入文件到 root 的临时目录并核对摘要；对不上就停、删掉临时目录。
    static func stageLine(src: String, dst: String, sha: String, first: Bool) -> String {
        let mk = first ? "/bin/rm -rf \"$S\" && /bin/mkdir -m 700 \"$S\" && /usr/sbin/chown root:wheel \"$S\" && " : ""
        return mk + "/bin/cp " + shellQuote(src) + " " + shellQuote(dst) +
            " && [ \"$(/usr/bin/shasum -a 256 " + shellQuote(dst) + " | /usr/bin/cut -c1-64)\" = " + shellQuote(sha) + " ] " +
            "|| { /bin/rm -rf \"$S\"; echo 'RESULT=\(exitInputChanged)'; exit 0; }"
    }

    // ---- 阶段 B（v0.71c）：安装、升级、卸载后台服务 ----

    /// 安装目录里没有卸载脚本（加这个功能之前装的、还没升级过）。
    public static let exitOldInstall = 30
    /// 应用包里的安装文件复制到 root 的临时目录后，和包里自带的 payload.sha256 对不上（内部不一致；不证明是不是原版）。
    public static let exitPayloadBad = 33

    /// 安装、升级：只用系统自带的程序把应用包里的 payload 整个复制（解引用符号链接）到属主 root、0700 的临时目录，
    /// 核对 payload.sha256，之后一切从这份副本执行 install.sh（--from 副本 --yes）。核对不过退出码 33，什么都不改。
    /// 卸载：执行已安装的、属主 root 的 uninstall.sh——先检查安装目录、scripts、三个脚本都是 root 的、组和其他人不可写、
    /// 不是符号链接（没有 scripts 时退出码 30），再复制到临时目录执行（脚本会删掉安装目录，不能在原处跑），带 --yes。
    public static func buildService(_ a: ServiceAction, paths: PrivilegedPaths, payload: String?) throws -> String {
        var lines = ["umask 077", "S=" + shellQuote(paths.staging) + "; I=" + shellQuote(paths.installDir)]
        guard acceptable(paths.staging), acceptable(paths.installDir) else { throw PrivilegedCommandError.badCharacter }
        switch a {
        case .install, .upgrade:
            guard let payload, acceptable(payload), payload.hasPrefix("/") else { throw PrivilegedCommandError.badCharacter }
            lines.append("P=" + shellQuote(payload))
            lines.append("/bin/rm -rf \"$S\" && /bin/mkdir -m 700 \"$S\" && /usr/sbin/chown root:wheel \"$S\" && /bin/cp -RL \"$P\" \"$S/p\" " +
                "&& ( cd \"$S/p\" && /usr/bin/shasum -a 256 -s -c payload.sha256 ) || { /bin/rm -rf \"$S\"; echo 'RESULT=\(exitPayloadBad)'; exit 0; }")
            var flags = ["--from", "\"$S/p\"", "--yes"]
            if case let .upgrade(acceptUnknown) = a {
                flags.append("--upgrade")
                if acceptUnknown { flags.append("--accept-unknown-version") }
            }
            lines.append("cd /; /bin/bash \"$S/p/scripts/m6/install/install.sh\" " + flags.joined(separator: " ") + " 2>&1; rc=$?; /bin/rm -rf \"$S\"; echo \"RESULT=$rc\"; exit 0")
        case let .uninstall(killBrowsers):
            lines.append("[ -d \"$I/scripts\" ] || { echo 'RESULT=\(exitOldInstall)'; exit 0; }")
            lines.append("for p in \"$I\" \"$I/scripts\" \"$I/scripts/m6/install/uninstall.sh\" \"$I/scripts/m6/install/lib.sh\" \"$I/scripts/lib/common.sh\"; do " +
                "[ -e \"$p\" ] && [ ! -L \"$p\" ] && [ \"$(/usr/bin/stat -f %u \"$p\")\" = 0 ] && [ $(( 0$(/usr/bin/stat -f %Lp \"$p\") & 022 )) = 0 ] " +
                "|| { echo 'RESULT=\(exitBadInstall)'; exit 0; }; done")
            lines.append("/bin/rm -rf \"$S\" && /bin/mkdir -m 700 \"$S\" && /usr/sbin/chown root:wheel \"$S\" && /bin/mkdir -m 700 \"$S/p\" && /bin/cp -R \"$I/scripts\" \"$S/p/scripts\" " +
                "|| { /bin/rm -rf \"$S\"; echo 'RESULT=\(exitBadInstall)'; exit 0; }")
            lines.append("cd /; /bin/bash \"$S/p/scripts/m6/install/uninstall.sh\" --yes" + (killBrowsers ? " --kill-browsers" : "") +
                " 2>&1; rc=$?; /bin/rm -rf \"$S\"; echo \"RESULT=$rc\"; exit 0")
        }
        return lines.joined(separator: "\n")
    }

    /// 解析输出：最后一行 RESULT=<退出码>，其余是命令行的输出（--json 时是最终的应答）。
    public static func parse(_ out: String) -> (exit: Int, body: String)? {
        var lines = out.split(separator: "\n", omittingEmptySubsequences: false).map(String.init)
        while let l = lines.last, l.isEmpty { lines.removeLast() }
        guard let last = lines.last, last.hasPrefix("RESULT="), let n = Int(last.dropFirst("RESULT=".count)) else { return nil }
        lines.removeLast()
        return (n, lines.joined(separator: "\n"))
    }
}
