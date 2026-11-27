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
    let m = await settingsCC(#"{"enabled":true,"settings":{"written":true,"effective":true},"listening":true,"port":47821,"effective":true}"#)
    #expect(m.claudeCodeText == L10n.claudeCodeEffective + "\n" + L10n.claudeCodeScope)
    #expect(m.claudeCodeWarn == false)
}

@MainActor
@Test func claudeCodeNotWritten() async {
    let m = await settingsCC(#"{"enabled":true,"settings":{"written":false,"effective":false},"listening":true,"port":47821,"effective":false}"#)
    #expect(m.claudeCodeText == L10n.claudeCodeNotWritten + "\n" + L10n.claudeCodeScope)
    #expect(m.claudeCodeWarn == false)
}

@MainActor
@Test func claudeCodeNotEffective() async {
    let m = await settingsCC(#"{"enabled":true,"settings":{"written":true,"effective":false,"problem":"unreadable"},"listening":false,"listen_error":"端口被别的程序占着或不能监听","effective":false}"#)
    #expect(m.claudeCodeText.hasPrefix(L10n.claudeCodeNotEffective + "：" + L10n.claudeCodeProblem("unreadable") + "；" + L10n.claudeCodePortDown))
    #expect(m.claudeCodeText.hasSuffix(L10n.claudeCodeScope))
    #expect(m.claudeCodeWarn)
}

@MainActor
@Test func claudeCodeConflicts() async {
    let m = await settingsCC(#"{"enabled":true,"settings":{"written":true,"effective":false,"conflicts":[{"file":"managed-settings.json","var":"HTTPS_PROXY"},{"file":"bad.json","var":""}]},"listening":true,"effective":false}"#)
    #expect(m.claudeCodeText.contains(L10n.claudeCodeConflicts(["managed-settings.json：HTTPS_PROXY", "bad.json：" + L10n.claudeCodeUnparsable])))
    #expect(m.claudeCodeWarn)
}

@MainActor
@Test func claudeCodeUnknown() async {
    let m = await settingsCC(nil)
    #expect(m.claudeCodeText == L10n.launchControlOff) // 没打开启动管控（M6-4b-1 返工）
    #expect(L10n.claudeCodeScope == "只覆盖 Claude Code，不覆盖 Claude 桌面版和浏览器")
}

// v0.68：第四道默认关——“未开启”（不是“未生效”），不标警告；后果和两条终端命令来自文字表。
@MainActor
@Test func claudeCodeOffByDefault() async {
    let m = await settingsCC(#"{"enabled":false,"listening":false,"effective":false,"recent":[]}"#)
    #expect(m.claudeCodeText == L10n.claudeCodeOff)
    #expect(!m.claudeCodeText.contains(L10n.claudeCodeNotEffective))
    #expect(m.claudeCodeWarn == false)
    #expect(m.claudeCodeSwitchShown)
    #expect(m.claudeCodeCommands.map(\.command) == ["sudo abroad claude-proxy on", "sudo abroad claude-proxy off"])
    #expect(m.claudeCodeCommands.map(\.command) == [L10n.claudeCodeOnCommand, L10n.claudeCodeOffCommand])
    let c = m.claudeCodeConsequenceText
    for s in L10n.claudeCodeConsequences { #expect(c.contains(s)) }
    #expect(c.contains("所有 Claude Code") && c.contains("已启用") && c.contains("别的代理线路") && c.contains("卸载"))
}

/// 关着、但托管设置目录里还有我们这个名字的文件（关掉时被改过没删、上一版写下的）：提示用户自己处理，标警告。
@MainActor
@Test func claudeCodeOffLeftover() async {
    let m = await settingsCC(#"{"enabled":false,"leftover":true,"listening":false,"effective":false,"recent":[]}"#)
    #expect(m.claudeCodeText == L10n.claudeCodeOff + "\n" + L10n.claudeCodeLeftover)
    #expect(m.claudeCodeWarn)
}

/// 开发构建只管测试目标：打开只监听，标“测试模式”。
@MainActor
@Test func claudeCodeTestMode() async {
    let m = await settingsCC(#"{"enabled":true,"test_mode":true,"listening":true,"effective":false,"recent":[]}"#)
    #expect(m.claudeCodeText == L10n.claudeCodeTestMode)
    #expect(m.claudeCodeWarn == false)
}

/// 没打开启动管控：显示“未启用”，没有开关（不显示后果和命令）。
@MainActor
@Test func claudeCodeNoSwitchWithoutLaunchControl() async {
    let m = await settingsCC(nil)
    #expect(m.claudeCodeText == L10n.launchControlOff)
    #expect(!m.claudeCodeSwitchShown)
}
