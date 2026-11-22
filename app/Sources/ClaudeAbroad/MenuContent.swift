// 菜单栏的菜单（design 11.2）：状态、主代理出口 IP 和地区、启用、停用、一键回国、打开主窗口。
import AbroadKit
import SwiftUI

struct MenuContent: View {
    @ObservedObject var model: StatusModel
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        Text(L10n.titleLine(model.phase.title))
        if let c = model.connectionText {
            Text(c)
        }
        if let e = model.exitInfo, model.reachable {
            if e.available {
                Text(L10n.exitLine(e.ip, e.place))
            } else {
                Text(L10n.exitNoteLine(e.note))
            }
        }
        if let s = model.stalledNote {
            Text(s)
        }
        if let p = model.protectionNote {
            Text(p)
        }
        if model.securityNote != nil {
            Text(L10n.securityNoteTitle)
        }
        Divider()
        Button(L10n.enable) { Task { await model.enable(); showIfNeeded() } }
            .disabled(!model.menu.enable)
        Button(L10n.disable) { Task { await model.disable() } }
            .disabled(!model.menu.disable)
        Button(L10n.restore) { Task { await model.restore(); showIfNeeded() } }
            .disabled(!model.menu.restore)
        Divider()
        Button(L10n.openWindow) { openMain() }
        Button(L10n.quit) { NSApplication.shared.terminate(nil) }
    }

    /// 需要确认或要去终端执行时，打开主窗口显示（菜单里放不下预览）。
    private func showIfNeeded() {
        if model.flow.isActive || model.terminalCommand != nil || model.notice != nil { openMain() }
    }

    private func openMain() {
        openWindow(id: "main")
        NSApplication.shared.activate(ignoringOtherApps: true)
    }
}
