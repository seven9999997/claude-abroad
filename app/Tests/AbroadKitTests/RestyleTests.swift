// 按原型改样式（2026-11-27）：状态色的含义表、配色集中、侧边栏的分组和顺序、界面状态不进业务模型。只读源码，不建窗口。
import AbroadAPI
import Foundation
import Testing
@testable import AbroadKit

/// 状态到颜色含义的对应（StatusTone.swift 是唯一的一张表）：青色放行、黄色检查中、红色断网、灰色本机网络。
@Test func statusToneTable() {
    #expect(Phase.abroad.tone == .pass)
    #expect(Phase.checking.tone == .checking)
    #expect(Phase.offline.tone == .blocked)
    #expect(Phase.error.tone == .blocked) // 出错按断网对待
    #expect(Phase.home.tone == .local)
    // 五种状态的颜色含义覆盖了全部四种。
    let all: [Phase] = [.abroad, .checking, .offline, .error, .home]
    #expect(Set(all.map { "\($0.tone)" }) == Set(StatusTone.allCases.map { "\($0)" }))

    #expect(StatusTone.checkResult("pass") == .pass)
    #expect(StatusTone.checkResult("fail") == .blocked)
    #expect(StatusTone.checkResult("skipped") == .local)
    #expect(StatusTone.checkResult("unknown") == .checking)
    #expect(StatusTone.checkResult("") == .checking)
    #expect(StatusTone.attention == .checking && StatusTone.failure == .blocked && StatusTone.success == .pass)

    let env = EnvRow(id: "08", name: "WebRTC", hard: true, result: "fail", detail: "", fix: "", enabled: true)
    #expect(env.tone == .blocked)
    var exit = ExitRow(tag: "wg", kind: "wireguard", up: true, reason: "", check: "pass", items: [:], tcp: 0, udp: 0)
    #expect(exit.tone == .pass)
    exit.up = false
    #expect(exit.tone == .blocked)

    var node = NodeRow(id: "n", name: "n", type: "socks5", source: "s")
    #expect(node.udpTone == .checking)
    node.udp = "available"
    #expect(node.udpTone == .pass)
    node.udp = "unavailable"
    #expect(node.udpTone == .blocked)

    #expect(ApplyState.idle.tone == nil)
    #expect(ApplyState.saved("").tone == nil)
    #expect(ApplyState.checking.tone == .checking)
    #expect(ApplyState.succeeded(country: "US", timezone: "").tone == .pass)
    #expect(ApplyState.failed(ErrorText(raw: "x"), .none).tone == .blocked)
    #expect(CheckState.idle.tone == nil)
    #expect(CheckState.ok(rule: "r", country: "US", timezone: "").tone == .pass)
    #expect(CheckState.failed(ErrorText(raw: "x")).tone == .blocked)
    #expect(Rollback.done.tone == nil && Rollback.none.tone == nil && Rollback.incomplete.tone == .checking)
}

/// 去掉整行注释和行尾注释后的代码（字符串里的 // 很少，视图里没有）。
private func codeOnly(_ src: String) -> [String] {
    src.split(separator: "\n", omittingEmptySubsequences: false).map { line in
        let s = String(line)
        if s.trimmingCharacters(in: .whitespaces).hasPrefix("//") { return "" }
        return s.components(separatedBy: "//").first ?? ""
    }
}

/// 视图里不出现写死的色值和直接颜色：颜色只从 Palette.swift 取（配色文件除外）。
@Test func viewsTakeColorsFromPaletteOnly() throws {
    let exempt: Set<String> = ["Palette.swift"]
    let dir = repoRoot.appendingPathComponent("app/Sources/ClaudeAbroad")
    let files = try FileManager.default.contentsOfDirectory(atPath: dir.path).filter { $0.hasSuffix(".swift") }
    #expect(files.contains("Palette.swift"))
    let named = "red|orange|yellow|green|mint|teal|cyan|blue|indigo|purple|pink|brown|gray|grey|black|white"
    let patterns = [
        "Color\\s*\\(", // Color(red:…)、Color(hex…)、Color(white:…)、Color(nsColor:…)
        "NSColor\\s*\\(", "NSColor\\.", "UIColor", "CGColor", "#colorLiteral",
        "Color\\.(\(named)|accentColor)\\b", // Color.red …
        "\\.foregroundColor\\(", // 旧写法
        "(foregroundStyle|tint|fill|background|stroke|strokeBorder|foregroundColor|accentColor)\\(\\s*\\.(\(named))\\b",
        "[?:]\\s*\\.(\(named))\\b", // 三元表达式里的 .red / .orange
    ]
    let regexes = try patterns.map { try NSRegularExpression(pattern: $0) }
    for f in files where !exempt.contains(f) {
        let src = try String(contentsOf: dir.appendingPathComponent(f), encoding: .utf8)
        for (i, line) in codeOnly(src).enumerated() {
            let range = NSRange(line.startIndex..., in: line)
            for (p, re) in zip(patterns, regexes) where re.firstMatch(in: line, range: range) != nil {
                Issue.record("\(f):\(i + 1) 直接写了颜色（\(p)）：\(line.trimmingCharacters(in: .whitespaces))")
            }
        }
    }
    // 反例：检查本身能抓到这些写法。
    for bad in ["Text(x).foregroundStyle(.orange)", "Color(red: 1, green: 0, blue: 0)", "x.fill(Color.yellow.opacity(0.1))", "a ? .red : .primary", "Color(hex: 0xA94E2E)"] {
        let range = NSRange(bad.startIndex..., in: bad)
        #expect(regexes.contains { $0.firstMatch(in: bad, range: range) != nil }, "没抓到：\(bad)")
    }
    for good in ["Text(x).foregroundStyle(.secondary)", "Palette.tone(.pass)", ".fill(Palette.redSoft)"] {
        let range = NSRange(good.startIndex..., in: good)
        #expect(!regexes.contains { $0.firstMatch(in: good, range: range) != nil }, "误报：\(good)")
    }
}

/// 配色文件：每个颜色浅色、深色各一份；四种状态含义各有颜色和浅底；状态色只经 StatusTone 选，不在别处另立含义。
@Test func paletteHasLightAndDark() throws {
    let src = try readRepo("app/Sources/ClaudeAbroad/Palette.swift")
    let defs = src.components(separatedBy: "= dynamic(light:").count - 1
    let darks = src.components(separatedBy: "), dark: (0x").count - 1
    #expect(defs >= 16 && defs == darks, "每个颜色都要有浅色和深色")
    for c in ["case .pass: return teal", "case .checking: return yellow", "case .blocked: return red", "case .local: return gray",
              "case .pass: return tealSoft", "case .checking: return yellowSoft", "case .blocked: return redSoft", "case .local: return graySoft"] {
        #expect(src.contains(c), "Palette.swift 缺少 \(c)")
    }
    // 强调色是原型的陶土橙（浅色 #A94E2E、深色 #D57954）。
    #expect(src.contains("static let accent = dynamic(light: (0xA9, 0x4E, 0x2E, 1), dark: (0xD5, 0x79, 0x54, 1))"))
}

/// 主窗口是侧边栏导航，分组和顺序照原型；选中项放在只管界面状态的对象里，不进 AbroadKit 的业务模型。
@Test func sidebarFollowsPrototype() throws {
    let ui = try readRepo("app/Sources/ClaudeAbroad/WindowUIState.swift")
    #expect(ui.contains("(L10n.navGroupNetwork, [.overview, .mainProxy, .exits, .apps])"))
    #expect(ui.contains("(L10n.navGroupEnvironment, [.env, .region])"))
    #expect(ui.contains("(L10n.navGroupSystem, [.engines])"))
    #expect(ui.contains("final class WindowUIState: ObservableObject"))
    let app = try readRepo("app/Sources/ClaudeAbroad/App.swift")
    #expect(app.contains("NavigationSplitView") && !app.contains("TabView"))
    #expect(app.contains("List(selection: $ui.selection)"))
    let kit = repoRoot.appendingPathComponent("app/Sources/AbroadKit")
    for f in try FileManager.default.contentsOfDirectory(atPath: kit.path) where f.hasSuffix(".swift") {
        let src = try String(contentsOf: kit.appendingPathComponent(f), encoding: .utf8)
        #expect(!src.contains("SidebarItem") && !src.contains("WindowUIState"), "\(f)：界面状态不放进业务模型")
    }
    // 侧边栏里各页的名字和原型一致。
    #expect(L10n.navOverview == "总览" && L10n.mainProxyTitle == "主代理" && L10n.exitsTitle == "分流出口" && L10n.appsTitle == "应用分流")
    #expect(L10n.envTitle == "环境检测" && L10n.regionTitle == "地区" && L10n.enginesTitle == "引擎与虚拟机")
}
