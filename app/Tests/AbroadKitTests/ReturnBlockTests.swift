// M6-11b：回国拦截在设置页、回国确认框、首页的显示（文字都来自文字表）；状态里没有这一项时（没打开启动管控）不显示。
import AbroadAPI
import Foundation
import Testing
@testable import AbroadKit

@MainActor
private func settingsWithRB(_ extra: String) async -> SettingsModel {
    var j = statusJSON(state: "RESTORED", gate: false)
    if !extra.isEmpty {
        j.removeLast()
        j += "," + extra + "}"
    }
    let body = j
    let t = FakeTransport { req in
        req.path == "/v1/restore/preview" ? json(200, "{\"processes\":[],\"will_kill\":true}") : json(200, body)
    }
    let m = SettingsModel(client: APIClient(transport: t))
    await m.load()
    return m
}

private let rbNets = #"["160.79.104.0/21","2607:6bc0::/32"]"#

@MainActor
@Test func returnBlockInSettings() async {
    var m = await settingsWithRB("")
    #expect(m.returnBlockText == nil, "没打开启动管控：不显示")
    m = await settingsWithRB(#""return_block":{"on":true,"active":true,"nets":"# + rbNets + #","note":"x"}"#)
    #expect(m.returnBlockText == L10n.returnBlockActive(["160.79.104.0/21", "2607:6bc0::/32"]) + "\n" + L10n.returnBlockConsequence)
    m = await settingsWithRB(#""return_block":{"on":true,"active":false,"nets":"# + rbNets + #","note":"x"}"#)
    #expect(m.returnBlockText == L10n.returnBlockOn(["160.79.104.0/21", "2607:6bc0::/32"]) + "\n" + L10n.returnBlockConsequence)
    m = await settingsWithRB(#""return_block":{"on":false,"active":false,"nets":"# + rbNets + #","note":"x"}"#)
    #expect(m.returnBlockText == L10n.returnBlockOff + "\n" + L10n.returnBlockConsequence)
}

@MainActor
@Test func returnBlockConfirmAndHome() async {
    // 确认框：后台说会保留回国拦截时才写后果
    let with = ConfirmPrompt.testMake(#"{"error":"e","code":"needs_confirm","confirm":"c","expires_in_s":60,"preview":{"processes":[],"will_kill":true},"return_block":true}"#)
    let without = ConfirmPrompt.testMake(#"{"error":"e","code":"needs_confirm","confirm":"c","expires_in_s":60,"preview":{"processes":[],"will_kill":true}}"#)
    #expect(with.returnBlock && !without.returnBlock)
    // 首页：回国状态且在生效时才显示
    func home(_ state: String, _ rb: String?) async -> String? {
        var j = statusJSON(state: state, gate: false)
        if let rb {
            j.removeLast()
            j += ",\"return_block\":" + rb + "}"
        }
        let body = j
        let t = FakeTransport { req in req.path == "/v1/status" ? json(200, body) : json(200, "{\"available\":false}") }
        let m = StatusModel(client: APIClient(transport: t))
        _ = await m.refresh()
        return m.returnBlockNotice
    }
    let active = #"{"on":true,"active":true,"nets":"# + rbNets + #","note":"x"}"#
    #expect(await home("RESTORED", active) == L10n.returnBlockHome)
    #expect(await home("RESTORED", nil) == nil)
    #expect(await home("RESTORED", #"{"on":true,"active":false,"nets":[],"note":"x"}"#) == nil)
    #expect(await home("ENABLED", active) == nil)
}

/// 开发构建的测试段：设置页标注“测试段（开发构建）”。
@MainActor
@Test func returnBlockTestTag() async {
    let m = await settingsWithRB(#""return_block":{"on":true,"active":true,"nets":["192.0.2.0/24","2001:db8:abd::/48"],"test":true,"note":"x"}"#)
    #expect(m.returnBlockText == L10n.returnBlockActive(["192.0.2.0/24", "2001:db8:abd::/48"]) + "\n" + L10n.returnBlockTestTag)
}
