# scripts/m6/e2e：开发安装的端到端（M6-9，真机）

从开发安装到卸载走一遍完整流程，中间带着“启用”状态重启一次电脑，看开机时的 guard 能不能在 abroadd 接管之前断网。
分三段（M6-11b 起：已回国状态下再重启一次，看开机时的 guard 加载回国拦截），结束时卸载开发安装，并和安装前的快照、开始前的地区逐项对照。

> **会断网、会改时区和语言。** 启用期间本机所有网络都走主代理；重启后到 abroadd 接管之间（一般一两分钟）本机没有网络。
> 时区和语言会改成出口所在地，回国时改回。Claude Code 会话会断开：请在不需要它的时候执行。
>
> **默认开发构建，不碰真实的 Claude。** 不结束任何 Claude 进程（回国只预览），解析锁只锁测试域名
> `lock-test.claude-abroad.invalid`，不写 Claude Code 的托管设置，回国拦截只拦测试段 `192.0.2.0/24`、`2001:db8:abd::/48`
> （文档保留地址，没有真实服务；装上即生效，到卸载为止）。对真实域名和真实程序的验证（R9、R18）
> 用 [scripts/m6/resolver](../resolver/README.md)。

## 应急

- 任何时候都可以执行 `sudo scripts/m6/e2e/restore.sh`（不需要网络）：卸载开发安装（先试 `abroad uninstall`，不行就按
  [docs/emergency.md](../../../docs/emergency.md) 兜底），按开始前的快照还原时区和语言。可以重复执行。
- 死人开关：每一段开始时启动，默认 3600 秒后自动执行 restore.sh。第一段结束、请你重启之前会取消（重启本身也会结束它），
  第二段开头重新启动。
- 重启后一直没有网络：在终端执行上面的 restore.sh。还不行就按 docs/emergency.md 手动恢复。

## 准备（以你自己的身份，不要 sudo）

```sh
scripts/dev/integrity-build.sh --dev
scripts/dev/app-bundle.sh
export M6E_NODES=/path/to/nodes.yaml
scripts/m6/e2e/run.sh --check-only
```

前提：没有别的代理软件的 TUN、VPN 和系统代理；开发安装的前提（见 scripts/m6/install）。

- **M5 第二级走查、R1、R2、M2 没做只提醒、不拒绝**（用户 2026-11-27 决定暂不测）。R1、R2、M2 没做时会打印警告：
  **第一次启用没有自动恢复**——启用后如果断网回不来，用下面任一办法恢复：① 应用里“一键回国”；② `sudo abroad restore`
  （命令行在 `/Library/Application Support/claude-abroad/bin/abroad`）；③ `sudo abroad-rescue`（后台服务不在或状态文件损坏时，
  先按 docs/emergency.md 停掉后台服务）。做过之后可以设 `M6E_R1R2M2_DONE=1`、`M6E_M5_PASSED=1` 关掉提醒。

## 第一段

```sh
sudo -E scripts/m6/e2e/run.sh
```

1. 拍地区快照，启动死人开关；
2. 开发安装（scripts/m6/install/install.sh），核对首次准备、完整性校验、解析锁（测试域名）——I1；
3. 以你自己的身份打开应用 `open /Applications/ClaudeAbroad.app`，按提示在应用里操作，每步做完按回车，脚本经接口核对：
   - P1 安装引导：添加来源（本地配置文件选 `M6E_NODES`）→ 选节点 → 应用主代理 → 完成；
   - P2 启用；
   - CC1 本机代理端口认出对端进程（R23；2026-11-27 从第四层实测挪来）：以你自己的身份用 curl 经 `127.0.0.1:47821` 发一次请求，状态里“最近的连接”应认出这个程序（不是 unknown、不按 Claude 处理）。第四道默认关（v0.68）：这一步之前脚本执行 `abroad --yes claude-proxy on`、做完执行 `abroad --yes claude-proxy off`（开发安装只管测试目标，打开只监听、不写 Claude Code 的托管设置），只有这一个显式带代理参数的 curl 经过端口；判断用的字段另存在 `out/m6e-cc1-*.txt`（不含目的域名）；
   - LAN1 允许局域网（补齐三，设了 `M6E_LAN_PROBE=<局域网设备的 地址:端口>` 时，如打印机 `192.168.1.20:631`）：
     `abroad lan off` 后连不上这台设备，`abroad lan on` 后恢复；没设时跳过；
   - P3 一键回国：本机网络恢复（直连探测 204）、解析锁仍在；
   - （开发安装之前先不带凭据地连一次 `192.0.2.1:443` 记为基线：本来就立即失败的话，下面这一项记“无法区分”，以锚点和状态为准）
   - RB1 回国拦截：状态里生效且是测试段、本项目的 pf 锚点里恰好是测试段的两条（只读）、不带凭据地连 `192.0.2.1:443`
     几秒内就被拒（没有拦截时这个地址不可达，会一直等到超时）；
   - K1 定向清理：`pfctl -k 0.0.0.0/0 -k 192.0.2.0/24`、`pfctl -k ::/0 -k 2001:db8:abd::/48`，只清目的地在测试段里的连接状态，
     看 macOS 接不接受这个写法（abroadd 的 `--return-block-kill-states` 默认关，等这一步的结论）；
   - L1 第三层演练：在开发安装的测试目录 `fake-claude` 里以你的身份启动一个测试进程（`/bin/sleep` 的副本），应在 6 秒内被结束（开发构建的第三层只针对这个目录，本机真实的 Claude 不受影响）；
   - P4 再启用；
4. 取消死人开关，请你**重启电脑**。

## 第二段（重启后，先不要打开应用）

```sh
sudo -E scripts/m6/e2e/run.sh --after-reboot
```

- B1 开机时的 guard：日志 `/var/log/claude-abroad/guard.log` 里这次开机加载了断网规则、完整性校验通过；
- B2 abroadd 接管后恢复启用（最多等 3 分钟）；
- 应急演练（E1–E3）：停掉后台服务（这时仍是断网的），执行应急程序 `abroad-rescue`（看过清单后输入 `rescue`），核对状态是已回国、本机网络恢复；再把后台服务注册回来，核对它不会重新接管；
- RB2 应急程序之后回国拦截（测试段）仍在（应急程序第 4 步换成这一组、不是清空）；
- P5 打开应用，确认显示“已回国”；
- 取消死人开关，请你**再重启一次电脑**（这次是已回国状态，开机后不会断网）。

## 第三段（已回国状态下重启后）

```sh
sudo -E scripts/m6/e2e/run.sh --after-reboot-2
```

- B3 开机时的 guard：日志里这次开机加载了回国拦截；
- RB3 abroadd 起来后回国拦截（测试段）在生效，同 RB1；
- U1 卸载（提示时输入 `uninstall`）：和安装前的快照逐项一致（含 pf 锚点为空、pf 是否启用和安装前一样）；
- R1 地区和开始前一致；最后执行 restore.sh 收尾。

结果在 `scripts/m6/e2e/out/m6e-results.txt`。请把它和 `scripts/m6/e2e/out/`、`scripts/m6/install/out/` 下的日志贴回。

## 会话里能做的

```sh
DRY_RUN=1 scripts/m6/e2e/run.sh >/tmp/e1; DRY_RUN=1 scripts/m6/e2e/run.sh --after-reboot >/tmp/e2
DRY_RUN=1 scripts/m6/e2e/run.sh --after-reboot-2 >/tmp/e3
scripts/m6/e2e/check-commands.sh /tmp/e1 /tmp/e2 /tmp/e3
scripts/m6/e2e/selftest.sh
```

## 没有覆盖的

- 第三层对真实 Claude 的效果：开发构建的第三层只针对测试目录里的进程（L1 验证机制本身）；对真实 Claude 的识别（签名）
  要用发布构建，会结束本机真实的 Claude 进程，是单独的一步（以后补，要输入确认）。
- 第四层（Claude Code 的托管设置）：开发构建只锁测试域名时不写托管设置。
- 回国拦截对真实地址段的效果：开发构建只拦测试段。**用发布构建验证那一节（以后补，要醒目说明、输入确认）的后果**：发布
  构建装上的那一刻起，这台 Mac 直连 Anthropic 地址段的流量全部被拒；这条规则不分网卡，经 VPN 隧道出去、目的地仍是这些
  地址的包同样被拒，所以正在运行的 Claude Code 会话会断开，直到卸载。
