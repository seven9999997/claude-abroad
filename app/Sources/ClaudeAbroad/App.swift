// Claude 出国的 Mac 应用入口（M5-7）：菜单栏图标和主窗口。所有判断在 abroadd 里，应用只是接口的一个客户端（v2.5 决定 1）。
// 只用 Command Line Tools 构建：不用 SwiftUI 的宏（@State、@Entry、#Preview、@Observable），状态用 ObservableObject。
// 会话里只编译、不启动（走查交用户，见 docs/m5-walkthrough-draft.md）。
import AbroadKit
import AppKit
import SwiftUI

@main
struct ClaudeAbroadApp: App {
    private static let client = APIClient(transport: UnixTransport.fromEnvironment())
    /// 各页面共用一个确认框（同一时间只有一个）和一个通知者。
    @MainActor private static let flow = ConfirmFlow(client: client)
    @MainActor private static let notifier = SystemServices.makeNotifier()
    @StateObject private var model = StatusModel(client: ClaudeAbroadApp.client, flow: ClaudeAbroadApp.flow, notifier: ClaudeAbroadApp.notifier)
    @StateObject private var setup = SetupModel(client: ClaudeAbroadApp.client)
    @StateObject private var main = MainProxyModel(client: ClaudeAbroadApp.client, flow: ClaudeAbroadApp.flow, notifier: ClaudeAbroadApp.notifier)
    @StateObject private var exits = ExitsModel(client: ClaudeAbroadApp.client, flow: ClaudeAbroadApp.flow)
    @StateObject private var apps = AppsModel(client: ClaudeAbroadApp.client, flow: ClaudeAbroadApp.flow)
    @StateObject private var env = EnvModel(client: ClaudeAbroadApp.client, flow: ClaudeAbroadApp.flow)
    @StateObject private var region = RegionModel(client: ClaudeAbroadApp.client, flow: ClaudeAbroadApp.flow)
    @StateObject private var engines = EnginesModel(client: ClaudeAbroadApp.client, flow: ClaudeAbroadApp.flow)
    @StateObject private var settings = SettingsModel(client: ClaudeAbroadApp.client, preference: ClaudeAbroadApp.notifier.preference)
    @StateObject private var diagnostics = DiagnosticsExportModel(client: ClaudeAbroadApp.client, saver: SavePanelDiagnosticsSaver())

    var body: some Scene {
        // 主窗口放在第一个：启动时自动打开（第一级走查 L1-01）。只放在菜单栏时，刘海屏上图标可能被挡住，打开后什么都看不到。
        Window(L10n.appName, id: "main") {
            MainWindow(status: model, setup: setup, main: main, exits: exits, apps: apps, env: env, region: region, engines: engines, flow: ClaudeAbroadApp.flow)
                .frame(minWidth: 620, minHeight: 520)
                .task {
                    model.start()
                    NSApplication.shared.activate(ignoringOtherApps: true) // 从终端启动时切到前台
                }
        }
        MenuBarExtra {
            MenuContent(model: model)
        } label: {
            Image(systemName: model.phase.symbol)
                .accessibilityLabel(Text(L10n.titleLine(model.phase.title)))
                .task { model.start() }
        }
        Settings {
            SettingsView(model: settings, env: settings.env, diagnostics: diagnostics)
        }
    }
}

/// 主窗口：引导没完成时显示引导（完成与否由后台的进度接口决定），之后是首页和主代理页面。
struct MainWindow: View {
    @ObservedObject var status: StatusModel
    @ObservedObject var setup: SetupModel
    @ObservedObject var main: MainProxyModel
    let exits: ExitsModel
    let apps: AppsModel
    let env: EnvModel
    let region: RegionModel
    let engines: EnginesModel
    @ObservedObject var flow: ConfirmFlow

    var body: some View {
        content
            .sheet(isPresented: Binding(get: { flow.isActive }, set: { if !$0, flow.isActive { flow.cancel() } })) {
                ConfirmSheet(flow: flow)
            }
            .safeAreaInset(edge: .top) {
                if let note = flow.completedNote {
                    HStack {
                        Label(note, systemImage: "checkmark.circle")
                        Spacer()
                        Button(L10n.close) { flow.dismissCompletedNote() }
                    }
                    .padding(8)
                }
            }
    }

    @ViewBuilder private var content: some View {
        if setup.shouldShow && setup.step != .done {
            SetupView(setup: setup, main: main, status: status)
        } else {
            TabView {
                HomeView(model: status).tabItem { Text(L10n.homeTab) }
                MainProxyPage(model: main).padding(20).tabItem { Text(L10n.mainProxyTitle) }
                ExitsPage(model: exits).tabItem { Text(L10n.exitsTitle) }
                AppsPage(model: apps).tabItem { Text(L10n.appsTitle) }
                EnvPage(model: env).tabItem { Text(L10n.envTitle) }
                RegionPage(model: region).tabItem { Text(L10n.regionTitle) }
                EnginesPage(model: engines).tabItem { Text(L10n.enginesTitle) }
            }
        }
    }
}
