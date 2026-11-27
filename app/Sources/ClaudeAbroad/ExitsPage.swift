// 分流出口（原型 ui/design/index.html 的 pageExits）：已添加的出口在一个分组框里逐行列出（图标、标签、类型、状态徽标、检查结果、连接数），
// 下面是导入出口的表单。
import AbroadKit
import SwiftUI

struct ExitsPage: View {
    @ObservedObject var model: ExitsModel
    var body: some View {
        LoadStateView(state: model.state, retry: { Task { await model.load() } }) {
            PageScroll {
                CardSection(L10n.exitsTitle, icon: "arrow.triangle.branch") {
                    if let e = model.emptyText {
                        CardRow {
                            IconBadge(symbol: "arrow.triangle.branch", size: 28)
                            Text(e).foregroundStyle(.secondary)
                        }
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
                .font(.system(size: 11, weight: .bold, design: .monospaced))
                .foregroundStyle(Palette.onAccent)
                .frame(width: 30, height: 30)
                .background(RoundedRectangle(cornerRadius: 8, style: .continuous).fill(Palette.accent))
                .accessibilityHidden(true)

            VStack(alignment: .leading, spacing: 6) {
                HStack(spacing: 8) {
                    Text(r.tag)
                        .font(.system(size: 14, weight: .semibold, design: .monospaced))
                    InfoPill(text: r.kind)
                    InfoPill(text: L10n.exitConns(r.tcp, r.udp), monospaced: true)
                }

                if !r.reason.isEmpty {
                    Text(r.reason)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                }

                if let a = r.affectedText {
                    ToneNote(tone: .attention, text: a, symbol: "arrow.uturn.backward.circle")
                }

                if !r.items.isEmpty {
                    HStack(spacing: 6) {
                        ForEach(r.items.sorted(by: { $0.key < $1.key }), id: \.key) { key, val in
                            let tone = StatusTone.checkResult(val)
                            HStack(spacing: 4) {
                                StatusMark(tone: tone, label: EnvModel.resultTitle(val), size: 14)
                                Text(key + " " + EnvModel.resultTitle(val))
                                    .font(.system(size: 11, weight: .medium))
                                    .foregroundStyle(.secondary)
                            }
                            .padding(.horizontal, 7)
                            .padding(.vertical, 3)
                            .background(RoundedRectangle(cornerRadius: 6, style: .continuous).fill(Palette.graySoft))
                        }
                    }
                }

                HStack(spacing: 8) {
                    Button { Task { await model.check(r.tag) } } label: {
                        Label(L10n.exitCheck, systemImage: "arrow.clockwise")
                    }
                    .buttonStyle(SecondaryButtonStyle(compact: true))

                    Button(L10n.delete) { Task { await model.remove(r.tag) } }
                        .buttonStyle(SecondaryButtonStyle(compact: true, tone: .blocked))
                }
                .padding(.top, 2)
            }
            .frame(maxWidth: .infinity, alignment: .leading)

            StatusBadge(tone: r.tone, text: r.up ? L10n.exitUp : L10n.exitDown, large: true)
        }
    }

    /// 导入出口（原型的“添加分流出口”在弹出框里；这里照旧是页面里的表单）。
    private var addBox: some View {
        CardSection(L10n.exitAdd, icon: "plus.circle") {
            CardRow {
                label(L10n.exitTag)
                StyledTextField(placeholder: L10n.exitTag, text: $model.formTag, monospaced: true)
            }
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
                Button {
                    let p = NSOpenPanel()
                    if p.runModal() == .OK { model.formFile = p.url }
                } label: {
                    Label(L10n.chooseFile, systemImage: "doc")
                }
                .buttonStyle(SecondaryButtonStyle(compact: true))

                if let file = model.formFile {
                    InfoPill(text: file.lastPathComponent, monospaced: true)
                }
                Spacer(minLength: 8)
                Button { Task { await model.add() } } label: {
                    Label(L10n.submit, systemImage: "plus")
                }
                .buttonStyle(PrimaryButtonStyle())
                .disabled(model.busy)
            }
        }
    }

    private func label(_ t: String) -> some View {
        Text(t)
            .font(.system(size: 13, weight: .medium))
            .foregroundStyle(.secondary)
            .frame(width: 90, alignment: .leading)
    }
}
