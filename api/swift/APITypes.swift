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

public struct ApiErrorBody: Codable, Sendable {
    public var error: String
    enum CodingKeys: String, CodingKey {
        case error = "error"
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

public struct ClaudeprocPreview: Codable, Sendable {
    public var processes: [ClaudeprocTarget]?
    public var unverified: [ClaudeprocTarget]?
    public var willKill: Bool
    public var notice: String?
    enum CodingKeys: String, CodingKey {
        case processes = "processes"
        case unverified = "unverified"
        case willKill = "will_kill"
        case notice = "notice"
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

public struct DaemonAppView: Codable, Sendable {
    public var app: String
    public var name: String
    public var conns: Int64
    public var exits: [String]?
    public var reason: String
    enum CodingKeys: String, CodingKey {
        case app = "app"
        case name = "name"
        case conns = "conns"
        case exits = "exits"
        case reason = "reason"
    }
}

public struct DaemonAppsView: Codable, Sendable {
    public var apps: [DaemonAppView]?
    public var truncated: Bool?
    enum CodingKeys: String, CodingKey {
        case apps = "apps"
        case truncated = "truncated"
    }
}

public struct DaemonBaselineSummary: Codable, Sendable {
    public var rule: String
    public var country: String
    public var timezone: String?
    enum CodingKeys: String, CodingKey {
        case rule = "rule"
        case country = "country"
        case timezone = "timezone"
    }
}

public struct DaemonConfirmNeeded: Codable, Sendable {
    public var error: String
    public var confirm: String
    public var expiresInS: Int64
    public var preview: ClaudeprocPreview
    public var region: DaemonRegionChange?
    enum CodingKeys: String, CodingKey {
        case error = "error"
        case confirm = "confirm"
        case expiresInS = "expires_in_s"
        case preview = "preview"
        case region = "region"
    }
}

public struct DaemonConfirmRequest: Codable, Sendable {
    public var confirm: String?
    enum CodingKeys: String, CodingKey {
        case confirm = "confirm"
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
    enum CodingKeys: String, CodingKey {
        case reachable = "reachable"
        case version = "version"
        case kernel = "kernel"
        case uptimeS = "uptime_s"
        case bootChanged = "boot_changed"
        case engines = "engines"
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
    enum CodingKeys: String, CodingKey {
        case tag = "tag"
        case kind = "kind"
        case avail = "avail"
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
    enum CodingKeys: String, CodingKey {
        case id = "id"
        case name = "name"
        case type = "type"
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
    enum CodingKeys: String, CodingKey {
        case baseline = "baseline"
        case region = "region"
        case synced = "synced"
        case note = "note"
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
    public var exits: [DaemonExitStatus]?
    public var opens: [String: DaemonExitCounts]?
    public var region: DaemonRegionStatus?
    public var envcheck: EnvcheckReport?
    public var security: ApiSecurity?
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
        case exits = "exits"
        case opens = "opens"
        case region = "region"
        case envcheck = "envcheck"
        case security = "security"
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

public struct DaemonUninstallSkeleton: Codable, Sendable {
    public var error: String
    enum CodingKeys: String, CodingKey {
        case error = "error"
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

