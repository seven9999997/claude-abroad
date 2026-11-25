# 开发脚本

在这台 Mac 上起一台不联网的开发虚拟机，验证 abroad-agent 能开机、控制面可达。

**不需要 sudo**，不改任何系统设置。虚拟机的网卡接到 abroad-vm 进程内的用户态网络（gvisor-tap-vsock）：只能访问公网 IPv4，每条出站都绑定当前物理网卡，网关 DNS 一律 REFUSED。不改本机任何网络设置。vm-up 的 4f 阶段会从虚拟机向公共 DNS（223.5.5.5，不通时 119.29.29.29）查询 example.com，用来验证出站。只写仓库内的 `scripts/dev/out/` 和 `guest/out/`（都已在 .gitignore）。

## 前提

- Apple Silicon，macOS 14 以上。
- Xcode 命令行工具（cgo 编译 Virtualization.framework 的绑定要用）。
- Go 1.26。

## 文件

| 文件 | 作用 |
|---|---|
| `vm-up.sh` | 阶段 A 全流程：构建 agent 和镜像 → verify → 构建并签名 abroad-vm → 启动 → 等 health → 经 ctl.sock 请求关机并验证自动重启 → SIGTERM 停止 → 确认清理干净 → panic 探测。加 `--no-panic` 跳过最后一轮。 |
| （`vm-up.sh` 的阶段）| 阶段 A（开机、控制面、对时、出站）、6b（回环白名单）、6c（出站收紧到第一跳）、阶段 B、C（主代理 = 本机回环上的开发上游）、阶段 E、阶段 D（见下）。 |
| `pf-syntax.sh` | pf 规则的语法检查（M2-6a）：用 `abroad-dev pf-render` 生成三份规则（完整、开机、关闭局域网），只运行 `/sbin/pfctl -n -f <文件>`，外加一份故意写错的反例，必须失败。`-n` 只解析不加载，脚本以 root 运行时拒绝，不会改动 pf 状态。结果写进 `out/pf-syntax-<时间>.txt`。 |
| `sign.sh` | 用 ad-hoc 签名（`codesign -s -`）加上 `com.apple.security.virtualization` 授权，不需要开发者账号。 |
| `entitlements.plist` | 只有上面这一个授权。 |

## 输出（`scripts/dev/out/`）

- `stage-a-<时间>.txt`：证据文件，包括每一步的结果和耗时、health 应答、控制台日志节选、abroad-vm 日志。
- `abroad-vm-<时间>-<状态目录名>.log`：abroad-vm 的日志。
- `state/`：常规轮的状态目录（0700），里面有 `console.log`（虚拟机控制台，0600）。运行时还有 `ctl.sock`（→ 控制面 vsock 1081）、`data.sock`（→ 数据面 vsock 1080）、`host.sock`（abroad-vm 自己应答的只读诊断：`curl --unix-socket scripts/dev/out/state/host.sock http://x/v1/netstats` 返回出站计数和绑定的网卡名）、`abroad-vm.pid`、`abroad-vm.lock`，socket 都是 0600。
- `bin/abroad-dev`：开发工具（不进发布包）。
- `state-panic/`、`panic-img/`：panic 探测轮用的状态目录和镜像（空 initramfs）。
- `bin/abroad-vm`：签过名的 abroad-vm。

## 阶段 E（tunsim）

`abroad-dev tunsim --data-sock <路径> [--name tunsim.test] [--udp-port 4500] [--timeout 30s]` 在用户态模拟“本机应用 → utun → 网络栈 → data.sock → 虚拟机”（`internal/tunsim`）：
- 网络栈接在内存管道上，不建真实 utun，不需要 root，不碰宿主网络，只连 data.sock；
- 依次跑七步：
  - a：DNS 拿到假地址；
  - b：连假地址，HTTP 拿到 204；
  - c：UDP 回显，且回包源地址正确。发往假地址，或者发往 `--udp-target` 指定的真实 IP；
  - d：连真实 IP 203.0.113.9，拿到 204；
  - e：连没有映射的假地址，被拒；
  - f：关闸门、断开已有连接后，新的 TCP 和 UDP 都不通，DNS 仍然应答；
  - g：网络栈按时关闭；
- 输出一行 JSON 报告，全部通过时退出 0。
- `--doh-name <域名>`：在 e、f 之间多跑一步 h_doh，经主代理的 DoH（1.1.1.1:443）查这个域名的 TXT；f 里再查一次，要求关闸门后得到 SERVFAIL。
  - 开发上游答不了 DoH，所以 vm-up 的阶段 E 不带它；真实节点验证时用 `--doh-name example.com`，不带 `--udp-target`。

vm-up 的阶段 E 接在阶段 C 之后，用同一台虚拟机，主代理仍是开发上游。它断言：
- 七步全部 ok；
- 开发上游收到三类请求：域名 tunsim.test 的 CONNECT、198.51.100.7:4500 的 UDP（ATYP=1，回显）、真实 IP 203.0.113.9 的 CONNECT；并且没有 `connect 1.1.1.1:443`。
  - 阶段 E 的 UDP 发往真实 IP，原因：主代理是开发上游时，mihomo 对 UDP 域名目标会先经主代理的 DoH 解析，开发上游答不了，包会被丢（design 7.2、阶段 C 的 c4）。
- 没有一次绑物理网卡的向外拨号；
- 关闸门之后虚拟机里没有新增数据流：`data.total_streams` 的增量等于关闸门前的开流次数。

## 阶段 D（本机独立的 mihomo，M2-3）

`scripts/dev/host-mihomo.sh` 在本机单独起一个 mihomo，用来实测“本机代理软件当主代理时，UDP 中继经 192.168.127.254 能不能用”（设计 4.5）：

```sh
scripts/dev/host-mihomo.sh start <开发上游端口>   # 打印 "listening 127.0.0.1:47893"
scripts/dev/host-mihomo.sh status                # running pid N，加上它监听的地址
scripts/dev/host-mihomo.sh stop                  # 再跑一次也不报错
```

- 版本和 sha256 写死在 `scripts/dev/host-mihomo.json`（darwin-arm64，与镜像里的 mihomo 同版本）；校验不过就删掉下载的文件并退出。
- 配置只有一个出站（开发上游）和 `MATCH,up`，不开 TUN、DNS、external-controller，只听 `127.0.0.1`；启动前自查配置。
- 端口默认 47893（`HOST_MIHOMO_PORT` 可改），被占用就报错退出。
- 只按它自己的 PID 文件停进程，停之前核对可执行文件就是它下载的那一个。**不按名字结束进程**：用户的 Clash 内核也叫 mihomo。
- 文件都在 `scripts/dev/out/host-mihomo/`，不读写 `~/.config/mihomo`。以 root 运行时拒绝。

vm-up 的阶段 D 接在阶段 E 之后（同一台虚拟机，阶段 B 起虚拟机时已把 `127.0.0.1:47893` 放进白名单）：
- d-neg（反例）：本机 mihomo 还没起，TCP、UDP 都失败，开发上游收不到，没有绑物理网卡的向外拨号；
- d：起本机 mihomo，TCP 198.51.100.7:80 经它拿到 204；UDP 198.51.100.7:53 记录是否回显，以及本机直接做 UDP ASSOCIATE 时它回的 BND 地址形态；
- 收尾：停掉本机 mihomo，端口释放、PID 文件删除。
- 脚本开始和结束各采一次用户环境（名字含 mihomo、clash 的进程、`route -n get 1.1.1.1` 的接口、`scutil --proxy`），不一致就判失败。

## abroadd 假系统模式（M2-9a）

```sh
scripts/dev/abroadd-fake.sh
```

编出 `abroadd`，以 `--fake-system` 启动（pf、路由、DNS、IPv6、utun、虚拟机全是内存里的假实现，不碰本机），用 curl 经状态目录里的 `api.sock` 走一遍 启用 → 状态 → 停用 → 再启用 → 恢复 → 状态，最后发 SIGTERM，确认以 0 退出、socket 已删除。证据在 `scripts/dev/out/abroadd-fake-<时间>.txt`。

手动起：`scripts/dev/out/bin/abroadd --fake-system --state-dir <目录>`，另一个终端 `curl --unix-socket <目录>/api.sock http://x/v1/status`。不带 `--fake-system` 时它只打印“真实模式尚未实现”并退出。

## 阶段 9b（abroadd 的数据面接真实虚拟机，M2-9b-2）

vm-up 在阶段 D 之后运行 `abroad-dev daemon-sim --vm-dir <状态目录> --state-dir <短路径> --exit-check-host generate.test --udp-target 198.51.100.7:4500 --log <文件>`：
在进程内启动 abroadd 的 daemon，系统层是假的（FakePF、sysnet.Fake + 命令模拟器），数据面是真的（网络栈、fake-IP DNS、代理、拨号器），utun 是内存管道，管道另一头接 tunsim 的客户端，数据通道连这台虚拟机的 data.sock。经本地接口走 启用 → 停用 → 再启用 → 恢复，各阶段跑 DNS、假地址 TCP、UDP、真实 IP TCP 的探测。不碰宿主机网络。断言：启用后全部通过、停用后全部失败且开流次数不增长、再启用后恢复（同一个假地址）、最后回到 RESTORED；绑物理网卡的拨号 +0；daemon 日志里搜不到探测用的域名、目的地址和假地址。

## M4-1 探查（命名空间、WireGuard、OpenVPN）

`scripts/dev/netns-probe.sh`：构建带 M4-1 软件包的镜像和开发版 abroad-vm（`-tags abroaddev`），起虚拟机（内核命令行带 `abroad.dev=1`），调用 `POST /v1/dev/netns-probe`。探查全部在虚拟机里：建对端和两个出口命名空间，起同机的 WireGuard、OpenVPN 对端，各通一条隧道，加 nft 断网规则，断开隧道后确认只能到达对端端点、53 端口也不通。不出虚拟机，所以本机拒绝绑物理网卡时也能跑；不需要 sudo，不改宿主机网络。报告在 `out/netns-probe-<时间>.json`（不含密钥），证据在 `out/netns-probe-<时间>.txt`。vm-up 的阶段 10a 在 9c 的虚拟机上跑同一个探查。

软件包清单由 `go run ./cmd/abroad-imagebuild pin-packages` 生成（校验索引签名、解依赖、核对每个包，打印新旧差异），见 `guest/manifest.json` 的 `packages`。

## 开发构建与发布构建（M6-1）

开发功能要带 `abroaddev` 构建标签才有；不带标签的默认构建就是发布形态（忘了加标签得到的是安全的那一种）：

| 程序 | 只在开发构建里有 |
|---|---|
| abroadd | `--fake-system`、`--fake-main-proxy`、`--dev-skip-claude-kill`、`--dev-skip-region` |
| abroad-vm | `--outbound-open`、`--dev-agent` |
| abroad-agent（镜像里） | 开发用的多出口 `/v1/dev-exits`、M4-1 的探查、M4-7 的同机对端（整段不编译） |

```sh
go test -tags abroaddev -race -count=1 ./internal/... ./cmd/...   # 开发构建的全量测试（含假系统、开发出口）
go build ./... && go vet ./... && go test ./internal/... ./cmd/... # 默认构建（CLAUDE.md 的命令，不改也能过）
go build -tags abroaddev -o scripts/dev/out/bin/abroadd ./cmd/abroadd    # 真机脚本、第一级走查要传开发参数
CGO_ENABLED=1 go build -tags abroaddev -o scripts/dev/out/bin/abroad-vm-dev ./cmd/abroad-vm
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -tags abroaddev -trimpath -ldflags "-s -w" -o guest/out/abroad-agent ./cmd/abroad-agent
```

用到开发参数的真机脚本在前提检查里核对二进制认识这些参数（`ab_dev_flags_missing`），不认识就在改动任何东西之前退出。vm-up.sh、exits-e2e.sh 等开发脚本自己按需带标签编译（agent 和开放模式的 abroad-vm 用开发构建；`scripts/dev/out/bin/abroad-vm` 仍是正式构建，留给真机脚本）。

## 开机时的 guard（M6-2b）

- `cmd/abroad-guard`：开机最早阶段读状态文件，按状态决定要不要把“只断网、不接管”的规则加载到本项目的 pf 锚点，加载后退出。回国、全新安装不碰 pf；abroadd 本次开机已经加载过时不动；参数文件读不出时加载最严的一组。必须以 root 运行，所以**会话里不运行它**，只跑单元测试（假 pf）。
- `scripts/dev/guard-plist.sh <abroad-guard 路径> <状态目录> [运行时目录]`：生成开发用的 LaunchDaemon 到 `scripts/dev/out/`，用 `plutil -lint` 检查格式。**只生成、不安装**（不调用 launchctl、不需要 sudo）。
- `scripts/dev/pf-syntax.sh` 多检查一组 `lockdown`（最严的一组），只做 `pfctl -n` 语法检查、不加载。

## 手动运行虚拟机

```sh
CGO_ENABLED=1 go build -o scripts/dev/out/bin/abroad-vm ./cmd/abroad-vm   # 必须开 cgo（全局 go env 可能设了 CGO_ENABLED=0）
scripts/dev/sign.sh scripts/dev/out/bin/abroad-vm
scripts/dev/out/bin/abroad-vm run --image-dir guest/out --state-dir scripts/dev/out/state
# 另一个终端：
curl --unix-socket scripts/dev/out/state/ctl.sock http://abroad-agent/v1/health
```

经数据面开一条流（开发工具，只依赖 data.sock）：

```sh
go build -o scripts/dev/out/bin/abroad-dev ./cmd/abroad-dev
scripts/dev/out/bin/abroad-dev probe --data-sock scripts/dev/out/state/data.sock \
  --exit main --net tcp --target 127.0.0.1:1090 --send-hex 050102
# 输出一行 JSON：{"ok":…,"code":…,"handshake_ms":…,"recv_hex":…,"eof":…,"timeout":…}
# 退出码：0 拿到了回应帧；1 连不上或帧层出错；2 参数错。UDP 用 --net udp，--send-hex 是一个数据报。
```

Ctrl-C 优雅停止（先请求虚拟机关机，10 秒不停再强停），再按一次立即强停。

- `ctl.sock` 的客户端不要依赖半关闭（先关写方向再读应答）：Virtualization.framework 的 vsock 连接不支持半关闭，桥接会退化为全关闭。curl 和 Go 的 HTTP 客户端都不会这样做。
- 没挂网卡时 health 的 `ok` 是 `false`，`net.error` 说明没有 eth0，这是预期的。

## 排错

- “已有实例在运行”：同一个状态目录只能跑一个 abroad-vm，先停掉它或换一个 `--state-dir`。
- 启动时报授权相关的错误：重新执行 `sign.sh`；每次重新编译后都要重签。
- health 不通：看 `state/console.log`。boot 阶段的错误以 `abroad-agent:` 开头，之后虚拟机会关机。

## 本机不能经物理网卡到公网时（2026-11-22 起）

有的开发机开着 VPN 或代理软件的“所有流量走隧道”，绑物理网卡访问公网会被本机拒绝（访问局域网照常）。不要为此改网络设置：

- `vm-up.sh` 的前置检查改用**局域网目标**：`VMUP_LAN_TARGET=<局域网 IPv4>:<端口>`，不给时用默认网关上第一个能连上的 TCP 端口（不用 443）。证据里只写目标是怎么选出来的，不写地址。
- 同时试连一次公网：连不上时，经物理网卡到公网的几项（g、4g-i、4g-iii-a、4g-iii-c）按“被本机环境挡住”记录，只核对策略放行、没有向外拨号成功，不算失败。
- 阶段 9c 的直连目标改成同一个局域网目标；daemon-sim 的直连应用连上后先发几个非 TLS 的字节（裸 IP 加 443 而客户端不先发数据时，daemon 会按“看不清去向”改走主代理）。
- `direct-probe.sh --lan`：按 IP 连局域网目标；按域名那一步照样跑，连不上如实记录。
- 直连 socket 的上级目录必须是自己的、权限 0700（abroad-vm 监听前检查），脚本会建好这样的目录。
