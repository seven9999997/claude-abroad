// swift-tools-version:5.10
// Claude 出国的 Mac 应用（M5-7，design 11.2、m5-plan v2.5）。只用 Command Line Tools 里的 Swift 构建，没有第三方依赖：
//
//	cd app && swift build          # 编译（含界面程序，会话里只编译、不启动）
//	cd app && swift test           # 单元测试（网络层、接口客户端、视图模型；不创建窗口）
//	scripts/dev/app-test.sh        # 再加上对开发模式假系统 abroadd 的集成测试
//
// 代码约定：不用需要宏插件的 SwiftUI 写法（@State、@Entry、#Preview、@Observable）；状态用 ObservableObject。
import PackageDescription

let package = Package(
    name: "ClaudeAbroad",
    platforms: [.macOS(.v14)],
    targets: [
        // 由 cmd/abroad-apigen 生成、入库的接口类型（符号链接到 api/swift/APITypes.swift，应用用的就是入库那一份）。
        .target(name: "AbroadAPI"),
        // 网络层、接口客户端、授权协议、视图模型、界面文字：与界面无关，全部可单测。
        .target(name: "AbroadKit", dependencies: ["AbroadAPI"]),
        // 菜单栏和窗口（SwiftUI）。
        .executableTarget(name: "ClaudeAbroad", dependencies: ["AbroadKit", "AbroadAPI"]),
        // appid-samples.json 由 Go 测试（internal/rules TestAppIDSamples）生成，测试按路径读，不作为资源打包。
        .testTarget(name: "AbroadKitTests", dependencies: ["AbroadKit", "AbroadAPI"], exclude: ["appid-samples.json"]),
    ]
)
