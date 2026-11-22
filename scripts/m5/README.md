# scripts/m5：M5 的走查（M5-11）

走查分两级，清单和完成标准对照表在 `docs/m5-walkthrough.md`。

| 文件 | 做什么 |
|---|---|
| `walk-fake.sh` | 第一级：启动假系统模式的 abroadd（`--fake-system --fake-main-proxy`）并打开打包好的应用；应用开着时按 `d` 只停假后台、`u` 用同一个 socket 路径再起来、`q` 全部停掉；`--app-first` 先开应用再起后台；退出、中断都会全部停掉、删临时目录。`--check` 起后台、只停、同一路径再起来、读状态，不打开应用。不需要管理员权限，不改本机网络 |
| `run.sh` | 第二级（真机）：以真实模式跑 abroadd，本地接口在 `/var/run/claude-abroad`，用户在应用里从安装引导配好主代理到启用、停用、一键回国，脚本在 P1–P6 检查点经接口核对。会改本机网络和时区、需要 sudo、会话会断开 |
| `restore.sh` | 第二级的还原：不依赖 abroadd，按脚本自己的快照还原网络和地区，删共享 socket 目录和命令行链接（只删本脚本建的） |
| `lib.sh`、`launchd.plist.tmpl` | 公共变量和 abroadd 的开发用 LaunchDaemon 模板 |
| `commands.txt`、`check-commands.sh` | 改动类命令清单和空跑输出的核对 |
| `check-criteria.sh` | 核对对照表里引用的自动测试真实存在、每条完成标准都有着落 |
| `selftest.sh` | 自测（不用 root、不改网络和地区） |
| `probe/` | M5-1 的探查（E1、E2 的 `run-e1e2.sh` 要用户执行） |

## 第二级的保护措施（与 M4 真机脚本逐项对照）

| M4 的保护 | M5 第二级 |
|---|---|
| 死人开关：改系统之前先启动，到点执行 restore.sh | 同（`M5_DEADMAN_SECS`，默认 2700 秒）；死人开关之前只有建状态目录（命令核对固定）。人工走查超过 45 分钟会自动恢复（安全的方向）；需要更久时用 `M5_DEADMAN_SECS` 调长 |
| 出错、Ctrl+C、非零退出时自动执行 restore.sh | 同（`m5_arm_restore_trap`） |
| 单独的 restore.sh，不依赖 abroadd，可以重复执行 | 同；顺序：停服务 → 清网络 → 还原地区 → 清 pf → 删 socket 目录和命令行链接 → 删用户 → 取消死人开关 → 删安装的程序 |
| 快照（网络、地区）在安装之前，另存一份重启不丢 | 同（`/Library/Application Support/claude-abroad-m5-snap`）；地区快照复用 `scripts/m4/region.sh` |
| `DRY_RUN=1` 空跑只打印改动命令；`check-commands.sh` 按清单核对，禁止项拦下 | 同；另禁止 `open`（脚本不替用户打开应用） |
| `--check-only` 只读前提检查 | 同；另要求 `M5_M2_PASSED`、`M5_M3_PASSED`、`M5_M4_PASSED` |
| 只按 PID 文件结束进程（核对命令行），不按名字结束，不执行 `pfctl -d`，不碰用户的 Clash | 同 |
| 默认不结束 Claude 进程（`--dev-skip-claude-kill`） | 同（`M5_REAL_KILL=1` 才真结束） |
| 专用测试用户、状态目录在 `/var/run` | 同（`_abroadm5`、`/var/run/claude-abroad-m5`） |
| 自测 | 同（`selftest.sh`：死人开关、按 PID 结束、空跑环境不变、命令核对和反例、前提拦截、源码检查、第一级空跑无残留、对照表核对） |

## 改动类命令清单

见 `commands.txt`（与 `check-commands.sh` 核对的是同一份）。第二级比 M4 多三类：建和删共享 socket 目录 `/var/run/claude-abroad`；在 `/usr/local/bin/abroad` 放和删指向测试安装的符号链接（原来没有时才放）；abroadd 的 plist 多了 `--socket-dir`。不预先推主代理节点（主代理在应用里配，由 abroadd 下发）。
