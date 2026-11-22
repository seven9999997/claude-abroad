// M5-1 探查 E6：只用 Command Line Tools 编出带菜单栏图标的 SwiftUI 程序（会话里只编译、不启动）。
import APITypes
import SwiftUI

/// 只装命令行工具时没有 SwiftUI 的宏插件（SwiftUIMacros），新 SDK 里的 @State 是宏，用不了；
/// 改用 ObservableObject + @StateObject（属性包装器，不是宏）。
final class Model: ObservableObject {
    @Published var state = "未知"
}

@main
struct MenuBarDemoApp: App {
    @StateObject private var model = Model()
    var state: String { model.state }
    var body: some Scene {
        MenuBarExtra("Claude 出国（探查）", systemImage: "globe.asia.australia") {
            Text("状态：\(state)")
            Divider()
            Button("退出") { NSApplication.shared.terminate(nil) }
        }
        Window("Claude 出国（探查）", id: "main") {
            VStack(alignment: .leading, spacing: 8) {
                Text("M5-1 探查用的最小窗口").font(.headline)
                Text("状态：\(state)")
            }
            .padding()
            .frame(minWidth: 320, minHeight: 160)
        }
    }
}
