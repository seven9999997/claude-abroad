// 由 scripts/m5/probe/apigen 生成，不要手改（M5-1 探查 E5 的原型）。

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

public struct DaemonDirectStatus: Codable, Sendable {
    public var attached: Bool
    public var configured: Bool
    enum CodingKeys: String, CodingKey {
        case attached = "attached"
        case configured = "configured"
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

public struct ProbeProbeEvent: Codable, Sendable {
    public var seq: Int64
    public var kind: String
    public var state: String?
    public var at: String
    enum CodingKeys: String, CodingKey {
        case seq = "seq"
        case kind = "kind"
        case state = "state"
        case at = "at"
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

