# R1：虚拟机能否在系统级后台服务里以非登录用户运行

设计文档第 15 节 R1、第 3.1 节。正式版里 abroad-vm 由 launchd 以专用系统用户 `_abroad` 运行。这个测试回答：Virtualization.framework 在这种身份下能不能起虚拟机。

- **不断网**：不改 pf、路由、网卡、DNS，虚拟机也不挂网卡。可以开着 Clash/Mihomo 的 TUN 跑。
- **约 3–5 分钟**（最坏约 10 分钟），需要 sudo。
- SMAppService 注册的服务是否表现相同属于 R8，这次不测（没有付费开发者账号）。

## 会做的系统改动（结束时全部撤销）

- 建一个临时用户 `_abroadm0`：没有 shell、没有 home，登录界面不显示。只有在加跑第三个用例时，才把它的 home 指到测试目录；
- 把 abroad-vm 和镜像复制到 `/Library/Application Support/claude-abroad-m0/r1/`；
- 用 `launchctl bootstrap system` 加载临时服务：`com.claude-abroad.m0.r1-user`（以 `_abroadm0` 运行）、`com.claude-abroad.m0.r1-root`（以 root 运行，作对照）。必要时再加载 `com.claude-abroad.m0.r1-userhome`。plist 放在上面的目录里，不放 `/Library/LaunchDaemons`，所以重启后不会自动加载。

用例**依次**运行，各自只跑一台虚拟机，验证完就卸载。最坏总时长约 600 秒，小于死人开关的 15 分钟。脚本结束（包括失败、Ctrl-C）时自动执行 `restore.sh`。另外有一个 15 分钟的死人开关：脚本被强行中断时，它到点也会还原。

## 步骤

在这台 Mac 的仓库根目录执行：

```sh
# 1. 以普通身份（不要 sudo）构建镜像和 abroad-vm 并签名，顺便跑一遍阶段 A（约 1–2 分钟）
scripts/dev/vm-up.sh --no-panic

# 2. 运行 R1
sudo scripts/m0/r1/run.sh
```

把 `scripts/m0/r1/out/r1-<时间>.log` 贴回给开发会话。

## 看什么

日志最后三行是各用例的结果。每个用例还记录了以下内容：

- health 应答和用时；
- abroad-vm 和 `com.apple.Virtualization.VirtualMachine` 进程各以哪个用户运行；
- `launchctl print` 的状态和 last exit code；
- abroad-vm 的日志、虚拟机控制台的末尾；
- bootout 后进程是否按时退出。

每个用例的结果是三种之一：**通过**、**不通过**、**未能执行**。“未能执行”是脚本或 launchd 层面的问题，比如 plist、bootstrap 出错，不能用来下结论。

`_abroadm0` 用例不通过时，脚本会自动加跑第三个用例“`_abroadm0` + 真实 home”：同一个用户，但给它一个可写的 home 目录。

| `_abroadm0` | root 对照 | `_abroadm0` + home | 结论 |
|---|---|---|---|
| 通过 | 通过 | 不跑 | R1 成立：设计文档 3.1 不变，abroad-vm 以 `_abroad` 运行 |
| 不通过 | 通过 | 通过 | 3.1 不变，但 `_abroad` 需要真实的 home 目录（回写设计 3.1、11.4） |
| 不通过 | 通过 | 不通过 | 走第 15 节 R1 的备选：root 运行虚拟机宿主，出站网络进程单独以 `_abroad` 运行，两者之间传网卡 fd（vmhost 已按这个边界拆分） |
| 不通过 | 不通过 | 不通过 | 系统级后台服务本身起不了虚拟机：把日志里的错误原文贴回来，再评估 LaunchAgent 等方案 |
| 通过 | 不通过 | 不跑 | 少见，把日志贴回来分析 |
| 任一项是“未能执行” | | | 不下结论，把日志贴回来 |

用例不通过时，日志里还会附上最近 3 分钟的相关系统日志（`log show`，只读），供分析原因。

## 出问题时

- 脚本中途卡住：Ctrl-C，脚本会自动还原。
- 想确认已经清理干净，或脚本被强行杀掉：再执行一次 `sudo scripts/m0/r1/restore.sh`（可以重复执行）。
- 提示“发现 R2 的残留状态”：执行 `sudo scripts/m0/r2/restore.sh`。R1 的还原脚本遇到 R2 的残留不会做任何事，以免打断 R2 的自动恢复。
- 提示“发现上次残留的状态”或“测试用户已存在”：执行 `sudo scripts/m0/r1/restore.sh`（R2 正常跑完留下的记录也由它清掉）。即使中途重启过电脑，它也能按特征删掉临时用户。
- 还原后可以用下面两条命令确认：
  - `launchctl print system/com.claude-abroad.m0.r1-user`：应报找不到；
  - `dscl . -read /Users/_abroadm0`：应报不存在。
