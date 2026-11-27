// 应用分流（原型 ui/design/index.html 的 pageApps）：应用逐行列在一个分组框里（图标、名字、应用 ID、连接数、去向的弹出菜单），
// 框底是拖入应用的提示；系统进程单列、默认折叠；下面是说明和“域名与其他规则”四小节。
import AbroadKit
import SwiftUI
import UniformTypeIdentifiers

struct AppsPage: View {
    @ObservedObject var model: AppsModel
    var body: some View {
        LoadStateView(state: model.state, retry: { Task { await model.load() } }) {
            PageScroll {
                CardSection(nil, footer: nil) {
                    ForEach(Array(model.appRows.enumerated()), id: \.element.id) { i, r in
                        if i > 0 { RowSeparator() }
                        appRow(r)
                    }
                    if !model.systemRows.isEmpty { // 补齐四：系统进程单列在后面，默认折叠
                        if !model.appRows.isEmpty { RowSeparator() }
                        DisclosureGroup(L10n.appsSystemSection) {
                            ForEach(model.systemRows) { r in
                                RowSeparator()
                                appRow(r)
                            }
                        }
                        .padding(.horizontal, 12)
                        .padding(.vertical, 8)
                    }
                    if !model.appRows.isEmpty || !model.systemRows.isEmpty { RowSeparator() }
                    HStack {
                        Spacer()
                        Label(L10n.dropAppHint, systemImage: "square.and.arrow.down").font(.caption).foregroundStyle(.secondary)
                        Spacer()
                    }
                    .padding(12)
                }
                .onDrop(of: [UTType.fileURL], isTargeted: nil) { providers in
                    for p in providers {
                        _ = p.loadObject(ofClass: URL.self) { url, _ in
                            if let url { Task { @MainActor in await model.addDropped(url, action: "proxy:main") } }
                        }
                    }
                    return true
                }
                CardSection(nil) {
                    CardRow { Toggle(L10n.dropAffected, isOn: $model.dropAffected) }
                }
                if let r = model.authorizationReason {
                    VStack(alignment: .leading, spacing: 6) {
                        Text(L10n.authorizationReason).bold()
                        ErrorBlock(text: r)
                    }
                }
                VStack(alignment: .leading, spacing: 2) {
                    Text(L10n.directClaudeDomains)
                    Text(L10n.claudeNoDirect)
                    Text(L10n.proxyNoDirect)
                }
                .font(.caption).foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
                .padding(.horizontal, 4)
                RuleListsSection(model: model)
            }
            .alert(L10n.sharedRuleWarnTitle, isPresented: Binding(get: { model.sharedPrompt != nil }, set: { if !$0 { model.cancelShared() } })) {
                Button(L10n.sharedRuleConfirm) { Task { await model.confirmShared() } }
                Button(L10n.cancel, role: .cancel) { model.cancelShared() }
            } message: {
                Text(model.sharedPrompt?.text ?? "")
            }
        }
        .modifier(PageChrome(model: model))
        .task { await model.load() }
    }

    func appRow(_ r: AppRow) -> some View {
        CardRow(alignment: .top) {
            AppIcon(app: r.app)
            VStack(alignment: .leading, spacing: 2) {
                Text(r.name).fontWeight(.semibold)
                Text(r.app).font(.caption.monospaced()).foregroundStyle(.secondary).lineLimit(1).truncationMode(.middle)
                if let s = r.shared { Text(s).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true) } // 补齐四：共用的系统进程
                if let f = r.fallback { ToneNote(tone: .attention, text: f, symbol: "arrow.uturn.backward.circle") } // 补齐二
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            if r.conns > 0 { Text("\(r.conns)").font(.caption.monospacedDigit()).foregroundStyle(.secondary).padding(.top, 3) }
            Menu(Self.title(r.action)) {
                ForEach(model.actions, id: \.self) { a in Button(Self.title(a)) { Task { await model.requestRule(app: r.app, action: a) } } }
                if r.action != nil { Divider(); Button(L10n.delete) { Task { await model.requestRule(app: r.app, action: nil) } } }
            }
            .fixedSize()
        }
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

/// 规则里的四项（补齐一）：本地域名名单、置顶域名（内置只读、追加可删）、离线放行名单、拦截 UDP 443。文字都来自文字表。
/// 每个名单一个分组（标题、说明、现有的项、添加输入框）。
struct RuleListsSection: View {
    @ObservedObject var model: AppsModel

    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            Text(L10n.rulesListsTitle).font(.system(size: 15, weight: .bold)).padding(.horizontal, 4).padding(.top, 6)
                .accessibilityAddTraits(.isHeader)
            ForEach(AppsModel.RuleList.allCases, id: \.self) { l in
                CardSection(AppsModel.title(l), footer: AppsModel.help(l)) {
                    if l == .pinned {
                        CardRow {
                            Text(L10n.rulesPinnedBuiltin + "：" + model.builtinPinned.joined(separator: L10n.listSeparator))
                                .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                        }
                        RowSeparator()
                    }
                    ForEach(model.items(l), id: \.self) { item in
                        CardRow {
                            Text(item).font(.callout.monospaced())
                            Spacer()
                            Button(L10n.delete) { Task { await model.deleteListItem(l, item) } }.disabled(model.busy)
                        }
                        RowSeparator()
                    }
                    CardRow {
                        TextField(L10n.rulesAddPlaceholder, text: Binding(get: { model.drafts[l] ?? "" }, set: { model.drafts[l] = $0 }))
                            .labelsHidden()
                        Button(L10n.rulesAddPlaceholder) { Task { await model.addDraft(l) } }
                            .disabled(model.busy || (model.drafts[l] ?? "").trimmingCharacters(in: .whitespaces).isEmpty)
                    }
                }
            }
            CardSection(nil, footer: L10n.rulesUDP443Help) {
                CardRow {
                    Text(L10n.rulesUDP443Title)
                    Spacer()
                    Toggle(L10n.rulesUDP443Title, isOn: Binding(get: { model.rules?.blockUDP443 ?? false },
                                                                  set: { v in Task { await model.setBlockUDP443(v) } }))
                        .toggleStyle(.switch)
                        .labelsHidden()
                        .disabled(model.busy || model.rules == nil)
                }
            }
        }
    }
}

/// 应用图标（本机取，只是显示）。
struct AppIcon: View {
    let app: String
    var body: some View {
        Group {
            if let url = NSWorkspace.shared.urlForApplication(withBundleIdentifier: app) {
                Image(nsImage: NSWorkspace.shared.icon(forFile: url.path)).resizable()
            } else {
                Image(systemName: "app.dashed").font(.system(size: 18)).foregroundStyle(.secondary)
            }
        }
        .frame(width: 24, height: 24)
        .accessibilityHidden(true)
    }
}
