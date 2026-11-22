// 拖进来的应用包 → 应用 ID（和后台的归一规则一致：internal/rules.NormalizeAppID；样本由 Go 测试生成，Swift 测试对照）。
// 图标和名字只是显示，在本机取（视图里用 NSWorkspace）。
import Foundation

public enum AppIdentity {
    /// 后台的归一：path:、system:、unknown 原样，其余（Bundle ID）一律小写。
    public static func normalize(_ raw: String) -> String {
        if raw == "unknown" || raw.hasPrefix("path:") || raw.hasPrefix("system:") { return raw }
        return raw.lowercased()
    }

    /// 应用包的应用 ID：有 Bundle ID 用它（归一后）；没有时用可执行文件的绝对路径（path:）。
    public static func appID(bundleAt url: URL) -> String? {
        if let b = Bundle(url: url) {
            if let id = b.bundleIdentifier, !id.isEmpty { return normalize(id) }
            if let exe = b.executableURL { return "path:" + exe.standardizedFileURL.path }
        }
        return url.isFileURL ? "path:" + url.standardizedFileURL.path : nil
    }
}
