// 其余页面（M5-9）：分流出口、应用分流、环境检测、地区、引擎与虚拟机。文字都来自 L10n；会话里只编译。
import AbroadAPI
import AbroadKit
import SwiftUI
import UniformTypeIdentifiers

/// 加载状态的统一显示：后台不可达、不提供、加载失败都有说明和“重试”，不出现空白页。
struct LoadStateView<Content: View>: View {
    let state: LoadState
    let retry: () -> Void
    @ViewBuilder let content: () -> Content

    var body: some View {
        switch state {
        case .loading: ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
        case .loaded: content()
        case .unreachable: message(L10n.unreachableTitle, L10n.backendDownDetail)
        case .unsupported(let t): message(L10n.unsupportedTitle, t.raw)
        case .failed(let t): VStack { ErrorBlock(text: t); Button(L10n.retry, action: retry) }.padding()
        }
    }

    private func message(_ title: String, _ detail: String) -> some View {
        VStack(spacing: 8) {
            Banner(title: title, detail: detail, symbol: "exclamationmark.circle")
            Button(L10n.retry, action: retry)
        }
        .padding()
    }
}

/// 页面共用的提示、终端退路、确认框。
struct PageChrome: ViewModifier {
    @ObservedObject var model: PageModel
    func body(content: Content) -> some View {
        content
            .safeAreaInset(edge: .bottom) {
                if let n = model.notice { ErrorBlock(text: n.text).padding(.horizontal) }
            }
            .sheet(isPresented: Binding(get: { model.terminal != nil }, set: { if !$0 { Task { await model.dismissTerminal() } } })) {
                if let t = model.terminal { TerminalSheet(prompt: t) { Task { await model.dismissTerminal() } } }
            }
    }
}

// MARK: - 分流出口

struct ExitsPage: View {
    @ObservedObject var model: ExitsModel
    var body: some View {
        LoadStateView(state: model.state, retry: { Task { await model.load() } }) {
            ScrollView {
                VStack(alignment: .leading, spacing: 12) {
                    if let e = model.emptyText { Text(e).foregroundStyle(.secondary) }
                    ForEach(model.rows) { r in
                        GroupBox {
                            VStack(alignment: .leading, spacing: 4) {
                                HStack {
                                    Text(r.tag).bold()
                                    Text(r.kind).foregroundStyle(.secondary)
                                    Spacer()
                                    Label(r.up ? L10n.exitUp : L10n.exitDown, systemImage: r.up ? "checkmark.circle" : "xmark.circle")
                                        .foregroundStyle(r.up ? .green : .red)
                                }
                                if !r.reason.isEmpty { Text(r.reason).font(.caption) }
                                if !r.items.isEmpty {
                                    Text(r.items.sorted(by: { $0.key < $1.key }).map { $0.key + " " + EnvModel.resultTitle($0.value) }.joined(separator: " · ")).font(.caption)
                                }
                                Text(L10n.exitConns(r.tcp, r.udp)).font(.caption).foregroundStyle(.secondary)
                                HStack {
                                    Button(L10n.exitCheck) { Task { await model.check(r.tag) } }
                                    Button(L10n.delete) { Task { await model.remove(r.tag) } }
                                }
                            }
                            .frame(maxWidth: .infinity, alignment: .leading)
                        }
                    }
                    GroupBox(L10n.exitAdd) {
                        VStack(alignment: .leading) {
                            TextField(L10n.exitTag, text: $model.formTag)
                            Picker(L10n.exitKind, selection: $model.formKind) {
                                ForEach(ExitsModel.kinds, id: \.self) { Text(verbatim: $0).tag($0) }
                            }
                            HStack {
                                Button(L10n.chooseFile) {
                                    let p = NSOpenPanel()
                                    if p.runModal() == .OK { model.formFile = p.url }
                                }
                                Text(model.formFile?.lastPathComponent ?? "").foregroundStyle(.secondary)
                                Spacer()
                                Button(L10n.submit) { Task { await model.add() } }.disabled(model.busy)
                            }
                        }
                    }
                }
                .padding()
            }
        }
        .modifier(PageChrome(model: model))
        .task { await model.load() }
    }
}

// MARK: - 应用分流

struct AppsPage: View {
    @ObservedObject var model: AppsModel
    var body: some View {
        LoadStateView(state: model.state, retry: { Task { await model.load() } }) {
            VStack(alignment: .leading, spacing: 8) {
                List(model.rows) { r in
                    HStack {
                        AppIcon(app: r.app)
                        VStack(alignment: .leading) {
                            Text(r.name)
                            Text(r.app).font(.caption).foregroundStyle(.secondary)
                        }
                        Spacer()
                        if r.conns > 0 { Text("\(r.conns)").font(.caption.monospacedDigit()) }
                        Menu(Self.title(r.action)) {
                            ForEach(model.actions, id: \.self) { a in Button(Self.title(a)) { Task { await model.setRule(app: r.app, action: a) } } }
                            if r.action != nil { Divider(); Button(L10n.delete) { Task { await model.removeRule(app: r.app) } } }
                        }
                        .fixedSize()
                    }
                }
                .onDrop(of: [UTType.fileURL], isTargeted: nil) { providers in
                    for p in providers {
                        _ = p.loadObject(ofClass: URL.self) { url, _ in
                            if let url { Task { @MainActor in await model.addDropped(url, action: "proxy:main") } }
                        }
                    }
                    return true
                }
                Text(L10n.dropAppHint).font(.caption).foregroundStyle(.secondary)
                Toggle(L10n.dropAffected, isOn: $model.dropAffected)
                if let r = model.authorizationReason {
                    Text(L10n.authorizationReason).bold()
                    ErrorBlock(text: r)
                }
                VStack(alignment: .leading, spacing: 2) {
                    Text(L10n.directClaudeDomains)
                    Text(L10n.claudeNoDirect)
                    Text(L10n.proxyNoDirect)
                }
                .font(.caption).foregroundStyle(.secondary)
            }
            .padding()
        }
        .modifier(PageChrome(model: model))
        .task { await model.load() }
    }

    static func title(_ action: String?) -> String {
        switch action {
        case "direct": return L10n.actionDirect
        case "block": return L10n.actionBlock
        case "proxy:main": return L10n.actionMain
        case nil: return L10n.actionDefault
        case let a? where a.hasPrefix("proxy:"): return L10n.actionExit(String(a.dropFirst("proxy:".count)))
        case let a?: return a
        }
    }
}

/// 应用图标（本机取，只是显示）。
struct AppIcon: View {
    let app: String
    var body: some View {
        if let url = NSWorkspace.shared.urlForApplication(withBundleIdentifier: app) {
            Image(nsImage: NSWorkspace.shared.icon(forFile: url.path)).resizable().frame(width: 20, height: 20)
        } else {
            Image(systemName: "app").frame(width: 20, height: 20)
        }
    }
}

// MARK: - 环境检测

struct EnvPage: View {
    @ObservedObject var model: EnvModel
    var body: some View {
        LoadStateView(state: model.state, retry: { Task { await model.load() } }) {
            List {
                Section(L10n.envHard) { ForEach(model.hardRows) { row($0) } }
                Section(L10n.envWarn) { ForEach(model.warnRows) { row($0) } }
                Section(L10n.envDevice) { Text(model.deviceCheckText).foregroundStyle(.secondary) }
            }
            .toolbar { Button(L10n.envRerun) { Task { await model.rerun() } }.disabled(model.busy) }
        }
        .modifier(PageChrome(model: model))
        .task { await model.load() }
    }

    private func row(_ r: EnvRow) -> some View {
        HStack(alignment: .top) {
            VStack(alignment: .leading, spacing: 2) {
                Text(r.id + " " + r.name).bold()
                Text(EnvModel.resultTitle(r.result))
                if !r.detail.isEmpty { Text(r.detail).font(.caption) }
                if !r.fix.isEmpty { Text(r.fix).font(.caption).foregroundStyle(.secondary) }
            }
            Spacer()
            if r.hard {
                Toggle("", isOn: .constant(true)).labelsHidden().disabled(true).help(L10n.hardItemLocked)
            } else {
                Toggle("", isOn: Binding(get: { r.enabled }, set: { on in Task { await model.setEnabled(r.id, on) } })).labelsHidden()
            }
        }
    }
}

// MARK: - 地区

struct RegionPage: View {
    @ObservedObject var model: RegionModel
    var body: some View {
        LoadStateView(state: model.state, retry: { Task { await model.load() } }) {
            Form {
                if let t = model.targetText {
                    Text(t)
                    if let s = model.sourceText { Text(L10n.regionSource + "：" + s) }
                } else {
                    Text(L10n.regionNone)
                }
                if !model.pending.isEmpty {
                    Section(L10n.regionPending) { ForEach(model.pending, id: \.self) { Text($0) } }
                }
                Section {
                    TextField(L10n.regionTimezone, text: $model.formTimezone)
                    TextField(L10n.regionLang, text: $model.formLang)
                    HStack {
                        Button(L10n.regionSet) { Task { await model.setManual() } }
                        Button(L10n.regionFollowExit) { Task { await model.followExit() } }
                    }
                    .disabled(model.busy)
                    if model.switched { Text(L10n.regionSwitched).foregroundStyle(.green) }
                }
            }
            .padding()
        }
        .modifier(PageChrome(model: model))
        .task { await model.load() }
    }
}

// MARK: - 引擎与虚拟机

struct EnginesPage: View {
    @ObservedObject var model: EnginesModel
    var body: some View {
        LoadStateView(state: model.state, retry: { Task { await model.load() } }) {
            Form {
                if let t = model.vmText {
                    Text(t).foregroundStyle(.secondary)
                } else if let v = model.view {
                    Text(L10n.vmVersion + "：" + (v.version ?? ""))
                    Text(L10n.vmKernel + "：" + (v.kernel ?? ""))
                    Text(L10n.vmUptime + "：" + L10n.seconds(v.uptimeS ?? 0))
                    if v.bootChanged { Text(L10n.vmBootChanged).foregroundStyle(.orange) }
                    ForEach(v.engines ?? [], id: \.name) { e in Text(L10n.engineLine(e.name, e.state, e.restarts)) }
                }
            }
            .padding()
        }
        .task { await model.load() }
    }
}
