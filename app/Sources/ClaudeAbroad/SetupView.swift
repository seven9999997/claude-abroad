// 安装引导（原型 ui/design/index.html 的 .setup）：左栏是标题和步骤（欢迎与说明、后台服务、配置主代理、完成），
// 右边是当前一步的内容，底部一条按钮栏。能不能下一步全由 SetupModel（后台的进度接口）决定。
import AbroadKit
import SwiftUI

struct SetupView: View {
    @ObservedObject var setup: SetupModel
    @ObservedObject var main: MainProxyModel
    @ObservedObject var status: StatusModel

    var body: some View {
        HStack(spacing: 0) {
            stepper
                .frame(width: 220)
                .frame(maxHeight: .infinity, alignment: .top)
                .background(Palette.side)
            Rectangle().fill(Palette.separator).frame(width: 1)
            VStack(spacing: 0) {
                content
                    .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
                if let n = setup.notice { ErrorBlock(text: n.text).padding(.horizontal, 24).padding(.bottom, 10) }
                Rectangle().fill(Palette.separator).frame(height: 1)
                HStack(spacing: 8) {
                    Spacer()
                    if setup.step == .mainProxy {
                        Button(L10n.recheck) { Task { await setup.recheck() } }
                    }
                    if setup.step != .done {
                        Button(setup.step == .mainProxy ? L10n.finish : L10n.next) { Task { await setup.next() } }
                            .buttonStyle(.borderedProminent)
                            .disabled(!setup.canAdvance || setup.busy)
                            .keyboardShortcut(.defaultAction)
                    }
                }
                .controlSize(.large)
                .padding(.horizontal, 18)
                .padding(.vertical, 12)
            }
            .background(Palette.window)
        }
        .task { await setup.refresh() }
    }

    /// 左栏：应用图标、标题和四步。做完的一步是实心勾，当前一步加粗、圆圈用强调色。
    private var stepper: some View {
        VStack(alignment: .leading, spacing: 14) {
            Image(nsImage: NSApplication.shared.applicationIconImage).resizable().frame(width: 48, height: 48)
                .accessibilityHidden(true)
            Text(L10n.setupTitle).font(.system(size: 15, weight: .bold))
            VStack(alignment: .leading, spacing: 2) {
                ForEach(SetupStepID.allCases, id: \.self) { s in
                    stepRow(s)
                }
            }
        }
        .padding(.horizontal, 18)
        .padding(.top, 24)
    }

    private func stepRow(_ s: SetupStepID) -> some View {
        let done = s < setup.step, current = s == setup.step
        return HStack(spacing: 9) {
            ZStack {
                if done {
                    Circle().fill(Palette.tone(.success))
                    Image(systemName: "checkmark").font(.system(size: 10, weight: .bold)).foregroundStyle(Palette.onTone)
                } else {
                    Circle().strokeBorder(current ? Palette.accent : Palette.gray, lineWidth: 1.5)
                    Text(verbatim: "\(s.rawValue + 1)").font(.system(size: 11, weight: .bold)).foregroundStyle(current ? Palette.accent : Palette.gray)
                }
            }
            .frame(width: 20, height: 20)
            .accessibilityHidden(true)
            Text(s.title).fontWeight(current ? .bold : .regular).foregroundStyle(current ? .primary : .secondary)
            Spacer(minLength: 0)
        }
        .padding(6)
        .background(RoundedRectangle(cornerRadius: 7).fill(current ? Palette.graySoft : Palette.box.opacity(0)))
        .accessibilityElement(children: .combine)
        .accessibilityAddTraits(current ? .isSelected : [])
    }

    @ViewBuilder private var content: some View {
        switch setup.step {
        case .mainProxy:
            VStack(alignment: .leading, spacing: 0) {
                stepHeader(L10n.setupMainProxyText).padding(.horizontal, 24).padding(.top, 28)
                MainProxyPage(model: main, onApplied: { Task { await setup.recheck() } })
            }
        default:
            ScrollView {
                VStack(alignment: .leading, spacing: 16) {
                    switch setup.step {
                    case .welcome:
                        stepHeader(L10n.setupWelcomeText)
                    case .service:
                        if setup.reachable {
                            stepHeader(L10n.setupServiceOK)
                        } else {
                            stepHeader(L10n.setupServiceText)
                            CodeBlock(text: setup.serviceCommand)
                            Text(L10n.setupTemporary).font(.caption).foregroundStyle(.secondary)
                        }
                    default:
                        stepHeader(L10n.setupDoneText)
                    }
                }
                .padding(.horizontal, 32)
                .padding(.top, 36)
                .padding(.bottom, 16)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
    }

    /// 每一步的大标题和说明（原型 .setup .body h1 / .lead）。
    private func stepHeader(_ lead: String) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            Text(setup.step.title).font(.system(size: 22, weight: .bold))
            Text(lead).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                .frame(maxWidth: 560, alignment: .leading)
        }
    }
}
