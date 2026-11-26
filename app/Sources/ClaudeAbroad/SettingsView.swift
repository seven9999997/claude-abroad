// 设置窗口（⌘,；design 11.2）：通用、环境检测、回国与 Claude、高级。
import AbroadKit
import SwiftUI

struct SettingsView: View {
    @ObservedObject var model: SettingsModel
    @ObservedObject var env: EnvModel
    @ObservedObject var diagnostics: DiagnosticsExportModel

    var body: some View {
        TabView {
            Form {
                LabeledContent(L10n.appName, value: "")
                Toggle(L10n.notificationsToggle, isOn: $model.notificationsEnabled)
                if let on = model.allowLAN { // 补齐三
                    Toggle(L10n.allowLANTitle, isOn: Binding(get: { on }, set: { v in Task { await model.setAllowLAN(v) } }))
                        .disabled(model.busy)
                    Text(L10n.allowLANHelp).font(.caption).foregroundStyle(.secondary)
                }
                if let s = model.signatureText { Text(s).font(.caption) }
            }
            .tabItem { Text(L10n.settingsGeneral) }
            Form {
                ForEach(env.warnRows) { r in
                    Toggle(r.id + " " + r.name, isOn: Binding(get: { r.enabled }, set: { on in Task { await env.setEnabled(r.id, on) } }))
                }
                Text(L10n.hardItemLocked).font(.caption).foregroundStyle(.secondary)
            }
            .tabItem { Text(L10n.settingsEnv) }
            Form {
                LabeledContent(L10n.processList) {
                    VStack(alignment: .leading, spacing: 2) {
                        if let t = model.processListText { Text(t).foregroundStyle(.secondary) }
                        ForEach(model.processes, id: \.pid) { p in Text("\(p.pid)  \(p.app)  \(p.cmd)").font(.caption.monospaced()) }
                        if !model.unverified.isEmpty {
                            Text(L10n.processesUnverified).font(.caption)
                            ForEach(model.unverified, id: \.pid) { p in Text("\(p.pid)  \(p.app)  \(p.note ?? "")").font(.caption.monospaced()).foregroundStyle(.secondary) }
                        }
                    }
                }
                LabeledContent(L10n.launchControl) { Text(model.launchControlText) }
                LabeledContent(L10n.resolverLockTitle) {
                    Text(model.resolverLockText).foregroundStyle(model.resolverLockWarn ? .orange : .primary)
                }
                LabeledContent(L10n.claudeCodeTitle) {
                    Text(model.claudeCodeText).foregroundStyle(model.claudeCodeWarn ? .orange : .primary)
                }
                if let t = model.returnBlockText {
                    LabeledContent(L10n.returnBlockTitle) { Text(t) }
                }
                if let t = model.prepareText {
                    LabeledContent(L10n.prepareTitle) { Text(t).foregroundStyle(model.prepareWarn ? .orange : .primary) }
                }
                LabeledContent(L10n.claudeDomainsTitle) {
                    VStack(alignment: .leading, spacing: 2) {
                        if let note = model.claudeDomains?.note { Text(note).font(.caption).foregroundStyle(.secondary) }
                        ForEach(model.claudeDomainLines, id: \.self) { Text($0).font(.caption.monospaced()) }
                        Button(L10n.claudeDomainsClear) { Task { await model.clearClaudeDomains() } }
                            .disabled((model.claudeDomains?.count ?? 0) == 0)
                    }
                }
                LabeledContent(L10n.integrityTitle) {
                    Text(model.integrityText).foregroundStyle(model.integrityWarn ? .red : .primary)
                }
            }
            .tabItem { Text(L10n.settingsClaude) }
            Form {
                LabeledContent(L10n.cliTitle) {
                    VStack(alignment: .leading) {
                        Text(model.cliPath).textSelection(.enabled)
                        Text(model.cliUsage).font(.caption)
                    }
                }
                if let s = model.signatureText { Text(s) }
                LabeledContent(L10n.diagnostics) { // 补齐五：应用里导出（只写用户在保存面板里选的那个文件）
                    VStack(alignment: .leading, spacing: 4) {
                        Toggle(L10n.diagnosticsIncludeDomains, isOn: $diagnostics.includeDomains)
                        Button(L10n.diagnosticsExport) { Task { await diagnostics.export() } }
                            .disabled(diagnostics.busy)
                        if let m = diagnostics.message {
                            Text(m).font(.caption).foregroundStyle(diagnostics.isError ? .red : .secondary)
                        }
                        Text(diagnostics.help).font(.caption).foregroundStyle(.secondary)
                    }
                }
                LabeledContent(L10n.uninstall) { Text(model.uninstallText) }
            }
            .tabItem { Text(L10n.settingsAdvanced) }
        }
        .padding()
        .frame(width: 520, height: 360)
        .task { await model.load() }
    }
}
