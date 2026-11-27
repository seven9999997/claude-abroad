// 由 cmd/abroad-apigen 从 daemon.RouteTable 生成，不要手改（m5-plan v2.1 决定 7）。

import Foundation

/// 任意 JSON 值（接口里少数 any 类型的字段）。
public enum JSONValue: Codable, Sendable, Equatable {
    case string(String), number(Double), bool(Bool), object([String: JSONValue]), array([JSONValue]), null
    public init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if c.decodeNil() { self = .null } else if let v = try? c.decode(Bool.self) { self = .bool(v) }
        else if let v = try? c.decode(Double.self) { self = .number(v) } else if let v = try? c.decode(String.self) { self = .string(v) }
        else if let v = try? c.decode([JSONValue].self) { self = .array(v) } else { self = .object(try c.decode([String: JSONValue].self)) }
    }
    public func encode(to encoder: Encoder) throws {
        var c = encoder.singleValueContainer()
        switch self {
        case .string(let v): try c.encode(v)
        case .number(let v): try c.encode(v)
        case .bool(let v): try c.encode(v)
        case .object(let v): try c.encode(v)
        case .array(let v): try c.encode(v)
        case .null: try c.encodeNil()
        }
    }
}

/// 错误应答里稳定的代码（internal/api.ErrorCodes）。
public enum ApiErrorCode: String, Codable, Sendable, CaseIterable {
    case needsAuthorization = "needs_authorization"
    case needsConfirm = "needs_confirm"
    case confirmInvalid = "confirm_invalid"
    case busy = "busy"
    case inUse = "in_use"
    case viaHostRequired = "via_host_required"
    case lanUnconfirmed = "lan_unconfirmed"
    case switchRolledBack = "switch_rolled_back"
    case switchRollbackIncomplete = "switch_rollback_incomplete"
    case noBaseline = "no_baseline"
    case unsupported = "unsupported"
    case invalid = "invalid"
    case notFound = "not_found"
    case tooLarge = "too_large"
    case notEnabled = "not_enabled"
    case unavailable = "unavailable"
    case `internal` = "internal"
    case uninstalled = "uninstalled"
    case rulesChanged = "rules_changed"
    case vmUnavailable = "vm_unavailable"
    case mainPushFailed = "main_push_failed"
    case mainUnreachable = "main_unreachable"
    case noMainSelected = "no_main_selected"
    case stateIncomplete = "state_incomplete"
}

public struct ApiErrorBody: Codable, Sendable {
    public var error: String
    public var code: String
    public var node: String?
    public var field: String?
    enum CodingKeys: String, CodingKey {
        case error = "error"
        case code = "code"
        case node = "node"
        case field = "field"
    }
}

public struct ApiSecurity: Codable, Sendable {
    public var callerVerification: String
    public var note: String
    public var caller: String
    enum CodingKeys: String, CodingKey {
        case callerVerification = "caller_verification"
        case note = "note"
        case caller = "caller"
    }
}

public struct BuildinfoInfo: Codable, Sendable {
    public var commit: String
    public var buildTime: String
    public var dirty: Bool
    enum CodingKeys: String, CodingKey {
        case commit = "commit"
        case buildTime = "build_time"
        case dirty = "dirty"
    }
}

public struct ClaudecfgConflict: Codable, Sendable {
    public var file: String
    public var `var`: String
    enum CodingKeys: String, CodingKey {
        case file = "file"
        case `var` = "var"
    }
}

public struct ClaudecfgStatus: Codable, Sendable {
    public var written: Bool
    public var effective: Bool
    public var problem: String?
    public var conflicts: [ClaudecfgConflict]?
    enum CodingKeys: String, CodingKey {
        case written = "written"
        case effective = "effective"
        case problem = "problem"
        case conflicts = "conflicts"
    }
}

public struct ClaudedomainsEntry: Codable, Sendable {
    public var domain: String
    public var count: Int64
    public var first: String
    public var last: String
    enum CodingKeys: String, CodingKey {
        case domain = "domain"
        case count = "count"
        case first = "first"
        case last = "last"
    }
}

public struct ClaudeprocCoverageRow: Codable, Sendable {
    public var kind: String
    public var name: String
    public var status: String
    public var reason: String
    enum CodingKeys: String, CodingKey {
        case kind = "kind"
        case name = "name"
        case status = "status"
        case reason = "reason"
    }
}

public struct ClaudeprocGuardRecord: Codable, Sendable {
    public var time: String
    public var pid: Int64
    public var kind: String
    public var name: String
    enum CodingKeys: String, CodingKey {
        case time = "time"
        case pid = "pid"
        case kind = "kind"
        case name = "name"
    }
}

public struct ClaudeprocPreview: Codable, Sendable {
    public var processes: [ClaudeprocTarget]?
    public var unverified: [ClaudeprocTarget]?
    public var willKill: Bool
    public var notice: String?
    public var browsers: [ClaudeprocTarget]?
    public var browserNote: String?
    enum CodingKeys: String, CodingKey {
        case processes = "processes"
        case unverified = "unverified"
        case willKill = "will_kill"
        case notice = "notice"
        case browsers = "browsers"
        case browserNote = "browser_note"
    }
}

public struct ClaudeprocTarget: Codable, Sendable {
    public var pid: Int64
    public var app: String
    public var cmd: String
    public var note: String?
    enum CodingKeys: String, CodingKey {
        case pid = "pid"
        case app = "app"
        case cmd = "cmd"
        case note = "note"
    }
}

public struct DaemonAddSourceRequest: Codable, Sendable {
    public var kind: String
    public var name: String
    public var url: String?
    public var content: String?
    public var node: [String: JSONValue]?
    public var viaHost: Bool?
    enum CodingKeys: String, CodingKey {
        case kind = "kind"
        case name = "name"
        case url = "url"
        case content = "content"
        case node = "node"
        case viaHost = "via_host"
    }
}

public struct DaemonAppKindView: Codable, Sendable {
    public var app: String
    public var kind: String
    public var shared: String?
    enum CodingKeys: String, CodingKey {
        case app = "app"
        case kind = "kind"
        case shared = "shared"
    }
}

public struct DaemonAppView: Codable, Sendable {
    public var app: String
    public var name: String
    public var conns: Int64
    public var exits: [String]?
    public var reason: String
    public var kind: String?
    public var shared: String?
    enum CodingKeys: String, CodingKey {
        case app = "app"
        case name = "name"
        case conns = "conns"
        case exits = "exits"
        case reason = "reason"
        case kind = "kind"
        case shared = "shared"
    }
}

public struct DaemonAppsView: Codable, Sendable {
    public var apps: [DaemonAppView]?
    public var truncated: Bool?
    public var fallbacks: [DaemonRuleFallback]?
    public var ruleKinds: [DaemonAppKindView]?
    enum CodingKeys: String, CodingKey {
        case apps = "apps"
        case truncated = "truncated"
        case fallbacks = "fallbacks"
        case ruleKinds = "rule_kinds"
    }
}

public struct DaemonBaselineSummary: Codable, Sendable {
    public var rule: String
    public var country: String
    public var timezone: String?
    public var borrowed: Bool?
    enum CodingKeys: String, CodingKey {
        case rule = "rule"
        case country = "country"
        case timezone = "timezone"
        case borrowed = "borrowed"
    }
}

public struct DaemonBorrowView: Codable, Sendable {
    public var op: String
    public var step: String
    public var since: String
    enum CodingKeys: String, CodingKey {
        case op = "op"
        case step = "step"
        case since = "since"
    }
}

public struct DaemonClaudeDomainsView: Codable, Sendable {
    public var count: Int64
    public var domains: [ClaudedomainsEntry]?
    public var note: String
    enum CodingKeys: String, CodingKey {
        case count = "count"
        case domains = "domains"
        case note = "note"
    }
}

public struct DaemonClaudeProxyChange: Codable, Sendable {
    public var target: Bool
    public var ok: Bool
    public var notes: [String]?
    public var conflicts: [ClaudecfgConflict]?
    enum CodingKeys: String, CodingKey {
        case target = "target"
        case ok = "ok"
        case notes = "notes"
        case conflicts = "conflicts"
    }
}

public struct DaemonClaudeProxyConn: Codable, Sendable {
    public var at: String
    public var app: String
    public var claude: Bool
    public var host: String?
    public var action: String
    public var exit: String?
    public var ok: Bool
    enum CodingKeys: String, CodingKey {
        case at = "at"
        case app = "app"
        case claude = "claude"
        case host = "host"
        case action = "action"
        case exit = "exit"
        case ok = "ok"
    }
}

public struct DaemonClaudeProxySetting: Codable, Sendable {
    public var enabled: Bool
    enum CodingKeys: String, CodingKey {
        case enabled = "enabled"
    }
}

public struct DaemonClaudeProxyStatus: Codable, Sendable {
    public var enabled: Bool
    public var testMode: Bool?
    public var leftover: Bool?
    public var settings: ClaudecfgStatus?
    public var listening: Bool
    public var port: Int64?
    public var listenError: String?
    public var effective: Bool
    public var accepted: Int64?
    public var refused: Int64?
    public var recent: [DaemonClaudeProxyConn]?
    enum CodingKeys: String, CodingKey {
        case enabled = "enabled"
        case testMode = "test_mode"
        case leftover = "leftover"
        case settings = "settings"
        case listening = "listening"
        case port = "port"
        case listenError = "listen_error"
        case effective = "effective"
        case accepted = "accepted"
        case refused = "refused"
        case recent = "recent"
    }
}

public struct DaemonClaudeProxyView: Codable, Sendable {
    public var available: Bool
    public var enabled: Bool
    public var testMode: Bool?
    public var leftover: Bool?
    public var note: String?
    public var notes: [String]?
    public var status: DaemonClaudeProxyStatus?
    enum CodingKeys: String, CodingKey {
        case available = "available"
        case enabled = "enabled"
        case testMode = "test_mode"
        case leftover = "leftover"
        case note = "note"
        case notes = "notes"
        case status = "status"
    }
}

public struct DaemonConfirmNeeded: Codable, Sendable {
    public var error: String
    public var code: String
    public var confirm: String
    public var expiresInS: Int64
    public var preview: ClaudeprocPreview
    public var region: DaemonRegionChange?
    public var returnBlock: Bool?
    public var claudeProxy: DaemonRestoreClaudeProxy?
    enum CodingKeys: String, CodingKey {
        case error = "error"
        case code = "code"
        case confirm = "confirm"
        case expiresInS = "expires_in_s"
        case preview = "preview"
        case region = "region"
        case returnBlock = "return_block"
        case claudeProxy = "claude_proxy"
    }
}

public struct DaemonConfirmRequest: Codable, Sendable {
    public var confirm: String?
    public var killBrowsers: Bool?
    enum CodingKeys: String, CodingKey {
        case confirm = "confirm"
        case killBrowsers = "kill_browsers"
    }
}

public struct DaemonDiagnosticDomains: Codable, Sendable {
    public var count: Int64
    public var domains: [ClaudedomainsEntry]?
    enum CodingKeys: String, CodingKey {
        case count = "count"
        case domains = "domains"
    }
}

public struct DaemonDiagnosticVersion: Codable, Sendable {
    public var module: String
    public var version: String
    public var revision: String?
    public var go: String
    public var os: String
    enum CodingKeys: String, CodingKey {
        case module = "module"
        case version = "version"
        case revision = "revision"
        case go = "go"
        case os = "os"
    }
}

public struct DaemonDiagnostics: Codable, Sendable {
    public var generatedAt: String
    public var version: DaemonDiagnosticVersion
    public var status: DaemonStatus
    public var envcheck: EnvcheckReport?
    public var launchguard: DaemonLaunchGuardView
    public var claudeDomains: DaemonDiagnosticDomains
    public var envcheckHistory: [EnvcheckRunEntry]?
    public var enableFailureSnapshot: DaemonEnableSnapshot?
    enum CodingKeys: String, CodingKey {
        case generatedAt = "generated_at"
        case version = "version"
        case status = "status"
        case envcheck = "envcheck"
        case launchguard = "launchguard"
        case claudeDomains = "claude_domains"
        case envcheckHistory = "envcheck_history"
        case enableFailureSnapshot = "enable_failure_snapshot"
    }
}

public struct DaemonDirectStatus: Codable, Sendable {
    public var attached: Bool
    public var configured: Bool
    enum CodingKeys: String, CodingKey {
        case attached = "attached"
        case configured = "configured"
    }
}

public struct DaemonEnablePreview: Codable, Sendable {
    public var canEnable: Bool
    public var blocker: String?
    public var reason: String?
    public var enabled: Bool?
    public var region: DaemonRegionChange?
    public var preview: ClaudeprocPreview?
    public var note: String
    enum CodingKeys: String, CodingKey {
        case canEnable = "can_enable"
        case blocker = "blocker"
        case reason = "reason"
        case enabled = "enabled"
        case region = "region"
        case preview = "preview"
        case note = "note"
    }
}

public struct DaemonEnableSnapshot: Codable, Sendable {
    public var time: String
    public var pf: PfCounters?
    public var ourUtun: String
    public var firstHopKind: String
    public var firstHopRouteIface: String?
    public var halfRouteIfaces: [String: String]?
    public var foreignTunnels: [String]?
    public var dialBoundIface: String?
    public var vmNetstats: [String: JSONValue]?
    public var errors: [String]?
    enum CodingKeys: String, CodingKey {
        case time = "time"
        case pf = "pf"
        case ourUtun = "our_utun"
        case firstHopKind = "first_hop_kind"
        case firstHopRouteIface = "first_hop_route_iface"
        case halfRouteIfaces = "half_route_ifaces"
        case foreignTunnels = "foreign_tunnels"
        case dialBoundIface = "dial_bound_iface"
        case vmNetstats = "vm_netstats"
        case errors = "errors"
    }
}

public struct DaemonEngineView: Codable, Sendable {
    public var name: String
    public var state: String
    public var restarts: Int64
    public var since: String?
    public var lastError: String?
    enum CodingKeys: String, CodingKey {
        case name = "name"
        case state = "state"
        case restarts = "restarts"
        case since = "since"
        case lastError = "last_error"
    }
}

public struct DaemonEnginesView: Codable, Sendable {
    public var reachable: Bool
    public var version: String?
    public var kernel: String?
    public var uptimeS: Int64?
    public var bootChanged: Bool
    public var engines: [DaemonEngineView]?
    public var vmManager: VmmgrStatus?
    enum CodingKeys: String, CodingKey {
        case reachable = "reachable"
        case version = "version"
        case kernel = "kernel"
        case uptimeS = "uptime_s"
        case bootChanged = "boot_changed"
        case engines = "engines"
        case vmManager = "vm_manager"
    }
}

public struct DaemonEnvHistoryView: Codable, Sendable {
    public var entries: [EnvcheckRunEntry]?
    enum CodingKeys: String, CodingKey {
        case entries = "entries"
    }
}

public struct DaemonEnvSettingsRequest: Codable, Sendable {
    public var disabled: [String]?
    enum CodingKeys: String, CodingKey {
        case disabled = "disabled"
    }
}

public struct DaemonEventRecord: Codable, Sendable {
    public var source: String
    public var kind: String
    public var time: String
    public var detail: String?
    enum CodingKeys: String, CodingKey {
        case source = "source"
        case kind = "kind"
        case time = "time"
        case detail = "detail"
    }
}

public struct DaemonExitCheckResult: Codable, Sendable {
    public var tag: String
    public var cached: Bool
    public var status: DaemonExitStatus
    enum CodingKeys: String, CodingKey {
        case tag = "tag"
        case cached = "cached"
        case status = "status"
    }
}

public struct DaemonExitCounts: Codable, Sendable {
    public var tcp: Int64
    public var udp: Int64
    public var failed: Int64
    enum CodingKeys: String, CodingKey {
        case tcp = "tcp"
        case udp = "udp"
        case failed = "failed"
    }
}

public struct DaemonExitStatus: Codable, Sendable {
    public var tag: String
    public var kind: String?
    public var avail: GatesmExitView
    public var affectedCount: Int64?
    enum CodingKeys: String, CodingKey {
        case tag = "tag"
        case kind = "kind"
        case avail = "avail"
        case affectedCount = "affected_count"
    }
}

public struct DaemonHint: Codable, Sendable {
    public var id: String
    public var title: String
    public var fix: String
    enum CodingKeys: String, CodingKey {
        case id = "id"
        case title = "title"
        case fix = "fix"
    }
}

public struct DaemonJob: Codable, Sendable {
    public var id: String
    public var op: String
    public var state: String
    public var error: String?
    public var started: String
    public var finished: String?
    enum CodingKeys: String, CodingKey {
        case id = "id"
        case op = "op"
        case state = "state"
        case error = "error"
        case started = "started"
        case finished = "finished"
    }
}

public struct DaemonLANConfirmRequest: Codable, Sendable {
    public var node: String
    enum CodingKeys: String, CodingKey {
        case node = "node"
    }
}

public struct DaemonLANSetting: Codable, Sendable {
    public var allowLan: Bool
    enum CodingKeys: String, CodingKey {
        case allowLan = "allow_lan"
    }
}

public struct DaemonLaunchGuardLayer: Codable, Sendable {
    public var layer: Int64
    public var name: String
    public var effective: Bool
    public var note: String?
    enum CodingKeys: String, CodingKey {
        case layer = "layer"
        case name = "name"
        case effective = "effective"
        case note = "note"
    }
}

public struct DaemonLaunchGuardView: Codable, Sendable {
    public var launchControl: Bool?
    public var allowed: Bool
    public var since: String?
    public var coverAll: Bool?
    public var layers: [DaemonLaunchGuardLayer]?
    public var coverage: [ClaudeprocCoverageRow]?
    public var recent: [ClaudeprocGuardRecord]?
    enum CodingKeys: String, CodingKey {
        case launchControl = "launch_control"
        case allowed = "allowed"
        case since = "since"
        case coverAll = "cover_all"
        case layers = "layers"
        case coverage = "coverage"
        case recent = "recent"
    }
}

public struct DaemonMainExitView: Codable, Sendable {
    public var available: Bool
    public var ip: String?
    public var country: String?
    public var region: String?
    public var city: String?
    public var timezone: String?
    public var at: String?
    public var note: String?
    enum CodingKeys: String, CodingKey {
        case available = "available"
        case ip = "ip"
        case country = "country"
        case region = "region"
        case city = "city"
        case timezone = "timezone"
        case at = "at"
        case note = "note"
    }
}

public struct DaemonMainProxyNodeView: Codable, Sendable {
    public var id: String
    public var name: String
    public var type: String
    public var udp: String?
    enum CodingKeys: String, CodingKey {
        case id = "id"
        case name = "name"
        case type = "type"
        case udp = "udp"
    }
}

public struct DaemonMainProxyRequest: Codable, Sendable {
    public var confirm: String?
    public var rule: String?
    enum CodingKeys: String, CodingKey {
        case confirm = "confirm"
        case rule = "rule"
    }
}

public struct DaemonMainProxyResult: Codable, Sendable {
    public var baseline: DaemonBaselineSummary
    public var region: RegionTarget?
    public var synced: Bool
    public var note: String?
    public var borrowed: Bool?
    enum CodingKeys: String, CodingKey {
        case baseline = "baseline"
        case region = "region"
        case synced = "synced"
        case note = "note"
        case borrowed = "borrowed"
    }
}

public struct DaemonMainProxySourceView: Codable, Sendable {
    public var id: String
    public var name: String
    public var kind: String
    public var updated: String
    public var error: String?
    public var skipped: Int64?
    public var nodes: [DaemonMainProxyNodeView]?
    enum CodingKeys: String, CodingKey {
        case id = "id"
        case name = "name"
        case kind = "kind"
        case updated = "updated"
        case error = "error"
        case skipped = "skipped"
        case nodes = "nodes"
    }
}

public struct DaemonMainProxyView: Codable, Sendable {
    public var selected: DaemonMainProxyNodeView?
    public var chain: [DaemonMainProxyNodeView]?
    public var applied: Bool
    public var needsReselect: Bool
    public var lanPending: Bool
    public var sources: Int64
    public var note: String?
    enum CodingKeys: String, CodingKey {
        case selected = "selected"
        case chain = "chain"
        case applied = "applied"
        case needsReselect = "needs_reselect"
        case lanPending = "lan_pending"
        case sources = "sources"
        case note = "note"
    }
}

public struct DaemonMainSelectRequest: Codable, Sendable {
    public var selected: String
    public var chain: [String]?
    public var rule: String?
    public var confirm: String?
    enum CodingKeys: String, CodingKey {
        case selected = "selected"
        case chain = "chain"
        case rule = "rule"
        case confirm = "confirm"
    }
}

public struct DaemonPreflightItem: Codable, Sendable {
    public var id: String
    public var title: String
    public var fix: String
    public var undecidable: Bool?
    enum CodingKeys: String, CodingKey {
        case id = "id"
        case title = "title"
        case fix = "fix"
        case undecidable = "undecidable"
    }
}

public struct DaemonPreflightView: Codable, Sendable {
    public var checkedAt: String
    public var cached: Bool
    public var blocking: [DaemonPreflightItem]?
    public var warnings: [DaemonPreflightItem]?
    enum CodingKeys: String, CodingKey {
        case checkedAt = "checked_at"
        case cached = "cached"
        case blocking = "blocking"
        case warnings = "warnings"
    }
}

public struct DaemonPrepareStatus: Codable, Sendable {
    public var done: Bool
    public var notes: [String]?
    enum CodingKeys: String, CodingKey {
        case done = "done"
        case notes = "notes"
    }
}

public struct DaemonRefreshRequest: Codable, Sendable {
    public var viaHost: Bool?
    enum CodingKeys: String, CodingKey {
        case viaHost = "via_host"
    }
}

public struct DaemonRegionChange: Codable, Sendable {
    public var from: String
    public var to: String
    enum CodingKeys: String, CodingKey {
        case from = "from"
        case to = "to"
    }
}

public struct DaemonRegionRequest: Codable, Sendable {
    public var mode: String
    public var timezone: String?
    public var lang: String?
    public var locale: String?
    public var confirm: String?
    enum CodingKeys: String, CodingKey {
        case mode = "mode"
        case timezone = "timezone"
        case lang = "lang"
        case locale = "locale"
        case confirm = "confirm"
    }
}

public struct DaemonRegionStatus: Codable, Sendable {
    public var target: RegionTarget?
    public var error: String?
    public var pending: [String]?
    public var notice: String?
    enum CodingKeys: String, CodingKey {
        case target = "target"
        case error = "error"
        case pending = "pending"
        case notice = "notice"
    }
}

public struct DaemonRestoreClaudeProxy: Codable, Sendable {
    public var current: Bool
    public var target: Bool
    public var testMode: Bool
    public var conflicts: [ClaudecfgConflict]?
    enum CodingKeys: String, CodingKey {
        case current = "current"
        case target = "target"
        case testMode = "test_mode"
        case conflicts = "conflicts"
    }
}

public struct DaemonRestorePreviewView: Codable, Sendable {
    public var processes: [ClaudeprocTarget]?
    public var unverified: [ClaudeprocTarget]?
    public var willKill: Bool
    public var notice: String?
    public var browsers: [ClaudeprocTarget]?
    public var browserNote: String?
    public var claudeProxy: DaemonRestoreClaudeProxy?
    public var note: String?
    enum CodingKeys: String, CodingKey {
        case processes = "processes"
        case unverified = "unverified"
        case willKill = "will_kill"
        case notice = "notice"
        case browsers = "browsers"
        case browserNote = "browser_note"
        case claudeProxy = "claude_proxy"
        case note = "note"
    }
}

public struct DaemonRestoreRequest: Codable, Sendable {
    public var confirm: String?
    public var killBrowsers: Bool?
    public var claudeProxy: Bool?
    enum CodingKeys: String, CodingKey {
        case confirm = "confirm"
        case killBrowsers = "kill_browsers"
        case claudeProxy = "claude_proxy"
    }
}

public struct DaemonReturnBlockStatus: Codable, Sendable {
    public var on: Bool
    public var active: Bool
    public var nets: [String]?
    public var test: Bool?
    public var note: String
    enum CodingKeys: String, CodingKey {
        case on = "on"
        case active = "active"
        case nets = "nets"
        case test = "test"
        case note = "note"
    }
}

public struct DaemonRuleFallback: Codable, Sendable {
    public var app: String
    public var exit: String
    public var reason: String
    enum CodingKeys: String, CodingKey {
        case app = "app"
        case exit = "exit"
        case reason = "reason"
    }
}

public struct DaemonRulesSummary: Codable, Sendable {
    public var `default`: String
    public var direct: Int64
    public var proxy: Int64
    public var block: Int64
    public var offlineAllow: Int64
    public var localDomains: Int64
    public var pinnedExtra: Int64
    public var blockUdp443: Bool
    enum CodingKeys: String, CodingKey {
        case `default` = "default"
        case direct = "direct"
        case proxy = "proxy"
        case block = "block"
        case offlineAllow = "offline_allow"
        case localDomains = "local_domains"
        case pinnedExtra = "pinned_extra"
        case blockUdp443 = "block_udp_443"
    }
}

public struct DaemonSetupStep: Codable, Sendable {
    public var id: String
    public var done: Bool
    enum CodingKeys: String, CodingKey {
        case id = "id"
        case done = "done"
    }
}

public struct DaemonSetupView: Codable, Sendable {
    public var completed: Bool
    public var completedAt: String?
    public var steps: [DaemonSetupStep]?
    enum CodingKeys: String, CodingKey {
        case completed = "completed"
        case completedAt = "completed_at"
        case steps = "steps"
    }
}

public struct DaemonSourceAdded: Codable, Sendable {
    public var id: String
    public var needsReselect: Bool?
    public var sources: [DaemonMainProxySourceView]?
    enum CodingKeys: String, CodingKey {
        case id = "id"
        case needsReselect = "needs_reselect"
        case sources = "sources"
    }
}

public struct DaemonStatus: Codable, Sendable {
    public var state: String
    public var corrupt: Bool?
    public var gateOpen: Bool
    public var protected: Bool
    public var utun: String?
    public var events: [DaemonEventRecord]?
    public var hints: [DaemonHint]?
    public var rules: DaemonRulesSummary?
    public var stack: StackProxyStats?
    public var direct: DaemonDirectStatus?
    public var gate: GatesmStatus
    public var vmManager: VmmgrStatus?
    public var borrow: DaemonBorrowView?
    public var resolverLock: ResolverlockStatus?
    public var claudeCode: DaemonClaudeProxyStatus?
    public var integrity: IntegrityResult?
    public var browsersLeft: [String]?
    public var claudeProxyChange: DaemonClaudeProxyChange?
    public var launchControl: Bool?
    public var prepare: DaemonPrepareStatus?
    public var returnBlock: DaemonReturnBlockStatus?
    public var allowLan: Bool?
    public var mainUdp: String?
    public var builtinPinned: [String]?
    public var exits: [DaemonExitStatus]?
    public var opens: [String: DaemonExitCounts]?
    public var region: DaemonRegionStatus?
    public var envcheck: EnvcheckReport?
    public var security: ApiSecurity?
    public var build: BuildinfoInfo?
    public var stateFormat: Int64?
    public var busy: Bool?
    public var starting: Bool?
    public var uninstall: DaemonUninstallFlags?
    enum CodingKeys: String, CodingKey {
        case state = "state"
        case corrupt = "corrupt"
        case gateOpen = "gate_open"
        case protected = "protected"
        case utun = "utun"
        case events = "events"
        case hints = "hints"
        case rules = "rules"
        case stack = "stack"
        case direct = "direct"
        case gate = "gate"
        case vmManager = "vm_manager"
        case borrow = "borrow"
        case resolverLock = "resolver_lock"
        case claudeCode = "claude_code"
        case integrity = "integrity"
        case browsersLeft = "browsers_left"
        case claudeProxyChange = "claude_proxy_change"
        case launchControl = "launch_control"
        case prepare = "prepare"
        case returnBlock = "return_block"
        case allowLan = "allow_lan"
        case mainUdp = "main_udp"
        case builtinPinned = "builtin_pinned"
        case exits = "exits"
        case opens = "opens"
        case region = "region"
        case envcheck = "envcheck"
        case security = "security"
        case build = "build"
        case stateFormat = "state_format"
        case busy = "busy"
        case starting = "starting"
        case uninstall = "uninstall"
    }
}

public struct DaemonStreamEvent: Codable, Sendable {
    public var seq: Int64
    public var source: String
    public var kind: String
    public var time: String
    public var detail: String?
    public var dropped: Bool?
    enum CodingKeys: String, CodingKey {
        case seq = "seq"
        case source = "source"
        case kind = "kind"
        case time = "time"
        case detail = "detail"
        case dropped = "dropped"
    }
}

public struct DaemonUninstallFlags: Codable, Sendable {
    public var running: Bool
    public var done: Bool
    enum CodingKeys: String, CodingKey {
        case running = "running"
        case done = "done"
    }
}

public struct DaemonUninstallPreviewView: Codable, Sendable {
    public var processes: [ClaudeprocTarget]?
    public var unverified: [ClaudeprocTarget]?
    public var willKill: Bool
    public var notice: String?
    public var browsers: [ClaudeprocTarget]?
    public var browserNote: String?
    public var removes: [String]?
    public var note: String
    enum CodingKeys: String, CodingKey {
        case processes = "processes"
        case unverified = "unverified"
        case willKill = "will_kill"
        case notice = "notice"
        case browsers = "browsers"
        case browserNote = "browser_note"
        case removes = "removes"
        case note = "note"
    }
}

public struct DaemonUninstallResult: Codable, Sendable {
    public var steps: [DaemonUninstallStep]?
    public var complete: Bool
    public var browsersLeft: [String]?
    enum CodingKeys: String, CodingKey {
        case steps = "steps"
        case complete = "complete"
        case browsersLeft = "browsers_left"
    }
}

public struct DaemonUninstallStep: Codable, Sendable {
    public var name: String
    public var done: Bool
    public var note: String?
    enum CodingKeys: String, CodingKey {
        case name = "name"
        case done = "done"
        case note = "note"
    }
}

public struct EnvcheckDeviceFinding: Codable, Sendable {
    public var font: String
    public var file: String
    public var location: String
    enum CodingKeys: String, CodingKey {
        case font = "font"
        case file = "file"
        case location = "location"
    }
}

public struct EnvcheckDeviceReport: Codable, Sendable {
    public var findings: [EnvcheckDeviceFinding]?
    public var note: String
    enum CodingKeys: String, CodingKey {
        case findings = "findings"
        case note = "note"
    }
}

public struct EnvcheckItemResult: Codable, Sendable {
    public var id: String
    public var name: String
    public var level: String
    public var result: String
    public var detail: String?
    public var fix: String?
    enum CodingKeys: String, CodingKey {
        case id = "id"
        case name = "name"
        case level = "level"
        case result = "result"
        case detail = "detail"
        case fix = "fix"
    }
}

public struct EnvcheckReport: Codable, Sendable {
    public var at: String
    public var items: [EnvcheckItemResult]?
    public var hard: String
    enum CodingKeys: String, CodingKey {
        case at = "at"
        case items = "items"
        case hard = "hard"
    }
}

public struct EnvcheckRunEntry: Codable, Sendable {
    public var at: String
    public var lastSeen: String
    public var hard: String
    public var items: [EnvcheckRunItem]?
    enum CodingKeys: String, CodingKey {
        case at = "at"
        case lastSeen = "last_seen"
        case hard = "hard"
        case items = "items"
    }
}

public struct EnvcheckRunItem: Codable, Sendable {
    public var id: String
    public var result: String
    enum CodingKeys: String, CodingKey {
        case id = "id"
        case result = "result"
    }
}

public struct GatesmExitView: Codable, Sendable {
    public var up: Bool
    public var reason: String?
    public var dev: Bool?
    public var pushed: Bool
    public var agentUp: Bool
    public var check: String?
    public var checkedAt: String?
    public var udp: String?
    public var items: [String: String]?
    enum CodingKeys: String, CodingKey {
        case up = "up"
        case reason = "reason"
        case dev = "dev"
        case pushed = "pushed"
        case agentUp = "agent_up"
        case check = "check"
        case checkedAt = "checked_at"
        case udp = "udp"
        case items = "items"
    }
}

public struct GatesmStatus: Codable, Sendable {
    public var open: Bool
    public var reasons: [String]?
    public var leaseLeftMs: Int64
    enum CodingKeys: String, CodingKey {
        case open = "open"
        case reasons = "reasons"
        case leaseLeftMs = "lease_left_ms"
    }
}

public struct IntegrityProblem: Codable, Sendable {
    public var file: String
    public var kind: String
    enum CodingKeys: String, CodingKey {
        case file = "file"
        case kind = "kind"
    }
}

public struct IntegrityResult: Codable, Sendable {
    public var checked: Bool
    public var ok: Bool
    public var note: String?
    public var problems: [IntegrityProblem]?
    enum CodingKeys: String, CodingKey {
        case checked = "checked"
        case ok = "ok"
        case note = "note"
        case problems = "problems"
    }
}

public struct PfCounters: Codable, Sendable {
    public var enabled: Bool
    public var referenced: Bool
    public var rules: [PfRuleCounter]?
    enum CodingKeys: String, CodingKey {
        case enabled = "enabled"
        case referenced = "referenced"
        case rules = "rules"
    }
}

public struct PfRuleCounter: Codable, Sendable {
    public var rule: String
    public var evaluations: Int64
    public var packets: Int64
    public var bytes: Int64
    public var states: Int64
    enum CodingKeys: String, CodingKey {
        case rule = "rule"
        case evaluations = "evaluations"
        case packets = "packets"
        case bytes = "bytes"
        case states = "states"
    }
}

public struct RegionTarget: Codable, Sendable {
    public var timezone: String
    public var lang: String
    public var locale: String
    public var country: String?
    public var source: String
    enum CodingKeys: String, CodingKey {
        case timezone = "timezone"
        case lang = "lang"
        case locale = "locale"
        case country = "country"
        case source = "source"
    }
}

public struct ResolverlockProblem: Codable, Sendable {
    public var domain: String
    public var kind: String
    enum CodingKeys: String, CodingKey {
        case domain = "domain"
        case kind = "kind"
    }
}

public struct ResolverlockStatus: Codable, Sendable {
    public var written: Bool
    public var domains: [String]?
    public var form: String
    public var effective: Bool
    public var problems: [ResolverlockProblem]?
    public var conflicts: [String]?
    enum CodingKeys: String, CodingKey {
        case written = "written"
        case domains = "domains"
        case form = "form"
        case effective = "effective"
        case problems = "problems"
        case conflicts = "conflicts"
    }
}

public struct RulesFile: Codable, Sendable {
    public var `default`: String
    public var rules: [RulesFileRule]?
    public var offlineAllow: [String]?
    public var localDomains: [String]?
    public var pinnedExtra: [String]?
    public var blockUdp443: Bool
    public var bypassExtra: [String]?
    enum CodingKeys: String, CodingKey {
        case `default` = "default"
        case rules = "rules"
        case offlineAllow = "offline_allow"
        case localDomains = "local_domains"
        case pinnedExtra = "pinned_extra"
        case blockUdp443 = "block_udp_443"
        case bypassExtra = "bypass_extra"
    }
}

public struct RulesFileRule: Codable, Sendable {
    public var app: String
    public var action: String
    enum CodingKeys: String, CodingKey {
        case app = "app"
        case action = "action"
    }
}

public struct StackProxyStats: Codable, Sendable {
    public var noMapping: Int64
    public var badDst: Int64
    public var gateClosed: Int64
    public var unknownExit: Int64
    public var exitDown: Int64
    public var dialFailed: Int64
    public var udpUnsupported: Int64
    public var openFailedOther: Int64
    public var zeroEof: Int64
    public var dnsTcpRejected: Int64
    public var dnsUdpDropped: Int64
    public var tcpProxied: Int64
    public var probeProxied: Int64
    public var probeRefused: Int64
    public var udpProxied: Int64
    public var dropped: Int64
    public var directOk: Int64
    public var directFail: [String: Int64]?
    public var directDropped: Int64
    public var reasons: [String: Int64]?
    public var sniff: [String: Int64]?
    public var udpPendingDropped: Int64
    public var recheckedDropped: Int64
    enum CodingKeys: String, CodingKey {
        case noMapping = "no_mapping"
        case badDst = "bad_dst"
        case gateClosed = "gate_closed"
        case unknownExit = "unknown_exit"
        case exitDown = "exit_down"
        case dialFailed = "dial_failed"
        case udpUnsupported = "udp_unsupported"
        case openFailedOther = "open_failed_other"
        case zeroEof = "zero_eof"
        case dnsTcpRejected = "dns_tcp_rejected"
        case dnsUdpDropped = "dns_udp_dropped"
        case tcpProxied = "tcp_proxied"
        case probeProxied = "probe_proxied"
        case probeRefused = "probe_refused"
        case udpProxied = "udp_proxied"
        case dropped = "dropped"
        case directOk = "direct_ok"
        case directFail = "direct_fail"
        case directDropped = "direct_dropped"
        case reasons = "reasons"
        case sniff = "sniff"
        case udpPendingDropped = "udp_pending_dropped"
        case recheckedDropped = "rechecked_dropped"
    }
}

public struct VmmgrStatus: Codable, Sendable {
    public var managed: Bool
    public var phase: String
    public var restarts: Int64
    public var failures: Int64
    public var lastError: String?
    public var borrowed: Bool?
    enum CodingKeys: String, CodingKey {
        case managed = "managed"
        case phase = "phase"
        case restarts = "restarts"
        case failures = "failures"
        case lastError = "last_error"
        case borrowed = "borrowed"
    }
}

