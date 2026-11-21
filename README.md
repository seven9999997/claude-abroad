<div align="center">

<img src="assets/banner.svg" alt="Claude 出国：整机出关，一键回国" width="900" />

**整台 Mac 一起出国：一个主代理管全部流量，断了就断网，一键回国。**

简体中文 | [English](README.en.md) | [繁體中文](README.zh-TW.md) | [日本語](README.ja.md)

</div>

## 这是什么

Claude 出国给整台 Mac 装上一块"出国网卡"：所有流量经过一台口袋大小的 Alpine 虚拟机，从你指定的**主代理**出去；时区和语言跟着出口一起换；服务一停，整机断网，绝不偷跑直连。

- **主代理**：安装时就选好，Mac 上所有流量默认都走它，就像插了一块出国网卡：TCP、DNS 全部从它出去，节点支持 UDP 时 UDP 也从它出去。支持 Clash/Mihomo 订阅、本地 YAML、手动 HTTP/SOCKS5 节点和链式代理（中转 → 住宅出口），有单独的"主代理"页面随时更换。
- **分流出口（可选）**：OpenVPN、WireGuard、其他订阅等装在虚拟机里，只给指定的应用用，比如 Chrome 走美国 WireGuard、终端走日本节点。一个都不装，所有流量照样走主代理。
- **整机出关**：虚拟网卡接管全部 TCP、UDP、DNS，不存在绕过它的直连路径。
- **口袋虚拟机**：约 100 MB 的无图形界面 Alpine，代理引擎都已内置，每个 VPN 有自己独立的网络命名空间，互不影响。
- **按应用回国**：微信等指定应用可以直接走本机网络。Claude 的域名例外：不管哪个应用访问，都不会走本机网络。
- **局域网照常用**：打印机、NAS、路由器后台默认放行，可以关。
- **地区同步**：主代理的出口检查通过后，虚拟机和 Mac 的时区、语言一起切换，恢复时还原成原来的设置。
- **断服即断网**：pf 防火墙兜底。服务停止、崩溃、停用，或主代理检查不通过，都保持断网，只有点"一键回国"才恢复本机网络。
- **原生 Mac 应用**：用 Swift（SwiftUI）写的 `Claude 出国.app`，菜单栏常驻图标随时看状态；启用、停用（断网）、恢复本机网络、切换地区都在应用里一键完成。
- **出国后环境检测**：启用后从本机应用的角度查 17 项：出口 IP、时区、语言、IPv6、DNS、WebRTC（STUN）、系统代理、Claude 去向与可达性等。硬性项不通过就不放行、不让 Claude 启动；查询服务临时不可用不会误断网。
- **Claude 跟着出国走**：地区只在你操作时才变，确认后强制结束 Claude，不会自动重开，由你点「打开 Claude」；需要重启电脑时引导重启。回国后 Claude 连不上、也不允许启动，只有一键卸载才解除。
- **一键卸载**：二次确认后结束 Claude、恢复网络和地区、解除管控、清理全部后台服务和数据。
- **敏感操作要本人确认**：回国、卸载、换主代理、把应用改成本机网络，都要 Touch ID 或登录密码，脚本和其他程序不能代你执行。
- **回国先清场**：点"一键回国"要二次确认，确认后先强制结束 Claude 桌面版、Claude Code 等所有 Claude 相关进程，确认都退出了才恢复本机网络，避免它们用本机 IP 重新连上。

## 状态

**开发中，还没有可用版本。** 下面列的是设计目标，不是已经交付的功能。

- **macOS（14+，Apple Silicon）**：虚拟机、数据面、pf 防火墙规则、主代理、出口检查、分流出口（WireGuard、OpenVPN、Clash 节点）已经完成，并通过了单元测试和虚拟机里的端到端测试。还差在真机上整体验证、原生应用界面、安装与卸载。
- **Windows（10/11 x64）**：已开工。所有代码都能编译成 Windows 版本，系统层暂时一律报错退出、不会放行；正在做基于 Windows 筛选平台（WFP）的断网规则。

文档：

- [项目介绍](docs/introduction.md)：做什么、仓库现状、复用了哪些现成代码
- [技术方案](docs/design.md)：完整设计、里程碑和待验证的风险
- [Windows 方案](docs/design-windows.md)
- [进度单](docs/PROGRESS.md)

## 从源码构建

需要 Go 1.26。

```sh
go build ./...
go vet ./...
go test ./internal/...
GOOS=linux GOARCH=arm64 go build ./...    # 虚拟机里运行的部分
GOOS=windows GOARCH=amd64 go build ./...  # Windows
```

需要 root、会碰真实网络或改系统状态的测试放在 `//go:build integration` 标签下，默认不跑。

## 许可证

本项目以 [MIT 许可证](LICENSE) 发布。来自其他项目的代码保留原有许可证，见 [NOTICE](NOTICE) 和 `licenses/` 目录。

## 致谢与声明

网络闸门、出口检查和虚拟机网络隔离的设计参考了 [Claude Cage](https://github.com/Lee-xeo/Claude-Cage)（MIT）。

Claude 出国是独立的社区项目，与 Anthropic 没有任何关联，也没有得到 Anthropic 的认可或授权。"Claude"是 Anthropic 的商标。吉祥物是本项目原创的插画，并非 Anthropic 的形象。本项目不提供代理或 VPN 服务，也不承诺规避任何平台的风控或限制。
