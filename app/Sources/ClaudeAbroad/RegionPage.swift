// 地区（原型 ui/design/index.html 的 pageRegion）：分组表单——当前锁定的地区和来源、待还原的项、手动指定（时区、语言）和跟随出口。
import AbroadKit
import SwiftUI

struct RegionPage: View {
    @ObservedObject var model: RegionModel
    var body: some View {
        LoadStateView(state: model.state, retry: { Task { await model.load() } }) {
            PageScroll {
                CardSection(L10n.regionTitle, icon: "globe") {
                    if let t = model.targetText {
                        CardRow(alignment: .top) {
                            IconBadge(symbol: "globe.americas.fill", tone: .pass, size: 32)
                            VStack(alignment: .leading, spacing: 4) {
                                Text(t)
                                    .font(.system(size: 13, weight: .semibold))
                                    .fixedSize(horizontal: false, vertical: true)
                                if let s = model.sourceText {
                                    HStack(spacing: 6) {
                                        Text(L10n.regionSource).font(.caption).foregroundStyle(.secondary)
                                        InfoPill(text: s)
                                    }
                                }
                            }
                            .frame(maxWidth: .infinity, alignment: .leading)
                        }
                    } else {
                        CardRow {
                            IconBadge(symbol: "globe", tone: .local, size: 32)
                            Text(L10n.regionNone).foregroundStyle(.secondary)
                        }
                    }
                }
                if !model.pending.isEmpty {
                    CardSection(L10n.regionPending, icon: "clock.arrow.circlepath") {
                        ForEach(Array(model.pending.enumerated()), id: \.offset) { i, p in
                            if i > 0 { RowSeparator() }
                            CardRow {
                                IconBadge(symbol: "clock.arrow.circlepath", tone: .attention, size: 26)
                                Text(p)
                            }
                        }
                    }
                }
                CardSection(L10n.regionManualSection, icon: "slider.horizontal.3") {
                    CardRow {
                        fieldLabel(L10n.regionTimezone)
                        StyledTextField(placeholder: L10n.regionTimezone, text: $model.formTimezone, monospaced: true)
                    }
                    RowSeparator()
                    CardRow {
                        fieldLabel(L10n.regionLang)
                        StyledTextField(placeholder: L10n.regionLang, text: $model.formLang, monospaced: true)
                    }
                    RowSeparator()
                    CardRow {
                        if model.switched {
                            ToneNote(tone: .success, text: L10n.regionSwitched, symbol: "checkmark.circle")
                        }
                        Spacer()
                        Button { Task { await model.followExit() } } label: {
                            Label(L10n.regionFollowExit, systemImage: "location")
                        }
                        .buttonStyle(SecondaryButtonStyle())

                        Button { Task { await model.setManual() } } label: {
                            Label(L10n.regionSet, systemImage: "checkmark")
                        }
                        .buttonStyle(PrimaryButtonStyle())
                    }
                    .disabled(model.busy)
                }
            }
        }
        .modifier(PageChrome(model: model))
        .task { await model.load() }
    }

    private func fieldLabel(_ t: String) -> some View {
        Text(t)
            .font(.system(size: 13, weight: .medium))
            .foregroundStyle(.secondary)
            .frame(width: 190, alignment: .leading)
    }
}
