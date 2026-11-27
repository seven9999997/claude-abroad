// 菜单栏的菜单（design 11.2，原型 ui/design/index.html 的 menuDrop）：头部是状态（图标加文字）、主代理出口 IP 和地区，
// 然后是启用、停用、一键回国、打开 Claude，最后是打开主窗口和退出。系统菜单的样式由系统决定，不另外上色。
import AbroadKit
import SwiftUI

struct MenuContent: View {
    @ObservedObject var model: StatusModel
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        Label(L10n.titleLine(model.phase.title), systemImage: model.phase.symbol)
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
        if let t = model.status?.region?.target, model.reachable {
            Text(L10n.regionLine(t.timezone, t.lang))
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
        if model.udpNotice != nil { // 补齐二
            Text(L10n.udpUnavailableTitle)
        }
        Divider()
        Button(L10n.enable) { Task { await model.enable(); showIfNeeded() } }
            .disabled(!model.menu.enable)
        Button(L10n.disable) { Task { await model.disable() } }
            .disabled(!model.menu.disable)
        Button(L10n.restore) { Task { await model.restore(); showIfNeeded() } }
            .disabled(!model.menu.restore)
        Button(L10n.openClaudeButton) { Task { await model.openClaude() } }
            .disabled(!model.canOpenClaude)
        Divider()
        Button(L10n.openWindow) { openMain() }
        Button(L10n.quit) { NSApplication.shared.terminate(nil) }
    }

    /// 需要确认或要去终端执行时，打开主窗口显示（菜单里放不下预览）。
    private func showIfNeeded() {
        if model.flow.isActive || model.terminalCommand != nil || model.notice != nil || model.privileged?.isActive == true { openMain() }
    }

    private func openMain() {
        openWindow(id: "main")
        NSApplication.shared.activate(ignoringOtherApps: true)
    }
}
