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

    public init(client: APIClient, authorizer: Authorizer = TerminalAuthorizer(), flow: ConfirmFlow? = nil) {
        self.client = client
        self.authorizer = authorizer
        self.flow = flow ?? ConfirmFlow(client: client)
    }

    func authorize(_ op: SensitiveOperation, _ reason: ErrorText) async {
        if case .runInTerminal(let cmd) = await authorizer.authorize(op, reason: reason) {
            terminal = TerminalPrompt(commands: [cmd])
        }
    }

    /// 读本页（子类覆盖）。
    public func load() async {}

    /// 关掉终端退路的提示后重新加载本页：用户可能刚在终端里改了东西（M5-9 复审必须改）。
    public func dismissTerminal() async {
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
                    items: x.avail.items ?? [:], tcp: opens[x.tag]?.tcp ?? 0, udp: opens[x.tag]?.udp ?? 0)
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
        conf = ""
        if await handle(o, op: .exitAdd(tag: tag, kind: kind, path: file.path)) {
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

    override public func load() async {
        let r = await client.rules()
        state = LoadState.from(r)
        guard case .ok(let doc) = r else { return }
        rules = doc
        var apps: [DaemonAppView] = []
        if case .ok(let a) = await client.apps() { apps = a.apps ?? [] }
        if case .ok(let xs) = await client.exits() { exitTags = xs.map(\.tag) } else { exitTags = [] }
        rows = Self.merge(apps: apps, rules: doc)
    }

    /// 在联网的应用和已有规则合并：先列在联网的（带规则的动作），再列只有规则的。
    static func merge(apps: [DaemonAppView], rules: RulesDoc) -> [AppRow] {
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

    /// 先读后写：提交前重新读一次规则，在刚读到的文档上应用修改再 PUT（不拿加载时缓存的文档整份覆盖，
    /// 免得冲掉别的客户端或终端里刚做的修改，包括页面上不显示的 local_domains、pinned_extra）。重新读失败时不提交。
    private func submit(op: SensitiveOperation, _ mutate: (inout RulesDoc) -> Void) async {
        busy = true
        defer { busy = false }
        authorizationReason = nil
        let fresh = await client.rules()
        guard case .ok(var doc) = fresh else {
            state = LoadState.from(fresh)
            notice = Notice(kind: .error, text: fresh.errorText ?? ErrorText(raw: L10n.loadFailed))
            return
        }
        mutate(&doc)
        let o = await client.putRules(doc, dropAffected: dropAffected)
        if case .needsAuthorization(let t) = o { authorizationReason = t }
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

@MainActor
public final class EnvModel: PageModel {
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
    /// 首次准备（M6-8a）；后台没打开时为 nil（不显示）。
    @Published public private(set) var prepare: DaemonPrepareStatus?
    public let env: EnvModel

    public let preference: NotificationPreference
    /// 通知开关（界面偏好，只影响通知）。
    @Published public var notificationsEnabled: Bool {
        didSet { preference.enabled = notificationsEnabled }
    }

    public init(client: APIClient, env: EnvModel? = nil, preference: NotificationPreference = MemoryPreference()) {
        self.env = env ?? EnvModel(client: client)
        self.preference = preference
        notificationsEnabled = preference.enabled
        super.init(client: client)
    }

    override public func load() async {
        let o = await client.status()
        state = LoadState.from(o)
        if case .ok(let st) = o { security = st.security; resolverLock = st.resolverLock; claudeCode = st.claudeCode; integrity = st.integrity; prepare = st.prepare }
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
        await env.load()
    }

    /// 回国会结束的 Claude 进程（只读预览；不签发确认码）。
    @Published public private(set) var processes: [ClaudeprocTarget] = []
    @Published public private(set) var unverified: [ClaudeprocTarget] = []
    /// 读不到清单时的原因（不留空白）。
    @Published public private(set) var previewError: String?

    /// 签名校验未启用的说明（后台给的）。
    public var signatureText: String? {
        guard let s = security, s.callerVerification == "disabled" else { return nil }
        return L10n.signatureDisabled + "：" + s.note
    }
    public var cliPath: String { L10n.cliPath }
    public var cliUsage: String { L10n.cliUsage }
    /// 启动管控的文字：现在允许与否、四层各自是否生效、第三层的覆盖范围、最近拦截；总带“登录时打开”的提示。
    public var launchControlText: String {
        guard let g = launchGuard else { return L10n.launchGuardUnknown }
        var lines = [g.allowed ? L10n.launchGuardAllowed : L10n.launchGuardBlocked]
        for l in g.layers ?? [] { lines.append(L10n.launchGuardLayer(l.name, l.effective) + ((l.note ?? "").isEmpty ? "" : "（" + (l.note ?? "") + "）")) }
        for c in g.coverage ?? [] { lines.append(L10n.launchGuardCoverage(c.name, c.reason)) }
        if let r = g.recent, !r.isEmpty { lines.append(L10n.launchGuardRecent(r.count)) }
        lines.append(L10n.loginItemHint)
        return lines.joined(separator: "\n")
    }
    /// 第一层（域名解析锁）的状态文字：生效、未写入、未生效（哪些域名什么问题）、有冲突。
    public var resolverLockText: String {
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
    /// 第四层的状态文字：生效、未写入、未生效（托管设置的问题或端口没在听）、有冲突；总带覆盖范围那句。
    public var claudeCodeText: String {
        guard let c = claudeCode else { return L10n.claudeCodeUnknown }
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
    /// 第四层是否有问题（界面标成警告色）。写入过但未生效、端口没在听、有冲突。
    public var claudeCodeWarn: Bool {
        guard let c = claudeCode else { return false }
        let written = c.settings?.written ?? false
        return (written && !c.effective) || !c.listening || !(c.settings?.conflicts ?? []).isEmpty
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
    public var diagnosticsText: String { L10n.diagnosticsNone }
}
