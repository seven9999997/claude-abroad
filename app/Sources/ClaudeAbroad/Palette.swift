// 全应用的配色（原型 ui/design/index.html 的 :root 和 [data-theme="dark"] 两套变量）。视图里的颜色只从这里取：
// 别的视图文件里不写色值、不用 .red / .orange 这类直接颜色（源码检查固定，见 RestyleTests.swift）。
// 每个颜色浅色、深色各一份，随系统外观自动切换。状态色的含义（什么状态用哪一种）在 AbroadKit 的 StatusTone.swift。
import AbroadKit
import AppKit
import SwiftUI

enum Palette {
    // 强调色：吉祥物的陶土橙（主按钮、侧边栏图标、当前一步）。
    static let accent = dynamic(light: (0xA9, 0x4E, 0x2E, 1), dark: (0xD5, 0x79, 0x54, 1))
    /// 强调色底上的文字。
    static let onAccent = dynamic(light: (0xFF, 0xFF, 0xFF, 1), dark: (0x1B, 0x11, 0x10, 1))
    /// 强调色的浅底和描边（选中态、徽标、步骤高亮）。
    static let accentSoft = dynamic(light: (0xA9, 0x4E, 0x2E, 0.11), dark: (0xD5, 0x79, 0x54, 0.16))
    static let accentLine = dynamic(light: (0xA9, 0x4E, 0x2E, 0.25), dark: (0xD5, 0x79, 0x54, 0.30))

    // 状态色（含义见 StatusTone）。
    static let teal = dynamic(light: (0x2F, 0x74, 0x86, 1), dark: (0x74, 0xB9, 0xC9, 1))
    static let yellow = dynamic(light: (0xA8, 0x77, 0x08, 1), dark: (0xE4, 0xB9, 0x4F, 1))
    static let red = dynamic(light: (0xC4, 0x32, 0x1F, 1), dark: (0xF2, 0x75, 0x63, 1))
    static let gray = dynamic(light: (0x85, 0x7E, 0x77, 1), dark: (0xA3, 0x9C, 0x95, 1))

    // 状态色的浅底（徽标、横幅、首页状态卡片的底色）。
    static let tealSoft = dynamic(light: (0x2F, 0x74, 0x86, 0.11), dark: (0x74, 0xB9, 0xC9, 0.15))
    static let yellowSoft = dynamic(light: (0xC9, 0x9A, 0x22, 0.15), dark: (0xE4, 0xB9, 0x4F, 0.16))
    static let redSoft = dynamic(light: (0xC4, 0x32, 0x1F, 0.10), dark: (0xF2, 0x75, 0x63, 0.15))
    static let graySoft = dynamic(light: (0x00, 0x00, 0x00, 0.06), dark: (0xFF, 0xFF, 0xFF, 0.07))

    // 状态色的描边（卡片、横幅、徽标的外框）。
    static let tealLine = dynamic(light: (0x2F, 0x74, 0x86, 0.24), dark: (0x74, 0xB9, 0xC9, 0.28))
    static let yellowLine = dynamic(light: (0xA8, 0x77, 0x08, 0.26), dark: (0xE4, 0xB9, 0x4F, 0.28))
    static let redLine = dynamic(light: (0xC4, 0x32, 0x1F, 0.24), dark: (0xF2, 0x75, 0x63, 0.28))
    static let grayLine = dynamic(light: (0x00, 0x00, 0x00, 0.11), dark: (0xFF, 0xFF, 0xFF, 0.12))

    // 窗口、卡片和控件层次。
    /// 页面底色。
    static let window = dynamic(light: (0xF6, 0xF5, 0xF3, 1), dark: (0x1F, 0x1E, 0x1D, 1))
    /// 分组框（白底卡片）。
    static let box = dynamic(light: (0xFF, 0xFF, 0xFF, 1), dark: (0x2A, 0x29, 0x28, 1))
    /// 稍高一层的表面（弹窗内卡片、侧边栏底部状态卡）。
    static let boxElevated = dynamic(light: (0xFF, 0xFF, 0xFF, 1), dark: (0x32, 0x30, 0x2F, 1))
    /// 卡片顶部内高光。
    static let boxHighlight = dynamic(light: (0xFF, 0xFF, 0xFF, 0.80), dark: (0xFF, 0xFF, 0xFF, 0.06))
    /// 分组框的描边。
    static let boxLine = dynamic(light: (0x00, 0x00, 0x00, 0.07), dark: (0xFF, 0xFF, 0xFF, 0.06))
    /// 卡片外阴影。
    static let cardShadow = dynamic(light: (0x1D, 0x16, 0x12, 0.06), dark: (0x00, 0x00, 0x00, 0.32))
    /// 行与行之间的分隔线。
    static let separator = dynamic(light: (0x00, 0x00, 0x00, 0.085), dark: (0xFF, 0xFF, 0xFF, 0.08))
    /// 悬停行的高亮底色。
    static let rowHover = dynamic(light: (0x00, 0x00, 0x00, 0.03), dark: (0xFF, 0xFF, 0xFF, 0.04))
    /// 控件表面（次级按钮、输入框）。
    static let control = dynamic(light: (0xFF, 0xFF, 0xFF, 1), dark: (0x36, 0x34, 0x32, 1))
    static let controlHover = dynamic(light: (0xF3, 0xF0, 0xEC, 1), dark: (0x40, 0x3D, 0x3B, 1))
    static let controlLine = dynamic(light: (0x00, 0x00, 0x00, 0.12), dark: (0xFF, 0xFF, 0xFF, 0.11))
    /// 安装引导左栏的底色（和侧边栏一致的暖灰）。
    static let side = dynamic(light: (0xEC, 0xE7, 0xE2, 0.86), dark: (0x2A, 0x28, 0x26, 0.86))
    /// 命令、等宽文字的底色和描边。
    static let codeBack = dynamic(light: (0x00, 0x00, 0x00, 0.05), dark: (0xFF, 0xFF, 0xFF, 0.07))
    static let codeBorder = dynamic(light: (0x00, 0x00, 0x00, 0.08), dark: (0xFF, 0xFF, 0xFF, 0.09))
    /// 状态标记里图形的颜色（实心圆上的勾、叉）。
    static let onTone = dynamic(light: (0xFF, 0xFF, 0xFF, 1), dark: (0x1B, 0x1A, 0x19, 1))
    /// 透明占位。
    static let clear = dynamic(light: (0x00, 0x00, 0x00, 0), dark: (0x00, 0x00, 0x00, 0))

    /// 状态色。
    static func tone(_ t: StatusTone) -> Color {
        switch t {
        case .pass: return teal
        case .checking: return yellow
        case .blocked: return red
        case .local: return gray
        }
    }

    /// 状态色的浅底。
    static func toneSoft(_ t: StatusTone) -> Color {
        switch t {
        case .pass: return tealSoft
        case .checking: return yellowSoft
        case .blocked: return redSoft
        case .local: return graySoft
        }
    }

    /// 状态色的描边。
    static func toneLine(_ t: StatusTone) -> Color {
        switch t {
        case .pass: return tealLine
        case .checking: return yellowLine
        case .blocked: return redLine
        case .local: return grayLine
        }
    }

    /// 按系统外观切换的颜色（红、绿、蓝 0–255，透明度 0–1）。
    private static func dynamic(light: (Int, Int, Int, Double), dark: (Int, Int, Int, Double)) -> Color {
        Color(nsColor: NSColor(name: nil) { appearance in
            let c = appearance.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua ? dark : light
            return NSColor(srgbRed: CGFloat(c.0) / 255, green: CGFloat(c.1) / 255, blue: CGFloat(c.2) / 255, alpha: CGFloat(c.3))
        })
    }
}
