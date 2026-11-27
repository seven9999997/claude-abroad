# scripts/m6/install：开发安装和卸载（M6-8b）

没有付费开发者账号时的安装方式（设计 11.4 的 M6-8a 段）：用脚本把程序装到固定位置、注册两个开发用 LaunchDaemon，
代替以后的 `SMAppService` 注册。M6-9 的端到端也从这里开始。

> **安装不会断网，也不会结束任何程序。** 装完是“已回国”状态：不加载断网规则、不接管网络。
> **装上即生效（M6-11b）**：abroadd 第一次启动做完首次准备后，马上加载“回国拦截”并启用 pf（之前没启用时）。开发安装
> 只管测试目标，拦的是测试段 `192.0.2.0/24`、`2001:db8:abd::/48`（文档保留地址，没有真实服务），本机真实的 Claude 和
> 这台 Mac 上别的连接不受影响；pf 从此保持启用到卸载为止（卸载后按快照复原）。
> 默认是开发构建：abroadd 带 `--launch-control`（打开启动管控）、`--dev-fake-claude-dir`（回国、卸载和第三层只针对
> 安装目录下 `fake-claude` 里的测试进程，本机真实的 Claude 不受影响）和
> `--dev-resolver-test-domain`（首次准备只锁测试域名 `lock-test.claude-abroad.invalid`；第四道默认关，打开（`sudo abroad claude-proxy on`）也只监听本机代理端口、不写 Claude Code 的托管设置），
> 本机真实的 Claude、Claude Code 不受影响。**启用**（应用里点“启用”或 `abroad up`）才会接管网络，那是 M6-9 的步骤。

## 准备（以你自己的身份，不要 sudo）

```sh
scripts/dev/vm-up.sh --no-panic          # 第一次：构建虚拟机镜像到 guest/out
scripts/dev/integrity-build.sh --dev     # 开发构建 + 完整性清单 + 版本号（build.json）→ scripts/dev/out/integrity/
scripts/dev/payload.sh                   # 组装 payload（程序、镜像、清单、版本号、安装脚本、逐文件摘要）→ scripts/dev/out/payload/
scripts/dev/app-bundle.sh                # 应用包 → app/.build/bundle/ClaudeAbroad.app
scripts/m6/install/install.sh --check-only   # 前提检查（只读）
```

## 安装

```sh
sudo scripts/m6/install/install.sh
```

按顺序做（任何一步失败都会自动执行卸载，回到安装前）：

1. 拍快照到 `/Library/Application Support/claude-abroad-install-snap`（安装位置、本地接口目录、日志目录、两个 plist、应用、
   命令行链接、`/etc/resolver`、Claude Code 的托管设置目录各自是否存在和内容摘要；专用用户、两个服务是否注册、
   pf 锚点里的规则数、DNS 键、各网络服务的 IPv6）。卸载后再拍一份，逐项对照。
2. 程序、镜像和完整性清单装到 `/Library/Application Support/claude-abroad/`（root 所有，组和其他人不能写）。
   其中 `bin/abroad` 就是应急文档里说的另存的命令行，`bin/abroad-rescue` 是应急程序（后台服务不在时恢复网络）。
3. 建专用用户 `_abroad`（无 shell、无主目录、隐藏），虚拟机以它运行，pf 只放行它的出站。
4. 状态目录 `…/claude-abroad/state/`（abroadd、虚拟机、主代理存储），日志目录 `/var/log/claude-abroad/`，
   本地接口目录 `/var/run/claude-abroad/`（开机时被清空，abroadd 和 guard 会自己重建）。
5. 应用复制到 `/Applications/ClaudeAbroad.app`，命令行链接 `/usr/local/bin/abroad`；已经有同名的都不覆盖。
6. 注册 `com.claude-abroad.dev.guard`（开机时运行一次，按状态文件加载断网规则）和 `com.claude-abroad.dev.abroadd`（常驻）。
   abroadd 由它自己拉起和管理虚拟机，第一次启动时做首次准备（写解析锁）。
7. 等 abroadd 起来，核对状态应为“已回国”、首次准备完成、完整性校验通过；回国拦截已生效且是测试段，锚点里恰好是这两条
   （只读核对）：

   ```
   block return out quick inet from any to 192.0.2.0/24
   block return out quick inet6 from any to 2001:db8:abd::/48
   ```

装好后以你自己的身份打开应用：`open /Applications/ClaudeAbroad.app`。

## 原地升级（v0.71）

```sh
sudo scripts/m6/install/install.sh --upgrade            # 或者直接 scripts/dev/reinstall.sh（装过时就是升级）
scripts/m6/install/install.sh --check-only --upgrade    # 只看能不能升级（只读）
```

升级 = 运行 `scripts/dev/reinstall.sh`（会同时更新后台和应用）；或者运行 `scripts/dev/build-app.sh`，然后在应用里点“升级后台服务”。只运行 `install.sh --upgrade` 不会更新“应用程序”里的应用（应用交接给你之后它归你管、不在安装记录里）。

只换程序、镜像、完整性清单、版本号（`build.json`）、安装目录里的 `scripts/`（卸载脚本，给应用里的卸载用）和两份 plist
（内容变了才换）；**状态目录整个保留**（主代理来源和选择、规则、分流出口、地区、各开关、首次准备标记、解析锁和托管设置的记录），
日志目录、`_abroad` 也不动。不碰 pf、DNS、网络服务。

- **只在“已回国”或“已停用”时**，并且没有正在进行的操作（不在启用中、回国中、借用虚拟机中、卸载中）。已启用时会说
  “升级前要先停用（`sudo abroad down`），升级后再启用”。后台服务不响应时拒绝，按 [docs/emergency.md](../../../docs/emergency.md) 处理或卸载重装。
- **不降级**：后台服务比要装的新就拒绝；一样就什么都不做。加版本号之前装的（版本不明）要多确认一次（非交互时加 `--accept-unknown-version`）。
- **状态格式版本号**不一样时拒绝，提示先卸载再重装（不做迁移：升级失败退回旧版本时旧版本还要能读懂状态）。
- 步骤：查状态 → 停 abroadd（它管的虚拟机随之停；pf 不动）→ 旧版搬进 `…/claude-abroad/prev/` → 放新文件 → plist 变了才换
  （guard 的换了时重新注册一次）→ 启动 abroadd → 核对版本是新的、完整性通过、状态和升级前一样 → 删 `prev/`。
- **任何一步失败或被中断**：把 `prev/` 里的旧版放回去、重启服务，报“升级失败，已退回旧版本”和原因（退出码 20）。退回本身也失败时
  （退出码 21）`prev/` 留着，再执行一次 `--upgrade` 会先接着退回；网络有异常时按应急文档处理。
- 退出码：0 完成（或已经是这个版本）、10 状态不允许、11 不降级、13 状态格式不同、14 版本不明要确认、20 已退回旧版本、21 退回没有完成、3 前提不满足。
- 以前用安装脚本放进“应用程序”的应用（安装记录里有它）会顺带换成 `app/.build/bundle/` 里的新应用包，属主改成执行 sudo 的你，并从安装记录里删掉：之后应用归你管，卸载不删它（卸载完成时会提示可以自己移到废纸篓）。拿不到执行 sudo 的用户时不替换，只提示。
- 其他参数：`--from <payload 目录>`（默认 `scripts/dev/out/payload`；给了就不复制应用，阶段 B 的应用用它）、`--yes`（不问回车）。

## 卸载

```sh
sudo scripts/m6/install/uninstall.sh          # 可加 --yes（不读终端，执行 abroad --yes uninstall）、--kill-browsers
```

1. 先走正常途径 `abroad uninstall`（提示时输入 `uninstall`）：结束 Claude（开发构建只预览）、恢复本机网络、删解析锁和托管设置。
2. 停掉两个后台服务。
3. 正常途径没走完时（abroadd 不在、卸载出错），按 [docs/emergency.md](../../../docs/emergency.md) 兜底：清空自己的 pf 锚点
   `com.apple/250.abroad`、删 DNS 键、按快照把原来是“自动”的 IPv6 改回来，按 abroadd 的记录删内容一致的解析锁和托管设置。
4. 按安装记录删掉 plist、`_abroad`、命令行链接、应用（只删安装时放的）、各个目录。
5. 和安装前的快照逐项对照（含 pf 锚点里的规则数和 pf 是否启用；应用那一行只在安装记录里有它时参与）：一致就删掉快照目录；不一致时列出差异、保留快照目录，退出码非零（可以重试卸载）。卸载后，Claude 出国的 pf 启用引用和锚点规则都不留下；pf 是否启用还受系统和别的软件持有的引用影响。卸载对照时，如果安装前没启用、卸载后启用，而启用引用都属于别的程序、我们的锚点是空的，这不算不一致，会写一条说明（2026-11-27 真机）。安装目录已经不在、只剩快照时（上一次卸载做完了、只是对照没过），再执行卸载只重做对照，对上了就删快照，不做别的。

可以重复执行；没有安装记录时什么都不做。

## 会话里能做的（不需要 sudo、不改系统）

```sh
DRY_RUN=1 scripts/m6/install/install.sh   >/tmp/i.out   # 空跑：只打印 CHANGE:、QUERY:、STATE:、WAIT:
DRY_RUN=1 scripts/m6/install/uninstall.sh >/tmp/u.out
DRY_RUN=1 scripts/m6/install/install.sh --upgrade >/tmp/up.out
scripts/m6/install/check-commands.sh /tmp/i.out /tmp/u.out /tmp/up.out   # 命令核对（清单在 commands.txt）
scripts/m6/install/selftest.sh             # 自测：在临时目录里真的装、真的卸，系统命令只记日志
```

## 注意

- `/usr/local/bin` 原来不存在时，安装会建它，卸载不删（空目录，不影响别的）。
- 走查脚本（scripts/m2–m5）留下的状态、已有的 `_abroad` 用户、已有的 `/var/run/claude-abroad` 都会让安装在一开始停下、什么都不改。
- 用发布构建验证真实效果（锁真实的 Claude 域名、结束真实的 Claude 进程）是 M6-9 的单独一节，要输入确认，这里不提供。

## 自测和空跑跳过的真机判断（2026-11-27 起维护）

起因：开发安装前提检查里“是不是开发构建”的判断，在测试模式下被跳过，真机路径从来没有执行过，结果一直误报（`abroadd -h` 的退出码是 2，接管道在 pipefail 下判断永远为假）。下面列出开发安装、端到端、第四层三份脚本里，自测（测试模式）和空跑**不会执行**的判断，以及各由什么覆盖。“只能真机”的项在第一次真机运行时看日志确认。

| 脚本 | 判断 | 覆盖 |
|---|---|---|
| 开发安装 | 前提检查：后台程序是开发构建、带两项开发参数 | `scripts/lib/buildcheck-selftest.sh`（退出码 2 的假程序、真编出的两种构建；还原成旧写法时失败） |
| 开发安装 | 专用用户是否存在（`dscl -read`） | 测试模式用 `M6I_TEST_USER_EXISTS` 走两个分支；`dscl` 命令本身只能真机 |
| 开发安装 | 找空闲的 UID、GID（`dscl -search`，400–499） | 只能真机（测试模式固定 450） |
| 开发安装（自测 h） | 运行前安装路径不存在；空跑不写系统路径 | 没装过的机器上照旧检查；这台 Mac 装着开发版时，前者跳过并说明，后者改为比较空跑前后这些路径的状态（`M6I_SELFTEST_INSTALLED_PATH` 换成不存在的路径可走“没装过”的分支） |
| 开发安装、卸载 | 系统快照：专用用户、两个服务、pf 锚点规则数、pf 是否启用、DNS 键、各网络服务的 IPv6 | 只能真机；pf 那一行认不出时记“无法判断”、不参与对照（已有容错） |
| 卸载 | 正常途径 `abroad uninstall`（经后台的本地接口） | 测试模式用 `M6I_TEST_NORMAL_FAILS` 模拟成功和失败两种；真实调用只能真机 |
| 卸载 | 兜底：按快照把 IPv6 改回“自动”、删 DNS 键 | 只能真机（空跑打印命令，命令清单核对写法） |
| 端到端 | 前提检查 | `--check-only` 缺“M5 已通过”和节点时拒绝（自测 c）；隧道、系统代理的检测只能真机 |
| 端到端 | 各检查点读状态：主代理已选、首次准备和完整性、已启用、已回国、回国拦截、应急程序之后 | 只能真机（读的字段名对照过后台的输出：`prepare.done`、`integrity.checked`、`resolver_lock.effective`、`return_block.active/test`、`gate_open`、`claude_code.*`） |
| 端到端 | 读 guard 日志的三个关键字（已加载断网规则、已加载回国拦截、完整性校验通过） | 关键字对照过 `internal/guard`、`internal/integrity` 的输出；读日志本身只能真机 |
| 端到端 | 连测试段的探测、LAN1、K1、L1、CC1 | 只能真机 |
| 第四层 | 分片的写入、删除、被改过时不删 | 自测 c1–c4（测试目录） |
| 第四层 | 启动 Claude Code、诱饵端口、T0–T5 的判定 | 只能真机（暂不测） |


## abroad-vm 必须带 cgo（2026-11-27）

只有 abroad-vm 运行时需要 cgo。`scripts/dev/integrity-build.sh` 编它时强制 cgo、编完核对；`scripts/dev/payload.sh` 和安装、升级的前提检查都用 grep 核对它链接了 `/System/Library/Frameworks/Virtualization.framework`，没有就拒绝、什么都不改（自测 vz）。
