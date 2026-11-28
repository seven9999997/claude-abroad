// 其余页面的视图模型（M5-9，m5-plan v2.7 决定 1–6）：分流出口、应用分流、环境检测、地区、引擎与虚拟机、设置。
// 每个页面都有加载状态（加载中、已加载、后台不可达、不提供、加载失败），不出现空白页。
// 能不能做由后台判断；敏感操作走“取得本人授权”的协议（现在是终端退路）。
import AbroadAPI
import Combine
import Foundation

/// 页面共用：授权、终端退路、提示。
@MainActor
public class PageModel: ObservableObject {
    @Published public internal(set) var state: LoadState = .loading
    @Published public internal(set) var notice: Notice?
    @Published public internal(set) var terminal: TerminalPrompt?
    @Published public internal(set) var busy = false

    public let client: APIClient
    public let authorizer: Authorizer

    /// 统一的确认框（各页面共用一个实例）。
    public let flow: ConfirmFlow
    /// B0：经系统密码框执行敏感操作的确认框（各页面共用一个；没有时走终端退路）。
    public let privileged: PrivilegedSession?
    /// 要经文件交给命令行的内容（出口配置）存成的临时文件。
    public let files = InputFiles()
    /// 确认框显示在哪个窗口（设置窗口覆盖）。
    var privilegedHost: PrivilegedHost { .main }

    public init(client: APIClient, authorizer: Authorizer = TerminalAuthorizer(), flow: ConfirmFlow? = nil, privileged: PrivilegedSession? = nil) {
        self.client = client
        self.authorizer = authorizer
        self.flow = flow ?? ConfirmFlow(client: client)
        self.privileged = privileged
    }

    func authorize(_ op: SensitiveOperation, _ reason: ErrorText, tempFile: URL? = nil) async {
        let tp = TerminalPrompt(commands: [op.terminalCommand], tempFile: tempFile)
        switch await authorizer.authorize(op, reason: reason) {
        case .runInTerminal(let cmd): terminal = TerminalPrompt(commands: [cmd], tempFile: tempFile)
        case .confirmInApp:
            guard let s = privileged else { terminal = tp; return }
            await s.present([op], tempFiles: tempFile.map { [$0] } ?? [], host: privilegedHost, terminal: tp,
                            onTerminal: { [weak self] t in self?.terminal = t },
                            onDone: { [weak self] in await self?.load() })
        case .authorized, .cancelled: break
        }
    }

    /// 应用里直接切换的入口（例如第四道的开关）：先弹确认框，用户点“执行”才执行。只能从按钮调用。
    public func userTappedPrivileged(_ op: SensitiveOperation) async {
        guard let s = privileged else { terminal = TerminalPrompt(commands: [op.terminalCommand]); return }
        await s.present([op], host: privilegedHost, terminal: TerminalPrompt(commands: [op.terminalCommand]),
                        onTerminal: { [weak self] t in self?.terminal = t },
                        onDone: { [weak self] in await self?.load() })
    }

    /// 读本页（子类覆盖）。
    public func load() async {}

    /// 关掉终端退路的提示后重新加载本页：用户可能刚在终端里改了东西（M5-9 复审必须改）。
    public func dismissTerminal() async {
        if let f = terminal?.tempFile { files.remove(f) }
        terminal = nil
        await load()
    }

    /// 操作的通用结果处理（成功时返回 true）。
    func handle<T>(_ o: Outcome<T>, op: SensitiveOperation?) async -> Bool {
        switch o {
        case .ok: notice = nil; return true
        case .needsAuthorization(let t):
            if let op { notice = Notice(kind: .info, text: t); await authorize(op, t) } else { notice = Notice(kind: .error, text: t) }
        case let .busy(t), let .invalid(t), let .failed(_, t): notice = Notice(kind: .error, text: t)
        case .unsupported(let t): notice = Notice(kind: .error, text: ErrorText(raw: L10n.unsupported + "：" + t.raw, code: t.code))
        case .unreachable(let c): state = .unreachable(c)
        case .needsConfirm(let p): p.discard() // 这些页面的操作不会要两步确认；地区页自己交给统一的确认框
        }
        return false
    }
}

// MARK: - 分流出口

public struct ExitRow: Sendable, Equatable, Identifiable {
    public var id: String { tag }
    public var tag: String
    public var kind: String
    public var up: Bool
    public var reason: String
    public var check: String // pass、fail、unknown；没检查过为空
    public var items: [String: String]
    public var tcp: Int64
    public var udp: Int64
    /// 补齐二：指向这个出口的规则；出口不可用时显示“受影响的规则”。
    public var affected: [String] = []
    public var affectedText: String? { !up && !affected.isEmpty ? L10n.exitAffected(affected) : nil }
}

@MainActor
public final class ExitsModel: PageModel {
    @Published public private(set) var rows: [ExitRow] = []
    // 导入表单（配置内容提交即清；配置文件用用户自己的路径）
    @Published public var formTag = ""
    @Published public var formKind = "wireguard"
    @Published public var formFile: URL?

    /// 出口种类；sharelink 是存着分享链接的文本文件（每行一个；M6-6c，后台转成订阅出口）。
    public static let kinds = ["wireguard", "openvpn", "clash", "sharelink"]

    /// 没有出口时的说明。
    public var emptyText: String? { state == .loaded && rows.isEmpty ? L10n.noExits : nil }

    override public func load() async {
        let o = await client.exits()
        state = LoadState.from(o)
        guard case .ok(let xs) = o else { return }
        var opens: [String: DaemonExitCounts] = [:]
        if case .ok(let st) = await client.status() { opens = st.opens ?? [:] }
        rows = xs.map { x in
            ExitRow(tag: x.tag, kind: x.kind ?? "", up: x.avail.up, reason: x.avail.reason ?? "", check: x.avail.check ?? "",
                    items: x.avail.items ?? [:], tcp: opens[x.tag]?.tcp ?? 0, udp: opens[x.tag]?.udp ?? 0, affected: x.affected ?? [])
        }
    }

    public func add() async {
        let tag = formTag.trimmingCharacters(in: .whitespaces)
        guard !tag.isEmpty, let file = formFile else { notice = Notice(kind: .error, text: ErrorText(raw: L10n.exitFormIncomplete)); return }
        var conf: String
        do { conf = try InputFiles.readUserFile(file) } catch { notice = Notice(kind: .error, text: ErrorText(raw: L10n.addReadFailed)); return }
        let kind = formKind
        formFile = nil // 提交的那一刻清掉（模型里不留配置内容和路径）
        busy = true
        defer { busy = false }
        let o = await client.putExit(tag: tag, kind: kind, conf: conf)
        if case .needsAuthorization(let t) = o {
            // 配置存成只有本人能读的临时文件，命令里只有路径（用户选的文件可能在受保护的目录里，root 的 shell 未必读得到）。
            notice = Notice(kind: .info, text: t)
            do {
                let u = try files.writePrivate(conf, ext: "conf")
                conf = ""
                await authorize(.exitAdd(tag: tag, kind: kind, path: u.path), t, tempFile: u)
            } catch {
                conf = ""
                notice = Notice(kind: .error, text: ErrorText(raw: L10n.tempFileFailed))
            }
            return
        }
        conf = ""
        if await handle(o, op: nil) {
            formTag = ""
            await load()
        }
    }

    public func remove(_ tag: String) async {
        if await handle(await client.deleteExit(tag), op: .exitRemove(tag: tag)) { await load() }
    }

    public func check(_ tag: String) async {
        let o = await client.checkExit(tag)
        if case .ok(let r) = o, let i = rows.firstIndex(where: { $0.tag == tag }) {
            let a = r.status.avail
            rows[i].up = a.up
            rows[i].reason = a.reason ?? ""
            rows[i].check = a.check ?? ""
            rows[i].items = a.items ?? [:]
            notice = nil
        } else {
            _ = await handle(o, op: nil)
        }
    }
}

// MARK: - 应用分流

public struct AppRow: Sendable, Equatable, Identifiable {
    public var id: String { app }
    public var app: String
    public var name: String
    public var conns: Int
    public var exits: [String]
    public var action: String? // 已有规则时的动作；没有规则时走默认
    /// 补齐二：这条规则正在回退到主代理时的说明（规则指向的分流出口现在不可用）；没有时为 nil。
    public var fallback: String? = nil
    /// 补齐四：种类（app、system、path、unknown；旧后台不给时为 nil）和共用系统进程的说明。
    public var kind: String? = nil
    public var shared: String? = nil
    public var isSystem: Bool { kind == "system" }
}

/// 给共用的系统进程设规则之前的提醒（补齐四）。
public struct SharedRulePrompt: Sendable, Equatable {
    public var app: String
    public var action: String?
    public var note: String
    public var text: String { L10n.sharedRuleWarn(note) }
}

@MainActor
public final class AppsModel: PageModel {
    @Published public private(set) var rows: [AppRow] = []
    @Published public private(set) var rules: RulesDoc?
    /// “断开受影响的连接”：默认不勾。
    @Published public var dropAffected = false
    /// 放松保护被要求本人授权时的原因（来自后台：代码加文字）。
    @Published public private(set) var authorizationReason: ErrorText?

    public static let fixedActions = ["direct", "block", "proxy:main"]
    /// 当前的分流出口（动作菜单在三种固定动作之后列出 proxy:<标签>；读不到时只有三种）。
    @Published public private(set) var exitTags: [String] = []

    public var actions: [String] { Self.fixedActions + exitTags.map { "proxy:" + $0 } }

    /// 内置的置顶域名（状态里的 builtin_pinned；和规则引擎、解析锁同一个来源）。只读显示，没有删除按钮。
    @Published public private(set) var builtinPinned: [String] = []

    /// 规则里的三个名单（补齐一）；rawValue 和命令行的子命令一致。
    public enum RuleList: String, Sendable, CaseIterable {
        case local, pinned, nofake, offline
    }

    public func items(_ l: RuleList) -> [String] {
        guard let r = rules else { return [] }
        switch l {
        case .local: return r.localDomains
        case .pinned: return r.pinnedExtra
        case .nofake: return r.bypassExtra
        case .offline: return r.offlineAllow
        }
    }

    public static func title(_ l: RuleList) -> String {
        switch l {
        case .local: return L10n.rulesLocalTitle
        case .pinned: return L10n.rulesPinnedTitle
        case .nofake: return L10n.rulesNoFakeTitle
        case .offline: return L10n.rulesOfflineTitle
        }
    }

    public static func help(_ l: RuleList) -> String {
        switch l {
        case .local: return L10n.rulesLocalHelp
        case .pinned: return L10n.rulesPinnedHelp
        case .nofake: return L10n.rulesNoFakeHelp
        case .offline: return L10n.rulesOfflineHelp
        }
    }

    /// 比较用的形式（只为“重复添加、删除不存在的项不提交”；合法性由后台判定）。
    static func key(_ s: String) -> String {
        var t = s.trimmingCharacters(in: .whitespaces).lowercased()
        while t.hasSuffix(".") { t.removeLast() }
        return t
    }

    static func get(_ doc: RulesDoc, _ l: RuleList) -> [String] {
        switch l {
        case .local: return doc.localDomains
        case .pinned: return doc.pinnedExtra
        case .nofake: return doc.bypassExtra
        case .offline: return doc.offlineAllow
        }
    }

    static func set(_ doc: inout RulesDoc, _ l: RuleList, _ xs: [String]) {
        switch l {
        case .local: doc.localDomains = xs
        case .pinned: doc.pinnedExtra = xs
        case .nofake: doc.bypassExtra = xs
        case .offline: doc.offlineAllow = xs
        }
    }

    /// 各名单输入框里还没提交的内容。
    @Published public var drafts: [RuleList: String] = [:]

    /// 提交输入框里的内容（提交后清空输入框）。
    public func addDraft(_ l: RuleList) async {
        let v = drafts[l] ?? ""
        drafts[l] = ""
        await addItem(l, v)
    }

    /// 加一项（先读后写，带条件头）。
    public func addItem(_ l: RuleList, _ raw: String) async {
        let item = raw.trimmingCharacters(in: .whitespaces)
        guard !item.isEmpty else { return }
        await submit(op: .ruleListAdd(kind: l.rawValue, item: item)) { doc in
            var xs = Self.get(doc, l)
            if !xs.contains(where: { Self.key($0) == Self.key(item) }) { xs.append(item) }
            Self.set(&doc, l, xs)
        }
    }

    /// 删一项。内置的置顶域名不在追加名单里：按状态里的内置名单拦下，不发请求。
    public func deleteListItem(_ l: RuleList, _ item: String) async {
        if l == .pinned {
            let k = Self.key(item)
            let base = k.hasPrefix("*.") ? String(k.dropFirst(2)) : k
            if !items(.pinned).contains(where: { Self.key($0) == k }) && builtinPinned.contains(base) {
                notice = Notice(kind: .error, text: ErrorText(raw: L10n.rulesBuiltinReadonly))
                return
            }
        }
        await submit(op: .ruleListRemove(kind: l.rawValue, item: item)) { doc in
            Self.set(&doc, l, Self.get(doc, l).filter { Self.key($0) != Self.key(item) })
        }
    }

    /// 拦截 UDP 443 的开关。
    public func setBlockUDP443(_ on: Bool) async {
        await submit(op: .udp443(on: on)) { doc in doc.blockUDP443 = on }
    }

    override public func load() async {
        let tr = await client.rulesTagged()
        let r: Outcome<RulesDoc>
        switch tr {
        case .ok(let t): r = .ok(t.doc)
        default: r = tr.mapError()
        }
        state = LoadState.from(r)
        guard case .ok(let doc) = r else { return }
        rules = doc
        if case .ok(let st) = await client.status() { builtinPinned = st.builtinPinned ?? [] }
        var apps: [DaemonAppView] = []
        var fallbacks: [DaemonRuleFallback] = []
        var ruleKinds: [DaemonAppKindView] = []
        if case .ok(let a) = await client.apps() { apps = a.apps ?? []; fallbacks = a.fallbacks ?? []; ruleKinds = a.ruleKinds ?? [] }
        if case .ok(let xs) = await client.exits() { exitTags = xs.map(\.tag) } else { exitTags = [] }
        rows = Self.merge(apps: apps, rules: doc, fallbacks: fallbacks)
        // 补齐四：种类和共用说明都由后台给（在联网的在 apps 里，只有规则的在 rule_kinds 里）
        var kinds: [String: (String?, String?)] = [:]
        for a in apps { kinds[a.app] = (a.kind, a.shared) }
        for k in ruleKinds { kinds[k.app] = (k.kind, k.shared) }
        for i in rows.indices { if let k = kinds[rows[i].app] { rows[i].kind = k.0; rows[i].shared = k.1 } }
    }

    /// 在联网的应用和已有规则合并：先列在联网的（带规则的动作），再列只有规则的。
    static func merge(apps: [DaemonAppView], rules: RulesDoc, fallbacks: [DaemonRuleFallback] = []) -> [AppRow] {
        var out = mergeRows(apps: apps, rules: rules)
        let fb = Dictionary(fallbacks.map { ($0.app, $0.exit) }, uniquingKeysWith: { a, _ in a })
        for i in out.indices { if let ex = fb[out[i].app] { out[i].fallback = L10n.fallbackMark(ex) } }
        return out
    }

    static func mergeRows(apps: [DaemonAppView], rules: RulesDoc) -> [AppRow] {
        var byApp: [String: String] = [:]
        for r in rules.rules { byApp[r.app] = r.action }
        var out = apps.map { AppRow(app: $0.app, name: $0.name, conns: Int($0.conns), exits: $0.exits ?? [], action: byApp[$0.app]) }
        let live = Set(out.map(\.app))
        for r in rules.rules where !live.contains(r.app) {
            out.append(AppRow(app: r.app, name: Self.displayName(r.app), conns: 0, exits: [], action: r.action))
        }
        return out
    }

    static func displayName(_ app: String) -> String {
        app.hasPrefix("path:") ? (app as NSString).lastPathComponent : app
    }

    /// 普通应用和系统进程分开（系统进程放在后面单列；旧后台不给种类时都算普通的）。
    public var appRows: [AppRow] { rows.filter { !$0.isSystem } }
    public var systemRows: [AppRow] { rows.filter(\.isSystem) }

    /// 给共用的系统进程设规则或删规则之前的提醒（确认才提交，取消不发请求）。
    @Published public private(set) var sharedPrompt: SharedRulePrompt?

    /// 界面上改规则的入口：共用的系统进程先提醒，其余直接提交。action 为 nil 表示删规则。
    public func requestRule(app: String, action: String?) async {
        if let note = rows.first(where: { $0.app == app })?.shared, !note.isEmpty {
            sharedPrompt = SharedRulePrompt(app: app, action: action, note: note)
            return
        }
        if let action { await setRule(app: app, action: action) } else { await removeRule(app: app) }
    }

    public func confirmShared() async {
        guard let p = sharedPrompt else { return }
        sharedPrompt = nil
        if let a = p.action { await setRule(app: p.app, action: a) } else { await removeRule(app: p.app) }
    }

    public func cancelShared() { sharedPrompt = nil }

    /// 改一个应用的规则（收紧直接提交；放松的由后台要求本人授权并给出原因）。
    public func setRule(app: String, action: String) async {
        let id = AppIdentity.normalize(app)
        await submit(op: .appSet(app: id, action: action)) { doc in
            if let i = doc.rules.firstIndex(where: { $0.app == id }) { doc.rules[i].action = action } else { doc.rules.append(.init(app: id, action: action)) }
        }
    }

    public func removeRule(app: String) async {
        await submit(op: .appRemove(app: app)) { doc in doc.rules.removeAll { $0.app == app } }
    }

    /// 拖进来的应用包：取应用 ID，加一条规则。
    public func addDropped(_ url: URL, action: String) async {
        guard let id = AppIdentity.appID(bundleAt: url) else { return }
        await setRule(app: id, action: action)
    }

    /// 先读后写：提交前重新读一次规则和它的标识，在刚读到的文档上应用修改，带条件头 PUT（补齐一）。没有变化时不提交。
    /// 后台回 rules_changed（读和写之间被别处改过、什么都没改）时重新加载并提示，不自动重试（要用户再点一次）。
    private func submit(op: SensitiveOperation, _ mutate: (inout RulesDoc) -> Void) async {
        busy = true
        defer { busy = false }
        authorizationReason = nil
        let fresh = await client.rulesTagged()
        guard case .ok(let t) = fresh else {
            state = LoadState.from(fresh)
            notice = Notice(kind: .error, text: fresh.errorText ?? ErrorText(raw: L10n.loadFailed))
            return
        }
        var doc = t.doc
        mutate(&doc)
        if doc == t.doc {
            notice = Notice(kind: .info, text: ErrorText(raw: L10n.rulesNoChange))
            return
        }
        let o = await client.putRules(doc, dropAffected: dropAffected, ifMatch: t.tag)
        if case .failed(_, let e) = o, e.code == .rulesChanged {
            await load()
            notice = Notice(kind: .info, text: ErrorText(raw: L10n.rulesChangedReloaded, code: .rulesChanged))
            return
        }
        if case .needsAuthorization(let e) = o { authorizationReason = e }
        if await handle(o, op: op) { await load() }
    }
}

// MARK: - 环境检测

public struct EnvRow: Sendable, Equatable, Identifiable {
    public var id: String
    public var name: String
    public var hard: Bool
    public var result: String // pass、fail、unknown、skipped
    public var detail: String
    public var fix: String
    public var enabled: Bool // 警告项的开关（硬性项总是开着）
}

/// 环境检测历史的一行（补齐四）：时间、总判定、和更早一条相比变了的项。
public struct EnvHistoryRow: Sendable, Equatable, Identifiable {
    public var id: String { at }
    public var at: String
    public var text: String
    public var changed: String?
}

public final class EnvModel: PageModel {
    /// 补齐四：历史（最新的在前）和它自己的加载状态（不可达、失败、成功）。
    @Published public private(set) var history: [EnvHistoryRow] = []
    @Published public private(set) var historyState: LoadState = .loading
    public var historyEmptyText: String? { historyState == .loaded && history.isEmpty ? L10n.envHistoryEmpty : nil }

    public func loadHistory() async {
        let o = await client.envHistory()
        historyState = LoadState.from(o)
        guard case .ok(let v) = o else { return }
        history = Self.historyRows(v.entries ?? [])
    }

    public func clearHistory() async {
        let o = await client.clearEnvHistory()
        if case .ok(let v) = o { history = Self.historyRows(v.entries ?? []); notice = nil } else { _ = await handle(o, op: nil) }
    }

    /// 每条和更早的那一条（列表里的下一条）相比，标出结果变了的项。
    static func historyRows(_ es: [EnvcheckRunEntry]) -> [EnvHistoryRow] {
        es.enumerated().map { i, e in
            var changed: String?
            if i + 1 < es.count {
                let prev = Dictionary((es[i + 1].items ?? []).map { ($0.id, $0.result) }, uniquingKeysWith: { a, _ in a })
                let ch = (e.items ?? []).compactMap { it -> String? in
                    let p = prev[it.id]
                    return p == it.result ? nil : it.id + " " + (p ?? "") + "→" + it.result
                }
                if !ch.isEmpty { changed = L10n.envHistoryChanged(ch) }
            }
            return EnvHistoryRow(at: e.at, text: L10n.envHistoryLine(e.at, e.hard, e.lastSeen == e.at ? nil : e.lastSeen), changed: changed)
        }
    }

    @Published public private(set) var hardRows: [EnvRow] = []
    @Published public private(set) var warnRows: [EnvRow] = []
    @Published public private(set) var hard = "" // 硬性项总判定
    @Published public private(set) var disabled: [String] = []

    override public func load() async {
        let s = await client.envSettings()
        if case .ok(let v) = s { disabled = v.disabled }
        let r = await client.envcheck()
        state = LoadState.from(r)
        if case .ok(let rep) = r { apply(rep) }
        await loadHistory()
    }

    func apply(_ rep: EnvcheckReport) {
        hard = rep.hard
        let rows = (rep.items ?? []).map { i in
            EnvRow(id: i.id, name: i.name, hard: i.level == "hard", result: i.result, detail: i.detail ?? "", fix: i.fix ?? "",
                   enabled: i.level == "hard" || !disabled.contains(i.id))
        }
        hardRows = rows.filter(\.hard)
        warnRows = rows.filter { !$0.hard }
    }

    /// 打开或关掉一个警告项（硬性项不可操作）。
    public func setEnabled(_ id: String, _ on: Bool) async {
        guard warnRows.contains(where: { $0.id == id }) else { notice = Notice(kind: .error, text: ErrorText(raw: L10n.hardItemLocked)); return }
        let fresh = await client.envSettings() // 先读后写：不拿缓存的 disabled 整份覆盖
        guard case .ok(let cur) = fresh else { _ = await handle(fresh, op: nil); return }
        var d = Set(cur.disabled)
        if on { d.remove(id) } else { d.insert(id) }
        let o = await client.putEnvSettings(EnvSettings(disabled: d.sorted()))
        if case .ok(let v) = o { disabled = v.disabled; notice = nil; await load() } else { _ = await handle(o, op: nil) }
    }

    /// 立即重新检测。
    public func rerun() async {
        busy = true
        defer { busy = false }
        let o = await client.runEnvcheck()
        if case .ok(let rep) = o { apply(rep); notice = nil } else { _ = await handle(o, op: nil) }
    }

    /// 设备体检的结果（没跑过时为 nil）。
    @Published public private(set) var device: EnvcheckDeviceReport?

    /// 跑一次设备体检（只读，不影响放行）。
    public func runDevice() async {
        busy = true
        defer { busy = false }
        let o = await client.deviceCheck()
        if case .ok(let rep) = o { device = rep; notice = nil } else { _ = await handle(o, op: nil) }
    }

    /// 设备体检的文字：没跑过时是提示，跑过时是结果。
    public var deviceCheckText: String {
        guard let d = device else { return L10n.deviceCheckHint }
        let fs = d.findings ?? []
        if fs.isEmpty { return L10n.deviceCheckClean }
        return ([L10n.deviceCheckFound(fs.count)] + fs.map { L10n.deviceCheckLine($0.location, $0.font, $0.file) } + [d.note]).joined(separator: "\n")
    }

    /// 首页环境检测要点的 id：没通过的项之后固定列这几项（接管、出口 IP、出口地区、DNS、WebRTC、时区），完整列表在“环境检测”页。
    public nonisolated static let homeKeyIDs = ["01", "02", "03", "07", "08", "04"]

    /// 首页环境检测要点：没通过的项（失败在前、无法判断在后）全部列出，之后按 homeKeyIDs 列通过的关键项。
    /// 之前只取前 6 项，有两项警告时 07 DNS、08 WebRTC 被挤掉（2026-11-28 用户反馈）。
    public nonisolated static func homeItems(_ raw: [EnvcheckItemResult]) -> [EnvcheckItemResult] {
        let rank: (String) -> Int = { $0 == "fail" ? 0 : ($0 == "pass" || $0 == "skipped" ? 2 : 1) }
        let problems = raw.filter { rank($0.result) < 2 }
            .sorted { rank($0.result) != rank($1.result) ? rank($0.result) < rank($1.result) : $0.id < $1.id }
        let keys = homeKeyIDs.compactMap { id in raw.first { $0.id == id && $0.result == "pass" } }
        return problems + keys
    }

    public static func resultTitle(_ r: String) -> String {
        switch r {
        case "pass": return L10n.resultPass
        case "fail": return L10n.resultFail
        case "skipped": return L10n.resultSkipped
        default: return L10n.resultUnknown
        }
    }
}

// MARK: - 地区

@MainActor
public final class RegionModel: PageModel {
    @Published public private(set) var region: DaemonRegionStatus?
    @Published public var formTimezone = ""
    @Published public var formLang = ""
    @Published public private(set) var switched = false

    public var targetText: String? {
        guard let t = region?.target else { return nil }
        return L10n.regionTarget(t.timezone, t.lang, t.locale)
    }
    public var sourceText: String? {
        guard let s = region?.target?.source else { return nil }
        return s == "exit" ? L10n.regionSourceExit : L10n.regionSourceManual
    }
    /// 待还原的项（网络已恢复但时区或语言没还原）。
    public var pending: [String] { region?.pending ?? [] }

    override public func load() async {
        let o = await client.region()
        state = LoadState.from(o)
        if case .ok(let r) = o { region = r }
    }

    public func setManual() async {
        let tz = formTimezone.trimmingCharacters(in: .whitespaces), lang = formLang.trimmingCharacters(in: .whitespaces)
        guard !tz.isEmpty, !lang.isEmpty else { notice = Notice(kind: .error, text: ErrorText(raw: L10n.regionFormIncomplete)); return }
        await put(.regionSet(timezone: tz, lang: lang)) { await self.client.putRegion(mode: "manual", timezone: tz, lang: lang) }
    }

    public func followExit() async {
        await put(.regionAuto) { await self.client.putRegion(mode: "exit") }
    }

    private func put(_ op: SensitiveOperation, _ call: @escaping () async -> Outcome<Empty>) async {
        guard !flow.isActive else { return }
        busy = true
        defer { busy = false }
        switched = false
        await finish(await call(), op: op, call: call)
    }

    /// 结果处理：会变时交给统一的确认框（确认码无效时重新发起第一步取新预览）。
    private func finish(_ o: Outcome<Empty>, op: SensitiveOperation, call: @escaping () async -> Outcome<Empty>) async {
        if case .needsConfirm(let p) = o {
            _ = flow.present(p, as: Empty.self, refetch: { [weak self] in
                let r = await call()
                if case .needsConfirm(let np) = r { return .prompt(np) }
                await self?.finish(r, op: op, call: call)
                if case .ok = r { return .done }
                return .failed
            }, onResult: { [weak self] r in await self?.finish(r, op: op, call: call) })
            return
        }
        if await handle(o, op: op) { switched = true; await load() }
    }
}

// MARK: - 引擎与虚拟机（只读）

@MainActor
public final class EnginesModel: PageModel {
    @Published public private(set) var view: DaemonEnginesView?

    /// 虚拟机不可达时的说明（不当作错误）。
    public var vmText: String? { view.map { $0.reachable ? nil : L10n.vmUnreachable } ?? nil }

    override public func load() async {
        let o = await client.engines()
        state = LoadState.from(o)
        if case .ok(let v) = o { view = v }
    }
}

// MARK: - 设置

@MainActor
public final class SettingsModel: PageModel {
    @Published public private(set) var security: ApiSecurity?
    /// Claude 域名解析锁（第一层，M6-3）的状态；后台没接时为 nil。
    @Published public private(set) var resolverLock: ResolverlockStatus?
    /// 第四层（Claude Code 的托管设置和本机代理端口，M6-3b）的状态；后台没接时为 nil。
    @Published public private(set) var claudeCode: DaemonClaudeProxyStatus?
    /// 启动管控（M6-4b）；后台没给时为 nil。
    @Published public private(set) var launchGuard: DaemonLaunchGuardView?
    /// 程序完整性（M6-7）；后台没接时为 nil。
    @Published public private(set) var integrity: IntegrityResult?
    /// Claude 访问过的域名（M6-12）；后台没给时为 nil。
    @Published public private(set) var claudeDomains: DaemonClaudeDomainsView?
    /// 首次准备（M6-8a）；后台没打开时为 nil（不显示）。
    @Published public private(set) var prepare: DaemonPrepareStatus?
    /// 补齐三：“允许局域网”（状态里的 allow_lan；旧后台不给时为 nil，不显示开关）。
    @Published public private(set) var allowLAN: Bool?

    /// 改“允许局域网”：关是收紧，直接提交；从关到开是放松，后台要求本人授权时给出终端命令。
    public func setAllowLAN(_ on: Bool) async {
        busy = true
        defer { busy = false }
        let o = await client.putLANSetting(on)
        if case .ok(let v) = o { allowLAN = v.allowLan; notice = nil; return }
        _ = await handle(o, op: .allowLAN(on: on))
    }

    /// 回国拦截（M6-11b）；没打开启动管控时后台不给，为 nil（不显示）。
    @Published public private(set) var returnBlock: DaemonReturnBlockStatus?
    public let env: EnvModel

    public let preference: NotificationPreference
    /// 通知开关（界面偏好，只影响通知）。
    @Published public var notificationsEnabled: Bool {
        didSet { preference.enabled = notificationsEnabled }
    }

    public init(client: APIClient, env: EnvModel? = nil, preference: NotificationPreference = MemoryPreference(),
                authorizer: Authorizer = TerminalAuthorizer(), privileged: PrivilegedSession? = nil) {
        self.env = env ?? EnvModel(client: client)
        self.preference = preference
        notificationsEnabled = preference.enabled
        super.init(client: client, authorizer: authorizer, privileged: privileged)
    }

    override var privilegedHost: PrivilegedHost { .settings }

    override public func load() async {
        let o = await client.status()
        state = LoadState.from(o)
        if case .ok(let st) = o { security = st.security; resolverLock = st.resolverLock; claudeCode = st.claudeCode; integrity = st.integrity; prepare = st.prepare; returnBlock = st.returnBlock; allowLAN = st.allowLan; launchControlOn = st.launchControl ?? (st.resolverLock != nil || st.claudeCode != nil) }
        switch await client.restorePreview() {
        case .ok(let pv):
            processes = pv.processes ?? []
            unverified = pv.unverified ?? []
            previewError = nil
        case .unreachable:
            previewError = L10n.backendDown
        case let other:
            previewError = other.errorText?.raw ?? L10n.loadFailed
        }
        if case .ok(let lg) = await client.launchGuard() { launchGuard = lg }
        if case .ok(let cd) = await client.claudeDomains() { claudeDomains = cd }
        await env.load()
    }

    /// 回国会结束的 Claude 进程（只读预览；不签发确认码）。
    @Published public private(set) var processes: [ClaudeprocTarget] = []
    @Published public private(set) var unverified: [ClaudeprocTarget] = []
    /// 读不到清单时的原因（不留空白）。
    @Published public private(set) var previewError: String?

    /// 签名校验未启用的说明（后台给的）。
    public var signatureText: String? {
        guard let s = security else { return nil }
        if s.caller == "unsigned" { return L10n.securityUnsignedTitle + "：" + s.note }
        guard s.callerVerification == "disabled" else { return nil }
        return L10n.signatureDisabled + "：" + s.note
    }
    public var cliPath: String { L10n.cliPath }
    public var cliUsage: String { L10n.cliUsage }
    /// 启动管控的文字：现在允许与否、四层各自是否生效、第三层的覆盖范围、最近拦截；总带“登录时打开”的提示。
    /// 这次安装打开了启动管控（M6-4b-1 返工；没打开时解析锁、第四层、启动管控都显示“未启用”）。状态里没有
    /// launch_control 时（没打开时后台不输出它，旧版本的后台也没有）看有没有解析锁或第四层的状态：没打开时这两项都没有。
    @Published public private(set) var launchControlOn = true

    public var launchControlText: String {
        guard let g = launchGuard else { return L10n.launchGuardUnknown }
        if !launchControlOn { return L10n.launchControlOff }
        var lines = [g.allowed ? L10n.launchGuardAllowed : L10n.launchGuardBlocked]
        for l in g.layers ?? [] { lines.append(L10n.launchGuardLayer(l.name, l.effective) + ((l.note ?? "").isEmpty ? "" : "（" + (l.note ?? "") + "）")) }
        for c in g.coverage ?? [] { lines.append(L10n.launchGuardCoverage(c.name, c.status, c.reason)) }
        if let r = g.recent, !r.isEmpty { lines.append(L10n.launchGuardRecent(r.count)) }
        lines.append(L10n.loginItemHint)
        return lines.joined(separator: "\n")
    }
    /// 回国拦截的文字（M6-11b）：是否生效、拦哪几段，加上后果那一句。状态里没有这一项时为 nil（不显示）。
    public var returnBlockText: String? {
        guard let r = returnBlock else { return nil }
        let nets = r.nets ?? []
        let head = !r.on ? L10n.returnBlockOff : (r.active ? L10n.returnBlockActive(nets) : L10n.returnBlockOn(nets))
        if r.test == true { return head + "\n" + L10n.returnBlockTestTag }
        return head + "\n" + L10n.returnBlockConsequence
    }
    /// 第一层（域名解析锁）的状态文字：生效、未写入、未生效（哪些域名什么问题）、有冲突。
    public var resolverLockText: String {
        if !launchControlOn { return L10n.launchControlOff }
        guard let l = resolverLock else { return L10n.resolverLockUnknown }
        var parts: [String] = []
        if !l.written {
            parts.append(L10n.resolverLockNotWritten)
        } else if l.effective {
            parts.append(L10n.resolverLockEffective(l.domains ?? []))
        } else {
            let ps = (l.problems ?? []).map { L10n.resolverLockProblem($0.domain, $0.kind) }
            parts.append(L10n.resolverLockNotEffective + "：" + ps.joined(separator: "；"))
        }
        if let c = l.conflicts, !c.isEmpty { parts.append(L10n.resolverLockConflicts(c)) }
        return parts.joined(separator: "\n")
    }
    /// 第四层的状态文字：未开启（默认关）、测试模式、生效、未写入、未生效（托管设置的问题或端口没在听）、有冲突；
    /// 打开着时总带覆盖范围那句。没打开启动管控时“未启用”（没有开关）。
    public var claudeCodeText: String {
        if !launchControlOn { return L10n.launchControlOff }
        guard let c = claudeCode else { return L10n.claudeCodeUnknown }
        if !c.enabled {
            return c.leftover == true ? L10n.claudeCodeOff + "\n" + L10n.claudeCodeLeftover : L10n.claudeCodeOff
        }
        if c.testMode == true {
            return L10n.claudeCodeTestMode + (c.listening ? "" : "\n" + L10n.claudeCodePortDown)
        }
        var parts: [String] = []
        let s = c.settings
        if s == nil || s?.written == false {
            parts.append(L10n.claudeCodeNotWritten)
        } else if c.effective {
            parts.append(L10n.claudeCodeEffective)
        } else {
            var why: [String] = []
            if let p = s?.problem, !p.isEmpty { why.append(L10n.claudeCodeProblem(p)) }
            if !c.listening { why.append(L10n.claudeCodePortDown) }
            parts.append(L10n.claudeCodeNotEffective + (why.isEmpty ? "" : "：" + why.joined(separator: "；")))
        }
        if let cs = s?.conflicts, !cs.isEmpty {
            parts.append(L10n.claudeCodeConflicts(cs.map { $0.var.isEmpty ? $0.file + "：" + L10n.claudeCodeUnparsable : $0.file + "：" + $0.var }))
        }
        parts.append(L10n.claudeCodeScope)
        return parts.joined(separator: "\n")
    }
    /// 第四层是否有问题（界面标成警告色）。打开着：写入过但未生效、端口没在听、有冲突；关着：只有留下的文件。
    public var claudeCodeWarn: Bool {
        guard let c = claudeCode else { return false }
        if !c.enabled { return c.leftover == true }
        if c.testMode == true { return !c.listening }
        let written = c.settings?.written ?? false
        return (written && !c.effective) || !c.listening || !(c.settings?.conflicts ?? []).isEmpty
    }
    /// 第四道的开关在这次安装里有没有（打开了启动管控、后台报告了第四层）：没有时界面不显示后果和命令。
    public var claudeCodeSwitchShown: Bool { launchControlOn && claudeCode != nil }
    /// 打开第四道的后果（来自文字表）。
    public var claudeCodeConsequenceText: String {
        L10n.claudeCodeConsequenceTitle + "\n" + L10n.claudeCodeConsequences.map { "· " + $0 }.joined(separator: "\n")
    }
    /// 打开、关闭各自对应的终端命令（应用里不直接切换：两个方向都要本人授权）。
    public var claudeCodeCommands: [(label: String, command: String)] {
        [(L10n.claudeCodeOnLabel, L10n.claudeCodeOnCommand), (L10n.claudeCodeOffLabel, L10n.claudeCodeOffCommand)]
    }
    /// Claude 访问过的域名：每个一行（域名和次数）；没有时一句“还没有记录”。
    public var claudeDomainLines: [String] {
        guard let cd = claudeDomains, let ds = cd.domains, !ds.isEmpty else { return [L10n.claudeDomainsNone] }
        return ds.map { L10n.claudeDomainLine($0.domain, Int($0.count)) }
    }

    /// 清空 Claude 访问过的域名。
    public func clearClaudeDomains() async {
        if case .ok(let cd) = await client.clearClaudeDomains() { claudeDomains = cd }
    }

    /// 程序完整性的一句话（M6-7）。
    public var integrityText: String {
        guard let r = integrity else { return L10n.integrityUnknown }
        if r.ok { return r.checked ? L10n.integrityOK : L10n.integrityNotChecked(r.note ?? "") }
        return L10n.integrityFailed((r.problems ?? []).map { L10n.integrityItem($0.file, $0.kind) })
    }
    public var integrityWarn: Bool { integrity.map { !$0.ok } ?? false }
    /// 首次准备的说明（M6-8a）；后台没打开首次准备时为 nil。
    public var prepareText: String? {
        guard let p = prepare else { return nil }
        return ([p.done ? L10n.prepareDone : L10n.prepareNotDone] + (p.notes ?? [])).joined(separator: "\n")
    }
    public var prepareWarn: Bool { prepare.map { !$0.done || !($0.notes ?? []).isEmpty } ?? false }
    /// 第一层是否有问题（界面标成警告色）。
    public var resolverLockWarn: Bool {
        guard let l = resolverLock else { return false }
        return (l.written && !l.effective) || !(l.conflicts ?? []).isEmpty
    }
    /// 进程清单的说明：读不到时是原因，没有进程时说明“没有”，有进程时为空（列表本身显示）。
    public var processListText: String? {
        if let e = previewError { return e }
        return processes.isEmpty && unverified.isEmpty ? L10n.processListNone : nil
    }
    public var uninstallText: String { L10n.uninstallInInstaller }
}
