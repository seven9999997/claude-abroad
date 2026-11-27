// 引擎与虚拟机（原型 ui/design/index.html 的 pageEngines，只读）：虚拟机一个分组（管理状态、版本、内核、运行时间），引擎一个分组。
import AbroadKit
import SwiftUI

struct EnginesPage: View {
    @ObservedObject var model: EnginesModel
    var body: some View {
        LoadStateView(state: model.state, retry: { Task { await model.load() } }) {
            PageScroll {
                CardSection(L10n.vmSection, icon: "desktopcomputer") {
                    if let m = model.view?.vmManager {
                        CardRow {
                            IconBadge(symbol: "desktopcomputer", tone: vmTone(m.phase), size: 32)
                            RowLabel(title: L10n.vmManagerLine(m.phase, m.restarts))
                            StatusBadge(tone: vmTone(m.phase), text: L10n.vmPhaseName(m.phase))
                        }
                        if m.borrowed == true {
                            RowSeparator()
                            CardRow { ToneNote(tone: .attention, text: L10n.vmBorrowed, symbol: "clock") }
                        }
                        if let e = m.lastError, !e.isEmpty {
                            RowSeparator()
                            CardRow(alignment: .top) {
                                RowLabel(title: L10n.vmLastError, bold: true)
                                Text(e)
                                    .font(.callout)
                                    .foregroundStyle(.secondary)
                                    .fixedSize(horizontal: false, vertical: true)
                            }
                        }
                        RowSeparator()
                    }
                    if let t = model.vmText {
                        CardRow {
                            IconBadge(symbol: "bolt.slash", tone: .local, size: 28)
                            Text(t).foregroundStyle(.secondary)
                        }
                    } else if let v = model.view {
                        CardRow {
                            RowLabel(title: L10n.vmVersion)
                            Text(v.version ?? "")
                                .font(.system(size: 12, design: .monospaced))
                                .foregroundStyle(.secondary)
                                .textSelection(.enabled)
                        }
                        RowSeparator()
                        CardRow {
                            RowLabel(title: L10n.vmKernel)
                            Text(v.kernel ?? "")
                                .font(.system(size: 12, design: .monospaced))
                                .foregroundStyle(.secondary)
                                .textSelection(.enabled)
                        }
                        RowSeparator()
                        CardRow {
                            RowLabel(title: L10n.vmUptime)
                            InfoPill(text: L10n.seconds(v.uptimeS ?? 0), monospaced: true)
                        }
                        if v.bootChanged {
                            RowSeparator()
                            CardRow { ToneNote(tone: .attention, text: L10n.vmBootChanged, symbol: "arrow.clockwise") }
                        }
                    }
                }
                if model.vmText == nil, let engines = model.view?.engines, !engines.isEmpty {
                    CardSection(L10n.enginesSection, icon: "cpu") {
                        ForEach(Array(engines.enumerated()), id: \.element.name) { i, e in
                            if i > 0 { RowSeparator() }
                            CardRow {
                                IconBadge(symbol: "cpu", tone: engineTone(e.state), size: 30)
                                RowLabel(title: e.name, detail: L10n.engineLine(e.name, e.state, e.restarts))
                                StatusBadge(tone: engineTone(e.state), text: e.state)
                            }
                        }
                    }
                }
            }
        }
        .task { await model.load() }
    }

    private func vmTone(_ phase: String) -> StatusTone {
        switch phase {
        case "running": return .pass
        case "starting", "stopping", "backoff": return .checking
        case "unhealthy", "failed", "stuck": return .blocked
        default: return .local
        }
    }

    private func engineTone(_ state: String) -> StatusTone {
        let s = state.lowercased()
        if s.contains("run") || s == "up" || s == "ok" { return .pass }
        if s.contains("start") || s.contains("wait") { return .checking }
        if s.contains("fail") || s.contains("err") || s.contains("down") { return .blocked }
        return .local
    }
}
