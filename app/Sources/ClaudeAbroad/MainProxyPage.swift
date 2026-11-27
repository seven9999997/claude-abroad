// 主代理页面（design 11.2，原型 ui/design/index.html 的 pageProxy）：分组表单——节点来源、添加来源、应用主代理（出口节点、中转、诊断）。
// 列表只显示后台返回的名字、类型、节点数、更新时间；密码输入框不回显。
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

    /// 节点来源（原型“节点来源”）：每行名字、种类和节点数，右边是更新、删除。
    private var sourcesBox: some View {
        CardSection(L10n.sources) {
            if model.sources.isEmpty {
                CardRow { Text(L10n.noSources).foregroundStyle(.secondary) }
            }
            ForEach(Array(model.sources.enumerated()), id: \.element.id) { i, s in
                if i > 0 { RowSeparator() }
                CardRow {
                    VStack(alignment: .leading, spacing: 2) {
                        Text(s.name).fontWeight(.semibold)
                        Text(L10n.sourceLine(kind: s.kind, nodes: s.nodeCount, updated: s.updated)).font(.caption).foregroundStyle(.secondary)
                        if let e = s.error { ToneNote(tone: .failure, text: e, symbol: "xmark.circle") }
                    }
                    .frame(maxWidth: .infinity, alignment: .leading)
                    if s.kind == "subscription" {
                        if model.refreshNeedsViaHost == s.id {
                            Button(L10n.refreshViaHost) { Task { await model.refreshSource(s.id, viaHost: true) } }
                        } else {
                            Button(L10n.refreshSource) { Task { await model.refreshSource(s.id) } }
                        }
                    }
                    Button(L10n.delete) { Task { await model.deleteSource(s.id) } }
                }
            }
            if model.needsReselect {
                RowSeparator()
                CardRow { ToneNote(tone: .attention, text: L10n.needsReselect) }
            }
        }
    }

    /// 添加来源：标题右边的分段控件选种类（原型“添加主代理的节点来源”的三种），下面是表单行。
    private var addBox: some View {
        CardSection(L10n.addSource) {
            Picker(L10n.addSource, selection: $model.addKind) {
                ForEach(AddKind.allCases, id: \.self) { Text($0.title).tag($0) }
            }
            .pickerStyle(.segmented)
            .labelsHidden()
            .fixedSize()
        } content: {
            CardRow { formLabel(L10n.sourceName); TextField(L10n.sourceName, text: $model.formName).labelsHidden() }
            RowSeparator()
            switch model.addKind {
            case .subscription:
                CardRow { formLabel(L10n.subscriptionURL); TextField(L10n.subscriptionURL, text: $model.formURL).labelsHidden() }
                RowSeparator()
                CardRow { Toggle(L10n.viaHostToggle, isOn: $model.viaHost) }
                if model.viaHostNeeded {
                    CardRow { Text(L10n.viaHostExplain).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true) }
                }
            case .localFile:
                CardRow {
                    Button(L10n.chooseFile) { chooseFile() }
                    Text(model.formFile?.lastPathComponent ?? L10n.dropFileHint).foregroundStyle(.secondary)
                    Spacer(minLength: 0)
                }
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
                CardRow { formLabel(L10n.nodeServer); TextField(L10n.nodeServer, text: $model.nodeServer).labelsHidden() }
                RowSeparator()
                CardRow { formLabel(L10n.nodePort); TextField(L10n.nodePort, text: $model.nodePort).labelsHidden() }
                RowSeparator()
                CardRow { formLabel(L10n.nodeUser); TextField(L10n.nodeUser, text: $model.nodeUser).labelsHidden() }
                RowSeparator()
                CardRow { formLabel(L10n.nodePassword); SecureField(L10n.nodePassword, text: $model.nodePassword).labelsHidden() }
            }
            RowSeparator()
            CardRow {
                Spacer()
                Button(L10n.submit) { Task { await model.submitAdd() } }.disabled(model.busy)
            }
        }
    }

    private func formLabel(_ t: String) -> some View {
        Text(t).frame(width: 110, alignment: .leading)
    }

    private func chooseFile() {
        let panel = NSOpenPanel()
        panel.allowsMultipleSelection = false
        panel.canChooseDirectories = false
        if panel.runModal() == .OK { model.formFile = panel.url }
    }

    /// 应用主代理（原型“更换主代理”）：出口节点、按顺序的中转、检查和应用，下面是结果。
    private var selectBox: some View {
        CardSection(L10n.applyMain) {
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
                CardRow {
                    Spacer().frame(width: 110)
                    StatusBadge(tone: n.udpTone, text: udp)
                    Spacer(minLength: 0)
                }
            }
            RowSeparator()
            CardRow(alignment: .top) {
                formLabel(L10n.relays)
                VStack(alignment: .leading, spacing: 6) {
                    ForEach(Array(model.via.enumerated()), id: \.offset) { i, id in
                        HStack {
                            Text(verbatim: "\(i + 1).").foregroundStyle(.secondary).monospacedDigit()
                            Text(model.nodes.first(where: { $0.id == id })?.name ?? id)
                            Spacer(minLength: 8)
                            Button(L10n.removeRelay) { model.via.remove(at: i) }
                        }
                    }
                    Menu(L10n.addRelay) {
                        ForEach(model.nodes) { n in Button(n.name) { model.via.append(n.id) } }
                    }
                    .fixedSize()
                }
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            RowSeparator()
            CardRow {
                Spacer()
                Button(L10n.checkMain) { Task { await model.check() } }
                Button(L10n.applyMain) { Task { await model.apply(); onApplied() } }
                    .buttonStyle(.borderedProminent)
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
            CardRow { ProgressView().controlSize(.small); Text(progress ?? L10n.applying) }
        case .saved(let note):
            RowSeparator()
            CardRow { Text(note.isEmpty ? L10n.applySaved : note) }
        case let .succeeded(c, tz):
            RowSeparator()
            CardRow { ToneNote(tone: model.applyState.tone ?? .success, text: L10n.applySucceeded(c, tz), symbol: "checkmark.circle") }
        case let .failed(t, rb):
            RowSeparator()
            CardRow {
                VStack(alignment: .leading, spacing: 6) {
                    if let title = L10n.borrowFailureTitle(t.code) { Text(title).bold() }
                    ErrorBlock(text: t)
                    switch rb {
                    case .done: Label(L10n.rollbackDone, systemImage: "arrow.uturn.backward.circle")
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
            CardRow { ProgressView().controlSize(.small); Text(progress ?? L10n.checking) }
        case let .ok(rule, c, tz):
            RowSeparator()
            CardRow { ToneNote(tone: model.checkState.tone ?? .pass, text: L10n.checkOK(c, tz, rule), symbol: "checkmark.circle") }
        case .failed(let t):
            RowSeparator()
            CardRow {
                VStack(alignment: .leading, spacing: 6) {
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
        VStack(alignment: .leading, spacing: 12) {
            SheetHeader(title: L10n.lanTitle, symbol: "network")
            Text(L10n.lanText(prompt.name)).fixedSize(horizontal: false, vertical: true)
            SheetFooter {
                Spacer()
                Button(L10n.cancel, action: cancel).keyboardShortcut(.cancelAction)
                Button(L10n.confirm, action: confirm).buttonStyle(.borderedProminent)
            }
            .controlSize(.large)
        }
        .padding(.horizontal, 20)
        .padding(.top, 18)
        .padding(.bottom, 16)
        .frame(width: 460)
        .tint(Palette.accent)
        .background(Palette.window)
    }
}
