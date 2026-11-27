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
    /// 阶段 B（v0.71c）：安装、升级、卸载后台服务（不是命令行的子命令）；payload 是应用包里的安装文件目录。
    public var service: ServiceAction?
    public var payload: String?
    /// 卸载：会删除的东西（只读卸载预览给的）。
    public var removes: [String] = []
    public var browsersOption: Bool {
        if case .uninstall = service { return !(preview?.browsers ?? []).isEmpty }
        return steps.contains(.restore) && !(preview?.browsers ?? []).isEmpty
    }
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
        if let now = await currentLine(first) { r.lines.insert(now, at: 0) }
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

    /// 阶段 B（v0.71c）：弹出安装、升级、卸载后台服务的确认框（不执行）。卸载先读只读的卸载预览。
    public func presentService(_ a: ServiceAction, payload: String?, lines: [String], host: PrivilegedHost = .main,
                               terminal: TerminalPrompt, onTerminal: @escaping @MainActor (TerminalPrompt) -> Void,
                               onDone: @escaping @MainActor () async -> Void) async {
        guard request == nil else { return }
        var r = PrivilegedRequest(steps: [], title: L10n.serviceTitle(a), lines: lines, inputFiles: [], tempFiles: [])
        r.host = host
        r.service = a
        r.payload = payload
        if case .uninstall = a {
            if case .ok(let pv) = await client.uninstallPreview() {
                // 卸载预览里的进程部分和回国预览同一个结构（多出的 removes、note 解码时不要）
                r.preview = (try? JSONEncoder().encode(pv)).flatMap { try? JSONDecoder().decode(ClaudeprocPreview.self, from: $0) }
                r.removes = pv.removes ?? []
                r.note = pv.note
            } else {
                r.note = L10n.privRecheck
            }
        }
        killBrowsers = false
        self.onDone = onDone
        self.onTerminal = onTerminal
        terminalPrompt = terminal
        request = r
        phase = .confirming
    }

    /// B0 建议改⑤（v0.71c）：地区、主代理、规则、出口的确认框加一行“现在：…”（读现有的只读接口；读不到就不写这一行）。
    func currentLine(_ op: SensitiveOperation) async -> String? {
        switch op {
        case .regionSet, .regionAuto:
            guard case .ok(let r) = await client.region() else { return nil }
            return L10n.privNow(r.target.map { "\($0.timezone)、\($0.lang)" } ?? L10n.privNowRegionNone)
        case .mainApply, .mainUse, .lanConfirm, .addSourceFile, .addSourceURLFile, .addNodeFile, .removeSource:
            guard case .ok(let m) = await client.mainProxy() else { return nil }
            let sel = m.selected.map { $0.name } ?? L10n.privNowNone
            return L10n.privNow(L10n.privNowMain(sel, applied: m.applied, sources: Int(m.sources)))
        case let .appSet(app, _), let .appRemove(app):
            guard case .ok(let d) = await client.rules() else { return nil }
            return L10n.privNow(d.rules.first(where: { $0.app == app }).map { "\(app) → \($0.action)" } ?? L10n.privNowNoRule(app))
        case let .ruleListAdd(kind, item), let .ruleListRemove(kind, item):
            guard case .ok(let d) = await client.rules() else { return nil }
            let list: [String]
            switch kind {
            case "local": list = d.localDomains
            case "pinned": list = d.pinnedExtra
            case "offline": list = d.offlineAllow
            default: list = d.bypassExtra
            }
            return L10n.privNow(L10n.privNowInList(item, list.contains(item), count: list.count))
        case .udp443:
            guard case .ok(let d) = await client.rules() else { return nil }
            return L10n.privNow(d.blockUDP443 ? L10n.privNowOn : L10n.privNowOff)
        case .exitAdd, .exitRemove:
            guard case .ok(let xs) = await client.exits() else { return nil }
            return L10n.privNow(xs.isEmpty ? L10n.privNowNone : xs.map(\.tag).joined(separator: "、"))
        default:
            return nil
        }
    }

    /// 用户点了“执行”：这一刻算输入文件的摘要、拼命令、交给执行层。只能从确认框的按钮调用（源码检查守着）。
    public func userTappedExecute() async {
        guard let r = request, phase == .confirming else { return }
        guard let runner else { userTappedTerminal(); return }
        if let a = r.service {
            let act: ServiceAction
            if case .uninstall = a { act = .uninstall(killBrowsers: killBrowsers) } else { act = a }
            guard let cmd = try? PrivilegedCommand.buildService(act, paths: runner.paths, payload: r.payload) else {
                phase = .done(ok: false, message: L10n.privBadChars, output: "")
                return
            }
            phase = .running
            executions += 1
            finish(await runner.run(command: cmd, prompt: L10n.servicePrompt(act)), service: act)
            return
        }
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
        finish(await runner.run(command: cmd, prompt: L10n.privPrompt(r.title)), service: nil)
    }

    /// 结果按退出码显示文字表里的说明，命令行（或安装脚本）的输出原样附在下面；不按输出文字分支。
    private func finish(_ res: PrivilegedRunResult, service: ServiceAction?) {
        switch res {
        case .cancelled:
            phase = .done(ok: false, message: L10n.privCancelled, output: "")
        case .failed(let why):
            phase = .done(ok: false, message: L10n.privUnavailable, output: why)
        case .finished(let out):
            if let (code, body) = PrivilegedCommand.parse(out) {
                let msg = service.map { L10n.serviceExit(code, $0) } ?? L10n.privExit(code)
                phase = .done(ok: code == 0, message: msg, output: body)
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
