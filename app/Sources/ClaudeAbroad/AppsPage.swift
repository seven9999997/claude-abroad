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
                CardSection(L10n.liveApps, icon: "square.grid.2x2") {
                    ForEach(Array(model.appRows.enumerated()), id: \.element.id) { i, r in
                        if i > 0 { RowSeparator() }
                        appRow(r)
                    }
                    if !model.systemRows.isEmpty { // 补齐四：系统进程单列在后面，默认折叠
                        if !model.appRows.isEmpty { RowSeparator() }
                        DisclosureGroup {
                            VStack(spacing: 0) {
                                ForEach(model.systemRows) { r in
                                    RowSeparator(inset: 0)
                                    appRow(r)
                                }
                            }
                        } label: {
                            HStack(spacing: 8) {
                                Image(systemName: "gearshape.2")
                                    .foregroundStyle(.secondary)
                                Text(L10n.appsSystemSection)
                                    .fontWeight(.medium)
                                InfoPill(text: "\(model.systemRows.count)", monospaced: true)
                            }
                        }
                        .padding(.horizontal, 16)
                        .padding(.vertical, 10)
                    }
                    if !model.appRows.isEmpty || !model.systemRows.isEmpty { RowSeparator() }
                    HStack(spacing: 8) {
                        Image(systemName: "square.and.arrow.down")
                            .foregroundStyle(Palette.accent)
                        Text(L10n.dropAppHint)
                            .font(.callout)
                            .foregroundStyle(.secondary)
                    }
                    .frame(maxWidth: .infinity)
                    .padding(.vertical, 14)
                    .padding(.horizontal, 16)
                    .background(RoundedRectangle(cornerRadius: 8, style: .continuous).fill(Palette.graySoft))
                    .overlay(
                        RoundedRectangle(cornerRadius: 8, style: .continuous)
                            .strokeBorder(Palette.controlLine, style: StrokeStyle(lineWidth: 1, dash: [5, 4]))
                    )
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
                    CardRow {
                        RowLabel(title: L10n.dropAffected)
                        Toggle(L10n.dropAffected, isOn: $model.dropAffected)
                            .labelsHidden()
                            .toggleStyle(.switch)
                    }
                }

                if let r = model.authorizationReason {
                    VStack(alignment: .leading, spacing: 8) {
                        Text(L10n.authorizationReason).font(.system(size: 13, weight: .semibold)).padding(.horizontal, 4)
                        ErrorBlock(text: r)
                    }
                }

                CardSection(nil) {
                    CardRow(alignment: .top) {
                        IconBadge(symbol: "info.circle", size: 26)
                        VStack(alignment: .leading, spacing: 4) {
                            Text(L10n.directClaudeDomains)
                            Text(L10n.claudeNoDirect)
                            Text(L10n.proxyNoDirect)
                        }
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineSpacing(2)
                        .fixedSize(horizontal: false, vertical: true)
                    }
                }

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
            VStack(alignment: .leading, spacing: 3) {
                Text(r.name).font(.system(size: 13, weight: .semibold))
                Text(r.app)
                    .font(.system(size: 11, design: .monospaced))
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
                    .truncationMode(.middle)
                if let s = r.shared {
                    Text(s)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                }
                if let f = r.fallback {
                    ToneNote(tone: .attention, text: f, symbol: "arrow.uturn.backward.circle")
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)

            if r.conns > 0 {
                InfoPill(text: "\(r.conns)", monospaced: true)
                    .padding(.top, 2)
            }

            Menu {
                ForEach(model.actions, id: \.self) { a in
                    Button(Self.title(a)) { Task { await model.requestRule(app: r.app, action: a) } }
                }
                if r.action != nil {
                    Divider()
                    Button(L10n.delete) { Task { await model.requestRule(app: r.app, action: nil) } }
                }
            } label: {
                HStack(spacing: 5) {
                    Text(Self.title(r.action))
                        .font(.system(size: 12, weight: .medium))
                    Image(systemName: "chevron.up.chevron.down")
                        .font(.system(size: 9, weight: .semibold))
                        .foregroundStyle(.secondary)
                }
                .padding(.horizontal, 10)
                .padding(.vertical, 5)
                .background(RoundedRectangle(cornerRadius: 7, style: .continuous).fill(Palette.control))
                .overlay(RoundedRectangle(cornerRadius: 7, style: .continuous).strokeBorder(Palette.controlLine, lineWidth: 1))
            }
            .menuStyle(.borderlessButton)
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
            HStack(spacing: 8) {
                IconBadge(symbol: "list.bullet.rectangle", size: 26)
                Text(L10n.rulesListsTitle)
                    .font(.system(size: 15, weight: .bold))
                    .accessibilityAddTraits(.isHeader)
            }
            .padding(.horizontal, 4)
            .padding(.top, 8)

            ForEach(AppsModel.RuleList.allCases, id: \.self) { l in
                CardSection(AppsModel.title(l), icon: listSymbol(l), footer: AppsModel.help(l)) {
                    if l == .pinned {
                        CardRow {
                            Text(L10n.rulesPinnedBuiltin + "：" + model.builtinPinned.joined(separator: L10n.listSeparator))
                                .font(.caption)
                                .foregroundStyle(.secondary)
                                .lineSpacing(1.5)
                                .fixedSize(horizontal: false, vertical: true)
                        }
                        RowSeparator()
                    }
                    ForEach(model.items(l), id: \.self) { item in
                        CardRow(verticalPadding: 8) {
                            Text(item)
                                .font(.system(size: 12, weight: .medium, design: .monospaced))
                                .textSelection(.enabled)
                            Spacer()
                            Button(L10n.delete) { Task { await model.deleteListItem(l, item) } }
                                .buttonStyle(SecondaryButtonStyle(compact: true, tone: .blocked))
                                .disabled(model.busy)
                        }
                        RowSeparator()
                    }
                    CardRow {
                        StyledTextField(
                            placeholder: L10n.rulesAddPlaceholder,
                            text: Binding(get: { model.drafts[l] ?? "" }, set: { model.drafts[l] = $0 }),
                            monospaced: true
                        )
                        Button(L10n.rulesAddPlaceholder) { Task { await model.addDraft(l) } }
                            .buttonStyle(SecondaryButtonStyle(compact: true))
                            .disabled(model.busy || (model.drafts[l] ?? "").trimmingCharacters(in: .whitespaces).isEmpty)
                    }
                }
            }

            CardSection(nil, footer: L10n.rulesUDP443Help) {
                CardRow {
                    RowLabel(title: L10n.rulesUDP443Title)
                    Toggle(L10n.rulesUDP443Title, isOn: Binding(get: { model.rules?.blockUDP443 ?? false },
                                                                  set: { v in Task { await model.setBlockUDP443(v) } }))
                        .toggleStyle(.switch)
                        .labelsHidden()
                        .disabled(model.busy || model.rules == nil)
                }
            }
        }
    }

    private func listSymbol(_ l: AppsModel.RuleList) -> String {
        switch l {
        case .local: return "house"
        case .pinned: return "pin"
        case .nofake: return "network"
        case .offline: return "shield.checkered"
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
                ZStack {
                    RoundedRectangle(cornerRadius: 7, style: .continuous).fill(Palette.graySoft)
                    Image(systemName: "app.dashed").font(.system(size: 16)).foregroundStyle(.secondary)
                }
            }
        }
        .frame(width: 30, height: 30)
        .clipShape(RoundedRectangle(cornerRadius: 7, style: .continuous))
        .accessibilityHidden(true)
    }
}
