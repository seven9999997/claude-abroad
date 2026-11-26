// M6-3：设置“回国与 Claude”里第一层（Claude 域名解析锁）的状态：生效、未写入、未生效、有冲突、后台没报。
import AbroadAPI
import Foundation
import Testing
@testable import AbroadKit

private func statusWithLock(_ lock: String?) -> String {
    var j = statusJSON(state: "RESTORED", gate: false)
    if let lock {
        j.removeLast()
        j += ",\"resolver_lock\":\(lock)}"
    }
    return j
}

@MainActor
private func settings(_ lock: String?) async -> SettingsModel {
    let t = FakeTransport { req in
        req.path == "/v1/restore/preview" ? json(200, "{\"processes\":[],\"will_kill\":true}") : json(200, statusWithLock(lock))
    }
    let m = SettingsModel(client: APIClient(transport: t))
    await m.load()
    return m
}

@MainActor
@Test func resolverLockEffective() async {
    let m = await settings(#"{"written":true,"domains":["claude.ai","claude.com","anthropic.com"],"form":"default","effective":true}"#)
    #expect(m.resolverLockText == L10n.resolverLockEffective(["claude.ai", "claude.com", "anthropic.com"]))
    #expect(m.resolverLockText.contains("追加的置顶域名不在锁里"))
    #expect(m.resolverLockWarn == false)
}

@MainActor
@Test func resolverLockNotWritten() async {
    let m = await settings(#"{"written":false,"domains":["claude.ai"],"form":"default","effective":false}"#)
    #expect(m.resolverLockText == L10n.resolverLockNotWritten)
    #expect(m.resolverLockWarn == false)
}

@MainActor
@Test func resolverLockNotEffective() async {
    let m = await settings(#"{"written":true,"domains":["claude.ai","claude.com"],"form":"default","effective":false,"problems":[{"domain":"claude.ai","kind":"missing"},{"domain":"claude.com","kind":"modified"}]}"#)
    #expect(m.resolverLockText == L10n.resolverLockNotEffective + "：" + L10n.resolverLockProblem("claude.ai", "missing") + "；" + L10n.resolverLockProblem("claude.com", "modified"))
    #expect(m.resolverLockWarn)
}

@MainActor
@Test func resolverLockConflicts() async {
    let m = await settings(#"{"written":true,"domains":["claude.ai","claude.com"],"form":"default","effective":false,"problems":[{"domain":"claude.com","kind":"modified"}],"conflicts":["claude.com"]}"#)
    #expect(m.resolverLockText.contains(L10n.resolverLockConflicts(["claude.com"])))
    #expect(m.resolverLockWarn)
}

@MainActor
@Test func resolverLockUnknown() async {
    let m = await settings(nil)
    // 状态里既没有解析锁也没有第四层、也没有 launch_control：这次安装没打开启动管控（M6-4b-1 返工）
    #expect(m.resolverLockText == L10n.launchControlOff)
    #expect(m.resolverLockWarn == false)
}
