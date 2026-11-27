# 一条命令构建、应用里装好后台服务、原地升级：实施计划（交架构顾问审）

> 版本：**v2**（2026-11-27）。v1 经顾问审过：通过，1 条必须改（见 1.1 的 `TestStateFormatPinned`），阶段 A 可以开工；第 0 节四处拍板和风险 3 的结论回填在下面。阶段 A 已按本计划实现（实现中的偏离见第 7 节），阶段 B 还没做。
> 依据：用户 2026-11-27 的选择“1”（原话“我不想每次安装都执行这么多命令，太啰嗦了”）；顾问 2026-11-27 的判定（分两阶段、同一份计划、分开交验收；设计都记 v0.71；“回国确认里选第四道”顺延到 v0.72；取消本机自用 pkg）。
> 现状核对过的文件：`scripts/m6/install/{install.sh,uninstall.sh,lib.sh,selftest.sh,commands.txt,check-commands.sh,README.md}`、`scripts/dev/{reinstall.sh,integrity-build.sh,app-bundle.sh,app-test.sh}`、`internal/daemon/{daemon.go,routes.go,uninstall.go,confirm.go}`、`internal/state/{file.go,machine.go}`、`app/Sources/AbroadKit/{Authorization.swift,SetupModel.swift,PageModels.swift,Strings.swift}`、`app/Tests/AbroadKitTests/RepoTests.swift`、设计 10.3、11.2、11.4。
> 会话里的边界：不提权（不 sudo、不弹管理员密码框）、不运行真实模式（只有自测 `M6I_TEST_DIR` 和空跑 `DRY_RUN=1`）、不碰真实系统路径（`/Library/Application Support/claude-abroad`、`/Applications/ClaudeAbroad.app`、`/Library/LaunchDaemons`、`/var/run`、`/var/log` 下我们的东西）、不改宿主机网络、不碰本机会话依赖的代理（见 `docs/LOCAL.md`）。本计划里出现的 `sudo …` 命令一律是**写给用户在真机上执行的**，会话不执行。

---

## 0. 结论先行（要顾问拍板的几处，详见第 5 节）

> **顾问 2026-11-27 拍板**：①加只读 `GET /v1/uninstall/preview`（不签发确认码，内容同卸载第一步的预览），预览里加一句（文字表）“执行时会按当时的情况重新列出；之后新启动的 Claude 也会被结束。”，“同时强制退出浏览器”原样传给 `--kill-browsers`（阶段 B）；②payload 带 `scripts/lib/common.sh`，放在和仓库相同的相对位置；③真机验证改为“借用虚拟机进行中被拒绝”，已启用、启用中、回国中、卸载中由自测 k 覆盖；④源码检查例外只在 `PrivilegedRunner.swift`、限定为一个常量（阶段 B）。下面是 v1 的原文。

1. 应用里卸载的“两步确认预览”：应用调用方不是“完全信任”，`POST /v1/actions/uninstall` 在出预览之前就 403（`routes.go` 的 `guard`）。提议照 `GET /v1/restore/preview` 的先例加只读的 `GET /v1/uninstall/preview`（不签发确认码），应用据此显示两步确认；用户确认、输密码后，root 脚本执行 `abroad --yes uninstall`（以 root 运行，是完全信任的调用方，后台自己再走一遍两步确认）。
2. `install.sh` 依赖 `scripts/lib/common.sh`（`ab_dev_flags_missing`）。payload 里除了三个脚本还要带上它（按仓库里的相对位置放），否则得把函数复制进 `lib.sh`。提议带上（只读引用，不改它）。
3. 真机验证“已启用状态下被拒绝”要先启用，而启用就是用户暂不测的 R1、R2。提议真机只验证“借用虚拟机进行中被拒绝”（不接管网络），“已启用、启用中、回国中、卸载中被拒绝”由自测覆盖；或者用户哪天真的启用了再补。
4. 源码检查的“禁止字符串”里有 `/Library/Application Support`；应用卸载要引用已安装的 `uninstall.sh`。提议例外只给 `PrivilegedRunner.swift`，且这个字符串只出现一次（一个常量）。

---

## 1. 共同的东西（两阶段都用）

### 1.1 版本号和状态格式版本号

- 新包 `internal/buildinfo`：`Commit`、`BuildTime`（UTC，`20261127T101500Z` 这种可按字典序比较的写法）、`Dirty`（工作区有未提交改动）三个由 `-ldflags -X` 写入的变量；没写入时是 `unknown`。常量 `StateFormat = 1`（**整个状态目录**的落盘格式版本：状态文件、主代理来源和选择、规则、分流出口、地区、各开关、首次准备标记、托管设置和解析锁的记录）。
  - 测试 `TestStateFormatPinned`：表里钉住状态目录下每种落盘格式自己的版本号（`state.Version` 以及实施时 grep 出的其余版本常量），任何一个变了而 `StateFormat` 没跟着改，测试失败并提示“改了落盘格式要同时改 StateFormat，原地升级会拒绝、改用卸载重装”。
  - **v2（顾问必须改）**：另外用反射把状态目录下每一种落盘结构的 JSON（规则文件是 YAML）字段名、标签和类型列成固定清单（golden，`internal/daemon/testdata/state-format.golden`，按结构体逐个列，内层结构体展开，自带序列化的类型只写名字），任何字段增删、改名、改标签、改类型都让测试失败，提示同时改 `StateFormat`；结构变了而 `StateFormat` 没加一时 `-update-state-format` 拒绝重写清单。反例 `TestStateFormatPinnedCounterexamples`（改字段名、改标签、改类型、改内层字段、加字段）。状态目录下的文件和 Go 类型的对照见第 7 节。现在不给那些文件补版本号。
  - 测试 `TestBuildInfoDefaults`：没写入时三项是 `unknown`/false，`Known()` 为假。
- `scripts/dev/integrity-build.sh`：编译所有程序都带 `-X claude-abroad/internal/buildinfo.{Commit,BuildTime,Dirty}=…`（`git rev-parse --short=12 HEAD`、`date -u +%Y%m%dT%H%M%SZ`、`git status --porcelain` 非空即 dirty）；另写 `out/integrity/build.json`：`{"commit":…,"build_time":…,"dirty":…,"state_format":1}`，`build.json` 加进完整性清单。`abroadd --version` 打印同样的 JSON（`cmd/abroadd`，测试 `TestVersionFlag`）；安装脚本的前提检查核对 `abroadd --version` 和 `build.json` 一致。
- `/v1/status` 新增（`daemon.Status`，apigen 重新生成 `app/Sources/AbroadAPI/APITypes.swift`）：
  - `build`（`{commit, build_time, dirty}`，总是给；老版本没有这一项 → 脚本和应用都按“版本不明”）；
  - `state_format`（整数）；
  - `busy`（状态机的忙标记，`Machine.Busy()` 只读）；
  - `uninstall`（`{running, done}`：正在一键卸载 / 已经卸载过）。
  - 测试：`TestStatusBuildInfo`、`TestStatusBusyAndUninstallFlags`（借用中、卸载中、已卸载、忙各一例）；apigen 的现有生成测试随之更新。
- 比较规则（脚本和应用各写一份，用同一份样例表 `scripts/m6/install/testdata/version-cases.tsv` 测）：只比 `build_time`；新包更新 → 升级；一样（时间和提交都一样）→ 什么都不做；后台更新 → 拒绝（不降级）；后台没有 `build` → 版本不明，允许但要多确认一次。同一提交、不同构建时间按时间算（开发时重编即“更新”）。

### 1.2 payload（安装用的一包东西，只有一种布局）

新脚本 `scripts/dev/payload.sh`（以用户身份、不用 sudo）：在 `integrity-build.sh --dev` 的产物上组装 `scripts/dev/out/payload/`：

```
payload/
├── bin/{abroad,abroad-rescue,abroadd,abroad-guard,abroad-vm}
├── image/{Image,initramfs.gz,modloop,rootfs.img}
├── integrity.json   build.json
├── scripts/m6/install/{install.sh,uninstall.sh,lib.sh}
├── scripts/lib/common.sh            # 见第 0 节第 2 条
└── payload.sha256                   # 上面每个文件一行（shasum -a 256 的格式，相对路径）
```

脚本放在和仓库相同的相对位置，`install.sh` 里 `ROOT="$(dirname "$0")/../../.."` 的算法不用改：从 payload 里运行时 ROOT 就是 payload 目录。命令行入口和应用都交这同一个目录（应用包里是它的一份副本）。

---

## 2. 阶段 A：命令行原地升级 `sudo scripts/m6/install/install.sh --upgrade`

### 2.1 允许升级的状态（脚本经只读状态接口自己查）

升级前用 `curl --unix-socket /var/run/claude-abroad/api.sock http://abroadd/v1/status`（只读）取状态，**全部满足才允许**：

| 条件 | 不满足时 |
|---|---|
| 接口在 10 秒内响应 | 拒绝：“后台服务没有响应，不能原地升级。按 docs/emergency.md 用应急程序 `sudo abroad-rescue` 恢复网络，或执行卸载后重装。”退出码 `10` |
| `state` 是 `RESTORED`（已回国）或 `DISABLED`（已停用），`corrupt` 不为真 | `ENABLED`：“已启用时不能升级：请先停用（`sudo abroad down`），升级后再启用。”；`ENABLING`/`RESTORING`：“正在启用 / 回国，等它完成再升级。”退出码 `10` |
| 没有 `borrow`（不在借用虚拟机中）、`busy` 为假 | “正在进行别的操作，等它完成再升级。”（`busy` 会被监护的轮次短暂占用：间隔 1 秒重查 5 次）退出码 `10` |
| `uninstall.running`、`uninstall.done` 都为假 | 卸载中：“正在卸载”；已卸载：“已经一键卸载，请执行卸载脚本后重新安装”。退出码 `10` |
| `state_format` 和新包 `build.json` 的一样 | “状态格式变了（旧 X、新 Y），不能原地升级：请先卸载再重装（主代理和设置要重新配置）。不做迁移，是为了升级失败退回旧版本时旧版本还能读懂状态。”退出码 `13` |
| 版本：新包比后台新 | 后台更新：“后台服务（构建于 …）比要装的（构建于 …）还新，不降级。”退出码 `11`；一样：“已经是这个版本，什么都不做。”退出码 `0`，什么都不改 |

- 老安装（没有 `build`、`state_format` 字段）：版本不明 → 要多确认一次（终端里再按一次回车并显示“后台服务的版本不明（是加版本号之前装的）”；非交互时要带 `--accept-unknown-version`，否则退出码 `14`）；`state_format` 缺失时退一步读 `state/abroadd/state.json` 的 `version`，等于 1 才按格式 1 处理，读不出就拒绝（退出码 `13`）。
- 检查和停服务之间仍有几秒窗口（用户此刻点了启用）：不加锁，靠停服务走的是现有崩溃恢复路径（和断电一样），重启后“状态和升级前一样”的核对会发现变化 → 退回旧版本。写进风险。

### 2.2 步骤

前提：已经装过（快照目录 `…/claude-abroad-install-snap` 和安装目录都在）、payload 齐全且 `payload.sha256` 核对通过、`abroadd --version` 和 `build.json` 一致；`upgrading` 标记不在（在就是上一次升级没走完，见 2.4）。

1. **确认状态**（2.1）。终端里显示“将把后台服务从 A 升级到 B，主代理和设置保留；期间后台服务会停一下，不会断网也不会接管网络”，按回车开始（`--yes` 跳过）。
2. 写 `upgrading` 标记（快照目录里，内容是旧、新两个版本）；`trap` 接管 EXIT/INT/TERM/HUP：此后任何失败或中断都走 2.4 的退回。
3. **停服务**：`launchctl bootout system/com.claude-abroad.dev.abroadd`，等 api.sock 不再响应（最多 30 秒）。abroadd 正常退出时停掉它管理的虚拟机（`stopInternals`）；pf 不动：已回国下回国拦截的锚点和 pf 启用引用留在内核里，已停用下断网规则照旧。guard 是一次性的，平时已经退出，不动。
4. **搬旧版**：`mkdir -m 700 …/claude-abroad/prev`，把 `bin/`、`image/`、`integrity.json`、`build.json`（有就搬）、`scripts/`（有就搬）用 `mv` 搬进去（同一个文件系统，原子改名）；两份 plist 复制一份进 `prev/plists/`。
5. **放新文件**：从 payload 复制程序、镜像、清单、`build.json`，以及 `scripts/m6/install/{uninstall.sh,lib.sh}`、`scripts/lib/common.sh` 到安装目录的 `scripts/`（给应用里的卸载用，见 3.6）；`chown -R root:wheel`、`chmod -R go-w`（和全新安装相同）。
6. **plist**：按新参数生成两份 plist，和现有的逐字节比较，**内容变了才换**。guard 的 plist 换了时：`bootout` 再 `bootstrap` guard（它会像开机时一样运行一次：已回国加载回国拦截、已停用加载断网规则，都是幂等的），等它退出。
7. **命令行链接**：`/usr/local/bin/abroad` 是安装时放的那个链接（记录里有 `cli_link`）且指向别处时改回 `…/bin/abroad`；不是我们放的不动（和安装一致）。
8. **应用（过渡）**：记录里有 `app`（老的安装脚本以 root 身份复制进 `/Applications` 的）而命令行升级时给了仓库的应用包，就替换它；没有记录不碰。见 5.6。
9. **启动服务**：`launchctl bootstrap system …abroadd.plist`，最多等 60 秒到状态接口响应。
10. **核对成功条件**（全部满足才算成功）：状态接口响应；`build` 等于新包的；`integrity.checked && integrity.ok`；`state` 和升级前一样（`corrupt` 不为真）；`prepare.done` 和升级前一样；已回国时 `return_block` 和升级前一样。
11. 成功：删 `prev/`、删 `upgrading` 标记，`trap -` ，打印“升级完成：A → B，主代理和设置已保留”。

**保留**（一个字节都不碰）：`state/` 整个目录、`/var/log/claude-abroad/`、`fake-claude/`、专用用户 `_abroad`、`/var/run/claude-abroad`（socket 由 abroadd 自己重建）、快照目录里安装时的记录和 `files.before`/`system.before`。**替换**：程序五个、镜像、完整性清单、`build.json`、两份 plist（变了才换）、命令行链接（只在是我们的时）、安装目录的 `scripts/`。

### 2.3 退回旧版本：触发条件和步骤

**触发**：第 3–10 步任何一步失败（命令非零、30/60 秒超时、版本不对、完整性校验不通过、状态和升级前不一样），或者脚本在第 2 步之后被中断。

**步骤**（`m6i_upgrade_rollback`）：
1. `launchctl bootout` abroadd（不在也不算错）；
2. 删掉安装目录里已经放进去的新文件（只删第 5 步的那几项，不碰 `state/`）；
3. `prev/` 里的东西 `mv` 回原处；plist 被换过的从 `prev/plists/` 放回，guard 的 plist 换过时 `bootout`+`bootstrap` guard；
4. `bootstrap` abroadd，最多等 60 秒；核对 `build` 等于旧版本（旧版没有版本字段时只核对接口响应）、`state` 和升级前一样；
5. 成功：删 `prev/`、`upgrading` 标记，打印“**升级失败，已退回旧版本**：<哪一步、原因>”，退出码 `20`；
6. 退回本身失败：**留着** `prev/` 和 `upgrading` 标记，打印“升级失败，退回旧版本也没有完成：<原因>。旧版本留在 …/claude-abroad/prev。网络有异常时按 docs/emergency.md 用 `sudo abroad-rescue`；再执行一次 `sudo scripts/m6/install/install.sh --upgrade` 会先接着退回；也可以执行卸载脚本”，退出码 `21`。

状态文件：升级期间脚本不读写 `state/`（只读接口查状态；格式回退判断时只读 `state.json`）。新 abroadd 起来后可能写状态目录（例如事件、记录），但状态格式版本号相同，旧版本能读懂——这正是“格式不一样就拒绝原地升级”的理由。

### 2.4 上一次升级没走完

`--upgrade` 启动时发现 `upgrading` 标记：不做新的升级，直接执行 2.3 的退回（`prev/` 在就从它退回；`prev/` 不在说明搬旧版之前就断了，只需重启服务并核对），打印结果，退出码 `20`/`21`。全新安装（不带 `--upgrade`）和卸载看到 `upgrading` 标记时拒绝并提示先执行 `--upgrade` 退回（卸载脚本例外见 3.6：标记在、`prev/` 在时卸载照常删整个安装目录，因为卸载本来就要删光）。

### 2.5 要改的文件、函数（阶段 A）

| 文件 | 改什么 |
|---|---|
| `internal/buildinfo/buildinfo.go`（新）+ `_test.go` | 1.1；`TestBuildInfoDefaults`、`TestStateFormatPinned` |
| `cmd/abroadd/main.go` + 测试 | `--version`；`TestVersionFlag` |
| `internal/daemon/daemon.go`（`Status`、`Daemon.Status()`）、`internal/state/machine.go`（`Busy()`） | 1.1 的四个字段；`TestStatusBuildInfo`、`TestStatusBusyAndUninstallFlags` |
| `internal/daemon` 新测试 `TestStopStartKeepsState` | 已回国、已停用两种：正常停止（`Shutdown`）后状态文件内容、pf 锚点规则、pf 启用引用都不变；用同一状态目录起新实例，状态和停止前一样、回国拦截照旧。用来证明“停服务、重启走现有路径、不改状态”（假系统，`bothSystems`） |
| `cmd/abroad-apigen` 生成物 `app/Sources/AbroadAPI/APITypes.swift` | 重新生成；生成测试更新 |
| `scripts/dev/integrity-build.sh` | ldflags、`build.json`、清单加 `build.json` |
| `scripts/dev/payload.sh`（新） | 1.2 |
| `scripts/m6/install/lib.sh` | `m6i_status`（真实：curl；自测：读 `M6I_TEST_STATUS`/`M6I_TEST_STATUS_AFTER` 指的 JSON 文件；空跑：打印 QUERY）；`m6i_version_cmp`；`m6i_upgrade_allowed`（2.1 的表，打印原因、返回退出码）；`m6i_fail_inject <步骤名>`（只在自测：`M6I_TEST_FAIL_AT=stop|move|place|plist|start|verify|rollback`）；`m6i_require_mode` 在 `--from` 模式下不要求 `SUDO_USER`（应用经管理员授权运行时没有它；仍要求 euid 0） |
| `scripts/m6/install/install.sh` | 参数：`--upgrade`、`--from <payload 目录>`（默认 `scripts/dev/out/payload`）、`--yes`、`--accept-unknown-version`、`--check-only`；`--from` 时不复制应用（应用就是 payload 的来源）、日志写 `/var/log/claude-abroad/`（不写仓库的 `out/`）；全新安装也把 `scripts/` 放进安装目录；`do_upgrade`、`m6i_upgrade_rollback`；结尾在标准输出打一行 `RESULT=<退出码>` |
| `scripts/m6/install/uninstall.sh` | `--yes`（正常途径执行 `abroad --yes uninstall`，不读 `/dev/tty`）、`--kill-browsers`；快照对照里 `/Applications/ClaudeAbroad.app` 那一行只在记录里有 `app` 时参与（应用由用户自己放进“应用程序”后，装卸前后它在不在不归脚本管，见 5.6） |
| `scripts/m6/install/commands.txt`、`check-commands.sh` | 升级的命令：`launchctl bootout system/com.claude-abroad.dev.abroadd`、`mv … prev/…`、`rm -rf …/claude-abroad/prev`、`cp …/payload/scripts/… …/claude-abroad/scripts/…`；反例：升级里出现 `pfctl`、`networksetup`、`scutil`、`dscl` 被拦下 |
| `scripts/m6/install/selftest.sh` | 新场景 i–s（2.6） |
| `scripts/dev/reinstall.sh` + `reinstall-selftest.sh` | 构建改为 `integrity-build.sh --dev` → `payload.sh` → `app-bundle.sh`；已安装时 `sudo scripts/m6/install/install.sh --upgrade`（不再卸载）；结尾提示改为“升级完成，主代理和设置已保留”；没装过时照旧安装；升级被拒（退出码 10/11/13/14）时原样转述原因、提示怎么办（已启用先停用、格式变了先卸载） |
| `scripts/m6/install/README.md`、`scripts/dev/README.md` | 用法 |

### 2.6 会话里怎么测（阶段 A）

全部不提权、不碰真实系统路径和宿主机网络：

- `go build ./...`、`go vet ./...`、`go test ./internal/... ./cmd/...`、`GOOS=linux GOARCH=arm64 go build ./...`。
- `scripts/m6/install/selftest.sh`（`M6I_TEST_DIR` 临时根目录里**真装、真升级、真退回**，系统命令只记 `SYS:`，状态接口用假 JSON 文件）：
  - **i. 升级成功**（已回国、已停用各一次）：先装旧 payload，在 `state/` 里放几个假文件（主代理来源、选择、规则、开关、首次准备标记、托管设置和解析锁的记录），记下整个 `state/`、日志目录的树和摘要；再用新 payload `--upgrade --yes`：五个程序、镜像、清单、`build.json`、`scripts/` 是新的；`state/` 和日志目录**逐字节不变**；`prev/` 和 `upgrading` 标记不在；`sys.log` 恰好是 `bootout abroadd`、`bootstrap abroadd`（plist 没变时不碰 guard、不重写 plist）；退出码 0，标准输出最后一行 `RESULT=0`。
  - **j. plist 变了才换**：新 payload 改了 abroadd 参数 → 只换 abroadd 的；改了 guard 参数 → guard 也 `bootout`/`bootstrap`。
  - **k. 状态不允许**：`ENABLED`、`ENABLING`、`RESTORING`、借用中、`busy` 一直为真、卸载中、已卸载、`corrupt`、接口不响应——每例退出码 10，原因文字对，**安装目录的树和 `sys.log` 和运行前完全一样**。
  - **l. 不降级**：后台更新 → 11、什么都不改；一样 → 0、什么都不改（`sys.log` 没有新行）。
  - **m. 状态格式不一样** → 13、什么都不改；缺字段时读 `state.json` 的 `version`（1 → 继续；2 或读不出 → 13）。
  - **n. 版本不明**：不带 `--accept-unknown-version` 且非交互 → 14、不改；带上 → 升级成功。
  - **o. 注入失败后退回**：`M6I_TEST_FAIL_AT` 依次取 `stop`、`move`、`place`、`plist`、`start`、`verify`（另用 `M6I_TEST_STATUS_AFTER` 给“版本不对”“完整性不通过”“状态变了”三种假应答）：每例退出码 20，输出含“升级失败，已退回旧版本”和哪一步；安装目录的程序、镜像、清单、plist **和升级前逐字节一样**，`state/` 不变，`prev/` 和标记不在，`sys.log` 最后是重启 abroadd。
  - **p. 退回也失败**（`M6I_TEST_FAIL_AT=start,rollback`）：退出码 21，`prev/` 和 `upgrading` 标记留着，输出指向 `docs/emergency.md`；再运行一次 `--upgrade`（不注入）→ 先退回成功，退出码 20。
  - **q. 中断**：在第 5 步给自己发 `TERM`（自测钩子 `M6I_TEST_KILL_AT=place`）→ 退回。
  - **r. 升级后再卸载**：和安装前的快照一致（证明升级没有破坏卸载的记录）。
  - **s. `--from` 模式**：从 payload 目录装（不复制应用）、`--from` 下不要 `SUDO_USER`、payload 缺文件或 `payload.sha256` 对不上时前提检查失败、什么都不改。
  - 现有场景 a–h 全部保持通过；`check-commands.sh` 核对空跑 `DRY_RUN=1 install.sh --upgrade` 的命令。
- `scripts/dev/reinstall-selftest.sh`：已安装时第 4 步是 `sudo scripts/m6/install/install.sh --upgrade`、没有卸载这一步；结尾文字；升级被拒的几种退出码的提示。
- `scripts/dev/privacy-check.sh`。

### 2.7 用户的真机验证（阶段 A）

前提：现在装着的是加版本号之前的开发版，已回国，主代理已经配好（这次升级要证明它还在）。

1. 以自己的身份：`git pull`，记下 `abroad main` 显示的来源、选中的节点和 `abroad status` 里的地区。
2. `scripts/dev/reinstall.sh`（不带 sudo；构建完会问一次管理员密码，执行 `install.sh --upgrade`；因为旧版没有版本号，会多问一次“版本不明，继续？”）。期望输出“升级完成”。
3. 核对：`abroad status` 里 `build` 是刚才的提交、状态仍是“已回国”、完整性通过；`abroad main` 的来源和选中节点和第 1 步一样；**不用重新添加主代理**。
4. 拒绝的情况（见第 0 节第 3 条，二选一由顾问定）：
   - a（不接管网络）：一个终端执行 `sudo abroad main check`（会借用虚拟机几十秒），期间另一个终端执行 `sudo scripts/m6/install/install.sh --upgrade`，期望“正在进行别的操作，等它完成再升级”，什么都没改（`abroad status` 的 `build` 不变）；
   - b（要启用，属于用户暂不测的 R1、R2）：`sudo abroad up` 后执行 `--upgrade`，期望“已启用时不能升级：请先停用…”。
5. 把两次的终端输出和 `/var/log/claude-abroad/` 下的升级日志贴回。

---

## 3. 阶段 B：应用里的入口

### 3.1 提权方式

`NSAppleScript` 执行 `do shell script "<命令>" with prompt "<文字>" with administrator privileges`：系统自带、不要签名、没有废弃。不用 `AuthorizationExecuteWithPrivileges`；`SMJobBless`/`SMAppService` 要签名，这一版不做（P2、P3 不变）。

### 3.2 构建：`scripts/dev/build-app.sh`（新，以用户身份、不用 sudo）

1. `integrity-build.sh --dev` → `payload.sh`；
2. `app-bundle.sh --payload scripts/dev/out/payload`：把 payload 用 `cp -R` 放进 `ClaudeAbroad.app/Contents/Resources/payload/`；`Info.plist` 加三个键 `ClaudeAbroadPayloadCommit`、`ClaudeAbroadPayloadBuildTime`、`ClaudeAbroadPayloadStateFormat`（应用用 `Bundle.main.object(forInfoDictionaryKey:)` 读，不读文件——“应用不写磁盘、只在 InputFiles.swift 读文件”的规则不用放宽）；然后才 `codesign --force --sign -`（签名覆盖 payload）和 `codesign --verify --strict`；
3. 复制到 `/Applications/ClaudeAbroad.app`：不存在 → 复制；存在且 `CFBundleIdentifier` 是 `com.claude-abroad.app` 且当前用户可写 → 先放到同目录的临时名再改名替换；包标识不是我们的 → 拒绝、不碰；是我们的但当前用户不可写（老安装脚本以 root 身份放的）→ 不碰，提示“这份应用是以前的安装脚本以管理员身份放的：请执行一次 `scripts/dev/reinstall.sh`（升级时会替换它）”（见 5.6）。
- 自测 `scripts/dev/build-app-selftest.sh`：`BUILD_APP_TEST_DEST=<临时目录>`、`BUILD_APP_TEST_BUNDLE=<假应用包>`（跳过 swift 编译）：别人的同名应用不碰、我们的被替换、不可写时提示、payload 在包里的位置和 `Info.plist` 三个键对、`--payload` 缺文件时失败。

### 3.3 应用打开时看要不要显示按钮

`ServiceInstallModel`（AbroadKit，新文件 `ServiceInstall.swift`）用已有的只读状态请求：

| 状态接口 | 显示 |
|---|---|
| 连不上（socket 不在 / 没人听） | 安装引导“后台服务”一步：“安装后台服务”按钮（代替现在的终端命令文字）。装过但没运行时脚本会拒绝并说明，应用照原文显示 |
| `build` 比包里的旧 | 主页顶部和设置“高级”：“升级后台服务（A → B）” |
| 没有 `build`（老安装） | 同上，文字写“版本不明”，点了先多一次确认，确认后命令带 `--accept-unknown-version` |
| `state_format` 和包里的不一样 | 不显示升级按钮，显示“状态格式变了，要先卸载再重装” |
| 一样，或后台更新 | 不显示（后台更新时设置里只读显示“后台服务比应用新”） |
| 已启用、启用中、回国中、借用中、忙、卸载中 | 升级按钮显示但不可点，旁边写原因（“已启用时不能升级：请先停用”等）；脚本里同样会再查一遍 |

**只有用户点按钮才弹密码框**：模型的 `refresh()`、`onAppear` 只读状态，不调用提权层（测试守着，见 3.8）。

### 3.4 防护①：检查和执行之间被调包——先复制到 root 的临时目录

传给管理员授权的命令**只做一件事：把包里的 payload 复制到 root 所有、0700 的临时目录，核对后从副本执行**（下面 `STG` 是 `/Library/Application Support/claude-abroad-staging`，`PL` 是应用包里的 payload 路径；两者都单引号）：

```sh
umask 077; /bin/rm -rf 'STG' && /bin/mkdir -m 700 'STG' && /usr/sbin/chown root:wheel 'STG' \
 && /bin/cp -RL 'PL' 'STG/p' && cd 'STG/p' && /usr/bin/shasum -a 256 -s -c payload.sha256 \
 && /bin/bash 'STG/p/scripts/m6/install/install.sh' --from 'STG/p' --yes [--upgrade] [--accept-unknown-version]; \
 rc=$?; /bin/rm -rf 'STG'; echo "RESULT=$rc"; exit $rc
```

- 复制之前只用系统自带的程序（`/bin`、`/usr/bin`、`/usr/sbin`），包里的任何东西都不在复制完成、核对通过之前执行；`cp -RL` 解引用符号链接，副本里只有普通文件，不会经链接指回用户可写的位置。
- `shasum -c` 核对副本和**包里自带的** `payload.sha256` 一致——**只证明内部一致（复制时没有被换掉一半），不证明是原版**；不一致就停（`&&` 链断开），什么都不改，`RESULT` 非零。
- 之后一切（脚本、程序、镜像）都从 root 所有的副本执行；用户权限的程序改不了副本。结束时删掉副本（失败也删）；下次开始前先删残留。
- 临时目录放在 `/Library/Application Support/` 下（父目录 root 所有，普通用户在那里建不了同名链接）。

### 3.5 防护②③④⑤

- **② 只从应用包内固定位置取文件**：payload 路径 = `Bundle.main.resourceURL` + `payload`，不接受外部参数、环境变量、用户输入；动作是枚举 `install`、`upgrade(acceptUnknown: Bool)`、`uninstall(killBrowsers: Bool)`，拼出的只有固定的开关。拼进 shell 命令的路径一律 `shellQuote`（单引号，路径里的 `'` 换成 `'\''`）；整条命令再按 AppleScript 字符串转义（`\` → `\\`、`"` → `\"`）放进 `do shell script "…"`。路径里有换行、NUL 或其他控制字符时拒绝（不拼命令，提示“应用所在的路径里有不支持的字符，请把应用放进‘应用程序’”）。
- **③ 密码框提示文字从文字表取**：安装和升级“安装 / 升级 Claude 出国的后台服务（会以管理员身份运行）”；卸载“卸载 Claude 出国的后台服务并恢复本机网络（会以管理员身份运行）”。
- **④ 只有一个文件能提权**：新文件 `app/Sources/ClaudeAbroad/PrivilegedRunner.swift`（实现 AbroadKit 里的协议 `PrivilegedRunning`），全应用只有它出现 `NSAppleScript` 和 `with administrator privileges`；界面里只有按钮点击能走到它。
- **⑤ 不允许降级**：同 1.1；应用不显示降级按钮，脚本也会拒绝（双重）。

### 3.6 卸载入口（设置“高级”）

1. 设置“高级”的“卸载”卡片：把现在的“应用里的卸载入口还没有提供……”换成按钮“卸载 Claude 出国…”。
2. 第一步：读 `GET /v1/uninstall/preview`（新的只读接口，见第 0 节第 1 条；内容和 `POST /v1/actions/uninstall` 第一步的预览相同：要结束的 Claude 进程、在运行的浏览器、要删除的东西；不签发确认码），用现有的两步确认框（`ConfirmFlow` 的样式和 3 秒等待）显示，可勾选“同时强制退出浏览器”。
3. 确认后弹密码框，执行**已安装的、属主 root 的** `uninstall.sh`（安装目录 `scripts/` 里那份，不用应用包里的）。为了不让脚本在运行中删掉自己，同样先复制到 root 的临时目录再执行：

```sh
umask 077; /bin/rm -rf 'STG' && /bin/mkdir -m 700 'STG' && /usr/sbin/chown root:wheel 'STG' \
 && /bin/cp -RL '/Library/Application Support/claude-abroad/scripts' 'STG/p' \
 && /bin/bash 'STG/p/scripts/m6/install/uninstall.sh' --yes [--kill-browsers]; \
 rc=$?; /bin/rm -rf 'STG'; echo "RESULT=$rc"; exit $rc
```

（安装目录的 `scripts/` 布局和 payload 相同，`ROOT` 算法照用。）

4. 已安装的目录里没有 `scripts/`（加这个功能之前装的、还没升级过）：脚本以退出码 `30` 结束，应用提示“这份安装太旧，请先升级后台服务，或在终端执行卸载脚本”。
5. 输密码本身就是本人授权，不再等 M5-3（M5-3 剩下其他敏感操作的应用内授权）。卸载完成后应用回到“后台服务没有安装”的引导，提示可以把应用移到废纸篓（应用不代为移动：应用不写磁盘）。

### 3.7 安全边界（如实写进设计 11.4 和验收清单第 7 节，不夸大）

- 这一版**没有签名**：应用包属于用户账户、用户可写。“包里的东西是不是原版”的信任程度**等于用户账户本身**——和现在在用户可写的仓库里 `sudo` 运行 `install.sh` 相同，没有变好也没有变差。
- 密码框能拦住的：别的程序在用户不知情时悄悄完成安装或升级（需要用户输入管理员密码）。
- 拦不住的：以用户权限运行的恶意程序可以在用户点“升级”之前改掉包里的 payload（连同 `payload.sha256` 一起改），用户输入密码后装上的就是被改过的东西。防住它需要签名并由系统核对签名，这一版不做。
- 复制到 root 临时目录只防“检查之后、执行之前被换掉”；`shasum` 核对只证明副本内部一致。
- 卸载用的是已安装的 root 所有的脚本，不受应用包被改的影响（但 `abroad` 程序本身照旧由完整性清单校验）。

### 3.8 要改的文件、函数、测试（阶段 B）

| 文件 | 改什么 |
|---|---|
| `internal/daemon/routes.go`、`uninstall.go` | `uninstall-preview`：`GET /v1/uninstall/preview`，`api.Read`，复用 `uninstallPreview`；测试 `TestUninstallPreviewReadOnly`（不签发确认码、不改状态、应用身份可读、已卸载后照常可读）；apigen 重新生成 |
| `scripts/dev/app-bundle.sh` | `--payload <目录>`；`Info.plist` 三个键；签名在放 payload 之后 |
| `scripts/dev/build-app.sh`（新）+ `build-app-selftest.sh`（新） | 3.2 |
| `app/Sources/AbroadKit/ServiceInstall.swift`（新） | 协议 `PrivilegedRunning { func run(command: String, prompt: String) async -> PrivilegedOutcome }`（`ok(stdout)`、`failed(code, output)`、`cancelled`）；`PrivilegedCommand`（纯函数：`shellQuote`、`appleScriptEscape`、`install/upgrade/uninstall` 的命令，路径校验）；`ServiceInstallModel`（3.3 的判断、按钮可用与原因、`userTappedInstall()`/`userTappedUpgrade()`/`userTappedUninstall(killBrowsers:)`，把退出码 0/10/11/13/14/20/21/30 和取消对到文字表，不按输出文字分支）；版本比较 |
| `app/Sources/ClaudeAbroad/PrivilegedRunner.swift`（新） | 唯一的 `NSAppleScript` 实现：拼 `do shell script "…" with prompt "…" with administrator privileges`；错误号 -128 → `cancelled`；在专门的串行队列上执行（不卡主线程；见风险 5.5）；结果只回传标准输出和退出码 |
| `app/Sources/ClaudeAbroad/SetupView.swift`、`HomeView.swift`、`SettingsView.swift`、`App.swift` | “安装后台服务”“升级后台服务”“卸载 Claude 出国…”按钮（沿用 Palette/Components 的控件）；进行中显示进度和“请勿关闭应用”；结果和脚本输出的最后几行；注入真实 runner |
| `app/Sources/AbroadKit/Strings.swift` | 密码框两句、按钮、各退出码的说明、安全边界的一句说明（设置“高级”里显示“没有签名：应用包的内容可信程度等于你的用户账户”）；删掉 `uninstallInInstaller` 旧文字 |
| `app/Sources/AbroadKit/SetupModel.swift` | “后台服务”一步改用 `ServiceInstallModel`（不再给终端命令；终端命令作为“也可以在终端执行”的次要文字保留） |
| `app/Tests/AbroadKitTests/ServiceInstallTests.swift`（新） | 见下 |
| `app/Tests/AbroadKitTests/RepoTests.swift` | 源码检查（见下） |
| `app/Tests/AbroadKitTests/testdata/privileged-commands.txt`（新） | 三种命令的标准样子（路径用占位），Swift 测试和脚本自测共用 |
| `scripts/m6/install/selftest.sh` | 场景 t：把 `privileged-commands.txt` 里的安装、升级命令的 `STG`、`PL` 换成自测目录下的路径、去掉 `chown`，用 `bash -c` 真跑（不提权）：payload 完好 → 装上；`payload.sha256` 被改一行 → 停在核对、安装目录不存在；payload 里放一个指向外面的符号链接 → 副本里是普通文件；结束后临时目录不在。卸载命令同样在自测目录里跑一遍 |

**Swift 测试**（`swift test`，提权层全是假实现，只验证传了什么、什么时候调用）：
- `privilegedCommandGolden`：三种命令和 `privileged-commands.txt` 一致；
- `shellQuoteCases`：普通路径、带空格、带 `'`、`"`、`$`、反引号、`\`；对每个结果再做一次 AppleScript 转义后反解析（测试里实现一个最小的 AppleScript 字符串和 sh 单引号解析器）能还原原路径；带换行、NUL、控制字符时拒绝、runner 没被调用；
- `promptFromStringTable`：两句提示文字来自 `L10n`；
- `noRunWithoutTap`：模型 `refresh()`、状态变化、`onAppear` 对应的方法多次调用后，假 runner 调用次数为 0；点一次按钮恰好 1 次，进行中再点不重复调用；
- `buttonsByStatus`：3.3 表格逐行（连不上、旧、版本不明、格式不同、一样、更新、各个不允许的状态）；
- `unknownVersionNeedsSecondConfirm`：不经第二次确认不调用；确认后命令带 `--accept-unknown-version`；
- `outcomeMapping`：退出码 0/10/11/13/14/20/21/30、取消，各对到正确文字，取消时什么提示都不弹；
- `uninstallFlow`：先读预览、两步确认、3 秒前确认不可点、取消不调用 runner；确认后命令里是已安装的脚本路径、勾选与否决定 `--kill-browsers`；
- 源码检查（`RepoTests`，把现有 `sourceRules` 的判断抽成函数 `sourceRuleViolations(files:)`，以便喂反例）：
  - `NSAppleScript`、`administrator privileges`、`do shell script` 只出现在 `PrivilegedRunner.swift`；`/Library/Application Support` 只在 `PrivilegedRunner.swift` 出现**一次**（卸载脚本的常量），其余文件照旧禁止；
  - `PrivilegedRunner(` 只在 `App.swift` 构造一次；`userTappedInstall`、`userTappedUpgrade`、`userTappedUninstall` 只出现在按钮动作里（所在行含 `Button(` 或 `action:`）；
  - 反例：构造假文件列表——`HomeView.swift` 里出现 `NSAppleScript`、`SettingsView.swift` 里出现 `with administrator privileges`、`PrivilegedRunner.swift` 里出现两次 `/Library/Application Support`、`onAppear` 里调用 `userTappedUpgrade`——每例都被报出来；
  - “应用不写磁盘”的规则不放宽：`PrivilegedRunner.swift` 照样受 `write(to`、`FileManager`、`Process(` 等禁止项约束；打开应用、保存面板的规则不变。

### 3.9 会话里怎么测（阶段 B）

- `scripts/dev/app-test.sh`（`swift build` + `swift test`，含假系统 abroadd 的集成测试）；`go test ./internal/... ./cmd/...`；`scripts/m6/install/selftest.sh`（含场景 t）；`scripts/dev/build-app-selftest.sh`；`scripts/dev/app-bundle.sh --payload …` 只编译和组装到 `app/.build/bundle/`。
- **不运行** `build-app.sh` 的“复制到 /Applications”这一步（会碰用户真实安装的应用），只跑它的自测；不启动真实的 `PrivilegedRunner`、不弹密码框。界面需要看时按以前的“会话代测”方式打开 `app/.build/bundle/` 里的应用、连假系统 abroadd 截图，不点安装、升级、卸载。

### 3.10 用户的真机验证（阶段 B，在阶段 A 的 2.7 之后）

1. `scripts/dev/build-app.sh`（不带 sudo）。如果提示“以前的安装脚本以管理员身份放的”，先做一次 `scripts/dev/reinstall.sh`，再执行本步。打开“应用程序”里的 Claude 出国：版本和后台一样，不显示升级按钮。
2. **应用里卸载**：设置 → 高级 → “卸载 Claude 出国…” → 看预览、等 3 秒确认 → 密码框文字是“卸载 Claude 出国的后台服务并恢复本机网络（会以管理员身份运行）”→ 输密码。期望：应用显示卸载完成、回到“后台服务没有安装”；终端里 `ls "/Library/Application Support/claude-abroad"` 不存在、`launchctl print system/com.claude-abroad.dev.abroadd` 找不到。（主代理配置会随卸载清空，这是预期的。）
3. **应用里第一次安装**：引导页“安装后台服务” → 密码框文字“安装 / 升级 Claude 出国的后台服务（会以管理员身份运行）”→ 输密码，等一两分钟。期望：引导进入“配置主代理”，`abroad status` 是已回国、首次准备完成、完整性通过。重新添加主代理来源、选节点、在终端 `sudo abroad main apply`（或应用里按提示）。
4. **应用里升级**：随便提交一处无关改动（或 `git commit --allow-empty`），再 `scripts/dev/build-app.sh`，重新打开应用：显示“升级后台服务（A → B）”→ 点 → 输密码。期望：升级完成，主代理来源和选择还在（第 3 步配的）。
5. 再点一次升级时在密码框点“取消”：什么都不变，应用不报错。
6. 贴回各步的应用提示、`/var/log/claude-abroad/` 下的安装和升级日志。

---

## 4. 文档（两个阶段各自交验收时一起改）

- 设计（v0.71）：
  - **11.4**：开发安装的三条路径（全新安装、原地升级、卸载）和同一份脚本；升级允许的状态、保留和替换、不降级、版本号和状态格式版本号、退回旧版本；应用里的入口、提权方式、临时目录防调包、安全边界（3.7 原样）；取消本机自用 pkg 的原因（用户要的是“一条命令 + 应用里点一下”，pkg 解决不了每次升级都清空主代理、还要多一个打包格式和一套卸载记录）。
  - **11.2**：安装引导“后台服务”一步改为按钮；主页和设置“高级”的升级、卸载入口；密码框文字。
  - **10.3**：应用里的卸载入口这一版有了（预览只读、确认后经管理员授权运行已安装的卸载脚本；不再等 M5-3）；删掉“应用里的卸载入口还没有提供”的说法。
  - **11.1**：`/v1/status` 的四个新字段；`GET /v1/uninstall/preview`。
  - **14**：M6 一行补“原地升级、应用里安装”。
  - **16**：v0.71 修订记录，照录用户原话“1”和“我不想每次安装都执行这么多命令，太啰嗦了”；注明“回国确认里选第四道”顺延到 v0.72。
- `docs/m6-acceptance.md`：第 1 节第 1 行改依据和“弱在哪里”（一条命令构建、应用里点一下安装和升级、保留主代理；仍要从源码构建、没有签名）；第 7 节加这一版的安全边界（3.7）。
- `docs/emergency.md`：升级失败且退回也失败时（退出码 21）怎么办。
- `docs/PROGRESS.md`：取消本机自用 pkg 及原因；阶段 A、B 的进度。
- `scripts/m6/install/README.md`、`scripts/dev/README.md`。

---

## 5. 风险和拿不准的地方

1. **卸载预览的权限**（第 0 节第 1 条）：现有 `POST /v1/actions/uninstall` 不给应用出预览。新只读接口里的预览和确认之后真正执行时之间没有确认码绑定（执行时 `abroad --yes uninstall` 会以当时的情况再算一遍），预览可能和实际结束的进程略有不同。替代：不做只读接口，应用只显示固定的说明文字。请顾问定。
2. **真机验证“已启用被拒绝”要先启用**（第 0 节第 3 条），而启用是用户暂不测的 R1、R2。
3. **停服务的行为**：计划假定 abroadd 正常退出（`stopInternals`）时不撤回国拦截、不释放 pf 启用引用、不改状态文件；`d.release()` 释放的是什么要在实现第一步读代码确认，`TestStopStartKeepsState` 守着。如果事实不是这样，停下来问顾问（可能要改成升级时 `kill -9` 走崩溃恢复，或者在 abroadd 里加“为升级而退出”的路径）。
   **顾问结论（v2）**：`daemon.go` 的 `Shutdown` 不恢复网络、不清 pf、状态文件不变；`d.release()` 释放的是进程锁；pf 启用引用是内核 token，不随进程退出释放——假定成立，`TestStopStartKeepsState` 照写作守护。实现时发现：已停用状态下 utun 是进程持有的设备，abroadd 退出时随之消失，重启时按现有恢复路径重建，名字可能变（例如 utun7 → utun8），状态文件的 `utun` 和锚点里的接口名随之变；测试对已停用只允许这一个名字不同，其余逐字相同。
4. **检查和停服务之间的窗口**：用户正好在这几秒点了启用。靠崩溃恢复和“状态和升级前不一样就退回”兜住；退回之后状态可能是“启用中断后回滚”的结果。没有加锁（加锁要改状态机，超出这次范围）。
5. **`NSAppleScript` 的线程**：苹果文档对它在非主线程执行的说法不一；放在串行队列上执行要真机确认不出问题，不行就退回主线程执行（期间界面不响应，先显示“正在安装，请输入密码……”再调用）。`do shell script` 在本进程内执行，预期不触发“自动化”权限弹窗，也要真机确认。
6. **`do shell script` 的环境**：没有终端（脚本不能读 `/dev/tty`，所以有 `--yes`）、`PATH` 只有系统目录、没有 `SUDO_USER`；脚本用到的 `curl`、`/usr/bin/python3`、`launchctl`、`dscl` 都在系统目录。`/usr/bin/python3` 在没装命令行工具的机器上会弹安装框（现有安装脚本同样如此）。输出只在结束后一次拿到，进行中应用只能显示“进行中”。
7. **应用包里放 Mach-O 程序**：`codesign --force --sign -` 加 `--verify --strict` 对 `Resources/` 下的可执行文件是否报错要实测；报错时改放 `Contents/Resources/payload` 之外的位置（例如 `Contents/SharedSupport/payload`），设计里按实际位置写。`abroad-vm` 自带的虚拟化授权签名在打包时不能被覆盖（不用 `--deep`）。
8. **`/Applications` 里现有的应用是 root 的**（老安装脚本以 root 复制的）：`build-app.sh` 以用户身份替换不了，过渡办法是升级时由 `install.sh --upgrade` 替换（记录里有 `app` 时），之后用户自己的构建再接管。卸载时应用那一行的快照对照改为只在记录里有 `app` 时参与，避免“应用后来才放进去”导致卸载报不一致。
9. **版本只比构建时间**：开发时检出旧提交重新构建会被当成“更新”。这是开发版的取舍；状态格式版本号挡住真正危险的情况（旧程序读不懂新格式）。
10. **`StateFormat` 靠人记得改**：`TestStateFormatPinned` 只能钉住有版本常量的格式；没有版本号的落盘文件（如果有）改了结构不会被发现。实现时把状态目录下的文件逐个列出，没有版本号的写进风险或补版本号（补版本号本身算一次格式变化，要问顾问）。
11. **应用包大小**：镜像进了应用包，`/Applications` 里多一份几十到上百 MB；仓库 `out/`、`app/.build/bundle/` 和 `/Applications` 各一份。
12. **安装中途应用被退出**：`do shell script` 的子进程是否随应用退出被结束不确定；脚本的 `trap` 覆盖 HUP/TERM，被结束时走自动卸载（全新安装）或退回（升级）；`kill -9` 时下次 `--upgrade` 看到 `upgrading` 标记会接着退回（2.4），全新安装看到 `installing` 标记会提示先卸载（现有逻辑）。

---

## 6. 实施顺序

**阶段 A**（交一次验收）：① `internal/buildinfo` + `abroadd --version` + `integrity-build.sh`（`build.json`）→ ② 状态接口四个字段 + apigen + `TestStopStartKeepsState`（先读 `stopInternals` 和 `d.release` 确认风险 3）→ ③ `payload.sh` → ④ `lib.sh` 的状态、比较、注入钩子 → ⑤ `install.sh` 的 `--upgrade`/`--from`/`--yes`/退回、安装目录的 `scripts/` → ⑥ `uninstall.sh` 的 `--yes` 和快照里的应用一行 → ⑦ 自测 i–s、`commands.txt` → ⑧ `reinstall.sh` 和它的自测 → ⑨ 文档（设计 11.1、11.4、14、16 的 A 部分，README，emergency）→ 全量验证。

**阶段 B**（再交一次验收）：① `GET /v1/uninstall/preview`（等顾问定第 5 节第 1 条）→ ② `app-bundle.sh --payload`、`build-app.sh` 和自测 → ③ `ServiceInstall.swift`（命令、转义、模型）和测试 → ④ `PrivilegedRunner.swift` → ⑤ 界面和文字表 → ⑥ 源码检查和反例 → ⑦ 自测场景 t → ⑧ 文档（设计 10.3、11.2、11.4 的 B 部分、16，验收清单第 1 行和第 7 节，进度单）→ 全量验证。

每个阶段交验收时附：`git diff --stat`、`go test`、`swift test`、两份自测、`check-commands.sh`、`privacy-check.sh --commit` 的输出。

---

## 7. 阶段 A 实现中的偏离和补充（v2）

- `TestStateFormatPinned` 和 golden 放在 `internal/daemon`（不是计划写的 `internal/buildinfo`）：要列 daemon 自己的未导出落盘结构（`setupFile`、`claudeSwitchFile`、`appliedRecord`、`lanConfirm`）；其他包的未导出结构由各包一个 `PersistedFormats()`（文件名 → 零值，只给这个测试用）交出来：`resolverlock`、`claudecfg`、`claudedomains`、`envcheck`、`rules`。
- 结尾的 `RESULT=<退出码>` 一行不在 `install.sh` 里打，留给阶段 B 应用的那条管理员命令（计划 3.4 里已经有 `echo "RESULT=$rc"`），避免两处重复。
- 升级只对新放的几项 `chown -R`/`chmod -R`（全新安装是整个安装目录）：状态目录里 `vm/` 属 `_abroad`，整个 `chown -R` 会改坏它。
- 命令行链接不在升级里改：安装目录的位置没变，链接一直指得对。
- `--from` 下全新安装时日志目录还没建，先只写标准输出（升级时写 `/var/log/claude-abroad/upgrade-<时间>.log`）。
- `reinstall.sh`：装过（安装目录在）→ 构建三步 + `sudo install.sh --upgrade`；只剩快照 → 构建三步 + 卸载 + 检查 + 安装；没装过 → 构建三步 + 检查 + 安装。升级的退出码 10/11/13/14/20/21 各有对应的下一步提示。
- 状态目录下的文件和 Go 类型（`state/abroadd` 是 `--state-dir`，`state/store` 是 `--main-proxy-store`，`state/vm` 只有运行时的 socket、pid、控制台日志）：

| 文件 | 编码 | Go 类型 |
|---|---|---|
| abroadd/state.json | json | `state.File` |
| abroadd/region-pending.json | json | `state.RegionPending` |
| abroadd/baseline.json | json | `exitcheck.Baseline` |
| abroadd/region.json | json | `region.Target` |
| abroadd/rules.yaml | yaml | `rules.file` |
| abroadd/setup.json | json | `daemon.setupFile` |
| abroadd/allow-lan.json | json | `daemon.LANSetting` |
| abroadd/claude-proxy-switch.json | json | `daemon.claudeSwitchFile` |
| abroadd/guard-params.json | json | `guard.Params` |
| abroadd/resolver-lock.json | json | `resolverlock.record` |
| abroadd/claude-code-settings.json | json | `claudecfg.record` |
| abroadd/claude-domains.json | json | `claudedomains.fileFormat` |
| abroadd/envcheck.json | json | `envcheck.settingsFile` |
| abroadd/envcheck-history.json | json | `envcheck.runHistFile` |
| abroadd/ip-history.json | json | `envcheck.histFile` |
| abroadd/secrets/exit-<标签>.json | json | `daemon.StoredExit` |
| store/proxy/sources.json | json | `managedproxy.Store` |
| store/proxy/applied.json | json | `daemon.appliedRecord` |
| store/proxy/lan-confirm.json | json | `[]daemon.lanConfirm` |
| store/proxy/pending.json | json | `map[string]string` |
| abroadd/prepared、abroadd/uninstalled | 文本 | 一行 RFC3339 时间 |
| abroadd/return-block-set | 文本 | 一行 `pf.ReturnBlockSet` |
| abroadd/ip-history.key | 二进制 | 出口 IP 历史的密钥 |
| abroadd/state.lock | 锁 | 没有内容 |

  运行目录 `/var/run/claude-abroad` 下的 `launchguard-boot`、`pf-loaded`、`guard-pf-tokens` 开机时清空，不算状态目录。fake-IP 映射的落盘（`fakedns.Pool.SaveFile`）现在没有接到 abroadd，不在状态目录里。
