// 其余页面的接口（M5-9）：分流出口、在联网的应用与规则、环境检测、地区、引擎与虚拟机。
import AbroadAPI
import Foundation

/// GET /v1/exits 的一项（摘要和可用状态；多出的字段忽略）。
public struct ExitItem: Decodable, Sendable, Equatable {
    public var tag: String
    public var kind: String?
    public var avail: Avail
    /// 补齐二：指向这个出口的规则（应用 ID）。
    public var affected: [String]?

    public struct Avail: Decodable, Sendable, Equatable {
        public var up: Bool
        public var reason: String?
        public var check: String?
        public var udp: String?
        public var items: [String: String]?
    }
}

struct ExitsList: Decodable, Sendable { var exits: [ExitItem] }

/// 规则文件（GET /v1/rules?format=json；字段与 YAML 一一对应，JSON 原样 PUT 回去）。
public struct RulesDoc: Codable, Sendable, Equatable {
    public var defaultAction: String
    public var rules: [Rule]
    public var offlineAllow: [String]
    public var localDomains: [String]
    public var pinnedExtra: [String]
    public var blockUDP443: Bool
    /// 补齐三：用户追加的不分配假地址的域名。
    public var bypassExtra: [String] = []

    public struct Rule: Codable, Sendable, Equatable {
        public var app: String
        public var action: String
        public init(app: String, action: String) { self.app = app; self.action = action }
    }

    enum CodingKeys: String, CodingKey {
        case defaultAction = "default", rules
        case offlineAllow = "offline_allow", localDomains = "local_domains", pinnedExtra = "pinned_extra", blockUDP443 = "block_udp_443"
        case bypassExtra = "bypass_extra"
    }

    public init(from d: Decoder) throws {
        let c = try d.container(keyedBy: CodingKeys.self)
        defaultAction = try c.decode(String.self, forKey: .defaultAction)
        rules = try c.decodeIfPresent([Rule].self, forKey: .rules) ?? []
        offlineAllow = try c.decodeIfPresent([String].self, forKey: .offlineAllow) ?? []
        localDomains = try c.decodeIfPresent([String].self, forKey: .localDomains) ?? []
        pinnedExtra = try c.decodeIfPresent([String].self, forKey: .pinnedExtra) ?? []
        blockUDP443 = try c.decodeIfPresent(Bool.self, forKey: .blockUDP443) ?? false
        bypassExtra = try c.decodeIfPresent([String].self, forKey: .bypassExtra) ?? []
    }
}

/// 规则和读取时的标识。
public struct TaggedRules: Sendable, Equatable {
    public var doc: RulesDoc
    public var tag: String
}

/// GET /v1/envcheck 的设置（关掉的警告项）。
public struct EnvSettings: Codable, Sendable, Equatable {
    public var disabled: [String]
}

extension APIClient {
    // 分流出口
    public func exits() async -> Outcome<[ExitItem]> {
        let o: Outcome<ExitsList> = await get("/v1/exits")
        if case .ok(let l) = o { return .ok(l.exits) }
        return o.mapError()
    }
    public func putExit(tag: String, kind: String, conf: String) async -> Outcome<Empty> {
        struct B: Encodable { var kind: String; var conf: String }
        return await call(json("PUT", "/v1/exits/" + Self.pathEscape(tag), B(kind: kind, conf: conf)))
    }
    public func deleteExit(_ tag: String) async -> Outcome<Empty> {
        await call(HTTPRequest(method: "DELETE", path: "/v1/exits/" + Self.pathEscape(tag)))
    }
    public func checkExit(_ tag: String) async -> Outcome<DaemonExitCheckResult> {
        await call(HTTPRequest(method: "POST", path: "/v1/exits/" + Self.pathEscape(tag) + "/check"))
    }

    // 在联网的应用与规则
    public func apps() async -> Outcome<DaemonAppsView> { await get("/v1/apps") }
    public func rules() async -> Outcome<RulesDoc> { await get("/v1/rules?format=json") }
    /// 规则和它的标识（ETag；条件写入用，补齐一）。
    public func rulesTagged() async -> Outcome<TaggedRules> {
        let req = HTTPRequest(method: "GET", path: "/v1/rules?format=json")
        let resp: HTTPResponse
        do {
            resp = try await transport.send(req)
        } catch let e as ConnectionError {
            return .unreachable(e)
        } catch {
            return .unreachable(.protocolError("连接出错"))
        }
        let o: Outcome<RulesDoc> = Self.map(resp, request: req)
        if case .ok(let doc) = o { return .ok(TaggedRules(doc: doc, tag: resp.headers["etag"] ?? "")) }
        return o.mapError()
    }
    /// 提交规则：ifMatch 是读取时的标识（我们自己的提交一律带上；后台回 rules_changed 时什么都没改）。
    public func putRules(_ doc: RulesDoc, dropAffected: Bool, ifMatch: String) async -> Outcome<Empty> {
        var req = json("PUT", "/v1/rules?drop_affected=" + (dropAffected ? "1" : "0"), doc)
        req.ifMatch = ifMatch
        return await call(req)
    }

    // 环境检测
    public func envcheck() async -> Outcome<EnvcheckReport> { await get("/v1/envcheck") }
    public func envSettings() async -> Outcome<EnvSettings> { await get("/v1/envcheck/settings") }
    public func putEnvSettings(_ s: EnvSettings) async -> Outcome<EnvSettings> { await call(json("PUT", "/v1/envcheck/settings", s)) }
    /// 补齐三：“允许局域网”开关。
    public func lanSetting() async -> Outcome<DaemonLANSetting> { await get("/v1/settings/lan") }
    public func putLANSetting(_ on: Bool) async -> Outcome<DaemonLANSetting> {
        struct B: Encodable { var allow_lan: Bool }
        return await call(json("PUT", "/v1/settings/lan", B(allow_lan: on)))
    }
    public func runEnvcheck() async -> Outcome<EnvcheckReport> { await call(HTTPRequest(method: "POST", path: "/v1/envcheck/run")) }
    /// 补齐四：环境检测的历史（最新的在前）。
    public func envHistory() async -> Outcome<DaemonEnvHistoryView> { await get("/v1/envcheck/history") }
    public func clearEnvHistory() async -> Outcome<DaemonEnvHistoryView> { await call(HTTPRequest(method: "DELETE", path: "/v1/envcheck/history")) }
    /// 设备体检（M6-6a）：字体等，只返回结果。
    public func deviceCheck() async -> Outcome<EnvcheckDeviceReport> { await call(HTTPRequest(method: "POST", path: "/v1/envcheck/device")) }

    // 地区
    public func region() async -> Outcome<DaemonRegionStatus> { await get("/v1/region") }
    public func putRegion(mode: String, timezone: String? = nil, lang: String? = nil, locale: String? = nil) async -> Outcome<Empty> {
        struct B: Encodable { var mode: String; var timezone: String?; var lang: String?; var locale: String? }
        return await call(json("PUT", "/v1/region", B(mode: mode, timezone: timezone, lang: lang, locale: locale)))
    }

    // 回国会结束的 Claude 进程（只读预览，不签发确认码）
    public func restorePreview() async -> Outcome<ClaudeprocPreview> { await get("/v1/restore/preview") }

    // 引擎与虚拟机
    public func engines() async -> Outcome<DaemonEnginesView> { await get("/v1/engines") }
    /// 启动管控（M6-4b）：现在是否允许启动 Claude、四层、覆盖范围、最近的拦截。
    public func launchGuard() async -> Outcome<DaemonLaunchGuardView> { await get("/v1/launchguard") }
    /// Claude 访问过的、不在内置 Claude 域名之下的域名（M6-12，只观察）。
    public func claudeDomains() async -> Outcome<DaemonClaudeDomainsView> { await get("/v1/claude-domains") }
    public func clearClaudeDomains() async -> Outcome<DaemonClaudeDomainsView> {
        await call(HTTPRequest(method: "DELETE", path: "/v1/claude-domains"))
    }
}

extension Outcome {
    /// 非成功的结果换个成功类型（错误照搬）。
    func mapError<U: Sendable>() -> Outcome<U> {
        switch self {
        case .ok: return .failed(0, ErrorText(raw: "读不懂后台服务的应答"))
        case .needsConfirm(let p): return .needsConfirm(p)
        case .needsAuthorization(let t): return .needsAuthorization(t)
        case .busy(let t): return .busy(t)
        case .invalid(let t): return .invalid(t)
        case .unsupported(let t): return .unsupported(t)
        case let .failed(c, t): return .failed(c, t)
        case .unreachable(let c): return .unreachable(c)
        }
    }

    /// 读取类结果的公共部分：错误文字（不可达时为 nil）。
    var errorText: ErrorText? {
        switch self {
        case .ok, .unreachable, .needsConfirm: return nil
        case let .needsAuthorization(t), let .busy(t), let .invalid(t), let .unsupported(t), let .failed(_, t): return t
        }
    }
}

/// 页面的加载状态（每个页面都有“后台不可达”和“加载失败”，不出现空白页；v2.7 决定 7）。
public enum LoadState: Sendable, Equatable {
    case loading
    case loaded
    case unreachable(ConnectionError)
    case unsupported(ErrorText) // 后台现在的模式不提供（例如假系统没有环境检测）
    case failed(ErrorText)

    static func from<T>(_ o: Outcome<T>) -> LoadState {
        switch o {
        case .ok: return .loaded
        case .unreachable(let c): return .unreachable(c)
        case .unsupported(let t): return .unsupported(t)
        default: return .failed(o.errorText ?? ErrorText(raw: L10n.loadFailed))
        }
    }
}
