// 安装引导和主代理的接口（M5-8）。请求体里的订阅地址、密码只在这一刻经接口发出，不在这里留下。
import AbroadAPI
import Foundation

/// 选择节点的应答：未启用时只保存（saved、note），已启用时是“应用主代理”的结果（baseline 等）。
public struct MainSelectOutcome: Decodable, Sendable {
    public var saved: Bool?
    public var note: String?
    public var baseline: DaemonBaselineSummary?
}

extension APIClient {
    func json<B: Encodable>(_ method: String, _ path: String, _ body: B) -> HTTPRequest {
        HTTPRequest(method: method, path: path, body: try? JSONEncoder().encode(body))
    }

    // ---- 安装引导 ----
    public func setup() async -> Outcome<DaemonSetupView> { await get("/v1/setup") }
    public func setupComplete() async -> Outcome<DaemonSetupView> { await call(HTTPRequest(method: "POST", path: "/v1/setup/complete")) }

    // ---- 主代理 ----
    public func sources() async -> Outcome<[DaemonMainProxySourceView]> { await get("/v1/main-proxy/sources") }
    public func addSource(_ req: AddSourceBody) async -> Outcome<DaemonSourceAdded> {
        await call(json("POST", "/v1/main-proxy/sources", req))
    }
    public func deleteSource(_ id: String) async -> Outcome<Empty> {
        await call(HTTPRequest(method: "DELETE", path: "/v1/main-proxy/sources/" + Self.pathEscape(id)))
    }
    public func refreshSource(_ id: String, viaHost: Bool) async -> Outcome<DaemonSourceAdded> {
        await call(json("POST", "/v1/main-proxy/sources/" + Self.pathEscape(id) + "/refresh", ViaHostBody(viaHost: viaHost)))
    }
    public func selectMain(selected: String, chain: [String]) async -> Outcome<MainSelectOutcome> {
        await call(json("PUT", "/v1/main-proxy", SelectBody(selected: selected, chain: chain.isEmpty ? nil : chain)))
    }
    public func applyMain() async -> Outcome<DaemonMainProxyResult> { await call(HTTPRequest(method: "POST", path: "/v1/main-proxy/apply")) }
    public func checkMain() async -> Outcome<DaemonBaselineSummary> { await call(HTTPRequest(method: "POST", path: "/v1/main-proxy/check")) }
    public func lanConfirm(node: String) async -> Outcome<Empty> {
        await call(json("POST", "/v1/main-proxy/lan-confirm", NodeBody(node: node)))
    }

    struct ViaHostBody: Encodable {
        var viaHost: Bool
        enum CodingKeys: String, CodingKey { case viaHost = "via_host" }
    }

    struct NodeBody: Encodable { var node: String }

    struct SelectBody: Encodable {
        var selected: String
        var chain: [String]?
    }

    static func pathEscape(_ s: String) -> String {
        s.addingPercentEncoding(withAllowedCharacters: .alphanumerics) ?? s
    }
}

/// POST /v1/main-proxy/sources 的请求体（三种形式：subscription 带 url、yaml 带 content、node 带 node）。
/// 只在提交的那一刻构造，用完即丢（视图模型里不留）。
public struct AddSourceBody: Encodable, Sendable {
    public var kind: String
    public var name: String
    public var url: String?
    public var content: String?
    public var node: [String: JSONValue]?
    public var viaHost: Bool?

    enum CodingKeys: String, CodingKey {
        case kind, name, url, content, node
        case viaHost = "via_host"
    }

    public init(kind: String, name: String, url: String? = nil, content: String? = nil, node: [String: JSONValue]? = nil, viaHost: Bool? = nil) {
        self.kind = kind
        self.name = name
        self.url = url
        self.content = content
        self.node = node
        self.viaHost = viaHost
    }
}
