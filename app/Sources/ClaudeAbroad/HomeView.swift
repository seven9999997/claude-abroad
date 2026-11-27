// 总览（首页；design 11.2，原型 ui/design/index.html 的 pageMain）：横幅在上，状态卡片里是当前状态、主代理出口、地区和右侧一列操作按钮。
// 显示出口 IP 的地方旁边不放“复制全部状态”一类入口（v2.5 决定 3）。
import AbroadAPI
import AbroadKit
import SwiftUI

struct HomeView: View {
    @ObservedObject var model: StatusModel

    var body: some View {
        PageScroll {
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
            hero
            if let n = model.notice {
                ErrorBlock(text: n.text)
            }
        }
        .sheet(isPresented: terminalShown) {
            if let cmd = model.terminalCommand {
                TerminalSheet(prompt: TerminalPrompt(commands: [cmd])) { model.dismissTerminal() }
            }
        }
    }

    private var terminalShown: Binding<Bool> {
        Binding(get: { model.terminalCommand != nil }, set: { shown in if !shown { model.dismissTerminal() } })
    }

    /// 状态卡片（原型 .hero）：底色是状态色的浅底，左边图标、中间状态和要点、右边一列按钮。
    private var hero: some View {
        let tone = model.phase.tone
        return HStack(alignment: .top, spacing: 16) {
            Image(systemName: model.phase.symbol)
                .font(.system(size: 22, weight: .semibold))
                .foregroundStyle(Palette.tone(tone))
                .frame(width: 44, height: 44)
                .background(Circle().fill(Palette.box))
                .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 8) {
                Text(model.phase.title).font(.system(size: 20, weight: .bold))
                if let st = model.status, let reasons = st.gate.reasons, !reasons.isEmpty, model.phase != .abroad {
                    Text(L10n.gateReasonsLine(reasons)).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                }
                facts
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            buttons.frame(width: 150)
        }
        .padding(.horizontal, 18)
        .padding(.vertical, 16)
        .background(RoundedRectangle(cornerRadius: 12).fill(Palette.box).overlay(RoundedRectangle(cornerRadius: 12).fill(Palette.toneSoft(tone))))
        .overlay(RoundedRectangle(cornerRadius: 12).strokeBorder(Palette.boxLine, lineWidth: 1))
    }

    /// 主代理出口和地区（原型 .hero .kv）：小字标题在上、值在下。
    private var facts: some View {
        VStack(alignment: .leading, spacing: 8) {
            if let e = model.exitInfo, e.available {
                fact(L10n.mainExit + " · " + L10n.homeExitIP) { Text(e.ip).font(.system(size: 13, weight: .semibold, design: .monospaced)).textSelection(.enabled) }
                fact(L10n.homePlace) { Text(L10n.placeLine(e.place, e.timezone)).fontWeight(.semibold) }
            } else {
                fact(L10n.mainExit) { Text(model.exitInfo?.note ?? L10n.mainExitNone).foregroundStyle(.secondary) }
            }
            if let t = model.status?.region?.target {
                fact(L10n.region) { Text(t.timezone + L10n.listSeparator + t.lang).fontWeight(.semibold) }
            }
        }
    }

    private func fact<V: View>(_ label: String, @ViewBuilder value: () -> V) -> some View {
        VStack(alignment: .leading, spacing: 1) {
            Text(label).font(.caption).foregroundStyle(.secondary)
            value().fixedSize(horizontal: false, vertical: true)
        }
        .accessibilityElement(children: .combine)
    }

    private var buttons: some View {
        VStack(spacing: 8) {
            Button { Task { await model.enable() } } label: { Label(L10n.enable, systemImage: "power").frame(maxWidth: .infinity) }
                .buttonStyle(.borderedProminent)
                .disabled(!model.menu.enable)
            Button { Task { await model.disable() } } label: { Label(L10n.disable, systemImage: "stop").frame(maxWidth: .infinity) }
                .disabled(!model.menu.disable)
            Button { Task { await model.restore() } } label: { Label(L10n.restore, systemImage: "house").frame(maxWidth: .infinity) }
                .disabled(!model.menu.restore)
            Button { Task { await model.openClaude() } } label: { Label(L10n.openClaudeButton, systemImage: "arrow.up.forward.app").frame(maxWidth: .infinity) }
                .disabled(!model.canOpenClaude)
                .help(model.openClaudeHint ?? "")
                .padding(.top, 6)
        }
        .controlSize(.large)
    }
}
