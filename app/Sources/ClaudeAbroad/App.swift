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
    /// B0：各页面共用一个“经系统密码框执行”的确认框；全应用唯一的提权实现在这里构造一次。
    @MainActor private static let privileged = PrivilegedSession(client: client, runner: PrivilegedRunner())
    private static let auth = PrivilegedAuthorizer()
    /// 阶段 B（v0.71c）：应用包里的安装文件（版本号来自 Info.plist，目录是应用包内的固定位置）。
    private static let payload = PayloadInfo.from(info: Bundle.main.infoDictionary ?? [:], resourcePath: Bundle.main.resourcePath)
    @StateObject private var model = StatusModel(client: ClaudeAbroadApp.client, authorizer: ClaudeAbroadApp.auth, flow: ClaudeAbroadApp.flow, notifier: ClaudeAbroadApp.notifier, privileged: ClaudeAbroadApp.privileged)
    @StateObject private var setup = SetupModel(client: ClaudeAbroadApp.client)
    @StateObject private var main = MainProxyModel(client: ClaudeAbroadApp.client, authorizer: ClaudeAbroadApp.auth, flow: ClaudeAbroadApp.flow, notifier: ClaudeAbroadApp.notifier, privileged: ClaudeAbroadApp.privileged)
    @StateObject private var exits = ExitsModel(client: ClaudeAbroadApp.client, authorizer: ClaudeAbroadApp.auth, flow: ClaudeAbroadApp.flow, privileged: ClaudeAbroadApp.privileged)
    @StateObject private var apps = AppsModel(client: ClaudeAbroadApp.client, authorizer: ClaudeAbroadApp.auth, flow: ClaudeAbroadApp.flow, privileged: ClaudeAbroadApp.privileged)
    @StateObject private var env = EnvModel(client: ClaudeAbroadApp.client, authorizer: ClaudeAbroadApp.auth, flow: ClaudeAbroadApp.flow, privileged: ClaudeAbroadApp.privileged)
    @StateObject private var region = RegionModel(client: ClaudeAbroadApp.client, authorizer: ClaudeAbroadApp.auth, flow: ClaudeAbroadApp.flow, privileged: ClaudeAbroadApp.privileged)
    @StateObject private var engines = EnginesModel(client: ClaudeAbroadApp.client, authorizer: ClaudeAbroadApp.auth, flow: ClaudeAbroadApp.flow, privileged: ClaudeAbroadApp.privileged)
    @StateObject private var service = ServiceInstallModel(client: ClaudeAbroadApp.client, payload: ClaudeAbroadApp.payload, privileged: ClaudeAbroadApp.privileged)
    @StateObject private var settings = SettingsModel(client: ClaudeAbroadApp.client, preference: ClaudeAbroadApp.notifier.preference, authorizer: ClaudeAbroadApp.auth, privileged: ClaudeAbroadApp.privileged)
    @StateObject private var diagnostics = DiagnosticsExportModel(client: ClaudeAbroadApp.client, saver: SavePanelDiagnosticsSaver())
    /// 主窗口的纯界面状态（侧边栏选中项），不进业务模型。
    @StateObject private var ui = WindowUIState()

    var body: some Scene {
        // 主窗口放在第一个：启动时自动打开（第一级走查 L1-01）。只放在菜单栏时，刘海屏上图标可能被挡住，打开后什么都看不到。
        Window(L10n.appName, id: "main") {
            MainWindow(ui: ui, status: model, setup: setup, main: main, exits: exits, apps: apps, env: env, region: region, engines: engines, flow: ClaudeAbroadApp.flow, privileged: ClaudeAbroadApp.privileged, service: service)
                .frame(minWidth: 820, idealWidth: 980, minHeight: 580, idealHeight: 680)
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
            SettingsView(model: settings, env: settings.env, diagnostics: diagnostics, privileged: ClaudeAbroadApp.privileged, service: service)
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
    @ObservedObject var privileged: PrivilegedSession
    @ObservedObject var service: ServiceInstallModel

    var body: some View {
        content
            .task { await service.refresh() }
            .onChange(of: status.reachable) { _, _ in Task { await service.refresh() } }
            .safeAreaInset(edge: .top) {
                if service.showsUpgrade && !(setup.shouldShow && setup.step != .done) { // 阶段 B：后台服务可以（或要）升级
                    HStack(spacing: 10) {
                        IconBadge(symbol: "arrow.up.circle", tone: .attention, size: 26)
                        Text(service.text)
                            .font(.callout)
                            .fixedSize(horizontal: false, vertical: true)
                        Spacer()
                        Button(L10n.serviceUpgradeButton) { Task { await service.userTappedUpgrade() } }
                            .buttonStyle(SecondaryButtonStyle(compact: true, tone: .attention))
                            .disabled(!service.upgradeEnabled)
                    }
                    .padding(.horizontal, 14)
                    .padding(.vertical, 9)
                    .background(Palette.box.overlay(Palette.toneSoft(.attention)))
                    .overlay(alignment: .bottom) { Rectangle().fill(Palette.toneLine(.attention)).frame(height: 1) }
                }
            }
            .sheet(isPresented: Binding(get: { flow.isActive }, set: { if !$0, flow.isActive { flow.cancel() } })) {
                ConfirmSheet(flow: flow)
            }
            .sheet(isPresented: Binding(get: { privileged.request?.host == .main },
                                        set: { if !$0, privileged.phase != .running { Task { await privileged.userClosed() } } })) {
                PrivilegedSheet(session: privileged)
            }
            .safeAreaInset(edge: .top) {
                if let note = flow.completedNote {
                    HStack(spacing: 10) {
                        IconBadge(symbol: "checkmark.circle.fill", tone: .success, size: 26)
                        Text(note).font(.callout)
                        Spacer()
                        Button(L10n.close) { flow.dismissCompletedNote() }
                            .buttonStyle(SecondaryButtonStyle(compact: true))
                    }
                    .padding(.horizontal, 14)
                    .padding(.vertical, 9)
                    .background(Palette.box.overlay(Palette.toneSoft(.success)))
                    .overlay(alignment: .bottom) { Rectangle().fill(Palette.toneLine(.success)).frame(height: 1) }
                }
            }
    }

    @ViewBuilder private var content: some View {
        if setup.shouldShow && setup.step != .done {
            SetupView(setup: setup, main: main, status: status, service: service)
        } else {
            NavigationSplitView {
                sidebar
            } detail: {
                detail(ui.selection ?? .overview)
            }
        }
    }

    /// 半透明侧边栏：分组导航（方向键、Tab 可以操作），底部是当前状态卡片。
    private var sidebar: some View {
        List(selection: $ui.selection) {
            ForEach(SidebarItem.groups, id: \.title) { group in
                Section {
                    ForEach(group.items) { item in
                        Label {
                            Text(item.title)
                                .font(.system(size: 13, weight: ui.selection == item ? .semibold : .regular))
                        } icon: {
                            Image(systemName: item.symbol)
                                .foregroundStyle(Palette.accent)
                        }
                        .padding(.vertical, 2)
                        .tag(item)
                    }
                } header: {
                    Text(group.title)
                        .font(.system(size: 11, weight: .semibold))
                        .foregroundStyle(.secondary)
                }
            }
        }
        .listStyle(.sidebar)
        .navigationSplitViewColumnWidth(min: 196, ideal: 216, max: 260)
        .safeAreaInset(edge: .bottom) { sidebarStatus }
    }

    private var sidebarStatus: some View {
        let tone = status.phase.tone
        return VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 8) {
                ZStack {
                    Circle().fill(Palette.toneSoft(tone)).frame(width: 14, height: 14)
                    Circle().fill(Palette.tone(tone)).frame(width: 7, height: 7)
                }
                Text(status.phase.title)
                    .font(.system(size: 12, weight: .semibold))
                Spacer(minLength: 4)
                Image(systemName: status.phase.symbol)
                    .font(.system(size: 11))
                    .foregroundStyle(Palette.tone(tone))
                    .accessibilityHidden(true)
            }
            .accessibilityElement(children: .combine)
            HStack(spacing: 6) {
                Circle()
                    .fill(status.reachable ? Palette.tone(.pass) : Palette.tone(.blocked))
                    .frame(width: 5, height: 5)
                Text(status.reachable ? L10n.backendRunning : (status.connectionText ?? L10n.backendDown))
                    .font(.system(size: 11))
                    .foregroundStyle(.secondary)
                    .lineLimit(2)
            }
        }
        .padding(10)
        .background(
            RoundedRectangle(cornerRadius: 10, style: .continuous)
                .fill(Palette.boxElevated)
                .shadow(color: Palette.cardShadow, radius: 4, x: 0, y: 1)
        )
        .overlay(
            RoundedRectangle(cornerRadius: 10, style: .continuous)
                .strokeBorder(Palette.boxLine, lineWidth: 1)
        )
        .padding(.horizontal, 10)
        .padding(.bottom, 10)
        .padding(.top, 4)
    }

    @ViewBuilder private func detail(_ item: SidebarItem) -> some View {
        Group {
            switch item {
            case .overview: HomeView(model: status)
            case .mainProxy: MainProxyPage(model: main, progress: L10n.borrowStep(status.borrowStep))
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
