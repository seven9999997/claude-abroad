// 接口客户端：把应答映射成结果（成功、需要确认、需要本人授权、忙、校验失败、不提供、其他），错误原样分三段取出。
// 能不能做、要不要确认、是不是敏感都由 abroadd 判断（m5-plan v2.5 决定 1）；这里只照着应答走。
import AbroadAPI
import Foundation

/// 后台的错误文字。abroadd 的格式是“<做什么>失败：<原因>。<下一步>”（internal/usererror）；
/// 这里只按这个格式切成三段，不改写任何字（v2.5 决定 7）。不是这个格式的整段放在 reason 里。
public struct ErrorText: Sendable, Equatable {
    public var action: String // “启用失败”
    public var reason: String
    public var next: String
    public var raw: String

    public init(raw: String) {
        self.raw = raw
        if let r = raw.range(of: "失败：") {
            action = String(raw[..<r.lowerBound]) + "失败"
            let rest = raw[r.upperBound...]
            if let dot = rest.range(of: "。") {
                reason = String(rest[..<dot.lowerBound])
                next = String(rest[dot.upperBound...])
            } else {
                reason = String(rest)
                next = ""
            }
        } else {
            action = ""
            reason = raw
            next = ""
        }
    }

    static func from(body: Data) -> ErrorText {
        struct E: Decodable { var error: String? }
        let s = (try? JSONDecoder().decode(E.self, from: body))?.error ?? ""
        return ErrorText(raw: s)
    }
}

public enum APIError: Error, Equatable, Sendable {
    case status(Int, ErrorText)
    case decode(String)
}

/// 两步确认的提示：预览给界面显示；确认码只在内存里，取一次就丢（confirm 用完即作废）。
public final class ConfirmPrompt: @unchecked Sendable {
    public let message: ErrorText
    public let preview: ClaudeprocPreview
    public let region: DaemonRegionChange?
    public let expiresIn: Int64
    let request: HTTPRequest // 第一次请求（确认时带上确认码再发一次）
    private let lock = NSLock()
    private var code: String?

    init(needed: DaemonConfirmNeeded, request: HTTPRequest) {
        message = ErrorText(raw: needed.error)
        preview = needed.preview
        region = needed.region
        expiresIn = needed.expiresInS
        self.request = request
        code = needed.confirm
    }

    /// 取出确认码（只能取一次）。
    func take() -> String? {
        lock.lock(); defer { lock.unlock() }
        let c = code
        code = nil
        return c
    }

    /// 放弃：确认码作废（用户点了取消）。
    public func discard() { _ = take() }

    public var isUsed: Bool { lock.lock(); defer { lock.unlock() }; return code == nil }
}

/// 一次调用的结果。
public enum Outcome<T: Sendable>: Sendable {
    case ok(T)
    case needsConfirm(ConfirmPrompt)
    case needsAuthorization(ErrorText) // 403：要本人授权（敏感操作，或这一次会变的启用、切换地区、放松规则）
    case busy(ErrorText) // 409：有别的操作在进行（或冲突）
    case invalid(ErrorText) // 400、422：校验失败
    case unsupported(ErrorText) // 501：后台现在的模式不提供
    case failed(Int, ErrorText)
    case unreachable(ConnectionError)
}

public final class APIClient: Sendable {
    public let transport: Transport

    public init(transport: Transport) { self.transport = transport }

    // ---- 只读 ----

    public func status() async -> Outcome<DaemonStatus> { await get("/v1/status") }
    public func mainExit() async -> Outcome<DaemonMainExitView> { await get("/v1/main-proxy/exit") }
    public func mainProxy() async -> Outcome<DaemonMainProxyView> { await get("/v1/main-proxy") }

    // ---- 操作 ----

    public func enable() async -> Outcome<DaemonStatus> { await call(HTTPRequest(method: "POST", path: "/v1/enable")) }
    public func disable() async -> Outcome<DaemonStatus> { await call(HTTPRequest(method: "POST", path: "/v1/disable")) }
    public func restore() async -> Outcome<DaemonStatus> { await call(HTTPRequest(method: "POST", path: "/v1/restore")) }

    /// 用户确认后带上确认码再发一次第一次的请求。确认码取出即作废，同一个提示不能用第二次。
    public func confirm<T: Decodable & Sendable>(_ prompt: ConfirmPrompt, as: T.Type = T.self) async -> Outcome<T> {
        guard let code = prompt.take() else {
            return .invalid(ErrorText(raw: "确认已经用过或已取消：请重新操作"))
        }
        var req = prompt.request
        var obj: [String: Any] = [:]
        if let b = req.body, let o = try? JSONSerialization.jsonObject(with: b) as? [String: Any] { obj = o }
        obj["confirm"] = code
        req.body = try? JSONSerialization.data(withJSONObject: obj)
        req.contentType = "application/json"
        return await call(req)
    }

    // ---- 通用 ----

    public func get<T: Decodable & Sendable>(_ path: String) async -> Outcome<T> {
        await call(HTTPRequest(method: "GET", path: path))
    }

    public func call<T: Decodable & Sendable>(_ req: HTTPRequest) async -> Outcome<T> {
        let resp: HTTPResponse
        do {
            resp = try await transport.send(req)
        } catch let e as ConnectionError {
            return .unreachable(e)
        } catch let e as HTTPError {
            return .unreachable(.protocolError(String(describing: e)))
        } catch {
            return .unreachable(.protocolError("连接出错"))
        }
        return Self.map(resp, request: req)
    }

    /// 状态码到结果的映射（纯函数，单测覆盖）。
    public static func map<T: Decodable & Sendable>(_ resp: HTTPResponse, request: HTTPRequest) -> Outcome<T> {
        switch resp.status {
        case 200..<300:
            if T.self == Empty.self { return .ok(Empty() as! T) }
            do {
                return .ok(try JSONDecoder().decode(T.self, from: resp.body))
            } catch {
                return .failed(resp.status, ErrorText(raw: "读不懂后台服务的应答"))
            }
        case 409:
            if let c = try? JSONDecoder().decode(DaemonConfirmNeeded.self, from: resp.body), c.confirm.count == 32 {
                return .needsConfirm(ConfirmPrompt(needed: c, request: request))
            }
            return .busy(ErrorText.from(body: resp.body))
        case 403: return .needsAuthorization(ErrorText.from(body: resp.body))
        case 400, 422: return .invalid(ErrorText.from(body: resp.body))
        case 501: return .unsupported(ErrorText.from(body: resp.body))
        default: return .failed(resp.status, ErrorText.from(body: resp.body))
        }
    }
}

/// 不关心应答内容时用。
public struct Empty: Decodable, Sendable {
    public init() {}
}
