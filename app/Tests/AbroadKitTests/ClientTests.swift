// 验收 c：接口客户端（假的传输层）。
import AbroadAPI
import Foundation
import Testing
@testable import AbroadKit

@Test func errorTextThreeParts() {
    let e = ErrorText(raw: "启用失败：还没有出口基准。请先建立出口基准（应用主代理），再启用。本机网络没有改动。")
    #expect(e.action == "启用失败")
    #expect(e.reason == "还没有出口基准")
    #expect(e.next == "请先建立出口基准（应用主代理），再启用。本机网络没有改动。")
    // 原样：三段拼回去就是原文（只切分，不改写）。
    #expect(e.action + "：" + e.reason + "。" + e.next == e.raw)
    let plain = ErrorText(raw: "需要本人授权：这是敏感操作")
    #expect(plain.action.isEmpty && plain.reason == plain.raw)
}

@Test func statusCodeMapping() async {
    let req = HTTPRequest(method: "POST", path: "/v1/enable")
    func m(_ r: HTTPResponse) -> Outcome<DaemonStatus> { APIClient.map(r, request: req) }
    guard case .ok(let st) = m(json(200, statusJSON(state: "ENABLED", gate: true))) else { Issue.record("200"); return }
    #expect(st.state == "ENABLED")
    guard case .needsConfirm(let p) = m(json(409, confirmJSON)) else { Issue.record("409 确认"); return }
    #expect(p.preview.processes?.first?.pid == 7100 && p.region?.to == "B" && p.expiresIn == 60)
    guard case .busy(let b) = m(json(409, "{\"error\":\"有别的操作在进行\"}")) else { Issue.record("409 忙"); return }
    #expect(b.raw == "有别的操作在进行")
    guard case .needsAuthorization(let a) = m(json(403, "{\"error\":\"需要本人授权：地区会变\"}")) else { Issue.record("403"); return }
    #expect(a.raw.contains("地区会变"))
    guard case .invalid = m(json(422, "{\"error\":\"x\"}")) else { Issue.record("422"); return }
    guard case .invalid = m(json(400, "{\"error\":\"x\"}")) else { Issue.record("400"); return }
    guard case .unsupported(let u) = m(json(501, "{\"error\":\"这个模式没有环境检测\"}")) else { Issue.record("501"); return }
    #expect(u.raw == "这个模式没有环境检测")
    guard case .failed(500, let f) = m(json(500, "{\"error\":\"内部错误\"}")) else { Issue.record("500"); return }
    #expect(f.raw == "内部错误")
    guard case .failed(200, _) = m(json(200, "not json")) else { Issue.record("读不懂的 200"); return }
}

@Test func connectionErrorsBecomeUnreachable() async {
    for err in [ConnectionError.notRunning, .noListener, .rejected, .timeout] {
        let c = APIClient(transport: FakeTransport { _ in throw err })
        guard case .unreachable(let got) = await c.status() else { Issue.record("\(err)"); continue }
        #expect(got == err)
    }
}

/// 确认码只在内存里，用一次就作废；取消也作废；确认时带上第一次的请求体和确认码。
@Test func confirmCodeOnce() async throws {
    let t = FakeTransport { req in
        if req.body.flatMap({ String(decoding: $0, as: UTF8.self) })?.contains("confirm") == true {
            return json(200, statusJSON(state: "ENABLED", gate: true))
        }
        return json(409, confirmJSON)
    }
    let c = APIClient(transport: t)
    guard case .needsConfirm(let p) = await c.call(HTTPRequest(method: "PUT", path: "/v1/region", body: Data("{\"mode\":\"exit\"}".utf8))) as Outcome<DaemonStatus> else {
        Issue.record("应要确认"); return
    }
    #expect(!p.isUsed)
    guard case .ok = await c.confirm(p, as: DaemonStatus.self) else { Issue.record("确认应成功"); return }
    #expect(p.isUsed)
    let sent = try #require(t.requests.last)
    let body = try #require(sent.body)
    let obj = try JSONSerialization.jsonObject(with: body) as? [String: String]
    #expect(sent.method == "PUT" && sent.path == "/v1/region" && obj == ["mode": "exit", "confirm": "0123456789abcdef0123456789abcdef"])
    // 第二次用同一个提示：不再发请求。
    let n = t.requests.count
    guard case .invalid = await c.confirm(p, as: DaemonStatus.self) else { Issue.record("用过的确认码不能再用"); return }
    #expect(t.requests.count == n)
    // 取消也作废。
    guard case .needsConfirm(let p2) = await c.enable() else { Issue.record("应要确认"); return }
    p2.discard()
    #expect(p2.isUsed)
    guard case .invalid = await c.confirm(p2, as: DaemonStatus.self) else { Issue.record("取消后不能再用"); return }
}
