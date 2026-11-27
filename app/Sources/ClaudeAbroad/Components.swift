// 各页面共用的样式部件（原型 ui/design/index.html 的分组表单 .grp/.box/.r、徽标 .badge、标记 .mk、横幅）。
// 颜色只从 Palette 取；用颜色表示状态的部件都同时带文字或图形。
import AbroadAPI
import AbroadKit
import SwiftUI

/// 页面：可滚动，内容按分组从上往下排（原型 .scroll）。
struct PageScroll<Content: View>: View {
    @ViewBuilder let content: () -> Content
    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 18) { content() }
                .padding(.horizontal, 24)
                .padding(.top, 18)
                .padding(.bottom, 28)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
        .background(Palette.window)
    }
}

/// 分组：标题（可带右侧的附加内容）、白底圆角框、框下的说明（原型 .grp）。
struct CardSection<Content: View, Trailing: View>: View {
    let title: String?
    var footer: String? = nil
    @ViewBuilder let trailing: () -> Trailing
    @ViewBuilder let content: () -> Content

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            if title != nil || Trailing.self != EmptyView.self {
                HStack(alignment: .lastTextBaseline, spacing: 8) {
                    if let title { Text(title).font(.system(size: 13, weight: .semibold)).accessibilityAddTraits(.isHeader) }
                    Spacer(minLength: 8)
                    trailing()
                }
                .padding(.horizontal, 4)
            }
            VStack(alignment: .leading, spacing: 0) { content() }
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(RoundedRectangle(cornerRadius: 10).fill(Palette.box))
                .overlay(RoundedRectangle(cornerRadius: 10).strokeBorder(Palette.boxLine, lineWidth: 1))
            if let footer {
                Text(footer).font(.caption).foregroundStyle(.secondary).padding(.horizontal, 4)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
    }
}

extension CardSection where Trailing == EmptyView {
    init(_ title: String?, footer: String? = nil, @ViewBuilder content: @escaping () -> Content) {
        self.title = title
        self.footer = footer
        self.trailing = { EmptyView() }
        self.content = content
    }
}

extension CardSection {
    init(_ title: String?, footer: String? = nil, @ViewBuilder trailing: @escaping () -> Trailing, @ViewBuilder content: @escaping () -> Content) {
        self.title = title
        self.footer = footer
        self.trailing = trailing
        self.content = content
    }
}

/// 分组框里的一行（原型 .r）。上一行和这一行之间要分隔线时在前面放 RowSeparator。
struct CardRow<Content: View>: View {
    var alignment: VerticalAlignment = .center
    @ViewBuilder let content: () -> Content
    var body: some View {
        HStack(alignment: alignment, spacing: 12) { content() }
            .padding(.horizontal, 12)
            .padding(.vertical, 8)
            .frame(maxWidth: .infinity, minHeight: 38, alignment: .leading)
    }
}

/// 行与行之间的分隔线（左边留出和文字对齐的缩进）。
struct RowSeparator: View {
    var body: some View {
        Rectangle().fill(Palette.separator).frame(height: 1).padding(.leading, 12)
    }
}

/// 一行里的左侧：标题和下面的小字说明（原型 .r .l）。
struct RowLabel: View {
    let title: String
    var detail: String? = nil
    var bold = false
    var body: some View {
        VStack(alignment: .leading, spacing: 1) {
            Text(title).fontWeight(bold ? .semibold : .regular)
            if let detail, !detail.isEmpty {
                Text(detail).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}

/// 徽标：圆点加文字，浅底（原型 .badge）。颜色之外总有文字。
struct StatusBadge: View {
    let tone: StatusTone
    let text: String
    var large = false
    var body: some View {
        HStack(spacing: 5) {
            Circle().fill(Palette.tone(tone)).frame(width: 7, height: 7)
            Text(text).font(.system(size: large ? 12 : 11, weight: .semibold)).lineLimit(1)
        }
        .foregroundStyle(Palette.tone(tone))
        .padding(.horizontal, 7)
        .frame(height: large ? 22 : 18)
        .background(RoundedRectangle(cornerRadius: 5).fill(Palette.toneSoft(tone)))
        .accessibilityElement(children: .combine)
    }
}

/// 状态标记：实心圆里的图形（勾、叉、感叹号、横线；原型 .mk）。图形本身区分状态，并带无障碍文字。
struct StatusMark: View {
    let tone: StatusTone
    let label: String
    var body: some View {
        ZStack {
            Circle().fill(tone == .local ? Palette.graySoft : Palette.tone(tone))
            Image(systemName: Self.symbol(tone))
                .font(.system(size: 9, weight: .bold))
                .foregroundStyle(tone == .local ? Palette.gray : Palette.onTone)
        }
        .frame(width: 18, height: 18)
        .accessibilityElement()
        .accessibilityLabel(Text(label))
    }

    static func symbol(_ t: StatusTone) -> String {
        switch t {
        case .pass: return "checkmark"
        case .blocked: return "xmark"
        case .checking: return "exclamationmark"
        case .local: return "minus"
        }
    }
}

/// 带图形的一行彩色说明（例如回退到主代理、待还原）：图形和文字同时出现，颜色只是辅助。
struct ToneNote: View {
    let tone: StatusTone
    let text: String
    var symbol: String = "exclamationmark.triangle"
    var body: some View {
        Label { Text(text).fixedSize(horizontal: false, vertical: true) } icon: { Image(systemName: symbol) }
            .font(.caption)
            .foregroundStyle(Palette.tone(tone))
            .padding(.horizontal, 7)
            .padding(.vertical, 3)
            .background(RoundedRectangle(cornerRadius: 5).fill(Palette.toneSoft(tone)))
    }
}

/// 横幅（原型 .pending）：需要留意的事，黄色浅底、图标和粗体标题。
struct Banner: View {
    let title: String
    let detail: String
    let symbol: String
    var body: some View {
        HStack(alignment: .top, spacing: 10) {
            Image(systemName: symbol).foregroundStyle(Palette.tone(.attention)).font(.system(size: 15))
            VStack(alignment: .leading, spacing: 2) {
                Text(title).bold()
                Text(detail).font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            }
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 9)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(RoundedRectangle(cornerRadius: 10).fill(Palette.toneSoft(.attention)))
    }
}

/// 后台的错误按“做什么失败了、原因、下一步”三段原样显示（v2.5 决定 7）。红色浅底加叉形图标。
struct ErrorBlock: View {
    let text: ErrorText
    var body: some View {
        HStack(alignment: .top, spacing: 10) {
            Image(systemName: "xmark.octagon").foregroundStyle(Palette.tone(.failure)).font(.system(size: 15))
            MessageText(text: text)
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 9)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(RoundedRectangle(cornerRadius: 10).fill(Palette.toneSoft(.failure)))
    }
}

/// 三段文字（不带底色）：确认框里后台的说明用它。
struct MessageText: View {
    let text: ErrorText
    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            if !text.action.isEmpty { Text(text.action).bold() }
            Text(text.reason).fixedSize(horizontal: false, vertical: true)
            if !text.next.isEmpty { Text(text.next).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true) }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}

/// 命令、路径这类要能选中复制的等宽文字（浅底）。
struct CodeBlock: View {
    let text: String
    var body: some View {
        Text(text).font(.system(size: 12, design: .monospaced)).textSelection(.enabled)
            .padding(.horizontal, 10).padding(.vertical, 8)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(RoundedRectangle(cornerRadius: 7).fill(Palette.codeBack))
    }
}

/// 弹出框的头部：应用图标加标题（原型 .sheet 头部）。
struct SheetHeader: View {
    let title: String
    var symbol: String? = nil
    var body: some View {
        HStack(alignment: .center, spacing: 12) {
            Image(nsImage: NSApplication.shared.applicationIconImage)
                .resizable().frame(width: 40, height: 40)
                .overlay(alignment: .bottomTrailing) {
                    if let symbol {
                        Image(systemName: symbol).font(.system(size: 11, weight: .bold))
                            .foregroundStyle(Palette.onAccent)
                            .frame(width: 20, height: 20)
                            .background(RoundedRectangle(cornerRadius: 5).fill(Palette.accent))
                            .offset(x: 4, y: 3)
                    }
                }
                .accessibilityHidden(true)
            Text(title).font(.system(size: 15, weight: .bold))
        }
    }
}

/// 弹出框底部的按钮条。
struct SheetFooter<Content: View>: View {
    @ViewBuilder let content: () -> Content
    var body: some View {
        HStack(spacing: 8) { content() }.padding(.top, 4)
    }
}
