<div align="center">

<img src="assets/banner.svg" alt="Claude 出國" width="900" />

**整台 Mac 一起出國：一個主代理管全部流量，斷了就斷網，一鍵回國。**

[简体中文](README.md) | [English](README.en.md) | 繁體中文 | [日本語](README.ja.md)

</div>

## 這是什麼

Claude 出國替整台 Mac 裝上一張「出國網卡」：所有流量經過一台口袋大小的 Alpine 虛擬機，從你指定的**主代理**出去；時區和語言跟著出口一起換；服務一停，整機斷網，絕不偷跑直連。

- **主代理**：安裝時就選好，Mac 上所有流量預設都走它：TCP、DNS 全部從它出去，節點支援 UDP 時 UDP 也從它出去。支援 Clash/Mihomo 訂閱、本機 YAML、手動 HTTP/SOCKS5 節點和鏈式代理（中轉 → 住宅出口），有獨立的「主代理」頁面隨時更換。
- **分流出口（選用）**：OpenVPN、WireGuard、其他訂閱等裝在虛擬機裡，只給指定的應用程式用，例如 Chrome 走美國 WireGuard、終端機走日本節點。一個都不裝，所有流量照樣走主代理。
- **整機出關**：虛擬網卡接管全部 TCP、UDP、DNS，不存在繞過它的直連路徑。
- **口袋虛擬機**：約 100 MB 的無圖形介面 Alpine，代理引擎都已內建，每個 VPN 有自己獨立的網路命名空間。
- **按應用程式回國**：指定的應用程式可以直接走本機網路。Claude 的網域例外：不管哪個應用程式存取，都不會走本機網路。
- **區域網路照常用**：印表機、NAS、路由器後台預設放行，可以關閉。
- **地區同步**：主代理的出口檢查通過後，虛擬機和 Mac 的時區、語言一起切換，恢復時還原。
- **斷服即斷網**：pf 防火牆兜底。服務停止、當機、停用，或主代理檢查不通過，都保持斷網，只有按「一鍵回國」才恢復本機網路。
- **原生 Mac 應用程式**：以 Swift（SwiftUI）撰寫的 `Claude 出国.app`，選單列圖示隨時看狀態；啟用、停用、恢復本機網路、切換地區都一鍵完成。
- **出國後環境檢測**：啟用後從本機應用程式的角度檢查 17 項：出口 IP、時區、語言、IPv6、DNS、WebRTC（STUN）、系統代理、Claude 去向與可達性等。硬性項目不通過就不放行、不讓 Claude 啟動；查詢服務暫時無法使用不會誤斷網。
- **Claude 跟著出國走**：地區只在你操作時才變，確認後強制結束 Claude，不會自動重開，由你按「打開 Claude」。回國後 Claude 連不上、也不允許啟動，只有一鍵移除才解除。
- **一鍵移除**：二次確認後結束 Claude、恢復網路和地區、解除管控、清理全部背景服務和資料。
- **敏感操作要本人確認**：回國、移除、換主代理、把應用程式改成本機網路，都需要 Touch ID 或登入密碼，腳本和其他程式不能代你執行。
- **回國先清場**：按「一鍵回國」需二次確認，確認後先強制結束 Claude 桌面版、Claude Code 等所有 Claude 相關程序，確認都退出了才恢復本機網路。

## 狀態

**開發中，還沒有可用版本。** 上面列的是設計目標，不是已經交付的功能。

- **macOS（14+，Apple Silicon）**：虛擬機、資料面、pf 防火牆規則、主代理、出口檢查、分流出口（WireGuard、OpenVPN、Clash 節點）已完成，並通過單元測試和虛擬機內的端對端測試。還差在實機上整體驗證、原生應用程式介面、安裝與移除。
- **Windows（10/11 x64）**：已開工。所有程式碼都能編譯成 Windows 版本，系統層暫時一律報錯退出、不會放行；正在做基於 Windows 篩選平台（WFP）的斷網規則。

文件（簡體中文）：

- [專案介紹](docs/introduction.md)
- [技術方案](docs/design.md)
- [Windows 方案](docs/design-windows.md)
- [進度單](docs/PROGRESS.md)

## 從原始碼建置

需要 Go 1.26。

```sh
go build ./...
go vet ./...
go test ./internal/...
GOOS=linux GOARCH=arm64 go build ./...    # 虛擬機內執行的部分
GOOS=windows GOARCH=amd64 go build ./...  # Windows
```

需要 root、會碰真實網路或改系統狀態的測試放在 `//go:build integration` 標籤下，預設不執行。

## 授權

本專案以 [MIT 授權](LICENSE) 發布。來自其他專案的程式碼保留原有授權，見 [NOTICE](NOTICE) 和 `licenses/` 目錄。

## 致謝與聲明

網路閘門、出口檢查和虛擬機網路隔離的設計參考了 [Claude Cage](https://github.com/Lee-xeo/Claude-Cage)（MIT）。

Claude 出國是獨立的社群專案，與 Anthropic 沒有任何關聯，也沒有得到 Anthropic 的認可或授權。「Claude」是 Anthropic 的商標。吉祥物是本專案原創的插畫，並非 Anthropic 的形象。本專案不提供代理或 VPN 服務，也不承諾規避任何平台的風控或限制。
