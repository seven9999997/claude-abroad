// 补齐五（architect 2026-11-27）：应用里导出诊断包。内容原样交给保存协议；写磁盘只经保存协议（测试只用假实现，
// 不弹保存面板）；模型里不留内容；各种结果各有提示，取消不算错误。
import AbroadAPI
import Foundation
import Testing
@testable import AbroadKit

/// 假的保存实现：记下收到的内容和建议的文件名，按设定返回结果；不碰磁盘。
@MainActor
final class FakeSaver: DiagnosticsSaver {
    var result: DiagnosticsSaveResult
    private(set) var got: [(Data, String)] = []
    init(_ result: DiagnosticsSaveResult) { self.result = result }
    func save(_ data: Data, suggestedName: String) async -> DiagnosticsSaveResult {
        got.append((data, suggestedName))
        return result
    }
}

/// 后台的诊断包：故意带不规整的空白和字段顺序，证明应用没有重新编排。
private let rawBody = Data("{\"generated_at\":\"2026-11-27T00:00:00Z\",  \"version\":{\"go\":\"go1.26\"},\n \"claude_domains\":{\"count\":2}}\n".utf8)

private func transport(_ resp: @escaping @Sendable (HTTPRequest) throws -> HTTPResponse) -> FakeTransport { FakeTransport(resp) }

private let fixedNow = { Date(timeIntervalSince1970: 1_800_000_000) }

/// 验收 a：保存的内容和后台应答逐字节相同；不勾时请求里不带 include_domains，勾了才带。
@MainActor
@Test func diagnosticsExportIsVerbatim() async {
    let t = transport { _ in HTTPResponse(status: 200, headers: ["content-type": "application/json"], body: rawBody) }
    let saver = FakeSaver(.saved(fileName: "诊断包.json"))
    let m = DiagnosticsExportModel(client: APIClient(transport: t), saver: saver, now: fixedNow)
    #expect(m.includeDomains == false, "默认不勾")
    #expect(await m.export() == .saved)
    #expect(saver.got.count == 1 && saver.got[0].0 == rawBody, "逐字节相同")
    #expect(saver.got[0].1.hasPrefix("诊断包-") && saver.got[0].1.hasSuffix(".json"))
    #expect(t.requests.last?.method == "GET" && t.requests.last?.path == "/v1/diagnostics")
    m.includeDomains = true
    #expect(await m.export() == .saved)
    #expect(t.requests.last?.path == "/v1/diagnostics?include_domains=1")
    #expect(saver.got[1].0 == rawBody)
    #expect(m.message == L10n.diagnosticsSaved("诊断包.json") && !m.isError && !m.busy)
}

/// 验收 b：成功；后台不可达；后台报错；写文件失败；用户取消（不显示错误，没有文件写入——假实现收到了内容但返回取消）。
@MainActor
@Test func diagnosticsExportOutcomes() async {
    // 后台不可达：不调用保存
    let down = FakeSaver(.saved(fileName: "x.json"))
    let m1 = DiagnosticsExportModel(client: APIClient(transport: transport { _ in throw ConnectionError.notRunning }), saver: down)
    #expect(await m1.export() == .backendDown)
    #expect(down.got.isEmpty && m1.message == L10n.diagnosticsBackendDown && m1.isError)
    // 后台报错：不调用保存，提示带后台的原因
    let errSaver = FakeSaver(.saved(fileName: "x.json"))
    let m2 = DiagnosticsExportModel(client: APIClient(transport: transport { _ in json(500, #"{"error":"生成诊断包失败"}"#) }), saver: errSaver)
    #expect(await m2.export() == .backendError)
    #expect(errSaver.got.isEmpty && m2.isError && m2.message == L10n.diagnosticsBackendError("生成诊断包失败"))
    // 写文件失败
    let ok = transport { _ in HTTPResponse(status: 200, headers: [:], body: rawBody) }
    let wf = FakeSaver(.failed(reason: "磁盘已满"))
    let m3 = DiagnosticsExportModel(client: APIClient(transport: ok), saver: wf)
    #expect(await m3.export() == .writeFailed)
    #expect(m3.message == L10n.diagnosticsWriteFailed("磁盘已满") && m3.isError)
    // 用户取消：不算错误，没有提示；上一次的错误提示也清掉
    wf.result = .cancelled
    #expect(await m3.export() == .cancelled)
    #expect(m3.message == nil && !m3.isError && !m3.busy)
    // 成功
    wf.result = .saved(fileName: "诊断包-1.json")
    #expect(await m3.export() == .saved)
    #expect(m3.message == L10n.diagnosticsSaved("诊断包-1.json") && !m3.isError)
}

/// 验收 c：模型里不留诊断包的内容——导出完成后，模型的所有存储属性里没有 Data，也没有内容里的任何片段。
@MainActor
@Test func diagnosticsExportKeepsNoContent() async {
    let t = transport { _ in HTTPResponse(status: 200, headers: [:], body: rawBody) }
    let m = DiagnosticsExportModel(client: APIClient(transport: t), saver: FakeSaver(.saved(fileName: "a.json")), now: fixedNow)
    await m.export()
    func scan(_ v: Any) -> Bool { // 有没有 Data 或包含内容片段的字符串
        if v is Data { return true }
        if let s = v as? String, s.contains("generated_at") || s.contains("go1.26") { return true }
        let mirror = Mirror(reflecting: v)
        if mirror.displayStyle == .class, !(v is DiagnosticsExportModel) { return false } // 不深入 client、saver
        return mirror.children.contains { scan($0.value) }
    }
    #expect(scan(m) == false)
}

/// 验收 d：说明来自文字表，写明有什么、没有什么，终端命令仍在；文字里没有地址。
@Test func diagnosticsHelpText() {
    let h = L10n.diagnosticsHelp
    for s in ["不含出口 IP", "订阅地址", "节点地址", "密码", "默认不含 Claude 访问过的域名", "abroad diagnostics", "--include-domains"] {
        #expect(h.contains(s), "说明里缺：\(s)")
    }
    let addr = try! NSRegularExpression(pattern: #"\d+\.\d+\.\d+\.\d+|https?://"#)
    for t in [h, L10n.diagnosticsBackendDown, L10n.diagnosticsSaved("a.json"), L10n.diagnosticsExport, L10n.diagnosticsIncludeDomains] {
        #expect(addr.firstMatch(in: t, range: NSRange(t.startIndex..., in: t)) == nil)
    }
}
