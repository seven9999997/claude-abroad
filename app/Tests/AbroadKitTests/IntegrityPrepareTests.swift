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
