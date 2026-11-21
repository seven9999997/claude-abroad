<div align="center">

<img src="assets/banner.svg" alt="Claude Abroad" width="900" />

**No second computer, no VM desktop, no fingerprint browser — send your whole Mac abroad.**

[![CI](https://github.com/seven9999997/claude-abroad/actions/workflows/ci.yml/badge.svg)](https://github.com/seven9999997/claude-abroad/actions/workflows/ci.yml) [![MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE) ![macOS 14+](https://img.shields.io/badge/macOS-14%2B%20Apple%20Silicon-black) ![Windows](https://img.shields.io/badge/Windows-in%20development-lightgrey)

[简体中文](README.md) | English | [繁體中文](README.zh-TW.md) | [日本語](README.ja.md)

</div>

> [!WARNING]
> **In development. There is no installable release yet.** What follows describes the design goals; see [Project status](#project-status) for what is done and what is still on the way.

## Fed up with Anthropic

All we want is to use Claude in peace: write code, chat, get work done.

The reality: rent a VPS as a jump host, install a fingerprint browser, go through a dozen checker sites item by item — time zone, language, DNS, WebRTC, IPv6. Screens full of Clash rules, and there is still always some background process quietly going direct. The proxy drops, Claude Code reconnects from your home IP, and the next morning you get an "account disabled" email and a subscription fee down the drain.

**Enough.**

So we built **Claude Abroad**: a native Mac app that plugs an "overseas network card" into your entire computer.

- **All you need is one good proxy.** Import a subscription, pick a node, and let it handle the rest.
- **Your whole Mac is abroad.** Not one browser, not one terminal — every app, every connection, every DNS query. Even the time zone and language follow the exit.
- **If it breaks, the network goes down. No silent direct connections.** Proxy dies, service crashes, a check fails: the Mac goes offline and waits for you. Better no internet than a single packet leaking your real IP.
- **Want to come back? One click.** Clean up first, then restore: network, time zone and language all back to how they were.

Keep using the Claude desktop app, `claude` in your terminal and your usual browser. Nothing to switch.

## Why the whole Mac

Most approaches "lock Claude inside an isolated environment": spin up a VM desktop and install a browser, Claude and your dev tools in it. Clean, but you keep jumping between two systems.

Claude Abroad goes the other way: **instead of locking Claude up, it sends the whole Mac out.**

| | Isolated-environment approach | Claude Abroad |
|---|---|---|
| Where Claude runs | Inside a VM desktop | Natively on your own Mac |
| Toolchain | Reinstalled inside the VM | The tools you already have |
| Traffic covered | The VM's | All TCP, UDP and DNS on the Mac |
| When the proxy drops | The VM goes offline | The whole Mac goes offline; no app can leak |
| WeChat, banking apps | Already outside | Put back on the local network per app (Claude's domains excepted — they never use the local network) |

Under the hood is a headless Alpine VM of about 100 MB. It does one thing — act as your overseas network card. You won't notice it's there.

## What it does

**🌏 One main proxy for all traffic**
Supports Clash/Mihomo subscriptions, local YAML, manual HTTP/SOCKS5 nodes, and chained proxies (relay → residential exit). All TCP and DNS go through it; UDP goes through it when the node supports UDP, and is blocked otherwise — never sent direct.

**🔀 Split routing per app**
OpenVPN, WireGuard and other subscriptions can be installed in the VM and used only by the apps you choose: Chrome through a US WireGuard, the terminal through a Japan node. Every VPN gets its own network namespace. Installing none is perfectly fine.

**🕐 Time zone and language follow the exit**
Once the exit check passes, the Mac's time zone and language switch to the exit's location, and are restored when you come back. The region is locked, too: node IP drift won't quietly change your time zone.

**🧪 17-point post-departure checkup**
Checks from a local app's point of view: exit IP, time zone, language, IPv6, DNS, WebRTC (STUN), system proxy, where Claude traffic goes and whether it gets there… If a hard check fails, traffic is not released and Claude can't start. A flaky lookup service won't cut you off by mistake.

**🛡️ Service down means network down**
pf firewall as the backstop: whether the service stops, crashes, is disabled, or the main proxy check fails, the Mac stays offline. Only clicking "Come home" yourself restores the local network.

**🧹 Clean up before coming home**
After you click "Come home", every Claude process — the desktop app, Claude Code and so on — is force-quit, and the local network comes back only once they have all exited, so none of them can reconnect from your home IP. While at home, Claude can't start either, guarding against autostart at boot or an accidental launch.

**🔐 Sensitive actions need you in person**
Coming home, uninstalling, changing the main proxy, putting an app back on the local network — all require Touch ID or your login password. Scripts and other programs can't press the button for you.

**🖨️ LAN keeps working**
Printers, NAS and the router admin page are allowed by default; turn it off if you don't want it.

**🍎 Native Mac app**
`Claude 出国.app`, written in SwiftUI, lives in the menu bar so status is always a glance away. Enable, disable, come home, switch region, uninstall — each one click.

## Project status

| Platform | Progress |
|---|---|
| **macOS 14+ (Apple Silicon)** | VM, data plane, pf firewall rules, main proxy, exit checks and split exits (WireGuard, OpenVPN, Clash nodes) are done and pass unit tests and end-to-end tests in the VM. Still to do: full verification on a real machine, the native app UI, install and uninstall. |
| **Windows 10/11 x64** | Started. All code builds for Windows; the system layer currently always fails with an error and never lets traffic through. Kill-switch rules based on the Windows Filtering Platform (WFP) are in progress. |

Want it as soon as it ships? Hit ⭐ Star to follow releases.

## The only thing you need

**A reliable proxy node.** This project does not provide or recommend any proxy or VPN service; you'll need to sort that out yourself.

## Building from source

Requires Go 1.26.

```sh
go build ./...
go vet ./...
go test ./internal/...
GOOS=linux GOARCH=arm64 go build ./...    # the parts that run in the VM
GOOS=windows GOARCH=amd64 go build ./...  # Windows
```

Tests that need root, touch the real network or change system state are behind the `//go:build integration` tag and don't run by default.

## Documentation (Chinese)

| To learn about | Read |
|---|---|
| How it works and what code it reuses | [Introduction](docs/introduction.md) |
| Full design, milestones, open risks | [Design](docs/design.md) |
| The Windows approach | [Windows design](docs/design-windows.md) |
| Where things stand | [Progress](docs/PROGRESS.md) |

## Community and feedback

The project is moving fast. [Issues](https://github.com/seven9999997/claude-abroad/issues) and PRs are welcome, and so are you on QQ — add us or join the group to talk features and report problems. Let's build it together.

| QQ: 731032177 | QQ group: 801350116 |
| :---: | :---: |
| <a href="assets/contact-qq.jpg"><img src="assets/contact-qq.jpg" alt="QQ QR code (731032177)" width="220" /></a> | <a href="assets/contact-qq-group.jpg"><img src="assets/contact-qq-group.jpg" alt="QQ group QR code (801350116)" width="220" /></a> |

## Before you use it

- Claude Abroad manages your **network and regional environment**. It isn't magic. The checks show the current configuration and sampled evidence, and **cannot guarantee that your account won't be restricted by any platform**.
- Once enabled, all traffic on the Mac goes through your chosen proxy, so its speed and stability directly determine your experience.
- When the main proxy doesn't support UDP, UDP is blocked: browsers fall back to TCP automatically, but video calls, voice calls and games may not work.

## License

Released under the [MIT License](LICENSE). Code from other projects keeps its original license; see [NOTICE](NOTICE) and the `licenses/` directory. mihomo, sing-box (GPL-3.0) and OpenVPN (GPL-2.0) run as separate programs inside the VM and are not linked into this project's code.

## Acknowledgements and disclaimer

The network gate, exit checks and VM network isolation draw on, and reuse code from, [Claude Cage](https://github.com/Lee-xeo/Claude-Cage) (MIT). Thanks to Cage for blazing the trail.

Claude Abroad is an independent community project. It is not affiliated with, endorsed by or authorized by Anthropic. "Claude" is a trademark of Anthropic. The mascot is an original illustration made for this project and is not an Anthropic character. This project does not provide proxy or VPN services and makes no promise to bypass any platform's risk controls or restrictions.
