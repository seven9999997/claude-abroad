// 状态色的含义：全应用只有这一张表（原型 ui/design/index.html 的约定：青色放行，黄色检查中，红色断网，灰色本机网络）。
// 这里只说“什么状态用哪一种含义”，由模型给出的状态决定，不按界面文字判断；具体色值（浅色、深色各一份）在应用的 Palette.swift。
// 每个用颜色表示状态的地方，旁边同时有文字或图形（颜色不是唯一的信息）。
import Foundation

public enum StatusTone: Sendable, Equatable, CaseIterable {
    /// 青色：放行（已出国）；检查通过、可用。
    case pass
    /// 黄色：检查中；还不知道、需要留意（例如回退到主代理、待还原）。
    case checking
    /// 红色：断网；不通过、不可用、出错（出错时网络按断网对待）。
    case blocked
    /// 灰色：本机网络（已回国）；未运行、已跳过。
    case local
}

public extension StatusTone {
    /// 横幅、回退到主代理、地区待还原这类“需要留意”的提示。
    static let attention: StatusTone = .checking
    /// 后台错误的三段说明、确认码过期这类失败。
    static let failure: StatusTone = .blocked
    /// 操作完成（“已完成”的提示、地区已切换、主代理已应用）。
    static let success: StatusTone = .pass

    /// 环境检测、出口检查的单项结果（pass、fail、skipped；其余是“无法判断”）。
    static func checkResult(_ result: String) -> StatusTone {
        switch result {
        case "pass": return .pass
        case "fail": return .blocked
        case "skipped": return .local
        default: return .checking
        }
    }
}

public extension Phase {
    /// 五种状态各自的含义：出错按断网对待（保护失效或后台不可达时，网络是否放行不确定，最需要提醒）。
    var tone: StatusTone {
        switch self {
        case .abroad: return .pass
        case .checking: return .checking
        case .offline, .error: return .blocked
        case .home: return .local
        }
    }
}

public extension EnvRow {
    var tone: StatusTone { StatusTone.checkResult(result) }
}

public extension ExitRow {
    /// 分流出口：可用为放行，不可用为不通过（指定它的应用改走主代理）。
    var tone: StatusTone { up ? .pass : .blocked }
}

public extension NodeRow {
    /// 节点的 UDP：可用、不可用（UDP 会被拦截）、还不知道。
    var udpTone: StatusTone {
        switch udp {
        case "available": return .pass
        case "unavailable": return .blocked
        default: return .checking
        }
    }
}

public extension ApplyState {
    /// 应用主代理的结果；进行中为检查中，没有结果时为空。
    var tone: StatusTone? {
        switch self {
        case .idle, .saved: return nil
        case .checking: return .checking
        case .succeeded: return .success
        case .failed: return .failure
        }
    }
}

public extension CheckState {
    var tone: StatusTone? {
        switch self {
        case .idle: return nil
        case .checking: return .checking
        case .ok: return .pass
        case .failed: return .failure
        }
    }
}

public extension Rollback {
    /// 切换失败后的回退：已回到原来的主代理不需要再做什么（无色），没有回退完需要留意。
    var tone: StatusTone? {
        switch self {
        case .none, .done: return nil
        case .incomplete: return .attention
        }
    }
}
