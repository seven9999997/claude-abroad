// Claude 出国的 Mac 应用入口（M5-7）：菜单栏图标和主窗口。所有判断在 abroadd 里，应用只是接口的一个客户端（v2.5 决定 1）。
// 只用 Command Line Tools 构建：不用 SwiftUI 的宏（@State、@Entry、#Preview、@Observable），状态用 ObservableObject。
// 会话里只编译、不启动（走查交用户，见 docs/m5-walkthrough-draft.md）。
import AbroadKit
import SwiftUI

@main
struct ClaudeAbroadApp: App {
    @StateObject private var model = StatusModel(client: APIClient(transport: UnixTransport.fromEnvironment()))

    var body: some Scene {
        MenuBarExtra {
            MenuContent(model: model)
        } label: {
            Image(systemName: model.phase.symbol)
                .accessibilityLabel(Text(L10n.appName + "：" + model.phase.title))
                .task { model.start() }
        }
        Window(L10n.appName, id: "main") {
            HomeView(model: model)
                .frame(minWidth: 520, minHeight: 420)
                .task { model.start() }
        }
    }
}
