# M4 真机验证（给用户执行）

## 先读这一段：这份脚本会改什么、什么时候能跑

- **会改这台 Mac 的时区**（改成和你的主代理出口一致的时区，结束时还原），会关掉“自动设置时区”（结束时还原）。**默认不改语言和地区格式**。
- 会改网络：启用期间本机只能经主代理和你给的分流出口上网；中途会三次故意制造“泄漏”（打开系统代理、启动一个带代理环境变量的假 Claude 进程、重新打开 IPv6），每次几秒后由脚本撤销。
- **只能在你不需要 Claude Code 会话的时候执行**：前提检查要求先关掉别的代理软件的 TUN 和 VPN，开发会话正是经它们连网的，执行期间会话会断开，这是预期的。开发会话不会替你执行这份脚本。
- 需要 `sudo`。开发会话只对它做过语法检查、`DRY_RUN=1` 空跑、命令核对、`--check-only` 和自测（`scripts/m4/selftest.sh`），没有真机执行过。

## 先后顺序（重要）

1. **R1**：`scripts/m0/r1/`，不断网。
2. **R2**：`scripts/m0/r2/`，断网 1–2 分钟。
3. **M2 脚本**：`scripts/m2/`。
4. **R3 脚本**：`sudo scripts/m3/r3-accuracy.sh`，不断网。
5. **M3 脚本**：`scripts/m3/`。
6. **本脚本**：`sudo scripts/m4/run.sh`。

前面任何一步不通过，都先不要跑本脚本。本脚本要求你用 `M4_M2_PASSED=1`、`M4_M3_PASSED=1` 明确确认。

## 两个开关和它们的影响

| 开关 | 默认 | 打开后 |
|---|---|---|
| `M4_CHANGE_LANGUAGE=1`（配 `M4_LANG`，默认 `en-US`） | 关：语言和地区格式保持你现在的，只换时区 | 首选语言换成目标语言（原有的其余语言保留在后面），地区格式随之改变。**新打开的应用用新语言，系统界面要重新登录才变**。结束时按快照还原。 |
| `M4_REAL_KILL=1` | 关：恢复时只**预览**会结束哪些 Claude 进程，不发任何信号 | 恢复时 abroadd **真的强制结束**这台电脑上所有 Claude 桌面版和 Claude Code 进程，连同它们启动的子进程，**未保存的内容会丢**。脚本自己启动的假 Claude 进程到那时已经结束；如果还在，它也会出现在预览里并被结束，属于预期。 |

## 准备

- 在这台 Mac 上、用你自己的账号操作（脚本用 `sudo` 提权，并通过 `SUDO_USER` 以你的身份运行 curl、dig）。
- **关掉 Clash 等代理软件的 TUN 模式和 VPN**，**关掉系统代理**。
- 主代理节点同 M2、M3（远端或局域网节点的 Clash YAML）。
- 分流出口（有哪种测哪种，没给的步骤记“跳过”）：真实服务商的 WireGuard `.conf`（`M4_WG_CONF`）、OpenVPN `.ovpn`（`M4_OVPN_CONF`）、订阅的 Clash YAML（`M4_SUB`）。
- 编出程序（在仓库根目录、普通用户）：
  ```sh
  scripts/dev/vm-up.sh --no-panic                  # 构建镜像、编出并签名 abroad-vm（正式构建，不带开发标记）、abroad-dev
  go build -tags abroaddev -o scripts/dev/out/bin/abroadd ./cmd/abroadd   # 开发构建：真机脚本要传 --dev-skip-… 参数（M6-1）
  ```
- 先跑一次前提检查（只读、不需要 sudo）：
  ```sh
  M4_M2_PASSED=1 M4_M3_PASSED=1 M4_NODES=~/m2-nodes.yaml M4_NODE=my-node scripts/m4/run.sh --check-only
  ```
  它会显示**将要改成的时区**（默认取“应用主代理”时按出口算出的时区）、**语言改不改**、**现在能看到的 Claude 进程数**。看到“前提检查通过”再往下。

## 运行

```sh
sudo M4_M2_PASSED=1 M4_M3_PASSED=1 M4_NODES=$HOME/m2-nodes.yaml M4_NODE=my-node \
  M4_WG_CONF=$HOME/wg.conf M4_OVPN_CONF=$HOME/vpn.ovpn M4_SUB=$HOME/sub.yaml scripts/m4/run.sh
```

开始前脚本再显示一遍时区、语言和进程数，**按回车才开始**。其他可选环境变量：`M4_TZ`（目标时区的显式覆盖）、`M4_EXIT_HOST`（出口检查主机，默认 `www.gstatic.com`）、`M4_DEADMAN_SECS`（死人开关，默认 2700 秒）、`M4_R7_SECS`（停用后观察的秒数，默认 120）、`M4_CAPTIVE=1`（额外做 R16，需要人工连认证 Wi-Fi）。

**总时长**约 15–20 分钟（三个出口都给时）。**断网时段**：从启用到 T13 恢复，除了经主代理和分流出口的流量，本机都上不了网，约 12–15 分钟；其中 T11 停用后的 2 分钟网络照常（只观察系统日志）。

安全约定同 M2、M3：改系统之前先启动**死人开关**（到点自动执行 `restore.sh`，启动后核对它确实在运行）；中途失败或 Ctrl-C 时立刻自动执行一次 `restore.sh`；网络和地区的快照另存一份到重启不丢的 `/Library/Application Support/claude-abroad-m4-snap`。`restore.sh` 只按脚本自己拍的快照还原，不依赖 abroadd。

## 每一步在验证什么

| 步骤 | 验证 | 判定 |
|---|---|---|
| T1（R4） | 先“应用主代理”，取它按出口算出的时区；锁定手动目标（这个时区 + 你现在的语言和地区格式）；两步启用；读回时区、自动时区、语言、地区格式 | 时区是目标、自动时区关；默认时语言列表和地区格式与启用前相同；打开改语言时首选语言和地区格式是目标。读回值另记进采样（R4：系统实际写下的地区格式写法） |
| T2 | 正常环境下环境检测 17 项 | 17 项都有结果，硬性项没有明确不通过（无法判断的列出来）。**第 12 项要看你平时的环境**：它检查正在运行的 Claude 进程有没有带代理环境变量（`HTTPS_PROXY` 等）。如果你平时经本机代理使用 Claude，这些进程多半带着它们，T2 会如实报第 12 项不通过——这是检测在正常工作，不是脚本出错。两种做法任选：执行前先退出所有 Claude（桌面版和 Claude Code）；或者照常执行，把第 12 项的不通过连同前提检查显示的 Claude 进程数一起贴回来，由开发者判定 |
| T3a / T3r | 打开系统代理（主网络服务的 HTTPS 代理指向 127.0.0.1:9） | 第 11 项检出、闸门关；撤销后通过、闸门开。T3r：撤销后读回开关、服务器、端口与启用前一致（原来没配过服务器时，脚本把服务器清空；`networksetup` 清不掉时 T3r 记“不通过”并写明哪一项不同，见“出问题怎么办”） |
| T3f / T3b | 假 Claude 进程：系统自带的 `sleep` 复制到带 Claude Code 安装位置特征的临时路径，以你的身份带 `HTTPS_PROXY` 启动 | T3f：识别子命令把它认作 Claude Code；T3b：第 12 项检出、闸门关，脚本结束这个假进程后通过、闸门开。**不启动任何真实的 Claude，不产生任何对外请求** |
| T3c | 重新打开主网络服务的 IPv6 | 第 06 项检出、闸门关；关回去后通过、闸门开 |
| T4w / T4o / T4s（R5） | 各分流出口：curl 走它，测出口 IP | 出口 IP 与本机真实 IP 不同；与主代理的不同（相同时如实记录：可能是同一家同一个出口） |
| T4*d | 同一份配置把端点端口换成不通的 9，模拟“出口起不来” | 三条同时成立：请求失败或出口 IP 等于主代理的；绝不是本机或原出口；物理网卡上没有发往回显服务的包。**“隧道用到一半断掉”在真实服务商那边没法制造，这里没有覆盖**（虚拟机里的同机测试覆盖过） |
| T4b | 订阅出口的 UDP 域名目标：`dig @dns.google example.com` 走订阅出口 | 有应答，物理网卡上没有发往 53 的包 |
| T5 | 按出口检查的结果、端点不通时的可用状态 | 记录（采样文件） |
| T6 | 第 10 项（Claude 可达性）；本机真实公网 IP 不落盘 | 状态目录、abroadd 和 abroad-vm 的日志、status 里真实 IP 出现 0 次 |
| T7（R10） | 装了桌面版时读 `config.json` 的顶层 `locale` | 记录（只读） |
| T8（R17） | 桌面版签名的 Team ID；几种安装位置是否命中特征 | Team ID 与假定值相同；安装位置的结果记录（只读） |
| T9 | 恢复的第一步：结束 Claude 的预览 | 有确认码，预览里的路径已换成 `~` |
| T10 | 切换到出口同一个国家的另一个时区，再切回 | 两步确认后读回一致；出口国家只有一个时区时跳过 |
| T11（R7） | 停用后 2 分钟系统推送和对时的日志 | 只统计行数和关键字次数（不写日志内容） |
| T12（R16） | 需要认证的 Wi-Fi | 默认跳过 |
| T13 | 两步恢复后与快照逐项比对 | 路由、DNS、IPv6、锚点、时区、自动时区、语言、地区格式都与启用前一致 |

本机真实公网 IP 在启用前用 `https://api.ipify.org` 测一次，只放在状态目录下 0700 的 `secret/` 子目录里，用完即删，`restore.sh` 兜底再删；结果和日志里只写“已测得/没测到”，比较只输出“相等/不等”。

## 改动类命令清单

脚本会执行的改动类命令全部列在 `scripts/m4/commands.txt`（正则），`scripts/m4/check-commands.sh` 用它核对空跑输出。要点：

- 建、删状态目录 `/var/run/claude-abroad-m4`、安装目录和持久快照目录；`dscl` 建、删测试用户 `_abroadm4`；安装、卸载 LaunchDaemon `com.claude-abroad.m4.abroadd`。
- `networksetup -setsecurewebproxy`、`-setsecurewebproxystate on/off`（T3a，成对；restore 按快照兜底）；`networksetup -setv6automatic` / `-setv6off`（T3c，成对；restore 按快照兜底）。
- 假 Claude 进程：`cp /bin/sleep …/m4fake/…/claude`、以你的身份启动、按 PID 文件核对路径后结束。
- `tcpdump -i <物理网卡> -n -l -q <过滤式>`：只读抓包，只统计行数，文本只给 root 读、统计完就删。
- restore：`route -n delete -net 0.0.0.0/1、128.0.0.0/1`（只删经 utun 的）、`scutil remove` 我们的 DNS 键、按快照还原 IPv6、`systemsetup -settimezone`、`defaults write/delete` 自动时区和语言（只按快照写回）、`pfctl -a com.apple/250.abroad -F all`、`pfctl -X <token>`。
- **不执行** `pfctl -d`，不按名字结束进程，不碰你的 Clash，不改 DNS 服务器设置，脚本自己不结束任何 Claude 进程（真的结束只经 abroadd，且只在 `M4_REAL_KILL=1` 时）。

## 出问题怎么办

- 中途卡住或失败：脚本会自动执行 `restore.sh`；没有的话手动执行 `sudo scripts/m4/restore.sh`（可以重复执行）。
- 还上不了网：重启电脑（pf 规则不跨重启），**重启后再执行一次 `sudo scripts/m4/restore.sh`**：时区、语言和 IPv6 的持久快照在 `/Library/Application Support/claude-abroad-m4-snap`，重启不丢，restore 按它还原。
- T3r 不通过或 restore 报“HTTPS 代理没有完全还原”：多半是“安全网页代理（HTTPS）”的服务器栏里留着 `127.0.0.1`、端口 `9`（开关是关的，功能上不影响上网）。在“系统设置 → 网络 → 你的网络 → 详细信息 → 代理 → 安全网页代理（HTTPS）”里把服务器和端口清空即可。
- restore 报“地区还原后读回与快照不一致”：在“系统设置 → 通用 → 日期与时间”“语言与地区”里手动核对，并把日志贴回来。

## 贴回什么

`scripts/m4/out/` 下本次的 `m4-<时间>.log`、`m4-results-<时间>.txt`、`m4-samples-<时间>.txt`、`m4-abroadd-<时间>.log`、`m4-abroad-vm-<时间>.log`。采样文件已经脱敏（地址换成占位，登录用户名换成占位，路径里的主目录换成 `~`）。
