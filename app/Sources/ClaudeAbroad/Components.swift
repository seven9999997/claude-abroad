// 各页面共用的样式部件（原型 ui/design/index.html 的分组表单 .grp/.box/.r、徽标 .badge、标记 .mk、横幅）。
// 颜色只从 Palette 取；用颜色表示状态的部件都同时带文字或图形。
import AbroadAPI
import AbroadKit
import AppKit
import SwiftUI

/// 页面：可滚动，内容按分组从上往下排并居中限宽，保持宽屏下的阅读舒适度。
struct PageScroll<Content: View>: View {
    var maxWidth: CGFloat = 860
    @ViewBuilder let content: () -> Content

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 20) { content() }
                .frame(maxWidth: maxWidth, alignment: .leading)
                .frame(maxWidth: .infinity, alignment: .center)
                .padding(.horizontal, 28)
                .padding(.top, 20)
                .padding(.bottom, 32)
        }
        .background(Palette.window)
    }
}

/// 分组：标题（可带图标和右侧的附加内容）、白底圆角卡片（带描边和柔和阴影）、框下的说明。
struct CardSection<Content: View, Trailing: View>: View {
    let title: String?
    var icon: String? = nil
    var footer: String? = nil
    @ViewBuilder let trailing: () -> Trailing
    @ViewBuilder let content: () -> Content

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            if title != nil || Trailing.self != EmptyView.self {
                HStack(alignment: .center, spacing: 6) {
                    if let icon {
                        Image(systemName: icon)
                            .font(.system(size: 12, weight: .semibold))
                            .foregroundStyle(Palette.accent)
                            .accessibilityHidden(true)
                    }
                    if let title {
                        Text(title)
                            .font(.system(size: 13, weight: .semibold))
                            .foregroundStyle(.primary)
                            .accessibilityAddTraits(.isHeader)
                    }
                    Spacer(minLength: 8)
                    trailing()
                }
                .padding(.horizontal, 4)
            }
            VStack(alignment: .leading, spacing: 0) { content() }
                .frame(maxWidth: .infinity, alignment: .leading)
                .background(
                    RoundedRectangle(cornerRadius: 12, style: .continuous)
                        .fill(Palette.box)
                        .shadow(color: Palette.cardShadow, radius: 6, x: 0, y: 2)
                )
                .overlay(
                    RoundedRectangle(cornerRadius: 12, style: .continuous)
                        .strokeBorder(Palette.boxLine, lineWidth: 1)
                )
            if let footer {
                Text(footer)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .lineSpacing(2)
                    .padding(.horizontal, 6)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
    }
}

extension CardSection where Trailing == EmptyView {
    init(_ title: String?, icon: String? = nil, footer: String? = nil, @ViewBuilder content: @escaping () -> Content) {
        self.title = title
        self.icon = icon
        self.footer = footer
        self.trailing = { EmptyView() }
        self.content = content
    }
}

extension CardSection {
    init(_ title: String?, icon: String? = nil, footer: String? = nil, @ViewBuilder trailing: @escaping () -> Trailing, @ViewBuilder content: @escaping () -> Content) {
        self.title = title
        self.icon = icon
        self.footer = footer
        self.trailing = trailing
        self.content = content
    }
}

/// 分组框里的一行（原型 .r）。上一行和这一行之间要分隔线时在前面放 RowSeparator。
struct CardRow<Content: View>: View {
    var alignment: VerticalAlignment = .center
    var verticalPadding: CGFloat = 10
    @ViewBuilder let content: () -> Content
    var body: some View {
        HStack(alignment: alignment, spacing: 12) { content() }
            .padding(.horizontal, 16)
            .padding(.vertical, verticalPadding)
            .frame(maxWidth: .infinity, minHeight: 42, alignment: .leading)
    }
}

/// 行与行之间的分隔线（左边留出和文字对齐的缩进）。
struct RowSeparator: View {
    var inset: CGFloat = 16
    var body: some View {
        Rectangle().fill(Palette.separator).frame(height: 1).padding(.leading, inset)
    }
}

/// 一行里的左侧：标题和下面的小字说明（原型 .r .l）。
struct RowLabel: View {
    let title: String
    var detail: String? = nil
    var bold = false
    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(title).fontWeight(bold ? .semibold : .medium)
            if let detail, !detail.isEmpty {
                Text(detail)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .lineSpacing(1.5)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}

/// 徽标：胶囊形浅底加描边、带状态圆点和文字（原型 .badge）。颜色之外总有文字。
struct StatusBadge: View {
    let tone: StatusTone
    let text: String
    var large = false
    var body: some View {
        HStack(spacing: large ? 6 : 5) {
            ZStack {
                if large {
                    Circle().fill(Palette.toneSoft(tone)).frame(width: 12, height: 12)
                }
                Circle().fill(Palette.tone(tone)).frame(width: large ? 7 : 6, height: large ? 7 : 6)
            }
            Text(text)
                .font(.system(size: large ? 12 : 11, weight: .semibold))
                .lineLimit(1)
        }
        .foregroundStyle(Palette.tone(tone))
        .padding(.horizontal, large ? 10 : 8)
        .frame(height: large ? 24 : 20)
        .background(Capsule().fill(Palette.toneSoft(tone)))
        .overlay(Capsule().strokeBorder(Palette.toneLine(tone), lineWidth: 1))
        .accessibilityElement(children: .combine)
    }
}

/// 状态标记：实心圆里的图形（勾、叉、感叹号、横线；原型 .mk）。图形本身区分状态，并带无障碍文字。
struct StatusMark: View {
    let tone: StatusTone
    let label: String
    var size: CGFloat = 20
    var body: some View {
        ZStack {
            Circle().fill(tone == .local ? Palette.graySoft : Palette.tone(tone))
            Image(systemName: Self.symbol(tone))
                .font(.system(size: size * 0.48, weight: .bold))
                .foregroundStyle(tone == .local ? Palette.gray : Palette.onTone)
        }
        .frame(width: size, height: size)
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

/// 轻量的标签胶囊（协议、编号、连接数等辅助信息）。
struct InfoPill: View {
    let text: String
    var monospaced = false
    var body: some View {
        Text(text)
            .font(.system(size: 11, weight: .medium, design: monospaced ? .monospaced : .default))
            .foregroundStyle(.secondary)
            .padding(.horizontal, 7)
            .padding(.vertical, 2.5)
            .background(RoundedRectangle(cornerRadius: 6, style: .continuous).fill(Palette.graySoft))
            .overlay(RoundedRectangle(cornerRadius: 6, style: .continuous).strokeBorder(Palette.boxLine, lineWidth: 0.5))
    }
}

/// 图标底托（列表行、卡片标题前的圆角方标）。
struct IconBadge: View {
    let symbol: String
    var tone: StatusTone? = nil
    var size: CGFloat = 28
    var body: some View {
        let fg = tone.map { Palette.tone($0) } ?? Palette.accent
        let bg = tone.map { Palette.toneSoft($0) } ?? Palette.accentSoft
        let bd = tone.map { Palette.toneLine($0) } ?? Palette.accentLine
        ZStack {
            RoundedRectangle(cornerRadius: size * 0.28, style: .continuous).fill(bg)
            RoundedRectangle(cornerRadius: size * 0.28, style: .continuous).strokeBorder(bd, lineWidth: 1)
            Image(systemName: symbol)
                .font(.system(size: size * 0.46, weight: .semibold))
                .foregroundStyle(fg)
        }
        .frame(width: size, height: size)
        .accessibilityHidden(true)
    }
}

/// 带图形的一行彩色说明（例如回退到主代理、待还原）：图形和文字同时出现，颜色只是辅助。
struct ToneNote: View {
    let tone: StatusTone
    let text: String
    var symbol: String = "exclamationmark.triangle"
    var body: some View {
        HStack(alignment: .top, spacing: 6) {
            Image(systemName: symbol)
                .font(.system(size: 11, weight: .semibold))
                .padding(.top, 1.5)
            Text(text)
                .font(.caption)
                .lineSpacing(1.5)
                .fixedSize(horizontal: false, vertical: true)
        }
        .foregroundStyle(Palette.tone(tone))
        .padding(.horizontal, 9)
        .padding(.vertical, 5)
        .background(RoundedRectangle(cornerRadius: 7, style: .continuous).fill(Palette.toneSoft(tone)))
        .overlay(RoundedRectangle(cornerRadius: 7, style: .continuous).strokeBorder(Palette.toneLine(tone), lineWidth: 1))
    }
}

/// 横幅（原型 .pending）：需要留意的事，黄色浅底、圆角图标托底和粗体标题。
struct Banner: View {
    let title: String
    let detail: String
    let symbol: String
    var body: some View {
        HStack(alignment: .top, spacing: 12) {
            IconBadge(symbol: symbol, tone: .attention, size: 30)
            VStack(alignment: .leading, spacing: 3) {
                Text(title).font(.system(size: 13, weight: .semibold))
                Text(detail)
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .lineSpacing(2)
                    .fixedSize(horizontal: false, vertical: true)
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(
            RoundedRectangle(cornerRadius: 12, style: .continuous)
                .fill(Palette.box)
                .overlay(RoundedRectangle(cornerRadius: 12, style: .continuous).fill(Palette.toneSoft(.attention)))
                .shadow(color: Palette.cardShadow, radius: 4, x: 0, y: 1)
        )
        .overlay(RoundedRectangle(cornerRadius: 12, style: .continuous).strokeBorder(Palette.toneLine(.attention), lineWidth: 1))
    }
}

/// 后台的错误按“做什么失败了、原因、下一步”三段原样显示（v2.5 决定 7）。红色浅底加叉形图标。
struct ErrorBlock: View {
    let text: ErrorText
    var body: some View {
        HStack(alignment: .top, spacing: 12) {
            IconBadge(symbol: "xmark.octagon", tone: .failure, size: 30)
            MessageText(text: text)
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(
            RoundedRectangle(cornerRadius: 12, style: .continuous)
                .fill(Palette.box)
                .overlay(RoundedRectangle(cornerRadius: 12, style: .continuous).fill(Palette.toneSoft(.failure)))
                .shadow(color: Palette.cardShadow, radius: 4, x: 0, y: 1)
        )
        .overlay(RoundedRectangle(cornerRadius: 12, style: .continuous).strokeBorder(Palette.toneLine(.failure), lineWidth: 1))
    }
}

/// 三段文字（不带底色）：确认框里后台的说明用它。
struct MessageText: View {
    let text: ErrorText
    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            if !text.action.isEmpty { Text(text.action).fontWeight(.semibold) }
            Text(text.reason).lineSpacing(1.5).fixedSize(horizontal: false, vertical: true)
            if !text.next.isEmpty {
                Text(text.next)
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .lineSpacing(1.5)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}

@MainActor
private final class CopyFeedbackState: ObservableObject {
    @Published var copied = false
}

/// 命令、路径这类要能选中复制的等宽文字（带细框和右上角快捷复制按钮）。
struct CodeBlock: View {
    let text: String
    @StateObject private var feedback = CopyFeedbackState()

    var body: some View {
        HStack(alignment: .top, spacing: 8) {
            Text(text)
                .font(.system(size: 12, design: .monospaced))
                .textSelection(.enabled)
                .lineSpacing(2)
                .frame(maxWidth: .infinity, alignment: .leading)
            Button {
                NSPasteboard.general.clearContents()
                NSPasteboard.general.setString(text, forType: .string)
                feedback.copied = true
                Task { @MainActor in
                    try? await Task.sleep(nanoseconds: 1_400_000_000)
                    feedback.copied = false
                }
            } label: {
                Image(systemName: feedback.copied ? "checkmark" : "doc.on.doc")
                    .font(.system(size: 11, weight: .medium))
                    .foregroundStyle(feedback.copied ? Palette.tone(.success) : .secondary)
                    .frame(width: 22, height: 22)
                    .background(RoundedRectangle(cornerRadius: 5, style: .continuous).fill(Palette.box))
                    .overlay(RoundedRectangle(cornerRadius: 5, style: .continuous).strokeBorder(Palette.boxLine, lineWidth: 0.5))
            }
            .buttonStyle(.plain)
            .help(L10n.copyCommand)
            .accessibilityLabel(Text(L10n.copyCommand))
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 9)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(RoundedRectangle(cornerRadius: 8, style: .continuous).fill(Palette.codeBack))
        .overlay(RoundedRectangle(cornerRadius: 8, style: .continuous).strokeBorder(Palette.codeBorder, lineWidth: 1))
    }
}

/// 统一的柔和输入框容器。
struct StyledTextField: View {
    let placeholder: String
    @Binding var text: String
    var secure = false
    var monospaced = false

    var body: some View {
        Group {
            if secure {
                SecureField(placeholder, text: $text)
            } else {
                TextField(placeholder, text: $text)
            }
        }
        .textFieldStyle(.plain)
        .font(.system(size: 13, design: monospaced ? .monospaced : .default))
        .padding(.horizontal, 10)
        .padding(.vertical, 6)
        .background(RoundedRectangle(cornerRadius: 7, style: .continuous).fill(Palette.control))
        .overlay(RoundedRectangle(cornerRadius: 7, style: .continuous).strokeBorder(Palette.controlLine, lineWidth: 1))
    }
}

/// 主操作按钮样式（陶土橙实底、圆角、按压反馈）。
struct PrimaryButtonStyle: ButtonStyle {
    var compact = false
    @Environment(\.isEnabled) private var isEnabled

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(.system(size: compact ? 12 : 13, weight: .semibold))
            .foregroundStyle(Palette.onAccent)
            .padding(.horizontal, compact ? 12 : 14)
            .frame(height: compact ? 26 : 32)
            .background(
                RoundedRectangle(cornerRadius: compact ? 7 : 8, style: .continuous)
                    .fill(Palette.accent)
                    .opacity(isEnabled ? (configuration.isPressed ? 0.82 : 1) : 0.42)
            )
            .scaleEffect(configuration.isPressed ? 0.985 : 1)
            .animation(.easeOut(duration: 0.12), value: configuration.isPressed)
    }
}

/// 次级操作按钮样式（卡片白底、细边框、按压反馈）。
struct SecondaryButtonStyle: ButtonStyle {
    var compact = false
    var tone: StatusTone? = nil
    @Environment(\.isEnabled) private var isEnabled

    func makeBody(configuration: Configuration) -> some View {
        let fg = tone.map { Palette.tone($0) }
        return configuration.label
            .font(.system(size: compact ? 12 : 13, weight: .medium))
            .foregroundStyle(fg ?? .primary)
            .padding(.horizontal, compact ? 10 : 13)
            .frame(height: compact ? 26 : 32)
            .background(
                RoundedRectangle(cornerRadius: compact ? 7 : 8, style: .continuous)
                    .fill(configuration.isPressed && isEnabled ? Palette.controlHover : Palette.control)
                    .shadow(color: Palette.cardShadow, radius: 1.5, x: 0, y: 0.5)
            )
            .overlay(
                RoundedRectangle(cornerRadius: compact ? 7 : 8, style: .continuous)
                    .strokeBorder(tone.map { Palette.toneLine($0) } ?? Palette.controlLine, lineWidth: 1)
            )
            .opacity(isEnabled ? (configuration.isPressed ? 0.85 : 1) : 0.45)
            .scaleEffect(configuration.isPressed ? 0.985 : 1)
            .animation(.easeOut(duration: 0.12), value: configuration.isPressed)
    }
}

/// 弹出框的头部：应用图标加标题（原型 .sheet 头部）。
struct SheetHeader: View {
    let title: String
    var symbol: String? = nil
    var tone: StatusTone? = nil
    var body: some View {
        HStack(alignment: .center, spacing: 14) {
            Image(nsImage: NSApplication.shared.applicationIconImage)
                .resizable()
                .frame(width: 44, height: 44)
                .clipShape(RoundedRectangle(cornerRadius: 10, style: .continuous))
                .shadow(color: Palette.cardShadow, radius: 4, x: 0, y: 2)
                .overlay(alignment: .bottomTrailing) {
                    if let symbol {
                        Image(systemName: symbol)
                            .font(.system(size: 11, weight: .bold))
                            .foregroundStyle(tone == nil ? Palette.onAccent : Palette.onTone)
                            .frame(width: 21, height: 21)
                            .background(RoundedRectangle(cornerRadius: 6, style: .continuous).fill(tone.map { Palette.tone($0) } ?? Palette.accent))
                            .overlay(RoundedRectangle(cornerRadius: 6, style: .continuous).strokeBorder(Palette.box, lineWidth: 1.5))
                            .offset(x: 5, y: 4)
                    }
                }
                .accessibilityHidden(true)
            Text(title).font(.system(size: 16, weight: .bold))
        }
    }
}

/// 弹出框底部的按钮条。
struct SheetFooter<Content: View>: View {
    @ViewBuilder let content: () -> Content
    var body: some View {
        VStack(spacing: 12) {
            Rectangle().fill(Palette.separator).frame(height: 1)
            HStack(spacing: 8) { content() }
        }
        .padding(.top, 4)
    }
}
