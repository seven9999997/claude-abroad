// B0（v0.71b，docs/app-privileged-plan.md v2）：应用里直接执行敏感操作。
// - PrivilegedAuthorizer：后台要本人授权时不直接提权，只回“在应用里确认”；
// - PrivilegedSession：各页面共用一个确认框——写清要做什么、现状和后果、只读接口给的预览；用户点“执行”（且只有点击）
//   才算输入文件的摘要、拼命令、交给 PrivilegedRunning（真实的实现只在 ClaudeAbroad/PrivilegedRunner.swift）；
// - 结果按退出码显示文字表里的说明，原样附上命令行的输出；不写日志；“在终端执行”的退路保留。
import AbroadAPI
import Combine
import Foundation

/// 经系统密码框执行的结果。
public enum PrivilegedRunResult: Sendable, Equatable {
    case finished(stdout: String) // 命令跑完（结果在最后一行 RESULT=）
    case cancelled // 用户在密码框里点了取消
    case failed(String) // 系统层面的失败（没能执行）
}

/// 执行层（真实的在 PrivilegedRunner.swift，测试里是假的：只记下传了什么）。
public protocol PrivilegedRunning: Sendable {
    /// 安装目录、命令行、临时目录（由唯一的基础路径常量拼出）。
    var paths: PrivilegedPaths { get }
    func run(command: String, prompt: String) async -> PrivilegedRunResult
}

/// 后台要本人授权时：在应用里确认、经系统密码框执行。
public struct PrivilegedAuthorizer: Authorizer {
    public init() {}
    public func authorize(_ op: SensitiveOperation, reason: ErrorText) async -> AuthorizationResult { .confirmInApp }
}

/// 确认框显示在哪个窗口（主窗口或设置窗口）。
public enum PrivilegedHost: Sendable, Equatable { case main, settings }

/// 确认框里显示的内容。
public struct PrivilegedRequest: Sendable {
    public var host: PrivilegedHost = .main
    public var steps: [SensitiveOperation]
    public var title: String
    public var lines: [String]
    public var region: DaemonRegionChange?
    public var preview: ClaudeprocPreview?
    /// 启用：现在不能启用时的原因（后台的启用预览给的）。
    public var blocked: String?
    public var note: String?
    public var inputFiles: [String]
    public var tempFiles: [URL]
    public var browsersOption: Bool { steps.contains(.restore) && !(preview?.browsers ?? []).isEmpty }
}

public enum PrivilegedPhase: Sendable, Equatable {
    case none
    case confirming
    case running
    case done(ok: Bool, message: String, output: String)
}

@MainActor
public final class PrivilegedSession: ObservableObject {
    @Published public private(set) var request: PrivilegedRequest?
    @Published public private(set) var phase: PrivilegedPhase = .none
    @Published public var killBrowsers = false

    public let client: APIClient
    public let runner: PrivilegedRunning?
    public let files: InputFiles
    private var onDone: (@MainActor () async -> Void)?
    private var onTerminal: (@MainActor (TerminalPrompt) -> Void)?
    private var terminalPrompt: TerminalPrompt?
    /// 测试用：执行次数（假 runner 也记）。
    public private(set) var executions = 0

    public init(client: APIClient, runner: PrivilegedRunning?, files: InputFiles = InputFiles()) {
        self.client = client
        self.runner = runner
        self.files = files
    }

    public var isActive: Bool { request != nil }

    /// 弹出确认框（不执行）。steps 依次执行；tempFiles 是应用写的临时文件（结束后删）；terminal 是终端退路的内容。
    public func present(_ steps: [SensitiveOperation], tempFiles: [URL] = [], host: PrivilegedHost = .main, terminal: TerminalPrompt,
                        onTerminal: @escaping @MainActor (TerminalPrompt) -> Void, onDone: @escaping @MainActor () async -> Void) async {
        guard request == nil, let first = steps.first else { return }
        var r = PrivilegedRequest(steps: steps, title: steps.map(L10n.privTitle).joined(separator: "，然后"), lines: steps.flatMap(L10n.privConsequences),
                                  inputFiles: steps.compactMap(\.inputFile), tempFiles: tempFiles)
        r.host = host
        switch first {
        case .restore:
            if case .ok(let pv) = await client.restorePreview() { r.preview = pv }
            r.note = L10n.privRecheck
        case .enable:
            if case .ok(let ep) = await client.enablePreview() {
                r.region = ep.region
                r.preview = ep.preview
                if !ep.canEnable { r.blocked = L10n.privEnableBlocked + (ep.reason ?? ep.blocker ?? "") }
                if ep.enabled == true { r.lines.append(L10n.privEnableAlready) } else if ep.region == nil { r.lines.append(L10n.privEnableNoChange) }
                r.note = ep.note
            } else {
                r.note = L10n.privRecheck
            }
        default: break
        }
        killBrowsers = false
        self.onDone = onDone
        self.onTerminal = onTerminal
        terminalPrompt = terminal
        request = r
        phase = .confirming
    }

    /// 用户点了“执行”：这一刻算输入文件的摘要、拼命令、交给执行层。只能从确认框的按钮调用（源码检查守着）。
    public func userTappedExecute() async {
        guard let r = request, phase == .confirming else { return }
        guard let runner else { userTappedTerminal(); return }
        var digests: [String: String] = [:]
        for f in r.inputFiles {
            guard let sum = try? InputFiles.sha256(of: URL(fileURLWithPath: f)) else {
                phase = .done(ok: false, message: L10n.privExit(PrivilegedCommand.exitInputChanged), output: "")
                return
            }
            digests[f] = sum
        }
        let steps = r.steps.map { op in PrivilegedStep(op, options: op == .restore && killBrowsers ? ["--kill-browsers"] : []) }
        let cmd: String
        do {
            cmd = try PrivilegedCommand.build(steps, paths: runner.paths, digests: digests)
        } catch {
            phase = .done(ok: false, message: L10n.privBadChars, output: "")
            return
        }
        phase = .running
        executions += 1
        let res = await runner.run(command: cmd, prompt: L10n.privPrompt(r.title))
        switch res {
        case .cancelled:
            phase = .done(ok: false, message: L10n.privCancelled, output: "")
        case .failed(let why):
            phase = .done(ok: false, message: L10n.privUnavailable, output: why)
        case .finished(let out):
            if let (code, body) = PrivilegedCommand.parse(out) {
                phase = .done(ok: code == 0, message: L10n.privExit(code), output: body)
            } else {
                phase = .done(ok: false, message: L10n.privUnavailable, output: out)
            }
        }
    }

    /// 改在终端执行：交给原来的终端退路（临时文件由终端退路的关闭来删）。
    public func userTappedTerminal() {
        guard request != nil, phase != .running, let t = terminalPrompt else { return }
        let cb = onTerminal
        request = nil
        phase = .none
        onDone = nil
        cb?(t)
    }

    /// 取消（执行前）或关掉结果：删临时文件、刷新页面。
    public func userClosed() async {
        guard let r = request, phase != .running else { return }
        for f in r.tempFiles { files.remove(f) }
        let cb = onDone
        request = nil
        phase = .none
        onDone = nil
        await cb?()
    }
}
