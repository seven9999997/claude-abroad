# M0 技术验证 实施计划

> 状态：**已作废**（2026-11-17 用户决定 M0 不单独做 demo，见 docs/m1-plan.md）。只有第 1 节 R2 已交付脚本，仍然有效。
> 原状态：v2（2026-11-17，已按 abroad-architect 审核意见修改）
> 依据：design.md 第 14、15 节；PROGRESS.md 的 M0 待办
> 环境：macOS 27，Apple Silicon，Go 1.26；开发机上可能开着代理软件的 TUN；不使用付费 Apple Developer 账号

## 总原则

- 每个风险项产出：最小 demo（Go 程序在 `cmd/m0-<编号>/`，脚本在 `scripts/m0/<编号>/`）、用户执行步骤、结论（写回 PROGRESS.md 和 design.md 第 15/16 节）。
- 主会话只写代码、编译、跑不需要 root 的测试；凡是 `sudo`、`pfctl`、`/etc/resolver`、`launchctl`、建用户的步骤，都写成脚本由用户执行。脚本 README 明确写出：会建临时用户、会加载临时后台服务、会断网多久。
- demo 不进正式包结构，M1 起按第 12 节重写；能复用的（IP_BOUND_IF 拨号、fd 传递）届时搬进 `internal/`。
- 加 gvisor 依赖时固定在 third_party 用的版本（`gvisor.dev/gvisor v0.0.0-20240916094835-a174eb65023f`），不升级。

### 脚本安全约定（所有改系统状态的脚本）

1. **开头先清残留**：检查上次留下的临时用户、锚点、token、plist、resolver 文件，有就先清理。
2. **先记状态、再起死人开关、最后才改系统**：
   - 原 pf 状态、token、建过的用户名、写过的文件，记到 root 所有的固定目录 `/var/run/claude-abroad-m0/`。
   - 死人开关用 `nohup … & disown` 起独立进程（不靠 `trap`，关终端、SIGKILL 都不影响它），到点执行 `restore.sh`。
   - `trap` 只作为正常退出时的第一道还原。
3. `restore.sh` 只从 `/var/run/claude-abroad-m0/` 读状态，可重复执行，也可以单独运行。
4. 测试程序和 unix socket 都先复制到 root 所有的 `/Library/Application Support/claude-abroad-m0/` 再运行（仓库在 `~/Desktop`，临时用户进不去，也不应以 root 运行用户可写目录里的程序）。
5. 日志写到 `scripts/m0/<编号>/out/`，结束后 `chown` 回 `$SUDO_USER`；`out/` 在 `.gitignore` 里。日志不写出口 IP；pcap 只留本机，不贴回。
6. 网络恢复验证用网关和国内地址（223.5.5.5），不用需要代理才通的地址。
7. 每个脚本支持 `--dry-run`：只打印将执行的命令，主会话可以跑。

## 顺序

R2 → R1 → R9 → R6（分段）→ R11（书面）。

---

## 1. R2：pf `user` 规则与"默认拦截所有接口"

### 要回答

1. `pass out quick proto {tcp udp} user _abroadm0` 能否匹配：
   a. 测试用户用 `IP_BOUND_IF` 绑定物理网卡发出的 TCP、UDP（已 connect 的 UDP，以及未 connect、`sendto` 多个目标的 UDP）；
   b. 测试用户建 socket 并 connect，经 `SCM_RIGHTS` 交给 root，由 root 收发（6.3）。TCP、UDP 都测；UDP 分"传 fd 前发过包 / 没发过包"；
   c. **反向对照**：root 自己建的绑定 en0 的 socket 必须落在兜底规则上；root 建 socket 传给测试用户发送，看 pf 认创建者还是使用者。
2. 加载第 6.1 节整套规则后：
   a. 普通用户进程的 TCP/UDP/ICMP 被拦（计数增加）；
   b. 局域网放行生效（到网关的 ping/TCP）；
   c. DHCP：`ipconfig set en0 DHCP` 触发续租，看 DHCP 规则计数。计数为 0 且续租成功，说明 macOS 的 DHCP 走 BPF、该规则多余；
   d. IPv6 全拦；**加测 `::1`**：监听 `[::1]` 再连接。6.1 现在 `block inet6` 排在 `pass on lo0` 前面，若 `::1` 被拦，6.1 要把 lo0 放行挪到第一条。同时观察 NDP（ICMPv6）和 awdl0（隔空投送）；
   e. 规则撤销后网络立即恢复。
3. **锚点顺序**：
   - 递归导出 `com.apple` 下所有子锚点的规则（`pfctl -vsA`，逐个 `-a … -sr`），记录应用防火墙开关状态。
   - 功能验证：放两个探针锚点，一个名字排在前、带 `pass out quick` 到某目标，一个排在后带 `block quick` 同一目标，确认"按名字排序、quick 跨兄弟锚点生效"。
   - 同时试加载到 `com.apple/100.abroadm0`（排在 AirDrop 和 ApplicationFirewall 前面）。实测成立后回写 6.1：正式锚点改为排在最前的名字。
4. `pfctl -E` / `pfctl -X <token>` 的引用计数：结束后 pf 回到开始前的状态。

### 判定

- **以 pf 规则计数为主、连通为辅**。每个用例先在无规则时跑一遍作基线（排除"被线路丢了"误判成"pf 拦了"）。
- 默认探测目标用国内直连稳定的地址：`223.5.5.5` 的 UDP 53 和 TCP 443；可通过参数改。
- 1a、1b 计数全部落在 `user` 规则上 → R2 成立（直连拨号部分）。gvisor-tap-vsock 的出站在 R1 合测（见下）。
- 结论注明："未在有 `0/1`、`128/1` 接管路由的情况下验证，留到 M2"。

### 产出

- `cmd/m0-r2-probe/`：Go 小程序，子命令
  - `dial`：以当前身份拨号 TCP / UDP(connect) / UDP(sendto 多目标)，可选 `IP_BOUND_IF`；
  - `fdserver` / `fdclient`：fd 传递两端，支持 `-presend`，也支持反过来（root 建、测试用户用）；
  - `listen6`：监听 `[::1]` 并自连。
- `scripts/m0/r2/run.sh`、`restore.sh`、`README.md`。
- 单元测试：规则文本生成、局域网段计算；fd 传递用 socketpair 在本进程内测；IP_BOUND_IF 绑 lo0 放在 `integration` 标签下。

### 用户需要注意

- 断网约 1–2 分钟：这个 Claude Code 会话、SSH、下载、通话、云同步都会中断；现有 `bridge100` 上的虚拟机流量也受影响。
- 执行前关掉 Clash/Mihomo 的 TUN 和系统代理（它们会接管路由、重载 pf）。

---

## 2. R1：Virtualization.framework 以非登录系统用户在 launchd 下运行

### 要回答

1. 带 `com.apple.security.virtualization`（ad-hoc 签名）的程序，作为 LaunchDaemon、`UserName=_abroadm0`（无登录会话、无 home）启动时，能否创建并启动 Linux 虚拟机。
2. vsock 桥接到 unix socket，与虚拟机内程序双向通信。
3. `VZFileHandleNetworkDeviceAttachment` + gvisor-tap-vsock 让虚拟机访问外网。
4. **与 R2 合测**：加载 R2 规则后让虚拟机 `curl`，看计数落在 `user` 规则上（这才是 R2 原文问的"gvisor-tap-vsock 发出的连接"）。
5. 冷启动到 vsock 可用的耗时、空闲内存占用。

### 产出

- `cmd/m0-r1-vm/`：宿主程序（cgo，`github.com/Code-Hex/vz/v3`）。串口输出写日志；vsock 1081 桥接到 unix socket；网络一端 socketpair 交给 VZ，另一端接 gvisor-tap-vsock 的 virtualnetwork。
- `cmd/m0-r1-guestinit/`（linux/arm64 静态）：作为 initramfs 的 `/init`。挂载 proc/sys/dev；按 `modules.dep` 顺序加载 vsock、virtio-net 模块（Alpine 的 `.ko` 是压缩的，先在用户态解压再 `init_module`）；配置 eth0；vsock 1081 上提供 `ping`、`curl <url>`、`bench`（R6 用）。
- `cmd/m0-r1-mkinitramfs/`：Go 写的 newc cpio 生成器，不依赖 Docker。
- `scripts/m0/r1/fetch-kernel.sh`：下载固定版本的 Alpine 3.22 `linux-virt` apk（固定 URL + SHA-256），先看 `vmlinuz-virt` 文件头判断是 gzip 还是 EFI zboot，再解成未压缩 `Image`，取出所需 `.ko` 和 `modules.dep`。
- `scripts/m0/r1/sign.sh`、`run-user.sh`（当前用户运行，主会话可自己跑）。
- `scripts/m0/r1/run-daemon.sh`（sudo）：plist 放在 `/Library/Application Support/claude-abroad-m0/`（**不放 `/Library/LaunchDaemons`**，中断后重启不会自动拉起），`launchctl bootstrap system <路径>`；依次跑 `UserName=_abroadm0` 和 root 对照两种；带 `--with-pf` 时同时加载 R2 规则做合测。
- `scripts/m0/r1/restore.sh`。

### 判定

- `_abroadm0` 能起来 → **R1 部分成立（launchctl 加载 + UserName）**。SMAppService 下是否相同，并入 R8，在 M5 开始前验证（需要开发者账号）。
- `_abroadm0` 起不来、root 能起来 → 备选改为"虚拟机宿主用 root，出站网络进程单独以 `_abroad` 运行，网卡 fd 传过去"（优于退到 LaunchAgent，开机到登录前仍能工作）。交 architect 拍板。
- root 也起不来 → 后台环境本身不行，再议 LaunchAgent。

### 已知遗留（记进度单）

gvisor-tap-vsock 的出站拨号是裸 `net.Dialer{}` / `net.Dial("udp")`（`pkg/services/forwarder/tcp.go:40`、`udp.go:49`），没有 `IP_BOUND_IF`。M2 有 `0/1` 路由后，虚拟机出站会被送回 utun 形成环路。M1/M2 需要新补丁。

---

## 3. R9：`/etc/resolver` 域名解析锁（并验证 R18）

### 判定方法

- **只以抓包为准**，应用表现只作记录（国内关掉代理后 Claude 本来就连不上，"打不开"证明不了什么）。
- 抓包：所有非回环接口（加上 lo0 单独一份），过滤 `port 53 or port 853 or port 443 or port 80 or host 198.18.0.2`，写 pcap。事后按 **DNS 查询名**和 **TLS SNI** 里是否出现 `claude` / `anthropic` 判定，不按 IP。
- **阳性对照**：写 resolver 文件之前，用同一套自动用例跑一遍，必须能在抓包里看到这些域名的查询。看不到说明方法有问题，该场景结论作废。
- 盲区：DoH + ECH 时 SNI 也看不到，这类记"无法用抓包判定"。
- 可选：试 `tcpdump -i pktap,all -k NP` 给每个包标进程名，可用就一起记录。
- 被测程序：`dscacheutil -q host`、`curl`、Claude Code（记录安装方式：原生安装版是 Bun 编译的；有 npm 版就两种都测）、Claude 桌面版、Safari、Chrome（安全 DNS 关/开）、Firefox（DoH 关/开）。自动部分脚本跑，GUI 部分脚本提示用户在限定时间内手动打开。

### 场景

| 场景 | 环境 | 预期 |
|---|---|---|
| A | 代理软件全关，纯直连 | 锁有效（待验证） |
| B | 开着平时的 Clash/Mihomo TUN | `dns-hijack any:53` 应答发往 198.18.0.2 的查询，锁失效 |
| C | 只开系统代理，或 shell 里有 `HTTPS_PROXY` | 浏览器、Electron、Claude Code 把域名交给代理解析，锁不起作用 |

每个场景分别跑两种 resolver 写法：

- **方案 1（现设计）**：`nameserver 198.18.0.2`、`timeout 1`；
- **方案 2（回环候选）**：`nameserver 127.0.0.1`、`port <固定端口>`。记录 mDNSResponder 是否认 `port`、没人监听时是立即失败还是等超时、查询是否离开本机、是否被 TUN 劫持。

另记录：子域名（`api.anthropic.com`、`x.claude.ai`）是否覆盖；`scutil --dns` 里是否出现这些 resolver。

### 产出

- `scripts/m0/r9/run.sh --scenario A|B|C`、`restore.sh`、`README.md`、`analyze.sh`（从 pcap 提取 DNS 查询名和 SNI，输出不含 IP 的汇总）。
- 结论交 architect，决定是否把解析锁地址改为回环方案。

---

## 4. R6：性能（M0 只做分段测量 + 全链路估算）

| 段 | 测法 | 需要 root |
|---|---|---|
| A. gVisor 网络栈 | `cmd/m0-r6-stack/`：进程内用 channel link endpoint 连两个 gVisor 栈，TCP 单流/多流吞吐与 CPU | 否 |
| B. vsock | R1 的虚拟机：unix socket → vsock → guestinit 回灌，吞吐与往返延迟 | 否 |
| C. 虚拟机出站（gvisor-tap-vsock） | guestinit 经虚拟网卡连宿主上的测试服务器 | 否 |
| D. utun 读写 | 推迟到 M2 | 是 |

CPU 用 `getrusage` 统计。结果写 `docs/m0-r6.md`，估算全链路能否达到 500 Mbps / CPU < 30%。全链路实测放进 M2 完成标准。

---

## 5. R11：Network Extension 透明代理（书面对比）

用户没有付费开发者账号，拿不到 Network Extension 授权，不做 demo。`docs/m0-r11.md` 对比：进程识别准确度、UDP/DNS 覆盖、扩展崩溃或被禁用时流量直连还是断开（透明代理不处理的流量默认直连，与"失败即断网"冲突）、开机到登录前的覆盖、分发与授权要求。不能实测的部分标"未实测"。预期结论：维持 utun + pf。

---

## 设计文档回写（开工前完成，v0.3）

- 第 14 节 M0 完成标准：改为"R1、R2、R9 有可运行的 demo 和结论；R6 有分段测量和全链路估算；R11 有书面对比和结论"。M2 完成标准加"全链路 R6 达标或有明确的降级决定"。
- 第 15 节：R1 拆出"SMAppService 注册下是否相同"并入 R8，提前到 M5 前；R11 验证方式改为书面对比；新增 R18（回国状态下其他代理软件让解析锁失效）。
- 第 10.2 节"限制"：补充开着 TUN（DNS 劫持）、系统代理、环境变量代理时锁不起作用。
- 第 8.1 第 01 项 / 6.1 巡检：补"`route get 198.18.0.2` 走的是我们的 utun"（别的 TUN 的 /16 比我们的 /15 更具体，会抢走 DNS）。
- 6.1 的锚点名、lo0 规则顺序、DHCP 规则、NDP 说法：等 R2 实测后再改。

## 文件清单

```
cmd/m0-r2-probe/           main.go, dial_darwin.go, fdpass_unix.go, rules.go, *_test.go
cmd/m0-r1-vm/              main.go（cgo, darwin）, net.go
cmd/m0-r1-guestinit/       main.go（linux）
cmd/m0-r1-mkinitramfs/     main.go, cpio.go, cpio_test.go
cmd/m0-r6-stack/           main.go
scripts/m0/lib.sh          公共函数：状态目录、死人开关、残留清理、dry-run
scripts/m0/r2/  r1/  r9/   run*.sh, restore.sh, README.md
docs/m0-plan.md, docs/m0-r6.md, docs/m0-r11.md
.gitignore：scripts/m0/*/out/
```

## 测试

- `go build ./...`、`go vet ./...`、`GOOS=linux GOARCH=arm64 go build ./cmd/m0-r1-guestinit`
- 单元测试：规则生成、cpio 生成、fd 传递（socketpair）、IP_BOUND_IF 绑 lo0（integration）
- 脚本：`bash -n`、`shellcheck`（如已安装）、`--dry-run`
