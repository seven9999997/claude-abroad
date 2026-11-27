// 总览（首页；design 11.2，原型 ui/design/index.html 的 pageMain）：横幅在上，状态卡片里是当前状态、主代理出口、地区和右侧一列操作按钮，
// 下面是只读的环境检测、地区与虚拟机、分流出口摘要卡片。显示出口 IP 的地方旁边不放“复制全部状态”一类入口（v2.5 决定 3）。
import AbroadAPI
import AbroadKit
import SwiftUI

struct HomeView: View {
    @ObservedObject var model: StatusModel

    var body: some View {
        PageScroll {
            banners
            hero
            if let st = model.status {
                summaryGrid(st)
            }
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

    @ViewBuilder private var banners: some View {
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
    }

    private var terminalShown: Binding<Bool> {
        Binding(get: { model.terminalCommand != nil }, set: { shown in if !shown { model.dismissTerminal() } })
    }

    /// 状态卡片（原型 .hero）：底色是状态色的浅底，左边双层徽标、中间状态和要点指标网格、右边一列操作按钮。
    private var hero: some View {
        let tone = model.phase.tone
        return HStack(alignment: .top, spacing: 20) {
            ZStack {
                Circle()
                    .fill(Palette.toneSoft(tone))
                    .frame(width: 58, height: 58)
                Circle()
                    .fill(Palette.box)
                    .frame(width: 44, height: 44)
                    .overlay(Circle().strokeBorder(Palette.toneLine(tone), lineWidth: 1.5))
                Image(systemName: model.phase.symbol)
                    .font(.system(size: 20, weight: .semibold))
                    .foregroundStyle(Palette.tone(tone))
            }
            .accessibilityHidden(true)

            VStack(alignment: .leading, spacing: 12) {
                HStack(alignment: .center, spacing: 10) {
                    Text(model.phase.title)
                        .font(.system(size: 22, weight: .bold))
                    StatusBadge(tone: tone, text: model.phase.title)
                }

                if let st = model.status, model.phase != .abroad, let line = L10n.homeGateLine(reasons: st.gate.reasons ?? [], state: st.state) {
                    ToneNote(tone: tone, text: line, symbol: "info.circle")
                }

                facts
            }
            .frame(maxWidth: .infinity, alignment: .leading)

            buttons.frame(width: 164)
        }
        .padding(.horizontal, 22)
        .padding(.vertical, 20)
        .background(
            RoundedRectangle(cornerRadius: 16, style: .continuous)
                .fill(Palette.box)
                .overlay(RoundedRectangle(cornerRadius: 16, style: .continuous).fill(Palette.toneSoft(tone)))
                .shadow(color: Palette.cardShadow, radius: 10, x: 0, y: 3)
        )
        .overlay(
            RoundedRectangle(cornerRadius: 16, style: .continuous)
                .strokeBorder(Palette.toneLine(tone), lineWidth: 1)
        )
    }

    /// 主代理出口、地区与 UDP 指标网格（原型 .hero .kv）。
    private var facts: some View {
        LazyVGrid(columns: [GridItem(.adaptive(minimum: 165, maximum: 260), spacing: 10, alignment: .topLeading)], alignment: .leading, spacing: 10) {
            if let e = model.exitInfo, e.available {
                factTile(L10n.mainExit + " · " + L10n.homeExitIP) {
                    Text(e.ip)
                        .font(.system(size: 13, weight: .semibold, design: .monospaced))
                        .textSelection(.enabled)
                }
                factTile(L10n.homePlace) {
                    Text(L10n.placeLine(e.place, e.timezone))
                        .font(.system(size: 13, weight: .semibold))
                }
            } else {
                factTile(L10n.mainExit) {
                    Text(model.exitInfo?.note ?? L10n.mainExitNone)
                        .font(.callout)
                        .foregroundStyle(.secondary)
                }
            }
            if let t = model.status?.region?.target {
                factTile(L10n.region) {
                    Text(t.timezone + L10n.listSeparator + t.lang)
                        .font(.system(size: 13, weight: .semibold))
                }
            }
            if let udp = model.status?.mainUdp {
                factTile("UDP") {
                    Text(L10n.udpLabel(udp))
                        .font(.system(size: 13, weight: .semibold))
                }
            }
        }
    }

    private func factTile<V: View>(_ label: String, @ViewBuilder value: () -> V) -> some View {
        VStack(alignment: .leading, spacing: 3) {
            Text(label)
                .font(.system(size: 11, weight: .medium))
                .foregroundStyle(.secondary)
            value()
                .fixedSize(horizontal: false, vertical: true)
        }
        .padding(.horizontal, 10)
        .padding(.vertical, 8)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(RoundedRectangle(cornerRadius: 8, style: .continuous).fill(Palette.box))
        .overlay(RoundedRectangle(cornerRadius: 8, style: .continuous).strokeBorder(Palette.boxLine, lineWidth: 1))
        .accessibilityElement(children: .combine)
    }

    private var buttons: some View {
        VStack(spacing: 8) {
            Button { Task { await model.enable() } } label: {
                Label(L10n.enable, systemImage: "power").frame(maxWidth: .infinity)
            }
            .buttonStyle(PrimaryButtonStyle())
            .disabled(!model.menu.enable)

            Button { Task { await model.disable() } } label: {
                Label(L10n.disable, systemImage: "stop").frame(maxWidth: .infinity)
            }
            .buttonStyle(SecondaryButtonStyle())
            .disabled(!model.menu.disable)

            Button { Task { await model.restore() } } label: {
                Label(L10n.restore, systemImage: "house").frame(maxWidth: .infinity)
            }
            .buttonStyle(SecondaryButtonStyle())
            .disabled(!model.menu.restore)

            Rectangle()
                .fill(Palette.separator)
                .frame(height: 1)
                .padding(.vertical, 2)

            Button { Task { await model.openClaude() } } label: {
                Label(L10n.openClaudeButton, systemImage: "arrow.up.forward.app").frame(maxWidth: .infinity)
            }
            .buttonStyle(SecondaryButtonStyle(tone: model.canOpenClaude ? .pass : nil))
            .disabled(!model.canOpenClaude)
            .help(model.openClaudeHint ?? "")
        }
    }

    /// 首页两栏只读摘要卡片：左侧环境检测要点，右侧地区/虚拟机与分流出口状态。
    @ViewBuilder private func summaryGrid(_ st: DaemonStatus) -> some View {
        let items = prioritizedEnvItems(st.envcheck?.items ?? [])
        let exits = st.exits ?? []
        if !items.isEmpty || st.region != nil || st.vmManager != nil || !exits.isEmpty {
            HStack(alignment: .top, spacing: 18) {
                if !items.isEmpty {
                    CardSection(L10n.envTitle, icon: "checkmark.shield") {
                        ForEach(Array(items.enumerated()), id: \.element.id) { i, it in
                            if i > 0 { RowSeparator() }
                            CardRow(verticalPadding: 8) {
                                let tone = StatusTone.checkResult(it.result)
                                StatusMark(tone: tone, label: EnvModel.resultTitle(it.result), size: 18)
                                Text(it.id)
                                    .font(.system(size: 11, design: .monospaced))
                                    .foregroundStyle(.tertiary)
                                Text(it.name)
                                    .font(.system(size: 13, weight: .medium))
                                    .lineLimit(1)
                                Spacer(minLength: 8)
                                StatusBadge(tone: tone, text: EnvModel.resultTitle(it.result))
                            }
                        }
                    }
                    .frame(maxWidth: .infinity, alignment: .top)
                }

                VStack(alignment: .leading, spacing: 18) {
                    if st.region?.target != nil || st.vmManager != nil {
                        CardSection(L10n.region, icon: "globe") {
                            if let t = st.region?.target {
                                CardRow {
                                    RowLabel(
                                        title: t.timezone,
                                        detail: t.lang + L10n.listSeparator + t.locale
                                    )
                                    InfoPill(text: t.source == "exit" ? L10n.regionSourceExit : L10n.regionSourceManual)
                                }
                            } else {
                                CardRow {
                                    Text(L10n.regionNone)
                                        .font(.callout)
                                        .foregroundStyle(.secondary)
                                }
                            }
                            if let vm = st.vmManager {
                                RowSeparator()
                                CardRow {
                                    RowLabel(title: L10n.vmSection)
                                    StatusBadge(tone: vmTone(vm.phase), text: L10n.vmPhaseName(vm.phase))
                                }
                            }
                        }
                    }

                    if !exits.isEmpty {
                        CardSection(L10n.exitsTitle, icon: "arrow.triangle.branch") {
                            ForEach(Array(exits.enumerated()), id: \.element.tag) { i, ex in
                                if i > 0 { RowSeparator() }
                                CardRow(verticalPadding: 8) {
                                    VStack(alignment: .leading, spacing: 2) {
                                        HStack(spacing: 6) {
                                            Text(ex.tag)
                                                .font(.system(size: 13, weight: .semibold, design: .monospaced))
                                            if let kind = ex.kind, !kind.isEmpty {
                                                InfoPill(text: kind)
                                            }
                                        }
                                        if let c = st.opens?[ex.tag] {
                                            Text(L10n.exitConns(c.tcp, c.udp))
                                                .font(.caption)
                                                .foregroundStyle(.secondary)
                                        }
                                    }
                                    .frame(maxWidth: .infinity, alignment: .leading)
                                    StatusBadge(tone: ex.avail.up ? .pass : .blocked, text: ex.avail.up ? L10n.exitUp : L10n.exitDown)
                                }
                            }
                        }
                    }
                }
                .frame(maxWidth: .infinity, alignment: .top)
            }
        }
    }

    private func prioritizedEnvItems(_ raw: [EnvcheckItemResult]) -> [EnvcheckItemResult] {
        let sorted = raw.sorted { a, b in
            let pa = a.result == "fail" ? 0 : (a.result == "unknown" ? 1 : 2)
            let pb = b.result == "fail" ? 0 : (b.result == "unknown" ? 1 : 2)
            if pa != pb { return pa < pb }
            return a.id < b.id
        }
        return Array(sorted.prefix(6))
    }

    private func vmTone(_ phase: String) -> StatusTone {
        switch phase {
        case "running": return .pass
        case "starting", "stopping", "backoff": return .checking
        case "unhealthy", "failed", "stuck": return .blocked
        default: return .local
        }
    }
}
