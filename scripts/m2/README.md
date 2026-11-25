# M2 真机验证（给用户执行）

这组脚本在**你这台 Mac** 上以真实模式跑 abroadd（Claude 出国的后台服务），逐项验证断网保护、接管、恢复，并采集真机样本。脚本会改本机网络、需要 `sudo`，所以由你执行；开发会话只做过语法检查、`DRY_RUN=1` 空跑和 `--check-only`。

## 先后顺序（重要）

真实模式下 pf 只放行“放行用户”的出站，所以 abroad-vm 必须以专用测试用户 `_abroadm2` 运行。这正是 **R1** 要验证的事（Virtualization.framework 能不能在非登录用户下跑）。所以顺序是：

1. **R1**：`scripts/m0/r1/`，不断网。
2. **R2**：`scripts/m0/r2/`，断网 1–2 分钟。
3. **本脚本**。

R1 不通过时不要跑本脚本，把 R1 的日志贴回来。本脚本的第一个动作就是以 `_abroadm2` 启动 abroad-vm，起不来会立刻停下并还原，说明这是 R1 的结论。

## 准备

- 在这台 Mac 上、用你自己的账号操作（脚本用 `sudo` 提权，并通过 `SUDO_USER` 以你的身份发测试请求）。
- **关掉 Clash 等代理软件的 TUN 模式**（它占着路由，脚本的前提检查会拦下）。代理软件本身可以继续开着。
- **关掉系统代理**（系统设置 → 网络 → 详细信息 → 代理里的网页代理、安全网页代理、SOCKS、自动代理配置、自动代理发现，或在代理软件里关掉“设为系统代理”）。开着时启用会被冲突检查拦下，前提检查会先拦。
- 准备一个**远端或局域网**的主代理节点（不能是本机的 Clash 端口），写成 Clash YAML，例如 `~/m2-nodes.yaml`：
  ```yaml
  proxies:
    - {name: my-node, type: trojan, server: node.example.com, port: 443, password: ..., udp: true}
  ```
- 编出程序（在仓库根目录、普通用户）：
  ```sh
  scripts/dev/vm-up.sh --no-panic                  # 构建镜像、编出并签名 abroad-vm、abroad-dev
  go build -tags abroaddev -o scripts/dev/out/bin/abroadd ./cmd/abroadd   # 开发构建：真机脚本要传 --dev-skip-… 参数（M6-1）
  ```
- 先跑一次前提检查（只读、不需要 sudo）：
  ```sh
  M2_NODES=~/m2-nodes.yaml M2_NODE=my-node scripts/m2/run.sh --check-only
  ```
  看到“前提检查通过”再往下。
- 结束 Claude Code 会话和其他要紧的网络任务（运行期间会断网几次）。

## 运行

```sh
sudo M2_NODES=$HOME/m2-nodes.yaml M2_NODE=my-node scripts/m2/run.sh
```

可选环境变量：
- `M2_EXIT_HOST`：出口检查访问的主机，默认 `www.gstatic.com`，要能返回 `/generate_204`。
- `M2_IPERF_SERVER`：iperf3 服务器，第 7 步测带宽用；没有就跳过。
- `M2_INTERACTIVE=1`：做第 9 步的睡眠唤醒和换 Wi-Fi，脚本会停下来等你操作。**这时请同时把 `M2_DEADMAN_SECS` 调大**（例如 `M2_DEADMAN_SECS=1800`），否则等你操作期间死人开关可能到点自动还原。
- `M2_LONGCONN`：第 8 步外部长连接的目标，默认 `www.baidu.com:80`。
- `M2_DEADMAN_SECS`：死人开关的时限，默认 900 秒。

**总时长**约 5–8 分钟（带 `M2_INTERACTIVE=1` 再加 2 分钟）。**断网时段**：
- 从第 2 步启用到第 6 步恢复，除了经主代理的流量，本机都上不了网（约 3–5 分钟）；
- 第 3 步结束 abroadd 后，连经主代理的也会断，约 10–30 秒，直到 launchd 把它拉起来；
- 第 4 步清空锚点后，保护会失效最多 5 秒。

脚本改系统之前（只有建状态目录在它之前）会先启动**死人开关**：15 分钟后不论脚本处在哪一步，都会自动执行 `restore.sh`；启动后脚本会核对它确实在运行，不在就退出、不做任何改动。另外，脚本中途失败或被 Ctrl-C 中断时，会立刻自动执行一次 `restore.sh`，不用等死人开关。

## 出问题怎么办

- 网络不通、脚本卡住或被中断：`sudo scripts/m2/restore.sh`。它不依赖 abroadd，可以重复执行。
- 仍然不行：**重启电脑**，pf 规则不跨重启保留。**重启后仍要执行一次 `sudo scripts/m2/restore.sh`**：各网络服务的 IPv6 关闭是持久的，重启不会恢复；脚本把 IPv6 的原值另存在重启不丢的 `/Library/Application Support/claude-abroad-m2-snap`，restore.sh 会按它还原，全部还原成功后才删。它也会移除开发用的 LaunchDaemon、删掉测试用户。
- restore.sh 会把 abroadd 的状态目录改名备份（`abroadd.bak-<时间>`），这样下一次运行不会一启动就按旧状态自动接管；前提检查也会在旧状态文件还在时拦下。
- 脚本不会执行 `pfctl -d`，不会按名字结束任何进程，不会碰你的 Clash。

## 步骤和判定表

每一步独立记“通过、不通过、跳过”，一步不通过不影响后面能做的步骤。结果汇总在 `scripts/m2/out/m2-results-<时间>.txt`。

| 步 | 做什么 | 通过的样子 | 不通过时贴回 |
|---|---|---|---|
| 1 | 用 `abroad-dev pf-render` 生成规则，`pfctl -n` 做语法检查；采集启用前的 pf 样本（含 `-E`、`-X`） | `1 通过 pfctl -n 语法检查` | results、samples（“pfctl -n -v -o none”一节） |
| 2 | 经本地接口启用；以你的身份 `curl` 公网 IP、`dig example.com`、`curl -6` | 拿到出口 IP（日志里已脱敏）；dig 得到 198.18/198.19 开头的假地址；IPv6 不通 | results、log、`m2-abroadd-*.log`、samples（启用后各节） |
| 3 | `kill -9` abroadd | 结束后外网不通；60 秒内 launchd 拉起，闸门重新打开 | results、`m2-abroadd-*.log` |
| 4 | `pfctl -a com.apple/250.abroad -F all` 清空锚点 | 6 秒内重新加载；本机回环长连接仍可用 | results、`m2-abroadd-*.log`（巡检事件） |
| 5 | ping 默认网关（路由器） | 可达 | results、samples（`abroad_lan` 表） |
| 6 | 经本地接口恢复；与启用前的快照逐项比对 | 路由、DNS、IPv6、锚点都与启用前一致 | results、log、samples（恢复后各节） |
| 7 | R6 性能：iperf3（可选）、网页加载 3 次、HTTP/3 首包 | 只记录数字，不判通过与否 | `m2-perf-*.txt` |
| 8 | 启用前建一条外部长连接和一条本机回环长连接（`scripts/m2/longconn.py`，以你的身份运行） | 启用前的外部长连接已断开，回环长连接仍可用。验证的是综合效果：启用时清掉旧连接的状态（KillStates），同时路由切到 utun，两者都会让旧连接断开，没法单独验证前者。系统自带的 python3 不可用时这一步记“跳过” | results、log |
| 9 | 睡眠唤醒、换 Wi-Fi（`M2_INTERACTIVE=1`）；`pfctl -s References` 样本 | 唤醒、换网后闸门仍打开（status 里的事件） | results、`m2-abroadd-*.log` |
| 10 | 安装开发用 LaunchDaemon（KeepAlive）启动 abroadd；restore.sh 移除 | abroadd 起来了；restore 后 `launchctl print system/com.claude-abroad.m2.abroadd` 找不到 | results、`m2-abroadd-*.log` |

步骤的执行顺序是 1、10、2、8、4、3、5、7、9、6，最后执行 restore.sh。编号沿用计划里的编号。

## 贴回哪些文件

`scripts/m2/out/` 下这次运行的：
- `m2-<时间>.log`：过程日志；
- `m2-results-<时间>.txt`：结果汇总；
- `m2-samples-<时间>.txt`：真机样本，已脱敏；
- `m2-perf-<时间>.txt`：性能数字；
- `m2-abroadd-<时间>.log`、`m2-abroad-vm-<时间>.log`：两个程序的日志。

另外附上 restore 的日志 `m2-restore-<时间>.log`。

脚本自己会脱敏：本机和局域网的 IPv4、全部 IPv6、MAC 换成占位；出口 IP 只记“拿到/没拿到”。贴之前请再扫一眼，确认没有节点地址和密码。

## 改动类命令清单

### 脚本自己执行的（`scripts/m2/commands.txt`，`check-commands.sh` 据此核对空跑输出）

```
^死人开关 [0-9]+ 秒后执行 .*/scripts/m2/restore\.sh$
^mkdir -p /var/run/claude-abroad-m2(/snap|/vm)?$
^chmod (755|700) /var/run/claude-abroad-m2(/vm)?$
^mkdir -p /Library/Application Support/claude-abroad-m2/bin /Library/Application Support/claude-abroad-m2/image$
^cp .*/scripts/dev/out/bin/(abroad-vm|abroadd|abroad-dev) /Library/Application Support/claude-abroad-m2/bin/(abroad-vm|abroadd|abroad-dev)$
^cp -R guest/out/\. /Library/Application Support/claude-abroad-m2/image/$
^chown -R root:wheel /Library/Application Support/claude-abroad-m2$
^chmod -R go-w /Library/Application Support/claude-abroad-m2$
^dscl \. -(create|delete) /(Users|Groups)/_abroadm2( .*)?$
^dscacheutil -flushcache$
^chown _abroadm2 /var/run/claude-abroad-m2/vm$
^/Library/Application Support/claude-abroad-m2/bin/abroad-dev apply-main-proxy --nodes .+ --selected .+ --(save-store /var/run/claude-abroad-m2/store|ctl-sock /var/run/claude-abroad-m2/vm/ctl\.sock)$
^/sbin/pfctl -E$
^/sbin/pfctl -X (<token[12]?>|[0-9]+)$
^/sbin/pfctl -a com\.apple/250\.abroad -F all$
^sudo -u _abroadm2 /Library/Application Support/claude-abroad-m2/bin/abroad-vm run --image-dir /Library/Application Support/claude-abroad-m2/image --state-dir /var/run/claude-abroad-m2/vm( --(allow-outbound|first-hop) [^ ]+)*$
^cp /var/run/claude-abroad-m2/com\.claude-abroad\.m2\.abroadd\.plist /Library/LaunchDaemons/com\.claude-abroad\.m2\.abroadd\.plist$
^chown root:wheel /Library/LaunchDaemons/com\.claude-abroad\.m2\.abroadd\.plist$
^chmod 644 /Library/LaunchDaemons/com\.claude-abroad\.m2\.abroadd\.plist$
^launchctl (bootstrap|bootout) system /Library/LaunchDaemons/com\.claude-abroad\.m2\.abroadd\.plist$
^rm -f /Library/LaunchDaemons/com\.claude-abroad\.m2\.abroadd\.plist$
^kill( -9| -TERM| -KILL)? (<[^>]+>|[0-9]+)$
^rm -f /var/run/claude-abroad-m2/(deadman_pid|user|[a-z-]+\.pid)$
^route -n delete -net (0\.0\.0\.0/1|128\.0\.0\.0/1)$
^scutil <<remove State:/Network/Service/claude-abroad/DNS$
^networksetup -setv6(automatic|off|LinkLocal) .+$
^rm -rf /Library/Application Support/claude-abroad-m2$
^mkdir -p /Library/Application Support/claude-abroad-m2-snap$
^chmod 700 /Library/Application Support/claude-abroad-m2-snap$
^cp -R /var/run/claude-abroad-m2/snap/\. /Library/Application Support/claude-abroad-m2-snap/$
^rm -rf /Library/Application Support/claude-abroad-m2-snap$
^chmod -R go-rwx /var/run/claude-abroad-m2/store$
^mv /var/run/claude-abroad-m2/abroadd /var/run/claude-abroad-m2/abroadd\.bak-[0-9-]+$
^cp .*/scripts/m2/longconn\.py /var/run/claude-abroad-m2/longconn\.py$
```

不需要 root 的自测：`scripts/m2/selftest.sh`（死人开关、按 PID 结束进程、前提检查、中途退出自动还原，都在临时目录里模拟）。空跑：`DRY_RUN=1 scripts/m2/run.sh`、`DRY_RUN=1 scripts/m2/restore.sh`，再用 `scripts/m2/check-commands.sh` 核对。

另外有只读查询，会执行但不改任何东西：`netstat -rn -f inet`、`networksetup -listallnetworkservices/-getinfo`、`route -n get`、`scutil --dns/--proxy`、scutil 的 `show`、`pfctl -s info/-s rules/-s References`、`pfctl -a <锚点> -s rules/-t abroad_lan -T show`、`pfctl -n -f`、`dscl . -read/-search`、`launchctl print`。

### abroadd 运行时执行的（`internal/daemon/testdata/commands.golden`，开发测试里有命令审计守着，出现新形态测试会失败）

```
/bin/ps -axo comm
/sbin/ifconfig utun<N> inet 198.18.0.1 198.18.0.1 mtu <MTU> up
/sbin/pfctl -E
/sbin/pfctl -F states
/sbin/pfctl -X <数字>
/sbin/pfctl -a com.apple/250.abroad -F all
/sbin/pfctl -a com.apple/250.abroad -s rules
/sbin/pfctl -a com.apple/250.abroad -t abroad_lan -T show
/sbin/pfctl -o none -a com.apple/250.abroad -f -
/sbin/pfctl -s info
/sbin/pfctl -s rules
/sbin/route -n add -net 0.0.0.0/1 -interface utun<N>
/sbin/route -n add -net 128.0.0.0/1 -interface utun<N>
/sbin/route -n change -net 0.0.0.0/1 -interface utun<N>
/sbin/route -n delete -net 0.0.0.0/1
/sbin/route -n delete -net 128.0.0.0/1
/sbin/route -n get 0.0.0.1
/sbin/route -n get 1.1.1.1
/sbin/route -n get 128.0.0.1
/sbin/route -n get 129.0.0.1
/sbin/route -n get 198.18.0.2
/sbin/route -n get default
/usr/sbin/netstat -rn -f inet
/usr/sbin/networksetup -getinfo <服务>
/usr/sbin/networksetup -listallnetworkservices
/usr/sbin/networksetup -setv6LinkLocal <服务>
/usr/sbin/networksetup -setv6automatic <服务>
/usr/sbin/networksetup -setv6manual <服务> <地址> <前缀> <路由器>
/usr/sbin/networksetup -setv6off <服务>
/usr/sbin/scutil --nwi
/usr/sbin/scutil --proxy
/usr/sbin/scutil <<d.init; d.add ServerAddresses * 198.18.0.2; d.add SupplementalMatchDomains * ""; set State:/Network/Service/claude-abroad/DNS; quit
/usr/sbin/scutil <<remove State:/Network/Service/claude-abroad/DNS; quit
/usr/sbin/scutil <<show State:/Network/Service/claude-abroad/DNS; quit
```

## 采样清单对照（开发进度单“带进 M2-9”的每一条）

| 采样项 | 脚本里的位置 | 输出文件里的标题 |
|---|---|---|
| `networksetup -getinfo` 在 IPv6 为 Off、Manual、Link-local 时的输出 | 第 1 步前（启用前，原样）、第 2 步后（启用后，应为 Off） | “networksetup -getinfo（启用前）”“（启用后…）”。Manual、Link-local 要你的服务本来就是这种模式才能采到；前提检查拒绝 Manual，所以 Manual 只能在 M3 另行采 |
| `-setv6manual` 不给路由器时的行为 | 不采：会改你的网络设置，而且前提检查拒绝 Manual | — |
| `route -n get` 经物理网卡、`not in table` 时的输出 | 第 1 步前（经物理网卡）；`not in table` 有默认路由时采不到 | “route -n get 1.1.1.1（启用前，经物理网卡）” |
| `route -n add` 已存在时 stderr 的原文 | abroadd 的监护在路由被改时会遇到，记在 `m2-abroadd-*.log`；第 4 步之外不刻意制造 | `m2-abroadd-*.log` |
| scutil 写入我们的键之后 `show` 的输出；系统是否把它当首选解析器 | 第 2 步后 | “scutil show 我们的 DNS 键”“scutil --dns（我们的解析器是否首选）”；第 2 步的 dig 结果 |
| networksetup 出错时的退出码和输出位置 | 第 1 步前 | “networksetup 出错时（不存在的服务）” |
| `pfctl -X <已释放的 token>` 的输出 | 第 1 步 | “pfctl -X 已释放的 token” |
| `pfctl -a <锚点> -sr` | 第 1 步（启用前，空）、第 2 步后（实际加载的打印） | “pfctl -a 锚点 -s rules（…）” |
| `pfctl -sr`（主规则集） | 第 1 步 | “pfctl -s rules（主规则集）” |
| `pfctl -s info` 的首行 | 第 1 步前后、第 2 步后、第 6 步后 | “pfctl -s info 首行（…）” |
| `pfctl -E` 在已启用和未启用两种情况下的输出 | 第 1 步 | “pfctl -E（第一次）”“pfctl -E（已启用时再来一次）” |
| `pfctl -a <锚点> -t abroad_lan -T show` 在表存在和不存在时的输出 | 第 1 步（不存在）、第 2 步后（存在） | “…（表不存在）”“…（表存在）” |
| 启用前建本机回环 TCP 长连接，启用后、清锚点重载后仍能收发 | 第 8 步、第 4 步 | results 的 8、4 |
| root 下 `-o none -a <锚点> -f -` 实际加载后 `-a <锚点> -sr` 的输出，对比 `-nvf -o none` 的样本 | 第 1 步（`-n -v -o none` 样本）、第 2 步后（实际加载的打印） | 两节对比 |
| R6：`udp_rejected`（DNS 的 UDP 流占用配额） | **这次采不到**：abroadd 的 status 里还没有网络栈的计数，要等计数接进 status（M3）后再采 | — |

## 时区和语言（M4-5 起）

这组脚本**不会改你的时区和语言**：abroadd 的 LaunchDaemon 带 `--dev-skip-region`，不同步地区。启用之前脚本会先“应用主代理”（经主代理跑一轮出口检查，建立出口基准；M4-3 起没有基准不能启用），这一步不涉及地区，不需要确认。地区的切换和还原由 M4 的真机脚本单独验证。
