// 安装引导（原型 ui/design/index.html 的引导四步）：欢迎与说明、后台服务、配置主代理、完成。
// 能不能下一步全由 SetupModel（后台的进度接口）决定。
import AbroadKit
import SwiftUI

struct SetupView: View {
    @ObservedObject var setup: SetupModel
    @ObservedObject var main: MainProxyModel
    @ObservedObject var status: StatusModel

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text(L10n.setupTitle).font(.title)
            stepper
            Divider()
            content
            if let n = setup.notice { ErrorBlock(text: n.text) }
            Spacer()
            HStack {
                Spacer()
                if setup.step == .mainProxy {
                    Button(L10n.recheck) { Task { await setup.recheck() } }
                }
                if setup.step != .done {
                    Button(setup.step == .mainProxy ? L10n.finish : L10n.next) { Task { await setup.next() } }
                        .disabled(!setup.canAdvance || setup.busy)
                        .keyboardShortcut(.defaultAction)
                }
            }
        }
        .padding(20)
        .task { await setup.refresh() }
    }

    private var stepper: some View {
        HStack(spacing: 12) {
            ForEach(SetupStepID.allCases, id: \.self) { s in
                HStack(spacing: 4) {
                    Image(systemName: s < setup.step ? "checkmark.circle.fill" : (s == setup.step ? "circle.inset.filled" : "circle"))
                    Text(s.title).fontWeight(s == setup.step ? .bold : .regular)
                }
            }
        }
    }

    @ViewBuilder private var content: some View {
        switch setup.step {
        case .welcome:
            Text(L10n.setupWelcomeText)
        case .service:
            if setup.reachable {
                Text(L10n.setupServiceOK)
            } else {
                VStack(alignment: .leading, spacing: 8) {
                    Text(L10n.setupServiceText)
                    Text(setup.serviceCommand).font(.body.monospaced()).textSelection(.enabled)
                    Text(L10n.setupTemporary).font(.caption).foregroundStyle(.secondary)
                }
            }
        case .mainProxy:
            VStack(alignment: .leading, spacing: 8) {
                Text(L10n.setupMainProxyText)
                MainProxyPage(model: main, onApplied: { Task { await setup.recheck() } })
            }
        case .done:
            Text(L10n.setupDoneText)
        }
    }
}
