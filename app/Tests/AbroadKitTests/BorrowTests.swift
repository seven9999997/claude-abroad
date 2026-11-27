// 借用虚拟机修复（v0.70，docs/vm-borrow-plan.md v2 第 7 节）：进度、引擎页的“临时运行”、新错误代码的说明、
// 分流出口未启用时的回答、请求时限。
import AbroadAPI
import Foundation
import Testing
@testable import AbroadKit

@Test func borrowStepNames() {
    for s in ["starting", "pushing", "checking", "stopping"] {
        let t = L10n.borrowStep(s)
        #expect(t != nil && t!.hasPrefix("正在"), "\(s)")
    }
    #expect(L10n.borrowStep("done") == nil)
    #expect(L10n.borrowStep(nil) == nil)
}

// 状态里带 borrow 时（事件推送触发刷新），主代理页用它显示进度；借用结束后没有。
@MainActor
@Test func applyShowsBorrowProgress() async {
    let step = Box("starting")
    let t = FakeTransport { req in
        if req.path == "/v1/status" {
            let base = statusJSON(state: "RESTORED", gate: false, reasons: ["state", "vm"])
            if step.v.isEmpty { return json(200, base) }
            let withBorrow = String(base.dropLast()) + ",\"borrow\":{\"op\":\"main_apply\",\"step\":\"\(step.v)\",\"since\":\"2026-11-27T00:00:00Z\"}}"
            return json(200, withBorrow)
        }
        return json(404, "{\"error\":\"没有\",\"code\":\"not_found\"}")
    }
    let m = StatusModel(client: APIClient(transport: t), sleeper: NoSleep(), flow: ConfirmFlow(client: APIClient(transport: t)))
    _ = await m.refresh()
    #expect(m.borrowStep == "starting")
    #expect(L10n.borrowStep(m.borrowStep) == "正在启动虚拟机（第一次可能要一两分钟）")
    step.v = "checking"
    _ = await m.refresh()
    #expect(L10n.borrowStep(m.borrowStep) == "正在检查出口")
    step.v = ""
    _ = await m.refresh()
    #expect(m.borrowStep == nil)
}

@Test func borrowErrorCodesHaveText() throws {
    for c: ApiErrorCode in [.vmUnavailable, .mainPushFailed, .mainUnreachable, .noMainSelected, .stateIncomplete] {
        let t = try #require(L10n.borrowFailureTitle(c), "\(c)")
        #expect(!t.contains("_"), "\(c) 的标题不应是代码")
    }
    #expect(L10n.borrowFailureTitle(.busy) == nil)
    // 后台回的代码能解出来
    let raw = #"{"error":"启动虚拟机失败：拉起虚拟机失败。请在“引擎与虚拟机”页看最近的错误。","code":"vm_unavailable"}"#
    let b = try JSONDecoder().decode(ApiErrorBody.self, from: Data(raw.utf8))
    #expect(ApiErrorCode(rawValue: b.code) == .vmUnavailable)
}

@Test func enginesShowsBorrowed() throws {
    let raw = #"{"reachable":true,"boot_changed":false,"engines":[],"vm_manager":{"managed":true,"phase":"running","restarts":0,"failures":0,"borrowed":true}}"#
    let v = try JSONDecoder().decode(DaemonEnginesView.self, from: Data(raw.utf8))
    #expect(v.vmManager?.borrowed == true)
    #expect(L10n.vmBorrowed.contains("临时运行"))
}

// 分流出口：未启用时后台回 409 not_enabled，页面原样显示原因。
@MainActor
@Test func exitCheckNotEnabledText() async {
    let t = FakeTransport { req in
        switch (req.method, req.path) {
        case ("GET", "/v1/exits"): return json(200, exitsJSON)
        case ("GET", "/v1/status"): return json(200, statusJSON(state: "RESTORED", gate: false))
        case ("POST", "/v1/exits/wg/check"): return errBody(409, "not_enabled", "没有启用时不检查分流出口：启用后会自动检查")
        default: return json(404, "{\"error\":\"没有\",\"code\":\"not_found\"}")
        }
    }
    let m = ExitsModel(client: APIClient(transport: t))
    await m.load()
    await m.check("wg")
    #expect(m.notice?.text.raw == "没有启用时不检查分流出口：启用后会自动检查")
    #expect(m.notice?.text.code == .notEnabled)
}

// 应用里应用主代理、检查主代理的请求时限不低于 300 秒（借用最坏约 230 秒）。
@Test func transportDefaultTimeoutAtLeast300() {
    #expect(UnixTransport(path: "/tmp/x.sock").timeout >= 300)
    #expect(UnixTransport.fromEnvironment(["ABROAD_SOCKET": "/tmp/x.sock"]).timeout >= 300)
}
