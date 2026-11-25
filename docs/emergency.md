# 应急恢复：没有应用、后台服务不在、命令行也用不了时

这份文档给最坏的情况用：Claude 出国的应用打不开，后台服务（abroadd）没有运行或连不上，命令行 `abroad` 也用不了，而这台 Mac 断网了，或者你想彻底回到原样。

**能用正常途径时先用正常途径**，按顺序试：

1. 应用里点“回国”（恢复本机网络），或“设置 → 高级 → 卸载”；
2. 终端里执行 `abroad restore`（回国）或 `sudo abroad uninstall`（卸载）；
3. 应用被删掉时，另存的命令行还在：`sudo "/Library/Application Support/claude-abroad/bin/abroad" restore`。

只有这三条都不行时，才按下面手动恢复。下面每一条都只撤 Claude 出国自己加的东西，可以重复执行。

## 第一步：先退出 Claude

**必须先做这一步**：网络恢复的那一刻，还开着的 Claude 会立刻用本机的 IP、时区和语言重新连接。

- Claude 桌面版：切到它，按 ⌘Q；没反应就在“活动监视器”里选中它，点“强制退出”。
- Claude Code：在运行它的每个终端里退出（输入 `/exit` 或按 Ctrl+D），然后关掉这些终端窗口；IDE 里的 Claude Code 插件一并关掉 IDE。
- 浏览器里打开的 claude.ai 页面：关掉。
- 确认：“活动监视器”里搜 `claude`，没有 Claude 相关的进程了再继续。

## 第二步：撤掉断网规则

```sh
sudo pfctl -a com.apple/250.abroad -F all
```

这只清空 Claude 出国自己的规则锚点，不碰系统和别的软件的规则。执行后网络应当立刻恢复（如果 pf 是 Claude 出国打开的，它会保持打开但锚点为空，无害，重启后消失）。

## 第三步：撤掉 DNS 设置

```sh
printf 'remove State:/Network/Service/claude-abroad/DNS\nquit\n' | sudo scutil
```

## 第四步：恢复 IPv6

Claude 出国启用时会关掉 IPv6。在“系统设置 → 网络 → 你正在用的网络 → 详细信息 → TCP/IP”里把“配置 IPv6”改回“自动”，或者在终端里（把“Wi-Fi”换成你的网络服务名）：

```sh
networksetup -listallnetworkservices
sudo networksetup -setv6automatic "Wi-Fi"
```

## 第五步：重启电脑

重启会清掉 Claude 出国的虚拟网卡和路由。开机后如果后台服务又起来了、又断网，说明它还注册着：

- 应用还在：打开应用，按提示回国或卸载；
- 应用不在了：在“系统设置 → 通用 → 登录项”里关掉 Claude 出国的后台服务，再重启。

## 第六步（只在要彻底卸载时）：解除 Claude 启动管控

下面这些会让 Claude 在本机网络下能正常连接。只删内容是 Claude 出国写的文件（打开看一眼再删）。

- Claude 域名解析锁：`/etc/resolver/claude.ai`、`/etc/resolver/claude.com`、`/etc/resolver/anthropic.com`，内容是 `nameserver 198.18.0.2`（或 `nameserver 127.0.0.1` 加 `port`）和 `timeout 1`：

  ```sh
  cat /etc/resolver/claude.ai /etc/resolver/claude.com /etc/resolver/anthropic.com
  sudo rm -f /etc/resolver/claude.ai /etc/resolver/claude.com /etc/resolver/anthropic.com
  ```

- Claude Code 的托管设置：`/Library/Application Support/ClaudeCode/managed-settings.d/claude-abroad.json`（内容是指向 `http://127.0.0.1:47821` 的代理变量）：

  ```sh
  cat "/Library/Application Support/ClaudeCode/managed-settings.d/claude-abroad.json"
  sudo rm -f "/Library/Application Support/ClaudeCode/managed-settings.d/claude-abroad.json"
  ```

- 时区和语言：在“系统设置 → 通用 → 日期与时间”“语言与地区”里改回你原来的设置。

## 还是不行

把下面两条命令的输出（会显示网络和规则状态，不含账号）发给维护者：

```sh
sudo pfctl -a com.apple/250.abroad -s rules
scutil --dns | head -40
```
