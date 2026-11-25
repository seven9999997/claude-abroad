// 设置窗口（⌘,；design 11.2）：通用、环境检测、回国与 Claude、高级。
import AbroadKit
import SwiftUI

struct SettingsView: View {
    @ObservedObject var model: SettingsModel
    @ObservedObject var env: EnvModel

    var body: some View {
        TabView {
            Form {
                LabeledContent(L10n.appName, value: "")
                Toggle(L10n.notificationsToggle, isOn: $model.notificationsEnabled)
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
                if let t = model.prepareText {
                    LabeledContent(L10n.prepareTitle) { Text(t).foregroundStyle(model.prepareWarn ? .orange : .primary) }
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
                LabeledContent(L10n.diagnostics) { Text(model.diagnosticsText) }
                LabeledContent(L10n.uninstall) { Text(model.uninstallText) }
            }
            .tabItem { Text(L10n.settingsAdvanced) }
        }
        .padding()
        .frame(width: 520, height: 360)
        .task { await model.load() }
    }
}
