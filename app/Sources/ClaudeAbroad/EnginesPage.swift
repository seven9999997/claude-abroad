// 引擎与虚拟机（原型 ui/design/index.html 的 pageEngines，只读）：虚拟机一个分组（管理状态、版本、内核、运行时间），引擎一个分组。
import AbroadKit
import SwiftUI

struct EnginesPage: View {
    @ObservedObject var model: EnginesModel
    var body: some View {
        LoadStateView(state: model.state, retry: { Task { await model.load() } }) {
            PageScroll {
                CardSection(L10n.vmSection) {
                    if let m = model.view?.vmManager {
                        CardRow { Text(L10n.vmManagerLine(m.phase, m.restarts)) }
                        if let e = m.lastError, !e.isEmpty {
                            RowSeparator()
                            CardRow { RowLabel(title: L10n.vmLastError); Text(e).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true) }
                        }
                        RowSeparator()
                    }
                    if let t = model.vmText {
                        CardRow { Text(t).foregroundStyle(.secondary) }
                    } else if let v = model.view {
                        CardRow { RowLabel(title: L10n.vmVersion); Text(v.version ?? "").foregroundStyle(.secondary).textSelection(.enabled) }
                        RowSeparator()
                        CardRow { RowLabel(title: L10n.vmKernel); Text(v.kernel ?? "").foregroundStyle(.secondary).textSelection(.enabled) }
                        RowSeparator()
                        CardRow { RowLabel(title: L10n.vmUptime); Text(L10n.seconds(v.uptimeS ?? 0)).foregroundStyle(.secondary) }
                        if v.bootChanged {
                            RowSeparator()
                            CardRow { ToneNote(tone: .attention, text: L10n.vmBootChanged, symbol: "arrow.clockwise") }
                        }
                    }
                }
                if model.vmText == nil, let engines = model.view?.engines, !engines.isEmpty {
                    CardSection(L10n.enginesSection) {
                        ForEach(Array(engines.enumerated()), id: \.element.name) { i, e in
                            if i > 0 { RowSeparator() }
                            CardRow {
                                Image(systemName: "cpu").foregroundStyle(Palette.accent).accessibilityHidden(true)
                                Text(L10n.engineLine(e.name, e.state, e.restarts))
                            }
                        }
                    }
                }
            }
        }
        .task { await model.load() }
    }
}
