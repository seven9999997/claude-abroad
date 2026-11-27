// 环境检测（原型 ui/design/index.html 的 pageEnv）：硬性项、警告项各一个分组，每行左边是结果标记（图形加颜色），
// 中间是编号、名称、明细和处理办法，右边是结果文字和开关；下面是历史和设备体检。
import AbroadKit
import SwiftUI

struct EnvPage: View {
    @ObservedObject var model: EnvModel
    var body: some View {
        LoadStateView(state: model.state, retry: { Task { await model.load() } }) {
            PageScroll {
                summaryStrip
                CardSection(L10n.envHard, icon: "checkmark.shield.fill") { rows(model.hardRows) }
                CardSection(L10n.envWarn, icon: "exclamationmark.triangle") { rows(model.warnRows) }
                CardSection(L10n.envHistoryTitle, icon: "clock.arrow.circlepath") { // 补齐四
                    if let t = model.historyEmptyText {
                        CardRow {
                            IconBadge(symbol: "clock", size: 28)
                            Text(t).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                        }
                    }
                    ForEach(Array(model.history.enumerated()), id: \.element.id) { i, h in
                        if i > 0 { RowSeparator() }
                        CardRow {
                            VStack(alignment: .leading, spacing: 4) {
                                Text(h.text).font(.system(size: 12, design: .monospaced))
                                if let c = h.changed { ToneNote(tone: .attention, text: c, symbol: "arrow.left.arrow.right") }
                            }
                        }
                    }
                    if !model.history.isEmpty {
                        RowSeparator()
                        CardRow {
                            Spacer()
                            Button(L10n.envHistoryClear) { Task { await model.clearHistory() } }
                                .buttonStyle(SecondaryButtonStyle(compact: true))
                                .disabled(model.busy)
                        }
                    }
                }
                CardSection(L10n.envDevice, icon: "textformat") {
                    CardRow(alignment: .top) {
                        IconBadge(symbol: "textformat", size: 30)
                        Text(model.deviceCheckText)
                            .font(.callout)
                            .foregroundStyle(.secondary)
                            .lineSpacing(2)
                            .fixedSize(horizontal: false, vertical: true)
                            .frame(maxWidth: .infinity, alignment: .leading)
                        Button { Task { await model.runDevice() } } label: {
                            Label(L10n.deviceCheckRun, systemImage: "magnifyingglass")
                        }
                        .buttonStyle(SecondaryButtonStyle())
                        .disabled(model.busy)
                    }
                }
            }
            .toolbar { Button(L10n.envRerun) { Task { await model.rerun() } }.disabled(model.busy) }
        }
        .modifier(PageChrome(model: model))
        .task { await model.load() }
    }

    private var summaryStrip: some View {
        let all = model.hardRows + model.warnRows
        let passCount = all.filter { $0.result == "pass" }.count
        let failCount = all.filter { $0.result == "fail" }.count
        let unknownCount = all.filter { $0.result != "pass" && $0.result != "fail" && $0.result != "skipped" }.count
        return CardSection(nil) {
            CardRow {
                HStack(spacing: 12) {
                    countBadge(.pass, label: L10n.resultPass, count: passCount)
                    if failCount > 0 { countBadge(.blocked, label: L10n.resultFail, count: failCount) }
                    if unknownCount > 0 { countBadge(.checking, label: L10n.resultUnknown, count: unknownCount) }
                }
                Spacer()
                Button { Task { await model.rerun() } } label: {
                    Label(L10n.envRerun, systemImage: "arrow.clockwise")
                }
                .buttonStyle(PrimaryButtonStyle(compact: true))
                .disabled(model.busy)
            }
        }
    }

    private func countBadge(_ tone: StatusTone, label: String, count: Int) -> some View {
        HStack(spacing: 6) {
            StatusMark(tone: tone, label: label, size: 18)
            Text("\(label) \(count)")
                .font(.system(size: 12, weight: .semibold))
                .foregroundStyle(Palette.tone(tone))
        }
        .padding(.horizontal, 10)
        .padding(.vertical, 5)
        .background(Capsule().fill(Palette.toneSoft(tone)))
        .overlay(Capsule().strokeBorder(Palette.toneLine(tone), lineWidth: 1))
    }

    @ViewBuilder private func rows(_ list: [EnvRow]) -> some View {
        ForEach(Array(list.enumerated()), id: \.element.id) { i, r in
            if i > 0 { RowSeparator() }
            row(r)
        }
    }

    private func row(_ r: EnvRow) -> some View {
        CardRow(alignment: .top) {
            StatusMark(tone: r.tone, label: EnvModel.resultTitle(r.result), size: 20)
                .padding(.top, 1)
            VStack(alignment: .leading, spacing: 4) {
                HStack(spacing: 7) {
                    InfoPill(text: r.id, monospaced: true)
                    Text(r.name).font(.system(size: 13, weight: .semibold))
                }
                if !r.detail.isEmpty {
                    Text(r.detail)
                        .font(.callout)
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                }
                if !r.fix.isEmpty {
                    HStack(alignment: .top, spacing: 5) {
                        Image(systemName: "wrench.and.screwdriver")
                            .font(.system(size: 10))
                            .foregroundStyle(Palette.accent)
                            .padding(.top, 2)
                        Text(r.fix)
                            .font(.caption)
                            .foregroundStyle(.secondary)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                    .padding(.top, 1)
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            StatusBadge(tone: r.tone, text: EnvModel.resultTitle(r.result))
            if r.hard {
                Toggle(L10n.hardItemLocked, isOn: .constant(true))
                    .labelsHidden()
                    .toggleStyle(.switch)
                    .controlSize(.small)
                    .disabled(true)
                    .help(L10n.hardItemLocked)
            } else {
                Toggle(r.name, isOn: Binding(get: { r.enabled }, set: { on in Task { await model.setEnabled(r.id, on) } }))
                    .labelsHidden()
                    .toggleStyle(.switch)
                    .controlSize(.small)
            }
        }
    }
}
