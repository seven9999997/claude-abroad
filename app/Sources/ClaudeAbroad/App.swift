// Claude 出国的 Mac 应用入口（M5-7）：菜单栏图标和主窗口。所有判断在 abroadd 里，应用只是接口的一个客户端（v2.5 决定 1）。
// 只用 Command Line Tools 构建：不用 SwiftUI 的宏（@State、@Entry、#Preview、@Observable），状态用 ObservableObject。
// 走查清单见 docs/m5-walkthrough.md。
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
    /// 主窗口的纯界面状态（侧边栏选中项），不进业务模型。
    @StateObject private var ui = WindowUIState()

    var body: some Scene {
        // 主窗口放在第一个：启动时自动打开（第一级走查 L1-01）。只放在菜单栏时，刘海屏上图标可能被挡住，打开后什么都看不到。
        Window(L10n.appName, id: "main") {
            MainWindow(ui: ui, status: model, setup: setup, main: main, exits: exits, apps: apps, env: env, region: region, engines: engines, flow: ClaudeAbroadApp.flow)
                .frame(minWidth: 780, minHeight: 560)
                .tint(Palette.accent)
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

/// 主窗口：引导没完成时显示引导（完成与否由后台的进度接口决定），之后是侧边栏导航和各页面（原型 ui/design/index.html）。
struct MainWindow: View {
    @ObservedObject var ui: WindowUIState
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
                    HStack(spacing: 10) {
                        Image(systemName: "checkmark.circle.fill").foregroundStyle(Palette.tone(.success))
                        Text(note)
                        Spacer()
                        Button(L10n.close) { flow.dismissCompletedNote() }
                    }
                    .padding(.horizontal, 14)
                    .padding(.vertical, 8)
                    .background(Palette.toneSoft(.success))
                }
            }
    }

    @ViewBuilder private var content: some View {
        if setup.shouldShow && setup.step != .done {
            SetupView(setup: setup, main: main, status: status)
        } else {
            NavigationSplitView {
                sidebar
            } detail: {
                detail(ui.selection ?? .overview)
            }
        }
    }

    /// 半透明侧边栏：分组导航（方向键、Tab 可以操作），底部是当前状态（圆点加文字）。
    private var sidebar: some View {
        List(selection: $ui.selection) {
            ForEach(SidebarItem.groups, id: \.title) { group in
                Section(group.title) {
                    ForEach(group.items) { item in
                        Label {
                            Text(item.title)
                        } icon: {
                            Image(systemName: item.symbol).foregroundStyle(Palette.accent)
                        }
                        .tag(item)
                    }
                }
            }
        }
        .listStyle(.sidebar)
        .navigationSplitViewColumnWidth(min: 180, ideal: 200, max: 260)
        .safeAreaInset(edge: .bottom) { sidebarStatus }
    }

    private var sidebarStatus: some View {
        VStack(alignment: .leading, spacing: 6) {
            Divider()
            HStack(spacing: 7) {
                Circle().fill(Palette.tone(status.phase.tone)).frame(width: 7, height: 7)
                Text(status.phase.title)
            }
            .accessibilityElement(children: .combine)
            Text(status.reachable ? L10n.backendRunning : (status.connectionText ?? L10n.backendDown)).foregroundStyle(.secondary).lineLimit(2)
        }
        .font(.system(size: 11))
        .padding(.horizontal, 14)
        .padding(.bottom, 10)
    }

    @ViewBuilder private func detail(_ item: SidebarItem) -> some View {
        Group {
            switch item {
            case .overview: HomeView(model: status)
            case .mainProxy: MainProxyPage(model: main)
            case .exits: ExitsPage(model: exits)
            case .apps: AppsPage(model: apps)
            case .env: EnvPage(model: env)
            case .region: RegionPage(model: region)
            case .engines: EnginesPage(model: engines)
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(Palette.window)
        .navigationTitle(item.title)
        .toolbar {
            ToolbarItem(placement: .automatic) {
                StatusBadge(tone: status.phase.tone, text: status.phase.title, large: true)
            }
        }
    }
}
