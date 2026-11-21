# M3 真机验证（给用户执行）

这里有两个脚本：

- `run.sh`：**M3 真机验证**。在你这台 Mac 上以真实模式跑 abroadd，验证分流：多出口、直连、Claude 相关域名不会从本机网络泄漏、停用后的“断网时仍可用”名单。会改本机网络、需要 `sudo`。
- `r3-accuracy.sh`：**R3**，查连接表识别进程的准确率。只连本机回环，不改网络（见文末）。

开发会话只对 `run.sh` 做过语法检查、`DRY_RUN=1` 空跑、命令核对、`--check-only` 和自测（`scripts/m3/selftest.sh`），没有真机执行过。

## 先后顺序（重要）

1. **R1**：`scripts/m0/r1/`，不断网。
2. **R2**：`scripts/m0/r2/`，断网 1–2 分钟。
3. **M2 脚本**：`scripts/m2/`。它验证的断网保护、接管、恢复是本脚本的基础。
4. **R3 脚本**：`sudo scripts/m3/r3-accuracy.sh`，不断网（本脚本的第 S5 步也会再跑一次）。
5. **本脚本**：`sudo scripts/m3/run.sh`。

前面任何一步不通过，都先不要跑本脚本，把那一步的结果贴回来。本脚本要求你用 `M3_M2_PASSED=1` 明确确认 M2 已经通过。

## 准备

- 在这台 Mac 上、用你自己的账号操作（脚本用 `sudo` 提权，并通过 `SUDO_USER` 以你的身份运行 Chrome、curl、nc）。
- **关掉 Clash 等代理软件的 TUN 模式**，**关掉系统代理**（同 M2）。代理软件本身可以继续开着。
- **完全退出 Chrome**（⌘Q）。脚本会用一个单独的临时配置启动它，不碰你自己的 Chrome 配置；已经开着的 Chrome 会把旧连接混进判定，前提检查会拦下。没装 Chrome 的，和 Chrome 有关的两步记“跳过”。
- 主代理节点同 M2（远端或局域网节点的 Clash YAML，例如 `~/m2-nodes.yaml`）。
- 编出程序（在仓库根目录、普通用户）：
  ```sh
  scripts/dev/vm-up.sh --no-panic                  # 构建镜像、编出并签名 abroad-vm、abroad-dev
  go build -o scripts/dev/out/bin/abroadd ./cmd/abroadd
  CGO_ENABLED=1 go build -tags abroaddev -o scripts/dev/out/bin/abroad-vm-dev ./cmd/abroad-vm
  scripts/dev/sign.sh scripts/dev/out/bin/abroad-vm-dev   # 开发标签构建的 abroad-vm（带开发用多出口），要签名才能起虚拟机
  ```
- 先跑一次前提检查（只读、不需要 sudo）：
  ```sh
  M3_M2_PASSED=1 M3_NODES=~/m2-nodes.yaml M3_NODE=my-node scripts/m3/run.sh --check-only
  ```
  看到“前提检查通过”再往下。
- 结束 Claude Code 会话和其他要紧的网络任务（运行期间会断网）。

## 运行

```sh
sudo M3_M2_PASSED=1 M3_NODES=$HOME/m2-nodes.yaml M3_NODE=my-node scripts/m3/run.sh
```

可选环境变量：
- `M3_DIRECT_HOST`：直连测试访问的域名，默认 `www.apple.com`，要能从你的本机网络直接连上 443 端口。
- `M3_DIRECT_TARGET`、`M3_DIRECT_TLS_NAME`：在途 fd 检查（S6）连的公网地址和它的 TLS 证书名，默认 `1.1.1.1:443` 和 `one.one.one.one`。这个地址在你的网络里连不上时，S6 会记“跳过：目标不可达”，换一个能直连的 HTTPS 地址重跑。
- `M3_INTERACTIVE=1`：额外做一次微信的观察（把微信设成直连，脚本停下来等你发一条消息，只看计数）。**这时请同时把 `M3_DEADMAN_SECS` 调大**。
- `M3_EXIT_HOST`：出口检查访问的主机，默认 `www.gstatic.com`。
- `M3_DEADMAN_SECS`：死人开关的时限，默认 1500 秒（25 分钟）。

**总时长**约 10–12 分钟（含 R3 那一步约 3 分钟）。**断网时段**：从启用到 S10 恢复，除了经主代理、开发出口和直连拨号模块的流量，本机都上不了网，约 8–10 分钟。其中 S8 停用后的约 15 秒里，只有 `nc`（“断网时仍可用”名单里的应用）能直连，其余都不通。

安全约定同 M2：改系统之前先启动**死人开关**（到点自动执行 `restore.sh`，启动后核对它确实在运行）；中途失败或 Ctrl-C 时立刻自动执行一次 `restore.sh`；拍的快照另存一份到重启不丢的 `/Library/Application Support/claude-abroad-m3-snap`。

## 出口 A、B 是什么

真实的分流出口（WireGuard、另一个 Clash 节点）要到 M4 才接进来。本脚本用两个**开发上游**充当出口 A（`wg-a`）和出口 B（`clash-b`）：它们是本机回环上的小程序，以测试用户 `_abroadm3` 运行，**在本机直接应答、不向外连接**，只用来分辨一条连接走的是哪个出口。每个应用访问一个带本次随机标记的地址（例如 `http://m3-1a2b3c4d-a.m3.test/whoami`），对应的上游把它记进自己的日志，脚本到日志里找这个标记。

## 出问题怎么办

- 网络不通、脚本卡住或被中断：`sudo scripts/m3/restore.sh`。它不依赖 abroadd，可以重复执行。
- 仍然不行：**重启电脑**（pf 规则不跨重启保留），**重启后仍要执行一次 `sudo scripts/m3/restore.sh`**（IPv6 的关闭是持久的，它按持久快照还原；它也会移除 LaunchDaemon、删掉测试用户、删掉 Chrome 的临时配置和抓包文本）。
- restore.sh 会把 abroadd 的状态目录改名备份（`abroadd.bak-<时间>`），下一次运行不会按旧状态自动接管。
- 脚本不会执行 `pfctl -d`，不会按名字结束任何进程（Chrome 也是按它的锁文件里的进程号、核对命令行里有临时配置目录之后才结束），不会碰你的 Clash，也不会碰你自己的 Chrome 配置。

## 步骤和判定表

每一步独立记“通过、不通过、跳过、记录”，一步不通过不影响后面能做的步骤。结果汇总在 `scripts/m3/out/m3-results-<时间>.txt`。

规则（脚本经本地接口写入）：Chrome（`com.google.chrome`）→ 出口 A；`/usr/bin/curl` → 出口 B；`/usr/bin/nc` → 直连，并在“断网时仍可用”名单里；带 `M3_INTERACTIVE=1` 时微信 → 直连。

| 步 | 做什么 | 通过的样子 | 不通过时贴回 |
|---|---|---|---|
| S1 | Chrome（你的身份、临时配置）打开带标记的 `…-a.m3.test` | 本次标记只出现在出口 A 的上游日志里。新配置的 Chrome 有一批后台连接也会进出口 A，只看带标记的那一条 | results、log、`m3-abroadd-*.log` |
| S2 | curl 访问带标记的 `…-b.m3.test/whoami` | 标记只在出口 B 的日志里，响应是 `clash-b` | 同上 |
| S3 | `nc` 按域名（`M3_DIRECT_HOST`）连 443 | 直连成功计数增长。用域名而不用裸 IP：`nc` 不发数据，裸 IP 的 443 等不到首包会改走主代理（见 S3b） | results、samples（status 里的 `direct_fail`） |
| S3b | `nc` 连裸 IP `1.1.1.1:443`、不发数据 | 按“443 端口首包超时”改走主代理：`port443_timeout` 增长，直连计数不变 | results |
| S4 | 把 Chrome 改成直连，打开 `https://claude.ai/`；同时在物理网卡上只读抓包 20 秒 | 四条同时成立：置顶判定计数增长；主代理一侧有新连接；**发往 Anthropic 地址段（160.79.104.0/21）的包 0 个**；**对 claude.ai、anthropic.com 的 DNS 查询 0 条**。抓包只数行数，53 端口的文本统计完立刻删除，日志里只有计数 | results、log |
| S5 | 系统进程的识别抽查（`abroad-dev ident-sample`）+ `r3-accuracy.sh` | 只记录：识别率、系统进程条目数；R3 的判定见文末 | samples（ident-sample 一节）、`r3-curl-*.txt` |
| S6 | 在途 fd：从 root 一侧经直连拨号模块拨 300 次（并发 10），每次收到连接后做一次完整的 TLS 握手 | 300 次全部成功（拨号失败、握手失败都是 0）。握手全部失败、拨号都成功时记“跳过：目标不可达” | results、samples（direct-probe 一节） |
| S6b | S6 的探针带了自己的配置；之后 `nc` 再按域名直连一次 | 直连拨号模块回了 `stale_config`，abroadd 自动补发配置后直连照常成功 | results |
| S7 | 直连拨号模块以测试用户运行时，出站是否被 pf 放行（对照 R2） | S3 或 S6 在启用状态下连上了 | results、log |
| S9 | R15：Docker、OrbStack 在运行时，容器里访问一次 `www.example.com` | 只记录退出码和经代理、直连的计数；没有在运行的就跳过 | results |
| WX | 微信（`M3_INTERACTIVE=1`）：你发一条消息 | 只记录直连计数的增长 | results |
| S8 | 停用；`nc` 按域名连 443，再连 `claude.ai:443`，同时只读抓包 | 普通域名经直连连上；claude.ai 没有经直连发出；发往 Anthropic 地址段的包 0 个 | results、log |
| S10 | 恢复，与启用前的快照逐项比对 | 路由、DNS、IPv6、锚点都与启用前一致 | results、log |

执行顺序是上表的顺序（S9、WX 在 S8 之前，因为它们要在启用状态下做），最后执行 restore.sh。

## 贴回哪些文件

`scripts/m3/out/` 下这次运行的：
- `m3-<时间>.log`：过程日志；
- `m3-results-<时间>.txt`：结果汇总；
- `m3-samples-<时间>.txt`：样本（status、ident-sample、direct-probe 的汇总），已脱敏；
- `m3-abroadd-<时间>.log`、`m3-abroad-vm-<时间>.log`：两个程序的日志；
- `r3-curl-<时间>.txt`：S5 跑出的 R3 结果（最新的一份）；
- restore 的日志 `m3-restore-<时间>.log`。

脚本自己会脱敏：本机和局域网的 IPv4、全部 IPv6、MAC 换成占位；抓包只记数量。贴之前请再扫一眼，确认没有节点地址和密码。

## 改动类命令清单

脚本自己执行的改动类命令见 `scripts/m3/commands.txt`（扩展正则，每行一条），`check-commands.sh` 据此核对空跑输出。概括起来：

- 建状态目录 `/var/run/claude-abroad-m3`、启动死人开关；拍快照（另存一份到 `/Library/Application Support/claude-abroad-m3-snap`）；
- 把 `abroad-vm-dev`、`abroadd`、`abroad-dev` 和镜像装到 `/Library/Application Support/claude-abroad-m3`；建测试用户 `_abroadm3`（无 shell、无 home、隐藏）；
- 以 `_abroadm3` 起两个开发上游和开发版 abroad-vm（带直连拨号模块）；下发主代理、设置开发出口；
- 装开发用 LaunchDaemon `com.claude-abroad.m3.abroadd` 启动 abroadd；经本地接口启用、写规则、停用、恢复；
- 建 Chrome 的临时配置目录（属主是你，700），以你的身份 `open -na "Google Chrome" --args --user-data-dir=… <地址>`；
- 以 root 只读抓包：`/usr/sbin/tcpdump -i <物理网卡> -n -l [-q] <过滤式>`，不写文件、不带执行命令的选项，按进程号结束；
- restore：卸载 LaunchDaemon → 按 PID 文件结束 abroad-vm、两个上游、抓包、Chrome → 删抓包文本和 Chrome 临时配置 → 删两条 `/1` 路由、我们的 DNS 键、按快照还原 IPv6 → 清空锚点 `com.apple/250.abroad`、释放 pf 引用 → 备份 abroadd 状态目录 → 删测试用户 → 取消死人开关 → 删安装目录和持久快照。

abroadd 运行时执行的命令同 M2（`internal/daemon/testdata/commands.golden`）。

## r3-accuracy.sh：查连接表识别进程的准确率（风险 R3）

**为什么要 root**：在 macOS 27.2.1 上实测，普通用户读连接表（`net.inet.tcp.pcblist_n`，netstat 用的也是它）只能看到**本进程自己**的套接字，连同一用户的其他进程都看不到。真实模式下 abroadd 以 root 运行，所以识别别的进程（这里是 curl）的准确率只能用 root 测。

**做什么**：
- 只在本机回环（127.0.0.1）上起一个 HTTP 服务，跑两档 curl：总数 1000、分 10 批每批并发 100；一次并发 300；
- 服务端每接受一条连接，就识别一次它属于哪个进程，统计识别正确（是 curl）、认错（是别的程序）、识别不到的比例，以及单次识别的耗时；
- 最后以 root 跑一次只读的子进程识别测试（`/usr/bin/nc` 连本机回环）。

**不会改动**任何网络设置：不碰 pf、路由、DNS、网卡，不断网，可以随时 Ctrl-C。

**执行**（约 2 分钟，两档之间等 30 秒；可以和 R1、utun 测试一起做）：

```sh
sudo scripts/m3/r3-accuracy.sh
```

完成后把 `scripts/m3/out/r3-curl-<时间>.txt` 贴回给主会话。

**目标**（architect 审 M3-3 时定）：
- 认错率：两档都必须为 0；
- 识别率：分批那档不低于 95%，一次并发 300 那档不低于 90%；
- 单次识别耗时的 99 分位：分批那档不超过 20 毫秒。

**已知的测量干扰**：并发数远超 `kern.ipc.somaxconn`（128）时，本机内核偶尔会把同一条连接交给 accept 两次，回环的监听也可能丢连接。工具按四元组去重，重复的单独计数（“内核重复交付”），不计入识别统计。这个现象只出现在测量工具的内核监听上，产品的连接走 gVisor 用户态协议栈，不经内核 accept。

会话里以普通用户跑过的部分（本进程自己的并发连接）见 `out/r3-self-*.txt`。

## 时区和语言（M4-5 起）

这组脚本**不会改你的时区和语言**：abroadd 的 LaunchDaemon 带 `--dev-skip-region`，不同步地区。启用之前脚本会先“应用主代理”（经主代理跑一轮出口检查，建立出口基准；M4-3 起没有基准不能启用），这一步不涉及地区，不需要确认。地区的切换和还原由 M4 的真机脚本单独验证。
