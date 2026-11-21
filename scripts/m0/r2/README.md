# M0 R2：pf 的 user 规则与"默认拦截所有接口"

验证设计文档 6.1 / 6.3：以 `_abroad` 身份（这里用临时用户 `_abroadm0`）绑定物理网卡发出的连接能否被 `pass … user` 放行；整套"默认拦截"规则会不会把系统搞坏。计划见 `docs/m0-plan.md` 第 1 节。

## 脚本会做什么

| 改动 | 撤销方式 |
|---|---|
| 建临时隐藏用户和组 `_abroadm0`（UID 400–499 内空闲的一个，无 shell、无 home） | restore.sh 删除 |
| 复制探测程序到 `/Library/Application Support/claude-abroad-m0/` | restore.sh 删除 |
| 状态记到 `/var/run/claude-abroad-m0/` | restore.sh 删除；重启也会清掉 |
| `pfctl -E` 取启用引用，往 `com.apple/250.abroadm0`、`com.apple/000.abroadm0order`、`com.apple/999.abroadm0order` 加载临时规则 | restore.sh 清空锚点、`pfctl -X` 释放引用；pf 规则不跨重启 |

- 规则生效期间 **Mac 断网约 1–2 分钟**：Claude Code 会话、SSH、下载、通话、云同步都会中断；`bridge100` 上的虚拟机流量也受影响。
- 改系统之前先启动**死人开关**（独立的后台进程，默认 360 秒后自动执行 restore.sh）。脚本被中断、终端被关掉，到点也会自动还原。
- 任何时候都可以手动执行 `sudo scripts/m0/r2/restore.sh`，可重复执行。
- 万一全部失效：重启电脑，pf 规则自动消失；然后再执行一次 restore.sh 删掉临时用户。

## 怎么跑

1. **关掉 Clash/Mihomo 的 TUN 模式**（脚本检测到 `0/1`、`128.0/1` 接管路由会拒绝运行）。系统代理可以不关。
2. 结束这次 Claude Code 会话（反正会断网），在终端里进入仓库根目录：

   ```sh
   go build -o scripts/m0/r2/out/m0-r2-probe ./cmd/m0-r2-probe
   scripts/m0/r2/run.sh --dry-run          # 可选：只打印将执行的命令，不改任何东西
   sudo scripts/m0/r2/run.sh               # 正式运行，约 3 分钟
   ```

3. 想再测"锚点排在最前"的写法：`sudo scripts/m0/r2/run.sh --anchor 100.abroadm0`。
4. 可选的 DHCP 用例（只在网络是"自动获取 IP"时有意义，会触发一次续租）：加 `--dhcp`。
5. 跑完重新打开 TUN，把 `scripts/m0/r2/out/r2-<时间>.log` 贴回来（日志里只有内网网关和网段，没有出口 IP）。

## 要看什么

日志里每个用例一行：

```
CASE phase=rules    tcp_test_bound   who=test | RESULT ok=true kind=ok detail=TLS 握手完成 | pf=m0_user:+12
```

- `phase=baseline` 是没加规则时的基线，`phase=rules` 是加了规则之后。基线就不通的用例（比如本机没有 IPv6），规则阶段的结果没有意义。
- `pf=` 是这个用例期间各条规则的包计数变化，**判定以它为准**：

| 用例 | 期望（R2 成立时） |
|---|---|
| `tcp_test_bound`、`udp_test_bound`、`udpsendto_test_bound` | 通，计数落在 `m0_user` |
| `tcp_user`、`udp_user`、`icmp_user` | 不通，计数落在 `m0_block_all` |
| `tcp_root_bound`、`udp_root_bound` | 不通，`m0_block_all`（root 不享受放行） |
| `fd_tcp_test2root`、`fd_udp_test2root_*` | 通，`m0_user`（测试用户建的 socket 交给 root 用，pf 认创建者） |
| `fd_udp_root2test_*` | 观察项：pf 认创建者（root → 不通）还是使用者 |
| `lan_gateway_*` | 通，`m0_lan`（`refused` 也算通） |
| `loop_v4` | 通 |
| `loop_v6` | 观察项：设计 6.1 里 `block inet6` 排在 lo0 放行之前，这里不通就说明 `::1` 被拦了 |
| `tcp6_user` | 不通，`m0_block_inet6` |
| `order_A_pass_B_block` / `order_A_block_B_pass` | 分别通 / 不通，且计数都落在 A(000) 上：说明锚点按名字排序、第一条 quick 生效 |
