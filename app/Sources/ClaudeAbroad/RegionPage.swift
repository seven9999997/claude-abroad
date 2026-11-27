// 地区（原型 ui/design/index.html 的 pageRegion）：分组表单——当前锁定的地区和来源、待还原的项、手动指定（时区、语言）和跟随出口。
import AbroadKit
import SwiftUI

struct RegionPage: View {
    @ObservedObject var model: RegionModel
    var body: some View {
        LoadStateView(state: model.state, retry: { Task { await model.load() } }) {
            PageScroll {
                CardSection(L10n.regionTitle) {
                    if let t = model.targetText {
                        CardRow { Text(t).fixedSize(horizontal: false, vertical: true) }
                        if let s = model.sourceText {
                            RowSeparator()
                            CardRow { RowLabel(title: L10n.regionSource); Text(s).foregroundStyle(.secondary) }
                        }
                    } else {
                        CardRow { Text(L10n.regionNone).foregroundStyle(.secondary) }
                    }
                }
                if !model.pending.isEmpty {
                    CardSection(L10n.regionPending) {
                        ForEach(Array(model.pending.enumerated()), id: \.offset) { i, p in
                            if i > 0 { RowSeparator() }
                            CardRow {
                                Image(systemName: "clock.arrow.circlepath").foregroundStyle(Palette.tone(.attention)).accessibilityHidden(true)
                                Text(p)
                            }
                        }
                    }
                }
                CardSection(L10n.regionManualSection) {
                    CardRow { TextField(L10n.regionTimezone, text: $model.formTimezone) }
                    RowSeparator()
                    CardRow { TextField(L10n.regionLang, text: $model.formLang) }
                    RowSeparator()
                    CardRow {
                        if model.switched { ToneNote(tone: .success, text: L10n.regionSwitched, symbol: "checkmark.circle") }
                        Spacer()
                        Button(L10n.regionFollowExit) { Task { await model.followExit() } }
                        Button(L10n.regionSet) { Task { await model.setManual() } }.buttonStyle(.borderedProminent)
                    }
                    .disabled(model.busy)
                }
            }
        }
        .modifier(PageChrome(model: model))
        .task { await model.load() }
    }
}
