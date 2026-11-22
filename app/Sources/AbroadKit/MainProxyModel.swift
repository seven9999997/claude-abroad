// 主代理页面的视图模型（m5-plan v2.6 决定 2–6）：来源列表、三种添加方式、删除、刷新、节点与链式的选择、
// 应用主代理（两步确认、本人授权、失败与回退）、检查、局域网节点的确认。
// 订阅地址和密码只在提交的那一刻进请求体，提交时立即从这里清掉；不写本地存储、不写日志、错误里不回显。
import AbroadAPI
import Combine
import Foundation

public enum AddKind: String, Sendable, CaseIterable {
    case subscription, localFile, manualNode
    public var title: String {
        switch self {
        case .subscription: return L10n.addSubscription
        case .localFile: return L10n.addLocalFile
        case .manualNode: return L10n.addManualNode
        }
    }
}

/// 来源列表的一行：只有后台返回的名字、类型、节点数、更新时间（没有订阅地址和服务器）。
public struct SourceRow: Sendable, Equatable, Identifiable {
    public var id: String
    public var name: String
    public var kind: String
    public var nodeCount: Int
    public var updated: String
    public var error: String?
}

public struct NodeRow: Sendable, Equatable, Identifiable {
    public var id: String
    public var name: String
    public var type: String
    public var source: String
}

/// 失败时回退的情况（取自后台“下一步”那一段的原话）。
public enum Rollback: Sendable, Equatable {
    case none // 失败发生在切换之前（例如校验），不涉及回退
    case done // “已经回到原来的主代理”
    case incomplete // “恢复原来的主代理没有完成，稍后会自动重试”

    /// 只看代码（m5-plan v2.7 决定 0）。
    static func of(_ t: ErrorText) -> Rollback {
        switch t.code {
        case .switchRolledBack: return .done
        case .switchRollbackIncomplete: return .incomplete
        default: return .none
        }
    }
}

public enum ApplyState: Sendable, Equatable {
    case idle
    case checking
    case saved(String) // 未启用：只保存了选择（并已建立出口基准时转为 succeeded）
    case succeeded(country: String, timezone: String)
    case failed(ErrorText, Rollback)
}

public enum CheckState: Sendable, Equatable {
    case idle, checking
    case ok(rule: String, country: String, timezone: String)
    case failed(ErrorText)
}

/// 局域网节点的确认（原型没有覆盖，按设计的文字做；第一跳的地址由后台决定显不显示，这里不取）。
public struct LANPrompt: Sendable, Equatable {
    public var node: String // 节点 ID
    public var name: String // 显示名
}

@MainActor
public final class MainProxyModel: ObservableObject {
    // 列表
    @Published public private(set) var sources: [SourceRow] = []
    @Published public private(set) var nodes: [NodeRow] = []
    @Published public private(set) var view: DaemonMainProxyView?
    @Published public private(set) var needsReselect = false

    // 添加来源的表单（订阅地址和密码提交即清）
    @Published public var addKind: AddKind = .subscription
    @Published public var formName = ""
    @Published public var formURL = ""
    @Published public var formFile: URL?
    @Published public var nodeType = "socks5"
    @Published public var nodeServer = ""
    @Published public var nodePort = ""
    @Published public var nodeUser = ""
    @Published public var nodePassword = ""
    @Published public var viaHost = false
    /// 后台要求显式选择“经本机网络”（未启用时抓取订阅）：界面给出这个选择并写明含义。
    @Published public private(set) var viaHostNeeded = false
    /// 刷新订阅时后台要求显式选择“经本机网络”的那个来源。
    @Published public private(set) var refreshNeedsViaHost: String?

    // 选择与应用
    @Published public var selectedExit: String?
    @Published public var via: [String] = []
    @Published public private(set) var applyState: ApplyState = .idle
    @Published public private(set) var checkState: CheckState = .idle
    @Published public var lanPrompt: LANPrompt?
    @Published public private(set) var terminal: TerminalPrompt?
    @Published public private(set) var notice: Notice?
    @Published public private(set) var busy = false

    public let client: APIClient
    public let authorizer: Authorizer
    public let files: InputFiles
    public let flow: ConfirmFlow
    public let notifier: Notifier

    public init(client: APIClient, authorizer: Authorizer = TerminalAuthorizer(), files: InputFiles = InputFiles(),
                flow: ConfirmFlow? = nil, notifier: Notifier? = nil) {
        self.client = client
        self.authorizer = authorizer
        self.files = files
        self.flow = flow ?? ConfirmFlow(client: client)
        self.notifier = notifier ?? Notifier()
    }

    // ---- 读 ----

    public func refresh() async {
        if case .ok(let vs) = await client.sources() {
            sources = vs.map { SourceRow(id: $0.id, name: $0.name, kind: $0.kind, nodeCount: $0.nodes?.count ?? 0, updated: $0.updated, error: $0.error) }
            nodes = vs.flatMap { s in (s.nodes ?? []).map { NodeRow(id: $0.id, name: $0.name, type: $0.type, source: s.name) } }
        }
        if case .ok(let v) = await client.mainProxy() {
            view = v
            needsReselect = needsReselect || v.needsReselect // 更新订阅时得到的提示保留到重新应用成功
            if selectedExit == nil { selectedExit = v.selected?.id }
        }
    }

    // ---- 添加来源 ----

    /// 把表单里的敏感内容清掉（提交的那一刻调用）。
    func clearSecrets() {
        formURL = ""
        nodePassword = ""
        nodeUser = ""
        nodeServer = ""
        nodePort = ""
        formFile = nil
    }

    /// 提交表单。请求体只在这里的局部变量里，构造完立即清空表单。
    public func submitAdd() async {
        guard !busy else { return }
        let name = formName.trimmingCharacters(in: .whitespaces)
        let kind = addKind
        var body: AddSourceBody
        var fallbackContent: (text: String, ext: String)? // 终端退路要存成临时文件的内容
        var fallbackPath: String? // 或用户自己选的文件
        switch kind {
        case .subscription:
            let url = formURL.trimmingCharacters(in: .whitespacesAndNewlines)
            guard url.hasPrefix("http://") || url.hasPrefix("https://") else { notice = Notice(kind: .error, text: ErrorText(raw: L10n.addBadURL)); return }
            body = AddSourceBody(kind: "subscription", name: name.isEmpty ? L10n.defaultSubscriptionName : name, url: url, viaHost: viaHost ? true : nil)
            fallbackContent = (url + "\n", "txt")
        case .localFile:
            guard let f = formFile else { notice = Notice(kind: .error, text: ErrorText(raw: L10n.addNoFile)); return }
            let content: String
            do { content = try InputFiles.readUserFile(f) } catch {
                notice = Notice(kind: .error, text: ErrorText(raw: L10n.addReadFailed)); return
            }
            body = AddSourceBody(kind: "yaml", name: name.isEmpty ? f.deletingPathExtension().lastPathComponent : name, content: content)
            fallbackPath = f.path
        case .manualNode:
            guard !nodeServer.isEmpty, let port = Int(nodePort), (1...65535).contains(port) else {
                notice = Notice(kind: .error, text: ErrorText(raw: L10n.addBadNode)); return
            }
            let nodeName = name.isEmpty ? L10n.defaultNodeName : name
            var node: [String: JSONValue] = ["name": .string(nodeName), "type": .string(nodeType), "server": .string(nodeServer), "port": .number(Double(port))]
            if !nodeUser.isEmpty { node["username"] = .string(nodeUser) }
            if !nodePassword.isEmpty { node["password"] = .string(nodePassword) }
            body = AddSourceBody(kind: "node", name: nodeName, node: node)
            fallbackContent = (Self.yaml(node), "yaml")
        }
        let sourceName = body.name
        let viaHostChosen = viaHost
        clearSecrets() // 提交的那一刻清掉
        busy = true
        defer { busy = false }
        let out = await client.addSource(body)
        body = AddSourceBody(kind: "", name: "") // 局部的请求体也不再留着
        switch out {
        case .ok(let r):
            notice = nil
            viaHostNeeded = false
            formName = ""
            needsReselect = r.needsReselect ?? false
            await refresh()
        case .needsAuthorization(let t):
            // 终端退路：内容存成只有本人能读的临时文件（本地配置文件直接用用户自己的路径），再给出命令。
            do {
                var temp: URL?
                let op: SensitiveOperation
                if let p = fallbackPath {
                    op = .addSourceFile(path: p, name: sourceName)
                } else if let c = fallbackContent {
                    let u = try files.writePrivate(c.text, ext: c.ext)
                    temp = u
                    switch kind {
                    case .subscription: op = .addSourceURLFile(path: u.path, name: sourceName, viaHost: viaHostChosen)
                    default: op = .addNodeFile(path: u.path, name: sourceName)
                    }
                } else { return }
                fallbackContent = nil
                await authorize(op, t, tempFile: temp)
            } catch {
                notice = Notice(kind: .error, text: ErrorText(raw: L10n.tempFileFailed))
            }
        case .invalid(let t) where t.code == .viaHostRequired:
            viaHostNeeded = true
            notice = Notice(kind: .info, text: ErrorText(raw: L10n.viaHostExplain))
        case let .invalid(t), let .failed(_, t), let .busy(t), let .unsupported(t):
            notice = Notice(kind: .error, text: t)
        case .unreachable:
            notice = Notice(kind: .error, text: ErrorText(raw: L10n.backendDown))
        case .needsConfirm:
            break
        }
    }

    /// 单个节点写成 YAML（每个值都写成 JSON 字符串或数字，JSON 是 YAML 的子集）。
    static func yaml(_ node: [String: JSONValue]) -> String {
        var lines: [String] = []
        for k in ["name", "type", "server", "port", "username", "password"] {
            guard let v = node[k] else { continue }
            switch v {
            case .string(let s):
                let q = (try? String(decoding: JSONEncoder().encode(s), as: UTF8.self)) ?? "\"\""
                lines.append("\(k): \(q)")
            case .number(let n): lines.append("\(k): \(Int(n))")
            default: break
            }
        }
        return lines.joined(separator: "\n") + "\n"
    }

    // ---- 删除、刷新 ----

    public func deleteSource(_ id: String) async {
        switch await client.deleteSource(id) {
        case .ok: notice = nil; await refresh()
        case .needsAuthorization(let t): await authorize(.removeSource(id: id), t)
        case let .busy(t), let .invalid(t), let .failed(_, t), let .unsupported(t): notice = Notice(kind: .error, text: t)
        case .unreachable: notice = Notice(kind: .error, text: ErrorText(raw: L10n.backendDown))
        case .needsConfirm: break
        }
    }

    /// 刷新订阅。未启用时后台要求显式选择“经本机网络”：记下是哪个来源，界面给出选择后带 viaHost 再调。
    public func refreshSource(_ id: String, viaHost: Bool = false) async {
        switch await client.refreshSource(id, viaHost: viaHost) {
        case .ok(let r):
            refreshNeedsViaHost = nil
            notice = nil
            needsReselect = r.needsReselect ?? false
            await refresh()
        case .invalid(let t) where t.code == .viaHostRequired:
            refreshNeedsViaHost = id
            notice = Notice(kind: .info, text: ErrorText(raw: L10n.viaHostExplain))
        case let .invalid(t), let .failed(_, t), let .busy(t), let .unsupported(t), let .needsAuthorization(t):
            notice = Notice(kind: .error, text: t)
        case .unreachable: notice = Notice(kind: .error, text: ErrorText(raw: L10n.backendDown))
        case .needsConfirm: break
        }
    }

    // ---- 选择与应用 ----

    /// 第一跳：有中转时是第一个中转，否则是出口节点。
    var firstHop: String? { via.first ?? selectedExit }

    /// 应用主代理：先保存选择（已启用时这一步就是切换），未启用时再建立出口基准。
    public func apply() async {
        guard let exit = selectedExit, !busy, !flow.isActive else { return }
        busy = true
        applyState = .checking
        defer { busy = false }
        let chain = via.isEmpty ? [] : via + [exit]
        lastSelect = (exit, chain)
        await handleSelect(await client.selectMain(selected: exit, chain: chain))
    }

    private var lastSelect: (String, [String])?

    private func handleSelect(_ out: Outcome<MainSelectOutcome>) async {
        switch out {
        case .ok(let o):
            if o.saved == true {
                applyState = .saved(o.note ?? "")
                await handleApply(await client.applyMain())
            } else {
                applyState = .succeeded(country: o.baseline?.country ?? "", timezone: o.baseline?.timezone ?? "")
                needsReselect = false
                await refresh()
            }
        case .needsConfirm(let p):
            applyState = .idle
            let sel = lastSelect
            _ = flow.present(p, as: MainSelectOutcome.self, refetch: { [weak self] in
                guard let self, let sel else { return .failed }
                let o = await self.client.selectMain(selected: sel.0, chain: sel.1)
                if case .needsConfirm(let np) = o { return .prompt(np) }
                await self.handleSelect(o)
                if case .ok = o { return .done }
                return .failed
            }, onResult: { [weak self] o in await self?.handleSelect(o) })
        case .needsAuthorization(let t):
            applyState = .idle
            guard let exit = selectedExit else { return }
            await authorize(.mainUse(node: exit, via: via), t, extra: [SensitiveOperation.mainApply.terminalCommand])
        case .invalid(let t) where t.code == .lanUnconfirmed:
            applyState = .idle
            if let hop = t.node ?? firstHop { // 节点以后台给的为准
                lanPrompt = LANPrompt(node: hop, name: nodes.first(where: { $0.id == hop })?.name ?? hop)
            }
        case let .invalid(t), let .failed(_, t), let .busy(t), let .unsupported(t):
            applyState = .failed(t, Rollback.of(t))
            notifier.switchFailed(t.code)
        case .unreachable:
            applyState = .failed(ErrorText(raw: L10n.backendDown), .none)
        }
    }

    private func handleApply(_ out: Outcome<DaemonMainProxyResult>) async {
        switch out {
        case .ok(let r):
            applyState = .succeeded(country: r.baseline.country, timezone: r.baseline.timezone ?? "")
            needsReselect = false
            await refresh()
        case .needsConfirm(let p):
            _ = flow.present(p, as: DaemonMainProxyResult.self, refetch: { [weak self] in
                guard let self else { return .failed }
                let o = await self.client.applyMain()
                if case .needsConfirm(let np) = o { return .prompt(np) }
                await self.handleApply(o)
                if case .ok = o { return .done }
                return .failed
            }, onResult: { [weak self] o in await self?.handleApply(o) })
        case .needsAuthorization(let t):
            await authorize(.mainApply, t)
        case let .invalid(t), let .failed(_, t), let .busy(t), let .unsupported(t):
            applyState = .failed(t, Rollback.of(t))
            notifier.switchFailed(t.code)
        case .unreachable:
            applyState = .failed(ErrorText(raw: L10n.backendDown), .none)
        }
    }

    /// 检查主代理（只检查、不保存）。
    public func check() async {
        checkState = .checking
        switch await client.checkMain() {
        case .ok(let b): checkState = .ok(rule: b.rule, country: b.country, timezone: b.timezone ?? "")
        case let .invalid(t), let .failed(_, t), let .busy(t), let .unsupported(t), let .needsAuthorization(t): checkState = .failed(t)
        case .unreachable: checkState = .failed(ErrorText(raw: L10n.backendDown))
        case .needsConfirm: checkState = .idle
        }
    }

    /// 确认局域网节点（用户在确认框里点了确认）。
    public func confirmLAN() async {
        guard let p = lanPrompt else { return }
        lanPrompt = nil
        switch await client.lanConfirm(node: p.node) {
        case .ok: notice = nil; await refresh()
        case .needsAuthorization(let t): await authorize(.lanConfirm(node: p.node), t)
        case let .invalid(t), let .failed(_, t), let .busy(t), let .unsupported(t): notice = Notice(kind: .error, text: t)
        case .unreachable: notice = Notice(kind: .error, text: ErrorText(raw: L10n.backendDown))
        case .needsConfirm: break
        }
    }

    // ---- 授权与终端退路 ----

    private func authorize(_ op: SensitiveOperation, _ reason: ErrorText, tempFile: URL? = nil, extra: [String] = []) async {
        switch await authorizer.authorize(op, reason: reason) {
        case .runInTerminal(let cmd):
            terminal = TerminalPrompt(commands: [cmd] + extra, tempFile: tempFile)
        case .authorized, .cancelled:
            // 系统授权（M5-3）接上之前不会走到 authorized；取消时删掉临时文件。
            if let tempFile { files.remove(tempFile) }
        }
    }

    /// 用户关掉了终端退路的说明（执行完了或取消了）：删掉临时文件，重新读列表。
    public func dismissTerminal() async {
        if let f = terminal?.tempFile { files.remove(f) }
        terminal = nil
        await refresh()
    }
}
