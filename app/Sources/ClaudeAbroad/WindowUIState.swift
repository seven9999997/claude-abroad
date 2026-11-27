// 主窗口的纯界面状态（侧边栏选中哪一页）。只管界面，不放进业务模型；不写磁盘（重开应用回到“总览”）。
import AbroadKit
import Combine
import SwiftUI

/// 侧边栏的各项。分组和顺序照原型 ui/design/index.html：网络（总览、主代理、分流出口、应用分流）、环境（环境检测、地区）、系统（引擎与虚拟机）。
enum SidebarItem: String, CaseIterable, Identifiable, Hashable {
    case overview, mainProxy, exits, apps, env, region, engines

    var id: String { rawValue }

    var title: String {
        switch self {
        case .overview: return L10n.navOverview
        case .mainProxy: return L10n.mainProxyTitle
        case .exits: return L10n.exitsTitle
        case .apps: return L10n.appsTitle
        case .env: return L10n.envTitle
        case .region: return L10n.regionTitle
        case .engines: return L10n.enginesTitle
        }
    }

    /// SF Symbols 名字（对应原型侧边栏的线性图标）。
    var symbol: String {
        switch self {
        case .overview: return "gauge.with.dots.needle.33percent"
        case .mainProxy: return "star"
        case .exits: return "arrow.triangle.branch"
        case .apps: return "square.grid.2x2"
        case .env: return "checkmark.shield"
        case .region: return "globe"
        case .engines: return "cpu"
        }
    }

    static let groups: [(title: String, items: [SidebarItem])] = [
        (L10n.navGroupNetwork, [.overview, .mainProxy, .exits, .apps]),
        (L10n.navGroupEnvironment, [.env, .region]),
        (L10n.navGroupSystem, [.engines]),
    ]
}

@MainActor
final class WindowUIState: ObservableObject {
    @Published var selection: SidebarItem? = .overview
}
