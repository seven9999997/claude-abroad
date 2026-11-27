// 主代理页面（design 11.2，原型 ui/design/index.html 的 pageProxy）：分组表单——当前主代理链路、节点来源、添加来源、应用主代理（出口节点、中转、诊断）。
// 列表只显示后台返回的名字、类型、节点数、更新时间；密码输入框不回显。
import AbroadAPI
import AbroadKit
import SwiftUI
import UniformTypeIdentifiers

struct MainProxyPage: View {
    @ObservedObject var model: MainProxyModel
    /// 借用虚拟机的进度（L10n.borrowStep(status.borrowStep)；v0.70）：应用、检查进行中显示它。
    var progress: String? = nil
    var onApplied: () -> Void = {}

    var body: some View {
        PageScroll {
            if let v = model.view, v.selected != nil {
                activeChainBox(v)
            }
            sourcesBox
            addBox
            selectBox
            if let n = model.notice { ErrorBlock(text: n.text) }
        }
        .task { await model.refresh() }
        .sheet(isPresented: lanShown) {
            if let p = model.lanPrompt {
                LANSheet(prompt: p, confirm: { Task { await model.confirmLAN() } }, cancel: { model.lanPrompt = nil })
            }
        }
        .sheet(isPresented: terminalShown) {
            if let t = model.terminal {
                TerminalSheet(prompt: t) { Task { await model.dismissTerminal(); onApplied() } }
            }
        }
    }

    private var lanShown: Binding<Bool> {
        Binding(get: { model.lanPrompt != nil }, set: { if !$0 { model.lanPrompt = nil } })
    }
    private var terminalShown: Binding<Bool> {
        Binding(get: { model.terminal != nil }, set: { if !$0 { Task { await model.dismissTerminal() } } })
    }

    /// 当前主代理链路可视化卡片（原型 .hops）。
    @ViewBuilder private func activeChainBox(_ v: DaemonMainProxyView) -> some View {
        let chainNodes = v.chain ?? []
        let hops = chainNodes.isEmpty ? (v.selected.map { [$0] } ?? []) : chainNodes
        CardSection(L10n.mainProxyTitle, icon: "star.circle.fill") {
            if v.applied {
                StatusBadge(tone: .pass, text: L10n.applySaved)
            } else {
                StatusBadge(tone: .checking, text: L10n.applySaved)
            }
        } content: {
            ScrollView(.horizontal, showsIndicators: false) {
                HStack(spacing: 8) {
                    hopTile(symbol: "laptopcomputer", subtitle: L10n.homeTab, title: L10n.appName, highlight: false)
                    ForEach(Array(hops.enumerated()), id: \.offset) { idx, hop in
                        Image(systemName: "arrow.right")
                            .font(.system(size: 11, weight: .semibold))
                            .foregroundStyle(.tertiary)
                        let isExit = idx == hops.count - 1
                        hopTile(
                            symbol: isExit ? "globe.americas.fill" : "point.3.connected.trianglepath.dotted",
                            subtitle: hop.type,
                            title: hop.name,
                            highlight: isExit
                        )
                    }
                }
                .padding(14)
            }
            if let note = v.note, !note.isEmpty {
                RowSeparator()
                CardRow {
                    Text(note).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                }
            }
        }
    }

    private func hopTile(symbol: String, subtitle: String, title: String, highlight: Bool) -> some View {
        HStack(spacing: 8) {
            Image(systemName: symbol)
                .font(.system(size: 13, weight: .semibold))
                .foregroundStyle(highlight ? Palette.tone(.pass) : Palette.accent)
            VStack(alignment: .leading, spacing: 1) {
                Text(subtitle.uppercased())
                    .font(.system(size: 10, weight: .semibold, design: .monospaced))
                    .foregroundStyle(.secondary)
                Text(title)
                    .font(.system(size: 12, weight: .semibold))
                    .lineLimit(1)
            }
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
        .background(
            RoundedRectangle(cornerRadius: 9, style: .continuous)
                .fill(highlight ? Palette.toneSoft(.pass) : Palette.graySoft)
        )
        .overlay(
            RoundedRectangle(cornerRadius: 9, style: .continuous)
                .strokeBorder(highlight ? Palette.toneLine(.pass) : Palette.boxLine, lineWidth: 1)
        )
    }

    /// 节点来源（原型“节点来源”）：每行图标、名字、种类和节点数，右边是更新、删除。
    private var sourcesBox: some View {
        CardSection(L10n.sources, icon: "tray.full") {
            if model.sources.isEmpty {
                CardRow {
                    IconBadge(symbol: "tray", size: 28)
                    Text(L10n.noSources).foregroundStyle(.secondary)
                }
            }
            ForEach(Array(model.sources.enumerated()), id: \.element.id) { i, s in
                if i > 0 { RowSeparator() }
                CardRow(alignment: .top) {
                    IconBadge(symbol: sourceSymbol(s.kind), size: 30)
                    VStack(alignment: .leading, spacing: 3) {
                        HStack(spacing: 8) {
                            Text(s.name).fontWeight(.semibold)
                            InfoPill(text: s.kind)
                        }
                        Text(L10n.sourceLine(kind: s.kind, nodes: s.nodeCount, updated: s.updated))
                            .font(.caption)
                            .foregroundStyle(.secondary)
                        if let e = s.error { ToneNote(tone: .failure, text: e, symbol: "xmark.circle") }
                    }
                    .frame(maxWidth: .infinity, alignment: .leading)
                    HStack(spacing: 6) {
                        if s.kind == "subscription" {
                            if model.refreshNeedsViaHost == s.id {
                                Button(L10n.refreshViaHost) { Task { await model.refreshSource(s.id, viaHost: true) } }
                                    .buttonStyle(SecondaryButtonStyle(compact: true, tone: .attention))
                            } else {
                                Button { Task { await model.refreshSource(s.id) } } label: {
                                    Label(L10n.refreshSource, systemImage: "arrow.clockwise")
                                }
                                .buttonStyle(SecondaryButtonStyle(compact: true))
                            }
                        }
                        Button(L10n.delete) { Task { await model.deleteSource(s.id) } }
                            .buttonStyle(SecondaryButtonStyle(compact: true, tone: .blocked))
                    }
                }
            }
            if model.needsReselect {
                RowSeparator()
                CardRow { ToneNote(tone: .attention, text: L10n.needsReselect) }
            }
        }
    }

    private func sourceSymbol(_ kind: String) -> String {
        switch kind {
        case "subscription": return "antenna.radiowaves.left.and.right"
        case "node": return "server.rack"
        default: return "doc.text"
        }
    }

    /// 添加来源：标题右边的分段控件选种类（原型“添加主代理的节点来源”的三种），下面是表单行。
    private var addBox: some View {
        CardSection(L10n.addSource, icon: "plus.circle") {
            Picker(L10n.addSource, selection: $model.addKind) {
                ForEach(AddKind.allCases, id: \.self) { Text($0.title).tag($0) }
            }
            .pickerStyle(.segmented)
            .labelsHidden()
            .fixedSize()
        } content: {
            CardRow {
                formLabel(L10n.sourceName)
                StyledTextField(placeholder: L10n.sourceName, text: $model.formName)
            }
            RowSeparator()
            switch model.addKind {
            case .subscription:
                CardRow {
                    formLabel(L10n.subscriptionURL)
                    StyledTextField(placeholder: L10n.subscriptionURL, text: $model.formURL, monospaced: true)
                }
                RowSeparator()
                CardRow {
                    Toggle(L10n.viaHostToggle, isOn: $model.viaHost)
                        .toggleStyle(.switch)
                }
                if model.viaHostNeeded {
                    RowSeparator()
                    CardRow {
                        ToneNote(tone: .attention, text: L10n.viaHostExplain, symbol: "network")
                    }
                }
            case .localFile:
                VStack(spacing: 10) {
                    Image(systemName: "doc.badge.plus")
                        .font(.system(size: 22))
                        .foregroundStyle(Palette.accent)
                    Text(model.formFile?.lastPathComponent ?? L10n.dropFileHint)
                        .font(model.formFile != nil ? .system(size: 13, weight: .semibold, design: .monospaced) : .callout)
                        .foregroundStyle(model.formFile != nil ? .primary : .secondary)
                    Button(L10n.chooseFile) { chooseFile() }
                        .buttonStyle(SecondaryButtonStyle(compact: true))
                }
                .frame(maxWidth: .infinity)
                .padding(.vertical, 20)
                .padding(.horizontal, 16)
                .background(
                    RoundedRectangle(cornerRadius: 10, style: .continuous)
                        .fill(Palette.graySoft)
                )
                .overlay(
                    RoundedRectangle(cornerRadius: 10, style: .continuous)
                        .strokeBorder(Palette.controlLine, style: StrokeStyle(lineWidth: 1.2, dash: [5, 4]))
                )
                .padding(14)
                .onDrop(of: [UTType.fileURL], isTargeted: nil) { providers in
                    guard let p = providers.first else { return false }
                    _ = p.loadObject(ofClass: URL.self) { url, _ in
                        if let url { Task { @MainActor in model.formFile = url } }
                    }
                    return true
                }
            case .manualNode:
                CardRow {
                    formLabel(L10n.nodeTypeLabel)
                    Picker(L10n.nodeTypeLabel, selection: $model.nodeType) {
                        Text(verbatim: "SOCKS5").tag("socks5")
                        Text(verbatim: "HTTP").tag("http")
                    }
                    .labelsHidden()
                    .fixedSize()
                    Spacer(minLength: 0)
                }
                RowSeparator()
                CardRow { formLabel(L10n.nodeServer); StyledTextField(placeholder: L10n.nodeServer, text: $model.nodeServer, monospaced: true) }
                RowSeparator()
                CardRow { formLabel(L10n.nodePort); StyledTextField(placeholder: L10n.nodePort, text: $model.nodePort, monospaced: true) }
                RowSeparator()
                CardRow { formLabel(L10n.nodeUser); StyledTextField(placeholder: L10n.nodeUser, text: $model.nodeUser) }
                RowSeparator()
                CardRow { formLabel(L10n.nodePassword); StyledTextField(placeholder: L10n.nodePassword, text: $model.nodePassword, secure: true) }
            }
            RowSeparator()
            CardRow {
                Spacer()
                Button { Task { await model.submitAdd() } } label: {
                    Label(L10n.submit, systemImage: "plus")
                }
                .buttonStyle(PrimaryButtonStyle())
                .disabled(model.busy)
            }
        }
    }

    private func formLabel(_ t: String) -> some View {
        Text(t)
            .font(.system(size: 13, weight: .medium))
            .foregroundStyle(.secondary)
            .frame(width: 120, alignment: .leading)
    }

    private func chooseFile() {
        let panel = NSOpenPanel()
        panel.allowsMultipleSelection = false
        panel.canChooseDirectories = false
        if panel.runModal() == .OK { model.formFile = panel.url }
    }

    /// 应用主代理（原型“更换主代理”）：出口节点、按顺序的中转、检查和应用，下面是结果。
    private var selectBox: some View {
        CardSection(L10n.applyMain, icon: "slider.horizontal.3") {
            CardRow {
                formLabel(L10n.exitNode)
                Picker(L10n.exitNode, selection: $model.selectedExit) {
                    Text(L10n.none).tag(String?.none)
                    ForEach(model.nodes) { n in
                        Text(L10n.nodeLine(n.name, n.type, n.source) + (n.udpLabel.map { L10n.listSeparator + $0 } ?? "")).tag(String?.some(n.id))
                    }
                }
                .labelsHidden()
            }
            if let id = model.selectedExit, let n = model.nodes.first(where: { $0.id == id }), let udp = n.udpLabel {
                RowSeparator()
                CardRow(verticalPadding: 8) {
                    Spacer().frame(width: 120)
                    StatusBadge(tone: n.udpTone, text: udp)
                    Spacer(minLength: 0)
                }
            }
            RowSeparator()
            CardRow(alignment: .top) {
                formLabel(L10n.relays)
                VStack(alignment: .leading, spacing: 8) {
                    ForEach(Array(model.via.enumerated()), id: \.offset) { i, id in
                        HStack(spacing: 8) {
                            InfoPill(text: "\(i + 1)", monospaced: true)
                            Text(model.nodes.first(where: { $0.id == id })?.name ?? id)
                                .font(.system(size: 13, weight: .medium))
                            Spacer(minLength: 8)
                            Button(L10n.removeRelay) { model.via.remove(at: i) }
                                .buttonStyle(SecondaryButtonStyle(compact: true, tone: .blocked))
                        }
                        .padding(.horizontal, 10)
                        .padding(.vertical, 6)
                        .background(RoundedRectangle(cornerRadius: 8, style: .continuous).fill(Palette.graySoft))
                    }
                    Menu {
                        ForEach(model.nodes) { n in Button(n.name) { model.via.append(n.id) } }
                    } label: {
                        Label(L10n.addRelay, systemImage: "plus")
                    }
                    .fixedSize()
                }
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            RowSeparator()
            CardRow {
                Spacer()
                Button { Task { await model.check() } } label: {
                    Label(L10n.checkMain, systemImage: "checkmark.circle")
                }
                .buttonStyle(SecondaryButtonStyle())

                Button { Task { await model.apply(); onApplied() } } label: {
                    Label(L10n.applyMain, systemImage: "paperplane.fill")
                }
                .buttonStyle(PrimaryButtonStyle())
                .disabled(model.selectedExit == nil || model.busy)
            }
            applyStateView
            checkStateView
        }
    }

    @ViewBuilder private var applyStateView: some View {
        switch model.applyState {
        case .idle: EmptyView()
        case .checking:
            RowSeparator()
            CardRow {
                ProgressView().controlSize(.small)
                Text(progress ?? L10n.applying).foregroundStyle(.secondary)
            }
        case .saved(let note):
            RowSeparator()
            CardRow { ToneNote(tone: .local, text: note.isEmpty ? L10n.applySaved : note, symbol: "bookmark") }
        case let .succeeded(c, tz):
            RowSeparator()
            CardRow { ToneNote(tone: model.applyState.tone ?? .success, text: L10n.applySucceeded(c, tz), symbol: "checkmark.circle") }
        case let .failed(t, rb):
            RowSeparator()
            CardRow {
                VStack(alignment: .leading, spacing: 8) {
                    if let title = L10n.borrowFailureTitle(t.code) { Text(title).bold() }
                    ErrorBlock(text: t)
                    switch rb {
                    case .done: Label(L10n.rollbackDone, systemImage: "arrow.uturn.backward.circle").font(.callout).foregroundStyle(.secondary)
                    case .incomplete: ToneNote(tone: rb.tone ?? .attention, text: L10n.rollbackIncomplete)
                    case .none: EmptyView()
                    }
                }
            }
        }
    }

    @ViewBuilder private var checkStateView: some View {
        switch model.checkState {
        case .idle: EmptyView()
        case .checking:
            RowSeparator()
            CardRow {
                ProgressView().controlSize(.small)
                Text(progress ?? L10n.checking).foregroundStyle(.secondary)
            }
        case let .ok(rule, c, tz):
            RowSeparator()
            CardRow { ToneNote(tone: model.checkState.tone ?? .pass, text: L10n.checkOK(c, tz, rule), symbol: "checkmark.circle") }
        case .failed(let t):
            RowSeparator()
            CardRow {
                VStack(alignment: .leading, spacing: 8) {
                    if let title = L10n.borrowFailureTitle(t.code) { Text(title).bold() }
                    ErrorBlock(text: t)
                }
            }
        }
    }
}

struct LANSheet: View {
    let prompt: LANPrompt
    let confirm: () -> Void
    let cancel: () -> Void
    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            SheetHeader(title: L10n.lanTitle, symbol: "network")
            Text(L10n.lanText(prompt.name))
                .lineSpacing(2)
                .fixedSize(horizontal: false, vertical: true)
            SheetFooter {
                Spacer()
                Button(L10n.cancel, action: cancel)
                    .buttonStyle(SecondaryButtonStyle())
                    .keyboardShortcut(.cancelAction)
                Button(L10n.confirm, action: confirm)
                    .buttonStyle(PrimaryButtonStyle())
            }
        }
        .padding(.horizontal, 22)
        .padding(.top, 20)
        .padding(.bottom, 18)
        .frame(width: 460)
        .tint(Palette.accent)
        .background(Palette.window)
    }
}
