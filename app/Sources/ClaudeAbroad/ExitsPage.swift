// 分流出口（原型 ui/design/index.html 的 pageExits）：已添加的出口在一个分组框里逐行列出（标签、类型、状态徽标、检查结果、连接数），
// 下面是导入出口的表单。
import AbroadKit
import SwiftUI

struct ExitsPage: View {
    @ObservedObject var model: ExitsModel
    var body: some View {
        LoadStateView(state: model.state, retry: { Task { await model.load() } }) {
            PageScroll {
                CardSection(nil) {
                    if let e = model.emptyText {
                        CardRow { Text(e).foregroundStyle(.secondary) }
                    }
                    ForEach(Array(model.rows.enumerated()), id: \.element.id) { i, r in
                        if i > 0 { RowSeparator() }
                        row(r)
                    }
                }
                addBox
            }
        }
        .modifier(PageChrome(model: model))
        .task { await model.load() }
    }

    private func row(_ r: ExitRow) -> some View {
        CardRow(alignment: .top) {
            Text(verbatim: String(r.kind.prefix(2)).uppercased())
                .font(.system(size: 10, weight: .bold))
                .foregroundStyle(Palette.onAccent)
                .frame(width: 24, height: 24)
                .background(RoundedRectangle(cornerRadius: 6).fill(Palette.accent))
                .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 3) {
                HStack(spacing: 6) {
                    Text(r.tag).font(.system(size: 13, weight: .semibold, design: .monospaced))
                    Text(r.kind).foregroundStyle(.secondary)
                }
                if !r.reason.isEmpty { Text(r.reason).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true) }
                if let a = r.affectedText { ToneNote(tone: .attention, text: a, symbol: "arrow.uturn.backward.circle") } // 补齐二
                if !r.items.isEmpty {
                    Text(r.items.sorted(by: { $0.key < $1.key }).map { $0.key + " " + EnvModel.resultTitle($0.value) }.joined(separator: " · ")).font(.caption)
                }
                Text(L10n.exitConns(r.tcp, r.udp)).font(.caption).foregroundStyle(.secondary)
                HStack {
                    Button(L10n.exitCheck) { Task { await model.check(r.tag) } }
                    Button(L10n.delete) { Task { await model.remove(r.tag) } }
                }
                .padding(.top, 2)
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            StatusBadge(tone: r.tone, text: r.up ? L10n.exitUp : L10n.exitDown)
        }
    }

    /// 导入出口（原型的“添加分流出口”在弹出框里；这里照旧是页面里的表单）。
    private var addBox: some View {
        CardSection(L10n.exitAdd) {
            CardRow { label(L10n.exitTag); TextField(L10n.exitTag, text: $model.formTag).labelsHidden() }
            RowSeparator()
            CardRow {
                label(L10n.exitKind)
                Picker(L10n.exitKind, selection: $model.formKind) {
                    ForEach(ExitsModel.kinds, id: \.self) { Text(verbatim: $0).tag($0) }
                }
                .labelsHidden()
                .fixedSize()
                Spacer(minLength: 0)
            }
            RowSeparator()
            CardRow {
                Spacer().frame(width: 90)
                Button(L10n.chooseFile) {
                    let p = NSOpenPanel()
                    if p.runModal() == .OK { model.formFile = p.url }
                }
                Text(model.formFile?.lastPathComponent ?? "").foregroundStyle(.secondary).lineLimit(1).truncationMode(.middle)
                Spacer(minLength: 8)
                Button(L10n.submit) { Task { await model.add() } }.buttonStyle(.borderedProminent).disabled(model.busy)
            }
        }
    }

    private func label(_ t: String) -> some View {
        Text(t).frame(width: 90, alignment: .leading)
    }
}
