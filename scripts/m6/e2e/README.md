# scripts/m6/e2e：开发安装的端到端（M6-9，真机）

从开发安装到卸载走一遍完整流程，中间带着“启用”状态重启一次电脑，看开机时的 guard 能不能在 abroadd 接管之前断网。
分两段，结束时卸载开发安装，并和安装前的快照、开始前的地区逐项对照。

> **会断网、会改时区和语言。** 启用期间本机所有网络都走主代理；重启后到 abroadd 接管之间（一般一两分钟）本机没有网络。
> 时区和语言会改成出口所在地，回国时改回。Claude Code 会话会断开：请在不需要它的时候执行。
>
> **默认开发构建，不碰真实的 Claude。** 不结束任何 Claude 进程（回国只预览），解析锁只锁测试域名
> `lock-test.claude-abroad.invalid`，不写 Claude Code 的托管设置。对真实域名和真实程序的验证（R9、R18）
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
export M6E_NODES=/path/to/nodes.yaml M6E_M5_PASSED=1
scripts/m6/e2e/run.sh --check-only
```

前提：M5 第二级走查已经通过；没有别的代理软件的 TUN、VPN 和系统代理；开发安装的前提（见 scripts/m6/install）。

## 第一段

```sh
sudo -E scripts/m6/e2e/run.sh
```

1. 拍地区快照，启动死人开关；
2. 开发安装（scripts/m6/install/install.sh），核对首次准备、完整性校验、解析锁（测试域名）——I1；
3. 以你自己的身份打开应用 `open /Applications/ClaudeAbroad.app`，按提示在应用里操作，每步做完按回车，脚本经接口核对：
   - P1 安装引导：添加来源（本地配置文件选 `M6E_NODES`）→ 选节点 → 应用主代理 → 完成；
   - P2 启用；
   - P3 一键回国：本机网络恢复（直连探测 204）、解析锁仍在；
   - P4 再启用；
4. 取消死人开关，请你**重启电脑**。

## 第二段（重启后，先不要打开应用）

```sh
sudo -E scripts/m6/e2e/run.sh --after-reboot
```

- B1 开机时的 guard：日志 `/var/log/claude-abroad/guard.log` 里这次开机加载了断网规则、完整性校验通过；
- B2 abroadd 接管后恢复启用（最多等 3 分钟）；
- P5 打开应用，一键回国；
- U1 卸载（提示时输入 `uninstall`）：和安装前的快照逐项一致；
- R1 地区和开始前一致；最后执行 restore.sh 收尾。

结果在 `scripts/m6/e2e/out/m6e-results.txt`。请把它和 `scripts/m6/e2e/out/`、`scripts/m6/install/out/` 下的日志贴回。

## 会话里能做的

```sh
DRY_RUN=1 scripts/m6/e2e/run.sh >/tmp/e1; DRY_RUN=1 scripts/m6/e2e/run.sh --after-reboot >/tmp/e2
scripts/m6/e2e/check-commands.sh /tmp/e1 /tmp/e2
scripts/m6/e2e/selftest.sh
```

## 没有覆盖的

- 第三层（回国后发现 Claude 启动就结束）：开发构建带 `--dev-skip-claude-kill`，第三层不接上，这里只核对第一层的锁。
  真实效果要用发布构建，会结束本机真实的 Claude 进程，是单独的一步（以后补，要输入确认）。
- 第四层（Claude Code 的托管设置）：开发构建只锁测试域名时不写托管设置。
