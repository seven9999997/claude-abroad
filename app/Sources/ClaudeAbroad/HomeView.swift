// 首页（design 11.2，原型 ui/design/index.html）：三个一键按钮、当前状态、主代理的出口 IP 和地区。
// 显示出口 IP 的地方旁边不放“复制全部状态”一类入口（v2.5 决定 3）。
import AbroadAPI
import AbroadKit
import SwiftUI

struct HomeView: View {
    @ObservedObject var model: StatusModel

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            header
            if let c = model.connectionText {
                Banner(title: c, detail: L10n.backendDownDetail, symbol: "bolt.horizontal.circle")
            }
            if !model.pendingRestore.isEmpty {
                Banner(title: L10n.regionPending, detail: model.pendingRestore.joined(separator: L10n.listSeparator), symbol: "clock.arrow.circlepath")
            }
            if let s = model.stalledNote {
                Banner(title: model.phase.title, detail: s, symbol: "pause.circle")
            }
            if let a = model.integrityAlert {
                Banner(title: L10n.integrityAlertTitle, detail: a, symbol: "exclamationmark.octagon")
            }
            if let n = model.udpNotice { // 补齐二
                Banner(title: L10n.udpUnavailableTitle, detail: n, symbol: "waveform.slash")
            }
            if let n = model.returnBlockNotice {
                Banner(title: L10n.returnBlockHomeTitle, detail: n, symbol: "hand.raised")
            }
            if let p = model.protectionNote {
                Banner(title: L10n.phaseError, detail: p, symbol: "exclamationmark.shield")
            }
            if let n = model.securityNote {
                Banner(title: L10n.securityNoteTitle, detail: n, symbol: "lock.open")
            }
            exitCard
            if let st = model.status {
                statusCard(st)
            }
            if let n = model.notice {
                ErrorBlock(text: n.text)
            }
            Spacer()
            buttons
        }
        .padding(20)
        .sheet(isPresented: terminalShown) {
            if let cmd = model.terminalCommand {
                TerminalSheet(prompt: TerminalPrompt(commands: [cmd])) { model.dismissTerminal() }
            }
        }
    }

    private var terminalShown: Binding<Bool> {
        Binding(get: { model.terminalCommand != nil }, set: { shown in if !shown { model.dismissTerminal() } })
    }

    private var header: some View {
        HStack(spacing: 12) {
            Image(systemName: model.phase.symbol).font(.system(size: 32))
            VStack(alignment: .leading) {
                Text(L10n.homeTitle).font(.caption).foregroundStyle(.secondary)
                Text(model.phase.title).font(.title)
            }
        }
    }

    private var exitCard: some View {
        GroupBox(L10n.mainExit) {
            VStack(alignment: .leading, spacing: 4) {
                if let e = model.exitInfo, e.available {
                    Text(e.ip).font(.title3.monospaced())
                    Text(L10n.placeLine(e.place, e.timezone)).foregroundStyle(.secondary)
                } else {
                    Text(model.exitInfo?.note ?? L10n.mainExitNone).foregroundStyle(.secondary)
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    private func statusCard(_ st: DaemonStatus) -> some View {
        GroupBox {
            VStack(alignment: .leading, spacing: 4) {
                if let reasons = st.gate.reasons, !reasons.isEmpty, model.phase != .abroad {
                    Text(L10n.gateReasonsLine(reasons))
                }
                if let t = st.region?.target {
                    Text(L10n.regionLine(t.timezone, t.lang))
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    private var buttons: some View {
        HStack {
            Button(L10n.enable) { Task { await model.enable() } }.disabled(!model.menu.enable)
            Button(L10n.disable) { Task { await model.disable() } }.disabled(!model.menu.disable)
            Button(L10n.openClaudeButton) { Task { await model.openClaude() } }
                .disabled(!model.canOpenClaude)
                .help(model.openClaudeHint ?? "")
            Spacer()
            Button(L10n.restore) { Task { await model.restore() } }.disabled(!model.menu.restore)
        }
        .controlSize(.large)
    }
}

struct Banner: View {
    let title: String
    let detail: String
    let symbol: String
    var body: some View {
        HStack(alignment: .top, spacing: 8) {
            Image(systemName: symbol)
            VStack(alignment: .leading, spacing: 2) {
                Text(title).bold()
                Text(detail).font(.callout).foregroundStyle(.secondary)
            }
        }
        .padding(10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(RoundedRectangle(cornerRadius: 8).fill(Color.yellow.opacity(0.15)))
    }
}

/// 后台的错误按“做什么失败了、原因、下一步”三段原样显示（v2.5 决定 7）。
struct ErrorBlock: View {
    let text: ErrorText
    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            if !text.action.isEmpty { Text(text.action).bold() }
            Text(text.reason)
            if !text.next.isEmpty { Text(text.next).foregroundStyle(.secondary) }
        }
        .padding(10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(RoundedRectangle(cornerRadius: 8).fill(Color.red.opacity(0.12)))
    }
}
