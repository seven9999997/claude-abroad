// 环境检测（原型 ui/design/index.html 的 pageEnv）：硬性项、警告项各一个分组，每行左边是结果标记（图形加颜色），
// 中间是编号、名称、明细和处理办法，右边是结果文字和开关；下面是历史和设备体检。
import AbroadKit
import SwiftUI

struct EnvPage: View {
    @ObservedObject var model: EnvModel
    var body: some View {
        LoadStateView(state: model.state, retry: { Task { await model.load() } }) {
            PageScroll {
                CardSection(L10n.envHard) { rows(model.hardRows) }
                CardSection(L10n.envWarn) { rows(model.warnRows) }
                CardSection(L10n.envHistoryTitle) { // 补齐四
                    if let t = model.historyEmptyText {
                        CardRow { Text(t).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true) }
                    }
                    ForEach(Array(model.history.enumerated()), id: \.element.id) { i, h in
                        if i > 0 { RowSeparator() }
                        CardRow {
                            VStack(alignment: .leading, spacing: 3) {
                                Text(h.text).font(.callout.monospaced())
                                if let c = h.changed { ToneNote(tone: .attention, text: c, symbol: "arrow.left.arrow.right") }
                            }
                        }
                    }
                    if !model.history.isEmpty {
                        RowSeparator()
                        CardRow {
                            Spacer()
                            Button(L10n.envHistoryClear) { Task { await model.clearHistory() } }.disabled(model.busy)
                        }
                    }
                }
                CardSection(L10n.envDevice) {
                    CardRow(alignment: .top) {
                        Image(systemName: "textformat").foregroundStyle(.secondary).accessibilityHidden(true)
                        Text(model.deviceCheckText).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                            .frame(maxWidth: .infinity, alignment: .leading)
                        Button(L10n.deviceCheckRun) { Task { await model.runDevice() } }.disabled(model.busy)
                    }
                }
            }
            .toolbar { Button(L10n.envRerun) { Task { await model.rerun() } }.disabled(model.busy) }
        }
        .modifier(PageChrome(model: model))
        .task { await model.load() }
    }

    @ViewBuilder private func rows(_ list: [EnvRow]) -> some View {
        ForEach(Array(list.enumerated()), id: \.element.id) { i, r in
            if i > 0 { RowSeparator() }
            row(r)
        }
    }

    private func row(_ r: EnvRow) -> some View {
        CardRow(alignment: .top) {
            StatusMark(tone: r.tone, label: EnvModel.resultTitle(r.result)).padding(.top, 1)
            VStack(alignment: .leading, spacing: 2) {
                HStack(spacing: 6) {
                    Text(r.id).font(.system(size: 12, design: .monospaced)).foregroundStyle(.tertiary)
                    Text(r.name).fontWeight(.semibold)
                }
                if !r.detail.isEmpty { Text(r.detail).font(.caption).fixedSize(horizontal: false, vertical: true) }
                if !r.fix.isEmpty { Text(r.fix).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true) }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            Text(EnvModel.resultTitle(r.result)).font(.callout).foregroundStyle(Palette.tone(r.tone))
            if r.hard {
                Toggle(L10n.hardItemLocked, isOn: .constant(true)).labelsHidden().toggleStyle(.switch).controlSize(.small).disabled(true).help(L10n.hardItemLocked)
            } else {
                Toggle(r.name, isOn: Binding(get: { r.enabled }, set: { on in Task { await model.setEnabled(r.id, on) } }))
                    .labelsHidden().toggleStyle(.switch).controlSize(.small)
            }
        }
    }
}
