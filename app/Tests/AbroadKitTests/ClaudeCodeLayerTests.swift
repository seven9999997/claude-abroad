// M6-3b：设置“回国与 Claude”里第四层（Claude Code 的托管设置和本机代理端口）的状态：生效、未写入、未生效、有冲突；
// 覆盖范围那句话来自文字表。
import AbroadAPI
import Foundation
import Testing
@testable import AbroadKit

private func statusWithCC(_ cc: String?) -> String {
    var j = statusJSON(state: "ENABLED", gate: true)
    if let cc {
        j.removeLast()
        j += ",\"claude_code\":\(cc)}"
    }
    return j
}

@MainActor
private func settingsCC(_ cc: String?) async -> SettingsModel {
    let t = FakeTransport { req in
        req.path == "/v1/restore/preview" ? json(200, "{\"processes\":[],\"will_kill\":true}") : json(200, statusWithCC(cc))
    }
    let m = SettingsModel(client: APIClient(transport: t))
    await m.load()
    return m
}

@MainActor
@Test func claudeCodeEffective() async {
    let m = await settingsCC(#"{"settings":{"written":true,"effective":true},"listening":true,"port":47821,"effective":true}"#)
    #expect(m.claudeCodeText == L10n.claudeCodeEffective + "\n" + L10n.claudeCodeScope)
    #expect(m.claudeCodeWarn == false)
}

@MainActor
@Test func claudeCodeNotWritten() async {
    let m = await settingsCC(#"{"settings":{"written":false,"effective":false},"listening":true,"port":47821,"effective":false}"#)
    #expect(m.claudeCodeText == L10n.claudeCodeNotWritten + "\n" + L10n.claudeCodeScope)
    #expect(m.claudeCodeWarn == false)
}

@MainActor
@Test func claudeCodeNotEffective() async {
    let m = await settingsCC(#"{"settings":{"written":true,"effective":false,"problem":"unreadable"},"listening":false,"listen_error":"端口被别的程序占着或不能监听","effective":false}"#)
    #expect(m.claudeCodeText.hasPrefix(L10n.claudeCodeNotEffective + "：" + L10n.claudeCodeProblem("unreadable") + "；" + L10n.claudeCodePortDown))
    #expect(m.claudeCodeText.hasSuffix(L10n.claudeCodeScope))
    #expect(m.claudeCodeWarn)
}

@MainActor
@Test func claudeCodeConflicts() async {
    let m = await settingsCC(#"{"settings":{"written":true,"effective":false,"conflicts":[{"file":"managed-settings.json","var":"HTTPS_PROXY"},{"file":"bad.json","var":""}]},"listening":true,"effective":false}"#)
    #expect(m.claudeCodeText.contains(L10n.claudeCodeConflicts(["managed-settings.json：HTTPS_PROXY", "bad.json：" + L10n.claudeCodeUnparsable])))
    #expect(m.claudeCodeWarn)
}

@MainActor
@Test func claudeCodeUnknown() async {
    let m = await settingsCC(nil)
    #expect(m.claudeCodeText == L10n.launchControlOff) // 没打开启动管控（M6-4b-1 返工）
    #expect(L10n.claudeCodeScope == "只覆盖 Claude Code，不覆盖 Claude 桌面版和浏览器")
}
