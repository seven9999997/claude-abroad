// 应用里导出诊断包（补齐五，architect 2026-11-27）：取后台给的原始内容，交给“保存到用户所选位置”的协议，给出结果提示。
// - 内容原样：应用不解码、不加工、不另外拼任何东西（后台已经脱敏）；
// - 写磁盘只经 DiagnosticsSaver：系统实现是保存面板（ClaudeAbroad/SystemServices.swift，只写用户选的那一个文件），测试用假实现；
// - 模型里不留诊断包的内容：内容只在 export() 的局部变量里，交给保存协议后就随函数结束释放。
import Foundation

/// 保存的结果。取消不算错误。
public enum DiagnosticsSaveResult: Sendable, Equatable {
    case saved(fileName: String)
    case cancelled
    case failed(reason: String)
}

/// 把诊断包保存到用户选的位置。实现只允许写用户选的那一个文件。
@MainActor
public protocol DiagnosticsSaver: AnyObject {
    func save(_ data: Data, suggestedName: String) async -> DiagnosticsSaveResult
}

/// 导出一次的结果（给界面和测试）。
public enum DiagnosticsExportOutcome: Sendable, Equatable {
    case saved
    case cancelled
    case backendDown
    case backendError
    case writeFailed
}

@MainActor
public final class DiagnosticsExportModel: ObservableObject {
    let client: APIClient
    let saver: DiagnosticsSaver
    let now: () -> Date

    /// 是否包含 Claude 访问过的域名清单（默认不勾）。
    @Published public var includeDomains = false
    @Published public private(set) var busy = false
    /// 结果提示；取消时为 nil。
    @Published public private(set) var message: String?
    @Published public private(set) var isError = false

    public init(client: APIClient, saver: DiagnosticsSaver, now: @escaping () -> Date = Date.init) {
        self.client = client
        self.saver = saver
        self.now = now
    }

    /// 导出前的说明：包里有什么、没有什么；应用打不开时的终端命令。
    public var help: String { L10n.diagnosticsHelp }

    /// 建议的文件名（带本机时间，避免覆盖上一次的）。
    public func suggestedName() -> String {
        let f = DateFormatter()
        f.locale = Locale(identifier: "en_US_POSIX")
        f.dateFormat = "yyyyMMdd-HHmmss"
        return L10n.diagnosticsFileName(f.string(from: now()))
    }

    @discardableResult
    public func export() async -> DiagnosticsExportOutcome {
        busy = true
        defer { busy = false }
        message = nil
        isError = false
        let outcome: DiagnosticsExportOutcome
        switch await client.diagnosticsRaw(includeDomains: includeDomains) {
        case .ok(let data):
            switch await saver.save(data, suggestedName: suggestedName()) {
            case .saved(let name):
                message = L10n.diagnosticsSaved(name)
                outcome = .saved
            case .cancelled:
                outcome = .cancelled
            case .failed(let reason):
                message = L10n.diagnosticsWriteFailed(reason)
                isError = true
                outcome = .writeFailed
            }
        case .unreachable:
            message = L10n.diagnosticsBackendDown
            isError = true
            outcome = .backendDown
        case let other:
            message = L10n.diagnosticsBackendError(other.errorText?.raw ?? L10n.loadFailed)
            isError = true
            outcome = .backendError
        }
        return outcome
    }
}
