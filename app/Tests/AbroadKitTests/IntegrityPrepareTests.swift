// M6-7、M6-8a：设置里的程序完整性和首次准备。
import AbroadAPI
import Foundation
import Testing
@testable import AbroadKit

@MainActor
private func settingsWith(_ extra: String) async -> SettingsModel {
    var j = statusJSON(state: "DISABLED", gate: false)
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

@MainActor
@Test func integrityStates() async {
    var m = await settingsWith("")
    #expect(m.integrityText == L10n.integrityUnknown)
    #expect(!m.integrityWarn)
    #expect(m.prepareText == nil)
    m = await settingsWith(#""integrity":{"checked":false,"ok":true,"note":"未启用（开发构建）"}"#)
    #expect(m.integrityText == L10n.integrityNotChecked("未启用（开发构建）"))
    m = await settingsWith(#""integrity":{"checked":true,"ok":true}"#)
    #expect(m.integrityText == L10n.integrityOK)
    m = await settingsWith(#""integrity":{"checked":true,"ok":false,"problems":[{"file":"bin/abroad-vm","kind":"mismatch"},{"file":"image/rootfs.img","kind":"missing"}]}"#)
    #expect(m.integrityWarn)
    #expect(m.integrityText == L10n.integrityFailed(["bin/abroad-vm 内容不对", "image/rootfs.img 不见了"]))
}

@MainActor
@Test func prepareStates() async {
    var m = await settingsWith(#""prepare":{"done":true}"#)
    #expect(m.prepareText == L10n.prepareDone)
    #expect(!m.prepareWarn)
    m = await settingsWith(#""prepare":{"done":false,"notes":["写入 Claude 域名解析锁失败（下次启动重试）"]}"#)
    #expect(m.prepareText == L10n.prepareNotDone + "\n写入 Claude 域名解析锁失败（下次启动重试）")
    #expect(m.prepareWarn)
}

/// M6-12：设置里显示 Claude 访问过的域名（每个一行、带次数），能清空；没有记录时一句话。
@MainActor
@Test func claudeDomainsInSettings() async {
    final class Box: @unchecked Sendable { var cleared = false }
    let box = Box()
    let t = FakeTransport { req in
        switch (req.method, req.path) {
        case ("GET", "/v1/claude-domains"):
            return json(200, box.cleared ? #"{"count":0,"domains":[],"note":"说明"}"# : #"{"count":1,"domains":[{"domain":"cdn.example.net","count":3,"first":"2026-11-26T00:00:00Z","last":"2026-11-26T01:00:00Z"}],"note":"说明"}"#)
        case ("DELETE", "/v1/claude-domains"):
            box.cleared = true
            return json(200, #"{"count":0,"domains":[],"note":"说明"}"#)
        case (_, "/v1/restore/preview"): return json(200, "{\"processes\":[],\"will_kill\":true}")
        default: return json(200, statusJSON(state: "RESTORED", gate: false))
        }
    }
    let m = SettingsModel(client: APIClient(transport: t))
    await m.load()
    #expect(m.claudeDomainLines == [L10n.claudeDomainLine("cdn.example.net", 3)])
    await m.clearClaudeDomains()
    #expect(m.claudeDomainLines == [L10n.claudeDomainsNone] && box.cleared)
}
