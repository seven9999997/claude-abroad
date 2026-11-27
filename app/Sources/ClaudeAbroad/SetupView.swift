// 安装引导（原型 ui/design/index.html 的 .setup）：左栏是标题和步骤（欢迎与说明、后台服务、配置主代理、完成），
// 右边是当前一步的内容，底部一条按钮栏。能不能下一步全由 SetupModel（后台的进度接口）决定。
import AbroadKit
import AppKit
import SwiftUI

struct SetupView: View {
    @ObservedObject var setup: SetupModel
    @ObservedObject var main: MainProxyModel
    @ObservedObject var status: StatusModel
    @ObservedObject var service: ServiceInstallModel

    var body: some View {
        HStack(spacing: 0) {
            stepper
                .frame(width: 236)
                .frame(maxHeight: .infinity, alignment: .top)
                .background(Palette.side)
            Rectangle().fill(Palette.separator).frame(width: 1)
            VStack(spacing: 0) {
                content
                    .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
                if let n = setup.notice {
                    ErrorBlock(text: n.text)
                        .padding(.horizontal, 28)
                        .padding(.bottom, 12)
                }
                Rectangle().fill(Palette.separator).frame(height: 1)
                HStack(spacing: 10) {
                    Spacer()
                    if setup.step == .mainProxy {
                        Button { Task { await setup.recheck() } } label: {
                            Label(L10n.recheck, systemImage: "arrow.clockwise")
                        }
                        .buttonStyle(SecondaryButtonStyle())
                    }
                    if setup.step != .done {
                        Button(setup.step == .mainProxy ? L10n.finish : L10n.next) { Task { await setup.next() } }
                            .buttonStyle(PrimaryButtonStyle())
                            .disabled(!setup.canAdvance || setup.busy)
                            .keyboardShortcut(.defaultAction)
                    }
                }
                .padding(.horizontal, 24)
                .padding(.vertical, 14)
                .background(Palette.box)
            }
            .background(Palette.window)
        }
        .task { await setup.refresh() }
    }

    /// 左栏：应用图标、标题和四步。做完的一步是实心勾，当前一步加粗、圆圈用强调色。
    private var stepper: some View {
        VStack(alignment: .leading, spacing: 18) {
            HStack(spacing: 12) {
                Image(nsImage: NSApplication.shared.applicationIconImage)
                    .resizable()
                    .frame(width: 46, height: 46)
                    .clipShape(RoundedRectangle(cornerRadius: 11, style: .continuous))
                    .shadow(color: Palette.cardShadow, radius: 4, x: 0, y: 2)
                    .accessibilityHidden(true)
                VStack(alignment: .leading, spacing: 2) {
                    Text(L10n.appName).font(.system(size: 15, weight: .bold))
                    Text(L10n.setupTitle).font(.caption).foregroundStyle(.secondary)
                }
            }

            VStack(alignment: .leading, spacing: 4) {
                ForEach(SetupStepID.allCases, id: \.self) { s in
                    stepRow(s)
                }
            }
        }
        .padding(.horizontal, 18)
        .padding(.top, 28)
    }

    private func stepRow(_ s: SetupStepID) -> some View {
        let done = s < setup.step, current = s == setup.step
        return HStack(spacing: 10) {
            ZStack {
                if done {
                    Circle().fill(Palette.tone(.success))
                    Image(systemName: "checkmark")
                        .font(.system(size: 10, weight: .bold))
                        .foregroundStyle(Palette.onTone)
                } else {
                    Circle()
                        .fill(current ? Palette.accentSoft : Palette.clear)
                    Circle()
                        .strokeBorder(current ? Palette.accent : Palette.grayLine, lineWidth: 1.5)
                    Text(verbatim: "\(s.rawValue + 1)")
                        .font(.system(size: 11, weight: .bold))
                        .foregroundStyle(current ? Palette.accent : Palette.gray)
                }
            }
            .frame(width: 22, height: 22)
            .accessibilityHidden(true)

            Text(s.title)
                .font(.system(size: 13, weight: current ? .semibold : .regular))
                .foregroundStyle(current ? .primary : .secondary)
            Spacer(minLength: 0)
        }
        .padding(.horizontal, 10)
        .padding(.vertical, 8)
        .background(RoundedRectangle(cornerRadius: 8, style: .continuous).fill(current ? Palette.boxElevated : Palette.clear))
        .overlay(RoundedRectangle(cornerRadius: 8, style: .continuous).strokeBorder(current ? Palette.accentLine : Palette.clear, lineWidth: 1))
        .accessibilityElement(children: .combine)
        .accessibilityAddTraits(current ? .isSelected : [])
    }

    @ViewBuilder private var content: some View {
        switch setup.step {
        case .mainProxy:
            VStack(alignment: .leading, spacing: 0) {
                stepHeader(L10n.setupMainProxyText)
                    .padding(.horizontal, 28)
                    .padding(.top, 26)
                MainProxyPage(model: main, progress: L10n.borrowStep(status.borrowStep), onApplied: { Task { await setup.recheck() } })
            }
        default:
            ScrollView {
                VStack(alignment: .leading, spacing: 20) {
                    switch setup.step {
                    case .welcome:
                        stepHeader(L10n.setupWelcomeText)
                        CardSection(nil) {
                            CardRow(verticalPadding: 16) {
                                Image(nsImage: NSApplication.shared.applicationIconImage)
                                    .resizable()
                                    .frame(width: 56, height: 56)
                                    .clipShape(RoundedRectangle(cornerRadius: 14, style: .continuous))
                                    .shadow(color: Palette.cardShadow, radius: 6, x: 0, y: 2)
                                    .accessibilityHidden(true)
                                VStack(alignment: .leading, spacing: 4) {
                                    Text(L10n.appName).font(.system(size: 16, weight: .bold))
                                    Text(L10n.setupWelcomeText)
                                        .font(.callout)
                                        .foregroundStyle(.secondary)
                                        .lineSpacing(2)
                                        .fixedSize(horizontal: false, vertical: true)
                                }
                            }
                        }
                    case .service:
                        if setup.reachable {
                            stepHeader(L10n.setupServiceOK)
                            CardSection(nil) {
                                CardRow {
                                    IconBadge(symbol: "checkmark.circle.fill", tone: .success, size: 32)
                                    Text(L10n.setupServiceOK).font(.system(size: 13, weight: .semibold))
                                }
                            }
                        } else {
                            if service.showsInstall { // 阶段 B（v0.71c）：应用里安装（点了先弹确认框，再点“执行”才输密码）
                                stepHeader(L10n.serviceNotInstalled)
                                CardSection(nil, footer: L10n.serviceBoundary) {
                                    CardRow(alignment: .top) {
                                        IconBadge(symbol: "gearshape.arrow.triangle.2.circlepath", tone: .attention, size: 32)
                                        VStack(alignment: .leading, spacing: 10) {
                                            Text(L10n.serviceInstallWhat)
                                                .font(.callout)
                                                .foregroundStyle(.secondary)
                                                .fixedSize(horizontal: false, vertical: true)
                                            Button { Task { await service.userTappedInstall() } } label: {
                                                Label(L10n.serviceInstallButton, systemImage: "lock.shield")
                                            }
                                            .buttonStyle(PrimaryButtonStyle())
                                        }
                                    }
                                }
                            } else {
                                stepHeader(L10n.setupServiceText)
                                CodeBlock(text: setup.serviceCommand)
                                Text(L10n.setupTemporary).font(.caption).foregroundStyle(.secondary)
                            }
                        }
                    default:
                        HStack(spacing: 14) {
                            IconBadge(symbol: "checkmark.seal.fill", tone: .success, size: 46)
                            stepHeader(L10n.setupDoneText)
                        }
                    }
                }
                .frame(maxWidth: 660, alignment: .leading)
                .padding(.horizontal, 36)
                .padding(.top, 38)
                .padding(.bottom, 20)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
    }

    /// 每一步的大标题和说明（原型 .setup .body h1 / .lead）。
    private func stepHeader(_ lead: String) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            Text(setup.step.title).font(.system(size: 24, weight: .bold))
            Text(lead)
                .font(.body)
                .foregroundStyle(.secondary)
                .lineSpacing(2)
                .fixedSize(horizontal: false, vertical: true)
                .frame(maxWidth: 580, alignment: .leading)
        }
    }
}
