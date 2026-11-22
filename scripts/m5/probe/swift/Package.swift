// swift-tools-version:5.10
// M5-1 探查（m5-plan v2 第 5 节）：E4 Swift 经 unix socket 访问 abroadd、E5 生成的类型、E6 只用命令行工具编界面程序、
// E1/E2 本人授权。只用 Command Line Tools 里的 Swift，不依赖 Xcode，没有外部依赖。
import PackageDescription

let package = Package(
    name: "M5Probe",
    platforms: [.macOS(.v14)],
    targets: [
        .target(name: "APITypes"),
        .target(name: "UDSHTTP"),
        .executableTarget(name: "UDSClient", dependencies: ["APITypes", "UDSHTTP"]),
        .executableTarget(name: "MenuBarDemo", dependencies: ["APITypes"]),
        .executableTarget(name: "AuthzProbe"),
        .testTarget(name: "UDSHTTPTests", dependencies: ["UDSHTTP", "APITypes"], exclude: ["status-sample.json"]),
    ]
)
