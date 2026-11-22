// 主代理页面（design 11.2）：来源（订阅地址 / 本地配置文件 / 手动节点）、节点与链式、“应用主代理”、诊断。
// 列表只显示后台返回的名字、类型、节点数、更新时间；密码输入框不回显。
import AbroadKit
import SwiftUI
import UniformTypeIdentifiers

struct MainProxyPage: View {
    @ObservedObject var model: MainProxyModel
    var onApplied: () -> Void = {}

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                sourcesBox
                addBox
                selectBox
                if let n = model.notice { ErrorBlock(text: n.text) }
            }
            .padding(.vertical, 4)
        }
        .task { await model.refresh() }
        .sheet(isPresented: confirmShown) {
            if let p = model.pendingConfirm {
                ConfirmSheet(prompt: p) { ok in Task { await model.answerConfirm(ok); onApplied() } }
            }
        }
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

    private var confirmShown: Binding<Bool> {
        Binding(get: { model.pendingConfirm != nil }, set: { if !$0, model.pendingConfirm != nil { Task { await model.answerConfirm(false) } } })
    }
    private var lanShown: Binding<Bool> {
        Binding(get: { model.lanPrompt != nil }, set: { if !$0 { model.lanPrompt = nil } })
    }
    private var terminalShown: Binding<Bool> {
        Binding(get: { model.terminal != nil }, set: { if !$0 { Task { await model.dismissTerminal() } } })
    }

    private var sourcesBox: some View {
        GroupBox(L10n.sources) {
            VStack(alignment: .leading, spacing: 6) {
                if model.sources.isEmpty { Text(L10n.noSources).foregroundStyle(.secondary) }
                ForEach(model.sources) { s in
                    HStack {
                        VStack(alignment: .leading) {
                            Text(s.name).bold()
                            Text(L10n.sourceLine(kind: s.kind, nodes: s.nodeCount, updated: s.updated)).font(.caption).foregroundStyle(.secondary)
                            if let e = s.error { Text(e).font(.caption).foregroundStyle(.red) }
                        }
                        Spacer()
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
                if model.needsReselect { Text(L10n.needsReselect).foregroundStyle(.orange) }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    private var addBox: some View {
        GroupBox(L10n.addSource) {
            VStack(alignment: .leading, spacing: 8) {
                Picker(L10n.addSource, selection: $model.addKind) {
                    ForEach(AddKind.allCases, id: \.self) { Text($0.title).tag($0) }
                }
                .pickerStyle(.segmented)
                .labelsHidden()
                TextField(L10n.sourceName, text: $model.formName)
                switch model.addKind {
                case .subscription:
                    TextField(L10n.subscriptionURL, text: $model.formURL)
                    Toggle(L10n.viaHostToggle, isOn: $model.viaHost)
                    if model.viaHostNeeded { Text(L10n.viaHostExplain).font(.caption).foregroundStyle(.secondary) }
                case .localFile:
                    HStack {
                        Button(L10n.chooseFile) { chooseFile() }
                        Text(model.formFile?.lastPathComponent ?? L10n.dropFileHint).foregroundStyle(.secondary)
                    }
                    .onDrop(of: [UTType.fileURL], isTargeted: nil) { providers in
                        guard let p = providers.first else { return false }
                        _ = p.loadObject(ofClass: URL.self) { url, _ in
                            if let url { Task { @MainActor in model.formFile = url } }
                        }
                        return true
                    }
                case .manualNode:
                    Picker(L10n.nodeTypeLabel, selection: $model.nodeType) {
                        Text(verbatim: "SOCKS5").tag("socks5")
                        Text(verbatim: "HTTP").tag("http")
                    }
                    TextField(L10n.nodeServer, text: $model.nodeServer)
                    TextField(L10n.nodePort, text: $model.nodePort)
                    TextField(L10n.nodeUser, text: $model.nodeUser)
                    SecureField(L10n.nodePassword, text: $model.nodePassword)
                }
                HStack {
                    Spacer()
                    Button(L10n.submit) { Task { await model.submitAdd() } }.disabled(model.busy)
                }
            }
        }
    }

    private func chooseFile() {
        let panel = NSOpenPanel()
        panel.allowsMultipleSelection = false
        panel.canChooseDirectories = false
        if panel.runModal() == .OK { model.formFile = panel.url }
    }

    private var selectBox: some View {
        GroupBox(L10n.applyMain) {
            VStack(alignment: .leading, spacing: 8) {
                Picker(L10n.exitNode, selection: $model.selectedExit) {
                    Text(L10n.none).tag(String?.none)
                    ForEach(model.nodes) { n in Text(L10n.nodeLine(n.name, n.type, n.source)).tag(String?.some(n.id)) }
                }
                Text(L10n.relays).font(.caption)
                ForEach(Array(model.via.enumerated()), id: \.offset) { i, id in
                    HStack {
                        Text(model.nodes.first(where: { $0.id == id })?.name ?? id)
                        Button(L10n.removeRelay) { model.via.remove(at: i) }
                    }
                }
                Menu(L10n.addRelay) {
                    ForEach(model.nodes) { n in Button(n.name) { model.via.append(n.id) } }
                }
                .fixedSize()
                HStack {
                    Button(L10n.applyMain) { Task { await model.apply(); onApplied() } }
                        .disabled(model.selectedExit == nil || model.busy)
                    Button(L10n.checkMain) { Task { await model.check() } }
                }
                applyStateView
                checkStateView
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    @ViewBuilder private var applyStateView: some View {
        switch model.applyState {
        case .idle: EmptyView()
        case .checking: HStack { ProgressView().controlSize(.small); Text(L10n.applying) }
        case .saved(let note): Text(note.isEmpty ? L10n.applySaved : note)
        case let .succeeded(c, tz): Label(L10n.applySucceeded(c, tz), systemImage: "checkmark.circle").foregroundStyle(.green)
        case let .failed(t, rb):
            VStack(alignment: .leading, spacing: 4) {
                ErrorBlock(text: t)
                switch rb {
                case .done: Label(L10n.rollbackDone, systemImage: "arrow.uturn.backward.circle")
                case .incomplete: Label(L10n.rollbackIncomplete, systemImage: "exclamationmark.triangle").foregroundStyle(.orange)
                case .none: EmptyView()
                }
            }
        }
    }

    @ViewBuilder private var checkStateView: some View {
        switch model.checkState {
        case .idle: EmptyView()
        case .checking: HStack { ProgressView().controlSize(.small); Text(L10n.checking) }
        case let .ok(rule, c, tz): Text(L10n.checkOK(c, tz, rule))
        case .failed(let t): ErrorBlock(text: t)
        }
    }
}

struct LANSheet: View {
    let prompt: LANPrompt
    let confirm: () -> Void
    let cancel: () -> Void
    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(L10n.lanTitle).font(.headline)
            Text(L10n.lanText(prompt.name))
            HStack {
                Spacer()
                Button(L10n.cancel, action: cancel).keyboardShortcut(.cancelAction)
                Button(L10n.confirm, action: confirm)
            }
        }
        .padding(20)
        .frame(minWidth: 420)
    }
}
