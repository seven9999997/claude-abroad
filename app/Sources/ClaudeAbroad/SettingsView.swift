// 设置窗口（⌘,；design 11.2，原型 ui/design/index.html 的 prefsWindow）：顶部是带图标的页签（通用、环境检测、回国与 Claude、高级），
// 每页是分组表单。颜色只从 Palette 取；需要留意、失败的说明都同时带图标和文字。
import AbroadKit
import AppKit
import SwiftUI

struct SettingsView: View {
    @ObservedObject var model: SettingsModel
    @ObservedObject var env: EnvModel
    @ObservedObject var diagnostics: DiagnosticsExportModel
    @ObservedObject var privileged: PrivilegedSession
    @ObservedObject var service: ServiceInstallModel

    var body: some View {
        TabView {
            general.tabItem { Label(L10n.settingsGeneral, systemImage: "gearshape") }
            envTab.tabItem { Label(L10n.settingsEnv, systemImage: "checkmark.shield") }
            claude.tabItem { Label(L10n.settingsClaude, systemImage: "house") }
            advanced.tabItem { Label(L10n.settingsAdvanced, systemImage: "cpu") }
        }
        .frame(width: 680, height: 560)
        .tint(Palette.accent)
        .task { await model.load(); await service.refresh() }
        .sheet(isPresented: Binding(get: { privileged.request?.host == .settings },
                                    set: { if !$0, privileged.phase != .running { Task { await privileged.userClosed() } } })) {
            PrivilegedSheet(session: privileged)
        }
        .sheet(isPresented: Binding(get: { model.terminal != nil }, set: { if !$0 { Task { await model.dismissTerminal() } } })) {
            if let t = model.terminal { TerminalSheet(prompt: t) { Task { await model.dismissTerminal() } } }
        }
    }

    // MARK: - 通用

    private var general: some View {
        PageScroll(maxWidth: 640) {
            CardSection(nil) {
                CardRow(verticalPadding: 12) {
                    Image(nsImage: NSApplication.shared.applicationIconImage)
                        .resizable()
                        .frame(width: 34, height: 34)
                        .clipShape(RoundedRectangle(cornerRadius: 8, style: .continuous))
                        .shadow(color: Palette.cardShadow, radius: 3, x: 0, y: 1)
                        .accessibilityHidden(true)
                    Text(L10n.appName).font(.system(size: 15, weight: .bold))
                    Spacer()
                }
                RowSeparator()
                CardRow {
                    RowLabel(title: L10n.notificationsToggle)
                    Toggle(L10n.notificationsToggle, isOn: $model.notificationsEnabled)
                        .labelsHidden()
                        .toggleStyle(.switch)
                }
            }
            if let on = model.allowLAN { // 补齐三
                CardSection(nil, footer: L10n.allowLANHelp) {
                    CardRow {
                        RowLabel(title: L10n.allowLANTitle)
                        Toggle(L10n.allowLANTitle, isOn: Binding(get: { on }, set: { v in Task { await model.setAllowLAN(v) } }))
                            .labelsHidden()
                            .toggleStyle(.switch)
                            .disabled(model.busy)
                    }
                }
            }
            if let s = model.signatureText {
                CardSection(nil) {
                    CardRow(alignment: .top) {
                        IconBadge(symbol: "lock.open", tone: .attention, size: 28)
                        Text(s)
                            .font(.caption)
                            .foregroundStyle(.secondary)
                            .lineSpacing(1.5)
                            .fixedSize(horizontal: false, vertical: true)
                            .frame(maxWidth: .infinity, alignment: .leading)
                    }
                }
            }
        }
    }

    // MARK: - 环境检测

    private var envTab: some View {
        PageScroll(maxWidth: 640) {
            CardSection(L10n.envWarn, icon: "exclamationmark.triangle") {
                ForEach(Array(env.warnRows.enumerated()), id: \.element.id) { i, r in
                    if i > 0 { RowSeparator() }
                    CardRow {
                        HStack(spacing: 8) {
                            InfoPill(text: r.id, monospaced: true)
                            Text(r.name).font(.system(size: 13, weight: .medium))
                        }
                        Spacer()
                        Toggle(r.id + " " + r.name, isOn: Binding(get: { r.enabled }, set: { on in Task { await env.setEnabled(r.id, on) } }))
                            .labelsHidden()
                            .toggleStyle(.switch)
                    }
                }
            }
            CardSection(L10n.envHard, icon: "lock.shield") {
                CardRow(alignment: .top) {
                    IconBadge(symbol: "lock", tone: .local, size: 28)
                    Text(L10n.hardItemLocked)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineSpacing(1.5)
                        .fixedSize(horizontal: false, vertical: true)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
            }
        }
    }

    // MARK: - 回国与 Claude

    private var claude: some View {
        PageScroll(maxWidth: 640) {
            CardSection(L10n.processList, icon: "list.bullet.rectangle") {
                if let t = model.processListText {
                    CardRow { Text(t).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true) }
                }
                ForEach(model.processes, id: \.pid) { p in
                    RowSeparator()
                    CardRow(verticalPadding: 8) {
                        InfoPill(text: "\(p.pid)", monospaced: true)
                        Text("\(p.app)  \(p.cmd)")
                            .font(.system(size: 12, design: .monospaced))
                            .frame(maxWidth: .infinity, alignment: .leading)
                    }
                }
                if !model.unverified.isEmpty {
                    RowSeparator()
                    CardRow { Text(L10n.processesUnverified).font(.caption).fontWeight(.semibold) }
                    ForEach(model.unverified, id: \.pid) { p in
                        RowSeparator()
                        CardRow(verticalPadding: 8) {
                            InfoPill(text: "\(p.pid)", monospaced: true)
                            Text("\(p.app)  \(p.note ?? "")")
                                .font(.system(size: 12, design: .monospaced))
                                .foregroundStyle(.secondary)
                                .frame(maxWidth: .infinity, alignment: .leading)
                        }
                    }
                }
            }
            CardSection(nil) {
                CardRow(alignment: .top) {
                    RowLabel(title: L10n.launchControl, bold: true)
                    Text(model.launchControlText)
                        .font(.callout)
                        .foregroundStyle(.secondary)
                        .lineSpacing(1.5)
                        .fixedSize(horizontal: false, vertical: true)
                        .frame(maxWidth: 340, alignment: .trailing)
                }
                RowSeparator()
                CardRow(alignment: .top) {
                    RowLabel(title: L10n.resolverLockTitle, bold: true)
                    status(model.resolverLockText, tone: model.resolverLockWarn ? .attention : nil)
                }
            }
            CardSection(L10n.claudeCodeTitle, icon: "terminal") {
                CardRow { status(model.claudeCodeText, tone: model.claudeCodeWarn ? .attention : nil, leading: true) }
                if model.claudeCodeSwitchShown { // v0.68：第四道是开关、默认关；两个方向都要本人授权（B0：应用里点了先弹确认框，再经系统密码框执行）
                    RowSeparator()
                    CardRow {
                        Text(model.claudeCodeConsequenceText)
                            .font(.caption)
                            .foregroundStyle(.secondary)
                            .lineSpacing(2)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                    RowSeparator()
                    CardRow {
                        Button(L10n.claudeCodeTurnOn) { Task { await model.userTappedPrivileged(.claudeProxy(on: true)) } }
                            .buttonStyle(SecondaryButtonStyle(compact: true))
                        Button(L10n.claudeCodeTurnOff) { Task { await model.userTappedPrivileged(.claudeProxy(on: false)) } }
                            .buttonStyle(SecondaryButtonStyle(compact: true))
                        Spacer()
                    }
                    ForEach(model.claudeCodeCommands, id: \.command) { c in
                        RowSeparator()
                        CardRow {
                            VStack(alignment: .leading, spacing: 6) {
                                Text(c.label).font(.caption).foregroundStyle(.secondary)
                                CodeBlock(text: c.command)
                            }
                        }
                    }
                }
            }
            if model.returnBlockText != nil || model.prepareText != nil {
                CardSection(nil) {
                    if let t = model.returnBlockText {
                        CardRow(alignment: .top) {
                            RowLabel(title: L10n.returnBlockTitle, bold: true)
                            Text(t)
                                .font(.callout)
                                .foregroundStyle(.secondary)
                                .fixedSize(horizontal: false, vertical: true)
                                .frame(maxWidth: 340, alignment: .trailing)
                        }
                    }
                    if let t = model.prepareText {
                        if model.returnBlockText != nil { RowSeparator() }
                        CardRow(alignment: .top) {
                            RowLabel(title: L10n.prepareTitle, bold: true)
                            status(t, tone: model.prepareWarn ? .attention : nil)
                        }
                    }
                }
            }
            CardSection(L10n.claudeDomainsTitle, icon: "network", footer: model.claudeDomains?.note) {
                ForEach(model.claudeDomainLines, id: \.self) { l in
                    CardRow(verticalPadding: 8) {
                        Text(l).font(.system(size: 12, design: .monospaced))
                    }
                    RowSeparator()
                }
                CardRow {
                    Spacer()
                    Button(L10n.claudeDomainsClear) { Task { await model.clearClaudeDomains() } }
                        .buttonStyle(SecondaryButtonStyle(compact: true))
                        .disabled((model.claudeDomains?.count ?? 0) == 0)
                }
            }
            CardSection(nil) {
                CardRow(alignment: .top) {
                    RowLabel(title: L10n.integrityTitle, bold: true)
                    status(model.integrityText, tone: model.integrityWarn ? .failure : nil)
                }
            }
        }
    }

    /// 状态文字：需要留意或失败时加图标并上色（颜色之外有图形），否则是普通的次要文字。
    @ViewBuilder private func status(_ text: String, tone: StatusTone?, leading: Bool = false) -> some View {
        let align: Alignment = leading ? .leading : .trailing
        if let tone {
            ToneNote(tone: tone, text: text, symbol: tone == .failure ? "xmark.octagon" : "exclamationmark.triangle")
                .frame(maxWidth: leading ? .infinity : 340, alignment: align)
        } else {
            Text(text)
                .font(.callout)
                .foregroundStyle(.secondary)
                .lineSpacing(1.5)
                .fixedSize(horizontal: false, vertical: true)
                .frame(maxWidth: leading ? .infinity : 340, alignment: align)
        }
    }

    // MARK: - 高级

    private var advanced: some View {
        PageScroll(maxWidth: 640) {
            CardSection(L10n.cliTitle, icon: "terminal") {
                CardRow(alignment: .top) {
                    IconBadge(symbol: "terminal", size: 30)
                    VStack(alignment: .leading, spacing: 5) {
                        Text(model.cliPath)
                            .font(.system(size: 12, design: .monospaced))
                            .textSelection(.enabled)
                            .fixedSize(horizontal: false, vertical: true)
                        Text(model.cliUsage)
                            .font(.caption)
                            .foregroundStyle(.secondary)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                    .frame(maxWidth: .infinity, alignment: .leading)
                }
                if let s = model.signatureText {
                    RowSeparator()
                    CardRow { Text(s).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true) }
                }
            }
            CardSection(L10n.diagnostics, icon: "doc.zipper", footer: diagnostics.help) { // 补齐五：应用里导出（只写用户在保存面板里选的那个文件）
                CardRow {
                    RowLabel(title: L10n.diagnosticsIncludeDomains)
                    Toggle(L10n.diagnosticsIncludeDomains, isOn: $diagnostics.includeDomains)
                        .labelsHidden()
                        .toggleStyle(.switch)
                }
                RowSeparator()
                CardRow {
                    if let m = diagnostics.message {
                        if diagnostics.isError {
                            ToneNote(tone: .failure, text: m, symbol: "xmark.octagon")
                        } else {
                            ToneNote(tone: .success, text: m, symbol: "checkmark.circle")
                        }
                    }
                    Spacer()
                    Button { Task { await diagnostics.export() } } label: {
                        Label(L10n.diagnosticsExport, systemImage: "square.and.arrow.up")
                    }
                    .buttonStyle(SecondaryButtonStyle())
                    .disabled(diagnostics.busy)
                }
            }
            CardSection(L10n.serviceSection, icon: "gearshape.2") { // 阶段 B（v0.71c）：后台服务的版本、升级和卸载
                CardRow(alignment: .top) {
                    IconBadge(symbol: "gearshape.2", size: 30)
                    Text(service.text)
                        .font(.callout)
                        .lineSpacing(1.5)
                        .fixedSize(horizontal: false, vertical: true)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
                if service.showsUpgrade || service.showsInstall || service.showsUninstall {
                    RowSeparator()
                    CardRow {
                        if service.showsInstall {
                            Button(L10n.serviceInstallButton) { Task { await service.userTappedInstall(host: .settings) } }
                                .buttonStyle(PrimaryButtonStyle(compact: true))
                        }
                        if service.showsUpgrade {
                            Button(L10n.serviceUpgradeButton) { Task { await service.userTappedUpgrade(host: .settings) } }
                                .buttonStyle(PrimaryButtonStyle(compact: true))
                                .disabled(!service.upgradeEnabled)
                        }
                        Spacer()
                        if service.showsUninstall {
                            Button(L10n.serviceUninstallButton) { Task { await service.userTappedUninstall(host: .settings) } }
                                .buttonStyle(SecondaryButtonStyle(compact: true, tone: .blocked))
                        }
                    }
                }
                RowSeparator()
                CardRow {
                    Text(L10n.serviceBoundary)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineSpacing(1.5)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
            CardSection(L10n.uninstall, icon: "trash") {
                CardRow(alignment: .top) {
                    IconBadge(symbol: "trash", tone: .blocked, size: 30)
                    Text(model.uninstallText)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineSpacing(1.5)
                        .fixedSize(horizontal: false, vertical: true)
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
            }
        }
    }
}
