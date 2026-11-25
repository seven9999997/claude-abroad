// M6-2a：由 abroadd 管理虚拟机时，引擎接口带管理器状态；应用能解出来并显示成中文；闸门原因 vm 有名称。
import AbroadAPI
import Foundation
import Testing
@testable import AbroadKit

@Test func enginesViewDecodesVMManager() throws {
    let raw = #"{"reachable":false,"boot_changed":false,"engines":[],"vm_manager":{"managed":true,"phase":"failed","restarts":5,"failures":6,"last_error":"abroad-vm 意外退出"}}"#
    let v = try JSONDecoder().decode(DaemonEnginesView.self, from: Data(raw.utf8))
    let m = try #require(v.vmManager)
    #expect(m.managed)
    #expect(m.phase == "failed")
    #expect(m.restarts == 5)
    #expect(m.lastError == "abroad-vm 意外退出")
    #expect(L10n.vmManagerLine(m.phase, m.restarts) == "虚拟机：连续启动失败，已停止重启（保持断网）（重新启动 5 次）")
}

@Test func enginesViewWithoutVMManager() throws {
    let raw = #"{"reachable":true,"boot_changed":false,"engines":[]}"#
    let v = try JSONDecoder().decode(DaemonEnginesView.self, from: Data(raw.utf8))
    #expect(v.vmManager == nil)
}

@Test func vmReasonAndPhaseNames() {
    #expect(L10n.gateReasonName("vm") == "虚拟机没有运行或不健康")
    for p in ["idle", "starting", "running", "unhealthy", "backoff", "stopping", "failed", "stuck"] {
        #expect(L10n.vmPhaseName(p) != p)
    }
    #expect(L10n.vmPhaseName("未知") == "未知")
    // vm 不是用户自己引起的原因：失去保护时要通知
    #expect(Notifier.userCodes.contains("vm") == false)
}

// M6-2a 复审必须改 2：vm 只在闸门原来开着时才算保护断开。事件序列取自 daemon 的 TestVMManagerGateEventOrder（实际顺序）。
@MainActor
@Test func vmReasonNotifiesOnlyWhenGateWasOpen() {
    let s = FakeSender()
    let n = Notifier(sender: s)
    // 回国状态连上：闸门因 state 关着
    n.observe(status: st(gate: false, reasons: ["state"]), reconnect: true)
    // 启用：vm 启动中（闸门已关）→ 打开：0 条
    n.observe(event: ev(1, "gate", "gate_closed", "vm：虚拟机启动中"))
    n.observe(event: ev(2, "gate", "gate_opened"))
    #expect(s.got.isEmpty)
    // 停用：state 关：0 条；再启用：打开：0 条
    n.observe(event: ev(3, "gate", "gate_closed", "state：停用"))
    n.observe(event: ev(4, "gate", "gate_opened"))
    #expect(s.got.isEmpty)
    // 已启用时虚拟机崩溃（由开变关）再恢复：1 条断开，1 条恢复
    n.observe(event: ev(5, "gate", "gate_closed", "vm：虚拟机没有运行"))
    n.observe(event: ev(6, "gate", "gate_opened"))
    #expect(s.got.map(\.kind) == [.protectionLost, .protectionRestored])
    #expect(s.got[0].body == L10n.notifyProtectionLost(L10n.gateReasonName("vm"), "虚拟机没有运行"))
    // 回国：state 关 → vm 正在关闭：不再多发
    n.observe(event: ev(7, "gate", "gate_closed", "state：恢复本机网络"))
    n.observe(event: ev(8, "gate", "gate_closed", "vm：虚拟机正在关闭"))
    #expect(s.got.count == 2)
    // 从回国再启用：vm 启动中 → 打开：0 条（没有通知过断开，所以恢复也不通知）
    n.observe(event: ev(9, "gate", "gate_closed", "vm：虚拟机启动中"))
    n.observe(event: ev(10, "gate", "gate_opened"))
    #expect(s.got.count == 2)
}

// 出口检查不通过期间虚拟机又崩溃：只有出口检查那 1 条断开，恢复 1 条。
@MainActor
@Test func vmCrashWhileExitCheckFailedNoExtra() {
    let s = FakeSender()
    let n = Notifier(sender: s)
    n.observe(status: st(gate: true), reconnect: true)
    n.observe(event: ev(1, "gate", "gate_closed", "exit_check：出口检查明确不通过"))
    n.observe(event: ev(2, "gate", "gate_closed", "vm：虚拟机没有运行"))
    #expect(s.got.map(\.kind) == [.protectionLost])
    n.observe(event: ev(3, "gate", "gate_opened"))
    #expect(s.got.map(\.kind) == [.protectionLost, .protectionRestored])
}
