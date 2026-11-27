// 设置窗口（⌘,；design 11.2，原型 ui/design/index.html 的 prefsWindow）：顶部是带图标的页签（通用、环境检测、回国与 Claude、高级），
// 每页是分组表单。颜色只从 Palette 取；需要留意、失败的说明都同时带图标和文字。
import AbroadKit
import SwiftUI

struct SettingsView: View {
    @ObservedObject var model: SettingsModel
    @ObservedObject var env: EnvModel
    @ObservedObject var diagnostics: DiagnosticsExportModel
    @ObservedObject var privileged: PrivilegedSession

    var body: some View {
        TabView {
            general.tabItem { Label(L10n.settingsGeneral, systemImage: "gearshape") }
            envTab.tabItem { Label(L10n.settingsEnv, systemImage: "checkmark.shield") }
            claude.tabItem { Label(L10n.settingsClaude, systemImage: "house") }
            advanced.tabItem { Label(L10n.settingsAdvanced, systemImage: "cpu") }
        }
        .frame(width: 620, height: 520)
        .tint(Palette.accent)
        .task { await model.load() }
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
        PageScroll {
            CardSection(nil) {
                CardRow {
                    Image(nsImage: NSApplication.shared.applicationIconImage).resizable().frame(width: 28, height: 28)
                        .accessibilityHidden(true)
                    Text(L10n.appName).fontWeight(.semibold)
                    Spacer()
                }
                RowSeparator()
                CardRow {
                    Text(L10n.notificationsToggle)
                    Spacer()
                    Toggle(L10n.notificationsToggle, isOn: $model.notificationsEnabled).labelsHidden().toggleStyle(.switch)
                }
            }
            if let on = model.allowLAN { // 补齐三
                CardSection(nil, footer: L10n.allowLANHelp) {
                    CardRow {
                        Text(L10n.allowLANTitle)
                        Spacer()
                        Toggle(L10n.allowLANTitle, isOn: Binding(get: { on }, set: { v in Task { await model.setAllowLAN(v) } }))
                            .labelsHidden().toggleStyle(.switch)
                            .disabled(model.busy)
                    }
                }
            }
            if let s = model.signatureText {
                CardSection(nil) {
                    CardRow(alignment: .top) {
                        Image(systemName: "lock.open").foregroundStyle(.secondary).accessibilityHidden(true)
                        Text(s).font(.caption).fixedSize(horizontal: false, vertical: true).frame(maxWidth: .infinity, alignment: .leading)
                    }
                }
            }
        }
    }

    // MARK: - 环境检测

    private var envTab: some View {
        PageScroll {
            CardSection(L10n.envWarn) {
                ForEach(Array(env.warnRows.enumerated()), id: \.element.id) { i, r in
                    if i > 0 { RowSeparator() }
                    CardRow {
                        HStack(spacing: 6) {
                            Text(r.id).font(.system(size: 12, design: .monospaced)).foregroundStyle(.tertiary)
                            Text(r.name)
                        }
                        Spacer()
                        Toggle(r.id + " " + r.name, isOn: Binding(get: { r.enabled }, set: { on in Task { await env.setEnabled(r.id, on) } }))
                            .labelsHidden().toggleStyle(.switch)
                    }
                }
            }
            CardSection(L10n.envHard) {
                CardRow(alignment: .top) {
                    Image(systemName: "lock").foregroundStyle(.secondary).accessibilityHidden(true)
                    Text(L10n.hardItemLocked).font(.caption).foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true).frame(maxWidth: .infinity, alignment: .leading)
                }
            }
        }
    }

    // MARK: - 回国与 Claude

    private var claude: some View {
        PageScroll {
            CardSection(L10n.processList) {
                if let t = model.processListText {
                    CardRow { Text(t).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true) }
                }
                ForEach(model.processes, id: \.pid) { p in
                    RowSeparator()
                    CardRow { Text("\(p.pid)  \(p.app)  \(p.cmd)").font(.caption.monospaced()) }
                }
                if !model.unverified.isEmpty {
                    RowSeparator()
                    CardRow { Text(L10n.processesUnverified).font(.caption) }
                    ForEach(model.unverified, id: \.pid) { p in
                        CardRow { Text("\(p.pid)  \(p.app)  \(p.note ?? "")").font(.caption.monospaced()).foregroundStyle(.secondary) }
                    }
                }
            }
            CardSection(nil) {
                CardRow(alignment: .top) {
                    RowLabel(title: L10n.launchControl, bold: true)
                    Text(model.launchControlText).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                        .frame(maxWidth: 300, alignment: .trailing)
                }
                RowSeparator()
                CardRow(alignment: .top) {
                    RowLabel(title: L10n.resolverLockTitle, bold: true)
                    status(model.resolverLockText, tone: model.resolverLockWarn ? .attention : nil)
                }
            }
            CardSection(L10n.claudeCodeTitle) {
                CardRow { status(model.claudeCodeText, tone: model.claudeCodeWarn ? .attention : nil, leading: true) }
                if model.claudeCodeSwitchShown { // v0.68：第四道是开关、默认关；两个方向都要本人授权（B0：应用里点了先弹确认框，再经系统密码框执行）
                    RowSeparator()
                    CardRow { Text(model.claudeCodeConsequenceText).font(.caption).fixedSize(horizontal: false, vertical: true) }
                    RowSeparator()
                    CardRow {
                        Button(L10n.claudeCodeTurnOn) { Task { await model.userTappedPrivileged(.claudeProxy(on: true)) } }
                        Button(L10n.claudeCodeTurnOff) { Task { await model.userTappedPrivileged(.claudeProxy(on: false)) } }
                        Spacer()
                    }
                    ForEach(model.claudeCodeCommands, id: \.command) { c in
                        RowSeparator()
                        CardRow {
                            VStack(alignment: .leading, spacing: 4) {
                                Text(c.label).font(.caption)
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
                            Text(t).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true).frame(maxWidth: 300, alignment: .trailing)
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
            CardSection(L10n.claudeDomainsTitle, footer: model.claudeDomains?.note) {
                ForEach(model.claudeDomainLines, id: \.self) { l in
                    CardRow { Text(l).font(.caption.monospaced()) }
                    RowSeparator()
                }
                CardRow {
                    Spacer()
                    Button(L10n.claudeDomainsClear) { Task { await model.clearClaudeDomains() } }
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
                .frame(maxWidth: leading ? .infinity : 300, alignment: align)
        } else {
            Text(text).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                .frame(maxWidth: leading ? .infinity : 300, alignment: align)
        }
    }

    // MARK: - 高级

    private var advanced: some View {
        PageScroll {
            CardSection(L10n.cliTitle) {
                CardRow(alignment: .top) {
                    Image(systemName: "terminal").foregroundStyle(Palette.accent).accessibilityHidden(true)
                    VStack(alignment: .leading, spacing: 4) {
                        Text(model.cliPath).font(.caption.monospaced()).textSelection(.enabled).fixedSize(horizontal: false, vertical: true)
                        Text(model.cliUsage).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                    }
                    .frame(maxWidth: .infinity, alignment: .leading)
                }
                if let s = model.signatureText {
                    RowSeparator()
                    CardRow { Text(s).font(.caption).fixedSize(horizontal: false, vertical: true) }
                }
            }
            CardSection(L10n.diagnostics, footer: diagnostics.help) { // 补齐五：应用里导出（只写用户在保存面板里选的那个文件）
                CardRow { Toggle(L10n.diagnosticsIncludeDomains, isOn: $diagnostics.includeDomains) }
                RowSeparator()
                CardRow {
                    if let m = diagnostics.message {
                        if diagnostics.isError {
                            ToneNote(tone: .failure, text: m, symbol: "xmark.octagon")
                        } else {
                            Text(m).font(.caption).foregroundStyle(.secondary)
                        }
                    }
                    Spacer()
                    Button(L10n.diagnosticsExport) { Task { await diagnostics.export() } }
                        .disabled(diagnostics.busy)
                }
            }
            CardSection(L10n.uninstall) {
                CardRow(alignment: .top) {
                    Image(systemName: "trash").foregroundStyle(.secondary).accessibilityHidden(true)
                    Text(model.uninstallText).font(.caption).fixedSize(horizontal: false, vertical: true)
                        .textSelection(.enabled).frame(maxWidth: .infinity, alignment: .leading)
                }
            }
        }
    }
}
