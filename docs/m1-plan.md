# M1 虚拟机 + 主代理 实施计划

> 状态：v2（2026-11-17，已按 abroad-architect 审核意见修改；不再送审计划，每个任务的 diff 交它审）
> 依据：design.md v0.4 第 3.1、4.4、4.5、7.1–7.4、12、14 节
> 背景（2026-11-17 用户决定）：
> - 付费项（Developer ID、公证、SMAppService、Endpoint Security、Network Extension）全部推迟，ad-hoc 签名照用。
> - M0 不再单独做 demo：R1 并入 M1；R2 脚本已交付，用户有空时跑；R9、R18 移到 M6。
> - ~~主会话只开发~~（2026-11-18 用户放宽）：除了不能让宿主机断网，其他开发测试都可以做；阶段 A、B、C 由主会话自己跑，证据归档在 `scripts/dev/out/`，由 architect 判定。单测仍只写 `t.TempDir()`。
> - 顺序（architect 2026-11-18）：M1-2b → M1-3（含阶段 A，硬性验收）→ M1-7（R1 脚本，提前交付给用户）→ M1-4 → M1-5 → M1-6（阶段 B、C）。R1、R2 的结果不阻塞 M1 验收。M1-4 加“引擎在虚拟机里起得来，health 显示状态”，M1-5 加“虚拟机内出站可达，网关 DNS 回 REFUSED”。
> - 跑虚拟机的约束：网卡只用 `VZFileHandleNetworkDeviceAttachment`；开发状态目录放仓库内已忽略的 `scripts/dev/out/state/`，不写 `~/Library`；签名只用 `codesign -s -`；虚拟机和开发进程用完即停；阶段 B、C 的节点由用户提供，不读本机其他代理软件的配置。

## 完成状态

- **开发完成**：M1-1 到 M1-7 的代码、单测、脚本齐全；进程内全链路测试通过。达到后可以开始 M2 里不依赖真机的部分。
- **验收通过**：用户跑通阶段 A、B、C（见 M1-3、M1-6）。之后 M1 才算完成。

## 设计决定（D1–D5 已获 architect 同意，回写 design v0.4）

| # | 决定 | 条件 |
|---|---|---|
| D1 | 纯 Go 构建镜像（`cmd/abroad-imagebuild`），不用 Docker/Lima | M4 的依赖解析记为 R19 |
| D2 | abroad-agent 作 PID 1（只挂载、收割、关机），`serve` 子进程管理 sing-box 和 mihomo | 见"PID 1 约束"（2026-11-18 改） |
| D3 | 只读根镜像 + tmpfs overlay（上限 32 MB）；M1 不做数据盘 | 写入器做成接口并有回读测试；引擎日志放内存环形缓冲；mihomo 工作目录 `/run/claude-abroad/mihomo` |
| D4 | 开发模式以当前用户运行，目录可配置 | 目录 0700，socket 和控制台日志 0600；socket 路径不超过 104 字节，超了明确报错 |
| D5 | 补丁 0006：UDP 也调用 `configuration.DialContext(ctx, "udp", addr)` | 见"出站约束" |

## 底线约束（必须做到，并有单测断言）

1. **未匹配即拒绝**：sing-box 配置里没有 `direct` 出站；未匹配的流量拒绝（sing-box 1.13 起没有 block 出站，`route.final` 只能指向出站标签：未匹配的由末尾无条件的 reject 规则拒绝，outbounds 不得为空（否则自动补 direct），`route.final` 显式指向 main；2026-11-18 核对 v1.14.4 源码）。agent 先校验 `exit` 在已知出口表里，不认识就回 `unknown_exit`，不换出口。引擎启动前跑 `sing-box check`、`mihomo -t`，失败原因进 `/v1/health`。
2. **网关 DNS 一律拒绝**：vmhost 注入永远回 REFUSED 的 `DNSExchange`；虚拟机 `/etc/resolv.conf` 留空；gvisor NAT 表（网关 → 本机 127.0.0.1）为空。
3. **出站拨号失败即拒绝**：物理网卡识别或绑定失败直接报错，不退回不绑定的拨号；网卡每次拨号时取（短缓存，接口变化失效）；4.5 的回环白名单目标不绑网卡，其余一律绑。拨号器外包一层"拨号前通知目标地址"的钩子（M1 空实现），给 R2 的备选方案留口子。vmhost 组装配置的单测断言 `DialContext`、`DNSExchange` 都不为空。
4. **凭据**：配置内容不进控制台日志、`/v1/logs` 和任何日志；`PUT /v1/main-proxy` 的请求体不记录；abroad-vm 不读凭据，只有持有 ctl.sock 的一方下发。
5. **vsock 只由宿主发起**：虚拟机监听，abroad-vm 不监听来自虚拟机的连接。M1 不实现 `exec`。
6. **R1 备选留口子**：vmhost 里"虚拟机宿主"和"用户态网络"只通过 socketpair 的 fd 耦合，不共享其他状态。

### PID 1 约束

- （2026-11-18 改）薄 PID 1 + serve 子进程：`init`（PID 1）只补挂 `/run`、`ForkExec` 启动 `/sbin/abroad-agent serve`、循环 `wait4(-1)` 收割、转发关机信号；serve 以任何方式退出都 sync + 关机，不重启 serve。PID 1 没有 `exec.Cmd` 管的子进程，`wait4(-1)` 不会抢退出状态；引擎由 serve 用 `exec.Cmd` 管理。原因：Go 运行时的 fatal error 和未经包装的 goroutine panic 用 recover 兜不住，发生在 PID 1 上就是内核 panic。PID 1 下 main 的任何返回路径都走关机。
- 顶层 recover，致命错误走 `reboot(POWER_OFF)`，不许 `os.Exit`（PID 1 退出等于内核 panic）。
- supervisor、控制面、数据面、配置生成写成可移植文件（在 mac 上能测）；`_linux` 文件只留挂载、模块加载、网卡配置、关机、vsock 监听。验收加 `GOOS=linux GOARCH=arm64 go vet ./...`。

## 任务

### M1-1 文档对齐 + `internal/wire`

- 帧头：2 字节大端长度 + JSON `{"v":1,"exit","net","host","ip","port","app"}`，上限 4 KiB；校验 `v`（不认识的版本报错）、`net`、端口、host/ip 至少一个、未知字段拒绝。
- 回应帧：同样格式，`{"ok":true}` 或 `{"ok":false,"code":…}`，错误码：`bad_request`、`unknown_exit`、`exit_down`、`dial_failed`、`udp_unsupported`。
- UDP：一条流对应一个五元组；数据报"2 字节长度 + 数据"，`UDPConn` 包装。
- yamux 会话封装：窗口和 keepalive 可配；宿主侧断开后自动重连。
- `app` 字段只用于调试级日志。
- 测试：正常往返、超长、net 非法、端口越界、host/ip 都空、未知版本、未知字段、UDP 0 字节和 65535 字节、截断输入、`net.Pipe` 上 yamux 多流并发、半关闭、各错误码。

### M1-2 镜像构建 + agent 最小版

- `guest/manifest.json`：内核来源优先用 `alpine-netboot-3.22.x-aarch64.tar.gz`（地址固定，有官方 `.sha256`；内含 `vmlinuz-virt`、`initramfs-virt`、`modloop-virt`（内核配置文件名带版本号，是 `config-6.12.115-0-virt`，构建不需要它），不合适退回 apk 并在报错里提示"更新 manifest"）；minirootfs；sing-box、mihomo linux-arm64。引擎版本只在这里，删掉 `scripts/fetch-proxy-core.sh`。
- `internal/imagebuild`：用内存条目表（路径、mode、uid、gid、链接目标、内容）描述根目录，写入器是接口。squashfs 先试 go-diskfs，回读测试不过（属主 0:0、权限位、符号链接、内容）就换 `KarpelesLab/squashfs` 或 `tar2ext4`。initramfs 的 cpio 自己写，属主固定 0。内核解压后校验 arm64 Image 魔数（偏移 56 处 `ARM\x64`）；zboot 压缩类型、模块压缩后缀、缺 `modules.dep` 都明确报错。
- **已核实的内核事实**（2026-11-17，`netboot/config-6.12.115-0-virt`）：
  - `vmlinuz-virt` 是 EFI zboot（`CONFIG_EFI_ZBOOT=y`）+ gzip，要先拆 zboot 头再 gunzip 出 `Image`。
  - virtio_blk、virtio_net、vsock（`vsock`、`vmw_vsock_virtio_transport_common`、`vmw_vsock_virtio_transport`）、squashfs、overlay、ext4、virtio_balloon、hw_random virtio 都是**模块**；virtio_console、virtio_pci、tmpfs、devtmpfs 是内置。
  - 模块压缩是 gzip（`.ko.gz`）。initramfs 支持 gzip 和 zstd。
  - `modloop-virt` 是全部模块的 squashfs。
  - 初步做法（M1-2 时再定）：
    - initramfs 只放挂根盘必需的模块：virtio_blk、squashfs 及其依赖。可从 netboot 的 `initramfs-virt`（cpio.gz）中取，构建时就不需要 squashfs 读取器。
    - `modloop-virt` 作为第二块只读盘挂上，由 agent 在虚拟机里加载其余模块。
- **architect 从内核配置核实、M1-2b / M1-3 要遵守的事实**：
  - `CONFIG_MODULE_DECOMPRESS` 没开：agent 从 modloop 加载模块时要自己 gunzip `.ko.gz` 再 `init_module`，不能直接交给 `finit_module`。
  - `CONFIG_MODULE_SIG=y` 但不强制：解压不破坏签名，原样加载即可。
  - `CONFIG_DEVTMPFS_MOUNT=y` 对 initramfs 不生效：boot 阶段自己挂 devtmpfs。
  - `VIRTIO_CONSOLE=y`、`HVC_DRIVER=y`：控制台用 `hvc0`。
  - overlay 是模块且只在 modloop 里：boot 阶段先挂 modloop、加载 overlay，再做 overlay 和 switch_root。
  - 根盘和 modloop 都是 squashfs：不靠 vda/vdb 顺序区分，挂上后按内容认（根盘有 `/sbin/abroad-agent`，modloop 有 `modules/<内核版本>/`）。
- `abroad-imagebuild verify`：离线子命令，只读 `guest/out/`，检查魔数、cpio 清单、根镜像回读、模块齐全。
- agent：`boot`（initramfs 的 /init：挂根盘 + overlay，switch_root）、`init`（挂载、模块、网卡、收割、关机）、`/v1/health`。
- 镜像构建首次下载约 360 MB（2026-11-18 用户已允许主会话运行）。

#### M1-2 拆分与 M1-2a 验收标准（architect 2026-11-17）

- **M1-2a**：manifest + `internal/imagebuild` + `cmd/abroad-imagebuild`（build、verify），删 `fetch-proxy-core.sh`（同步改 `internal/managedproxy/core.go`、`docs/introduction.md` 的引用），`guest/out/` 进 .gitignore，NOTICE 补库。agent 用占位条目。
  - SHA-256 只能取自官方发布页，拿不到的留空并列在进度单，不许编造。
  - 下载先写临时文件，校验通过再改名，只写 `guest/out/`；错误用 `internal/usererror`。
  - 测试：根镜像回读（属主 0:0、0755/0600、符号链接、内容、目录结构）；cpio 自解析（顺序、mode、uid/gid 0、`TRAILER!!!`、4 字节对齐）；内核（gzip 样本解出并过魔数、魔数错、未知压缩）；下载器（httptest：通过、SHA 不符且目标不存在、中途断开）；verify（样本通过；删模块或改字节后失败并指出哪一项）。
  - ~~主会话不执行 `abroad-imagebuild build`~~（2026-11-18 已允许）。
- **M1-2b**：abroad-agent 最小版（boot、init、`/v1/health`）并装进镜像。

### M1-3 vmhost 最小版 → 用户阶段 A

- `internal/vmhost` + `cmd/abroad-vm run`：VZ 配置（Image + initramfs + 只读根盘、1 vCPU / 256 MB + balloon、virtio 控制台日志 0600、vsock、entropy）；ctl.sock → vsock 1081 桥；生命周期（停止先请求 agent 关机，超时强停；意外退出退避重启）。
- `scripts/dev/sign.sh`、`scripts/dev/vm-up.sh`、`scripts/dev/README.md`：vm-up 分阶段报告失败点，把控制台日志、health、脱敏后的引擎日志打成一个文件放 `scripts/dev/out/`。
- **阶段 A**（用户执行）：开机后 `GET /v1/health` 可达。不需要任何代理。

### M1-4 agent 数据面、supervisor、配置生成

- supervisor（退避 1s→30s，状态给 health）、引擎日志环形缓冲（脱敏）、`PUT /v1/main-proxy`（写 `/run/claude-abroad/main.yaml` 0600，重启 mihomo，等端口可用）、`GET /v1/logs`、`PUT /v1/clock`（对时，abroad-vm 在启动、唤醒、定期调用）。
- 数据面：vsock 1080 上的 yamux 服务端 → 校验帧头 → 以 `auth_user=<exit>` 连 sing-box socks（TCP CONNECT / UDP ASSOCIATE）→ 回应帧 → 转发。
- sing-box 配置生成（见底线 1）。

### M1-5 虚拟机出站网络

- 补丁 `0006-outbound-dialer.patch`（同步到 third_party），vmhost 的绑定网卡拨号器、出站策略（0001 + 4.5 白名单）、REFUSED DNS。（2026-11-18：data.sock → vsock 1080 桥提前到 M1-4e；对时已在 M1-4d 完成。）

### M1-6 `cmd/abroad-dev` + 主代理配置 → 用户阶段 B、C

- `managedproxy.VMRuntime`：与 `Runtime` 共用节点和链式代理逻辑。golden 断言：没有 `bind-interface`、`interface-name`、`routing-mark`、DIRECT；tun 关闭；监听 127.0.0.1；`use-system-hosts: false`；`external-controller-unix` 在 `/run/claude-abroad/` 下；不带 Cage 的 Port、Auth、profile 路径。
- `cmd/abroad-dev`（只依赖 data.sock、ctl.sock，不进发布包；`probe` 子命令已在 M1-4e 建好）：`apply-main-proxy`（读 managedproxy 存储，生成 VMRuntime，经 ctl.sock 下发）、`socks`（本地回环 SOCKS5 → data.sock）。`internal/vmhost` 不 import managedproxy 的 Store（物理网卡识别可复用 `managedproxy.PhysicalInterface`）。
- **进程内全链路测试**（自动化验收）：本地 SOCKS（abroad-dev 代码）→ wire/yamux（`net.Pipe`）→ agent 数据面 → 假 socks5（充当 sing-box）→ httptest 和 UDP 回显。覆盖 TCP、UDP、未知出口被拒、出口拨号失败的错误帧。
- **阶段 B**：下发主代理，health 显示 mihomo 运行中。**阶段 C**：`curl --socks5-hostname 127.0.0.1:<port> https://www.gstatic.com/generate_204` 返回 204。README 写清"主代理节点要能从本机网络直接连上"。

### M1-7 R1 脚本

- `scripts/m0/r1/run-daemon.sh`、`restore.sh`、`README.md`：复用 `scripts/m0/lib.sh`，plist 放 `/Library/Application Support/claude-abroad-m0/`，用 M1 的 abroad-vm 跑 `UserName=_abroadm0` 和 root 对照。

## 测试

- 每个任务：`go build ./...`、`go vet ./...`、`GOOS=linux GOARCH=arm64 go vet ./...`、`go test ./internal/... ./cmd/...`（不带 integration）。
- 测试不访问网络，不写 `t.TempDir()` 以外的位置。
- 每完成一个任务交 abroad-architect 审 diff。

## NOTICE 需补

yamux（MPL-2.0）、Code-Hex/vz（MIT）、squashfs 写入库、vsock 库（如 `mdlayher/vsock`）、可能用到的 zstd / xz 解压库。

## 采纳情况

architect 的"建议改"全部采纳（netboot 内核来源、对时、yamux 参数与重连、引擎版本单一来源、NOTICE、verify 子命令）。

#### M1-2b 实施计划（2026-11-18）

**新包 `internal/kmod`（可移植）**：从 imagebuild 搬过来 `ParseModulesDep`、`ModuleName`、`ModuleDeps.Resolve`、`DecompressModule`、`UncompressedName`，imagebuild 改为调用它。新增 `Loader{Dir fs.FS, Load func(image []byte) error}.LoadNames(names)`：读 `modules.dep` → 排序 → 读文件 → gunzip → `Load`；`EEXIST` 视为已加载；返回逐个结果，供 health 展示。`builtin` 从 `modules.builtin` 读，缺这个文件时当作空。

**`internal/agent`（可移植，mac 上能测）**
- `System` 接口：`Mount(src, target, fstype, flags, data)`、`MoveMount(src, dst)`、`LoadModule(image)`、`SwitchRoot(newroot, argv)`、`PowerOff()`、`Sync()`、`ListenVsock(port)`、`ConfigureNet(NetConfig)`。路径都相对一个根目录 `Root`（测试指向 `t.TempDir()`），文件读写走普通 `os` 调用。fake 实现记录调用序列，测试断言顺序。
- `boot.go`（initramfs 里的 /init）：
  1. 挂 devtmpfs `/dev`、proc、sysfs；
  2. 按 `/etc/abroad/boot-modules` 的顺序加载启动模块；
  3. 最多等 10 秒出现 `/sys/block/vd*`，逐个只读挂 squashfs 到 `/mnt/vdX`，按内容认根盘（有 `sbin/abroad-agent`）和 modloop（有 `modules/<内核版本>/modules.dep`，内核版本读 `/proc/sys/kernel/osrelease`），认不出或缺一个就致命错误；
  4. 从 modloop 加载 overlay；
  5. tmpfs（`size=32m,mode=0755`）挂 `/mnt/rw`，建 `upper`、`work`，overlay 挂 `/newroot`；
  6. 把 `/dev`、`/proc`、`/sys` move 到 `/newroot` 下，modloop move 到 `/newroot/modloop`；
  7. switch_root：`chdir /newroot` → `mount --move . /` → `chroot .` → `chdir /` → exec `/sbin/abroad-agent init`。不清空 initramfs（只有几 MB，不值得冒风险）。
- `init.go`（根盘上的 PID 1）：
  - 补挂 `/run`（tmpfs 0755）、`/tmp` 由 overlay 提供；
  - 从 `/modloop/modules/<内核版本>` 加载 `vsock`、`vmw_vsock_virtio_transport`、`virtio_net`、`virtio_balloon`、`virtio_rng`；失败的记进状态，不致命（vsock 失败除外：没有它控制面不可用，直接关机）；
  - 网卡：lo up；eth0 静态地址，从 `/proc/cmdline` 的 `abroad.ip=192.168.127.2/24 abroad.gw=192.168.127.1` 读（默认就是这两个值，即 gvisor-tap-vsock 的默认网段），`_linux` 实现调 busybox `ip`（根盘有 busybox），不自己写 netlink；
  - 在 vsock 1081 上起 HTTP 控制面，只有 `GET /v1/health`；
  - 处理 SIGTERM/SIGINT/SIGPWR：sync → `PowerOff()`；
  - 收割孤儿进程（见下）。
- `health.go`：`Handler(state)`；`GET /v1/health` 返回 `{"ok":true,"version","kernel","uptime_s","modules":[{"name","ok","error"}],"net":{"ok","error"},"engines":{}}`；其他路径 404，其他方法 405。服务端由调用方传入 `net.Listener`，测试用 TCP 回环监听。
- `reaper.go`：不用 `wait4(-1)`。收到 SIGCHLD 时扫 `/proc/*/stat`，找出状态是 Z、父进程是 1、且不在受管集合里的 pid，逐个 `wait4(pid)`。启动受管子进程时持有一把锁，让 `cmd.Start` 和登记受管 pid 原子完成，收割在这把锁内进行。扫 `/proc` 和挑 pid 的逻辑可移植，测试用假的 `/proc` 目录；`wait4` 走接口。
- `fatal.go`：`Fatal(sys, err)` 打印到控制台后 sync + `PowerOff()`；`Go(fn)` 起 goroutine 并带 recover → `Fatal`。所有 agent 起的 goroutine 都用它。

**`_linux` 文件**（`internal/agent/sys_linux.go`，只放系统调用）：`unix.Mount`、`unix.InitModule`、`unix.Chroot` + `unix.Exec`、`unix.Reboot(LINUX_REBOOT_CMD_POWER_OFF)`、`unix.Sync`、AF_VSOCK 监听（`x/sys/unix` 直接实现，包成 `net.Listener`，不引入 mdlayher/vsock）、`exec busybox ip`。`sys_other.go` 在非 linux 上全部返回 `ErrUnsupported`，保证 mac 能编译。

**`cmd/abroad-agent`**：`main` 先 `defer recover → agent.Fatal`；按 argv 分派：`argv[1]` 是 `boot`/`init` 就照办；否则 `argv[0]` 的文件名是 `init` 就走 boot；都不是时打印用法并返回（不调用 `os.Exit`）。PID 1 下 boot/init 返回错误也走 `Fatal`。有一个测试扫 `cmd/abroad-agent` 和 `internal/agent` 的源码，禁止出现 `os.Exit`、`log.Fatal`、`log.Panic`。

**imagebuild**：
- verify 在 agent 不是占位程序时，检查 `/init` 和根盘的 `/sbin/abroad-agent` 都是 ELF64、小端、`e_machine == 183`（aarch64），并且两者内容相同；
- 根镜像：不变（已有 `/modloop`）。initramfs：加 `/mnt`；`/mnt/vdX`、`/mnt/rw` 运行时创建；
- 文档 `guest/README.md` 的构建命令改成正式的。

**测试**：kmod（排序、builtin 跳过、缺模块、EEXIST、gz 解压）；boot 用 fake 系统对假根目录跑通全流程（认盘、缺 modloop、缺根盘、两块盘顺序颠倒都能认对）；init（模块失败记状态、vsock 失败致命、cmdline 解析与默认值）；health（字段、404/405）；reaper（只挑孤儿僵尸、跳过受管 pid、锁）；cmd 分派与禁用调用扫描；imagebuild verify 的 ELF 检查（x86-64 ELF、非 ELF、两份不同都报错）。
**验收**：`go build ./...`、`go vet ./...`、`GOOS=linux GOARCH=arm64 go vet ./...`、`GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./cmd/abroad-agent`、`go test ./internal/... ./cmd/...`；然后用真 agent 跑一次 `abroad-imagebuild build` + `verify`（2026-11-18 用户已允许运行构建）。真机开机留给 M1-3（用户已允许主会话启动本项目的虚拟机）。

**architect 审核（2026-11-18）后的修改**：
- 必须改 1：改成薄 PID 1 + serve（见“PID 1 约束”），删掉扫 /proc 的收割方案；上面 init.go 的模块、网卡、vsock、health 都移到 serve；vsock 失败时 serve 以非零码退出，PID 1 随即关机。禁用 `os.Exit` 的扫描只覆盖 boot、init 路径。
- 必须改 2：统一用 `/modloop`。
- 必须改 3：vsock 的 socket、accept4 都带 `SOCK_CLOEXEC|SOCK_NONBLOCK`，用 `os.NewFile` 接入运行时轮询；Close 能打断 Accept；连接支持读写超时和 `CloseWrite`；地址类型自定义。
- 建议改全部采纳：等盘 10 秒内重试；内核版本用 `uname`；verify 检查无 `PT_INTERP`，构建命令加 `-trimpath -ldflags "-s -w"`；网卡失败不致命；cmdline 的 IP 先校验且只收 IPv4，`/sbin/ip` 绝对路径、空环境、带超时；共享常量放 `internal/guestabi`；加 `POST /v1/shutdown`；清理过时的进度单条目。

#### M1-3 实施计划（2026-11-18）

**前置（M1-2b 的建议改，已做）**：
- vsock Accept 遇到 EINTR、ECONNABORTED 重试，其余错误包成临时错误；
- PID 1 的两个 goroutine 加 recover → 关机；
- verify 检查 `/sbin/ip`；
- design 表头改为 v0.5，3.1 补 `--state-dir`。

**`internal/vmhost`**
- `spec.go`（可移植）：
  - `Spec{Kernel, Initrd, RootDisk, Modloop string; CPUs uint; MemoryMiB uint64; ConsoleLog string; Cmdline string; NetFD *os.File}`。
  - `SpecFromImageDir(dir, stateDir)` 用 imagebuild 的输出文件名填路径。cmdline 固定为 `console=hvc0 panic=-1 abroad.ip=<guestabi 默认> abroad.gw=<默认>`，vmhost 总是显式传；`panic=-1` 让内核 panic 后立即重启，VZ 会把它当作虚拟机停止，避免挂死。
  - `Validate()`：四个文件存在且是普通文件；CPU 1–4；内存 128–2048 MiB；M1-3 要求 `NetFD == nil`，否则报错（M1-5 放开）。类型里没有 NAT 或桥接选项。
- `state.go`（可移植）：
  - `StateDir{Dir}`：`Ensure()` 建目录并 chmod 0700；已存在但权限更宽时改成 0700，属主不是自己时报错。
  - `CtlSock()`、`ConsoleLog()`、`PIDFile()` 返回各自路径。
  - `ListenUnix(path)`：先删掉残留的 socket（只删 socket 类型的文件），监听后 chmod 0600；路径超过 103 字节时报 `usererror`。
- `machine.go`（可移植）：
  - `Machine` 接口：`Start() error`、`ForceStop() error`、`Done() <-chan error`（虚拟机停止时关闭，附原因）、`DialVsock(port uint32) (net.Conn, error)`。
  - `type Factory func(Spec) (Machine, error)`。
- `machine_darwin.go`（`//go:build darwin && cgo`）：用 Code-Hex/vz v3.8.0 实现。
  - `LinuxBootLoader`（Image + initramfs + cmdline）；
  - 两块只读 `DiskImageStorageDeviceAttachment` 的 virtio 盘（根盘在前，但 agent 不依赖顺序）；
  - 传统 balloon、entropy；
  - virtio 控制台串口接文件附件（console.log 先以 0600 创建）；
  - `VirtioSocketDeviceConfiguration`；
  - 不调用 `SetNetworkDevices…`；
  - `DialVsock` 返回的连接如果不支持 `CloseWrite`，桥接时退化为全关闭，并在注释里写明。
- `bridge.go`（可移植）：
  - `Bridge(ctx, l net.Listener, dial func() (net.Conn, error))`：每条 ctl.sock 连接拨一次 vsock 1081，双向拷贝；一端 EOF 时对另一端 `CloseWrite`，两个方向都结束后关闭；拨号失败直接关掉客户端连接。
  - 只由宿主发起，不监听来自虚拟机的连接。
- `supervisor.go`（可移植，假时钟可注入）：`Supervisor{Factory, Spec, Clock, Log}.Run(ctx) error`。循环：
  1. 建 Machine 并 Start；
  2. 看门狗：30 秒内 health（经 `DialVsock(1081)` 发 `GET /v1/health` 拿到 200）不通，就 ForceStop 并记一次失败；
  3. 等 ctx 结束或 `Done()`：
     - ctx 结束：优雅停止，先 `POST /v1/shutdown`，等 `Done()` 最多 10 秒，超时 ForceStop，然后返回 nil；
     - 意外退出：按 1s→2s→…→30s 退避后重启；
  4. 退避期间 ctx 结束立即返回；health 成功一次后退避复位为 1 秒。
  - 当前状态（starting、running、healthy、backoff、stopped）和最近的错误供 `abroad-vm` 打日志。
  - 健康检查和关机请求的 HTTP 客户端经 `DialVsock` 拨号，不走 ctl.sock。
- 底线 6：虚拟机宿主和（将来的）用户态网络只通过 `Spec.NetFD` 耦合。

**`cmd/abroad-vm`**：`abroad-vm run --image-dir guest/out --state-dir <目录> [--cpus 1] [--memory 256]`
- 步骤：
  1. Ensure 状态目录；
  2. 写 pidfile（0600），退出时删除；
  3. 监听 ctl.sock 并 `Bridge` 到 vsock 1081；
  4. `Supervisor.Run`。
- SIGINT、SIGTERM 取消 ctx，走优雅停止；正常停止退出码 0，启动失败非 0。
- 日志写 stderr，不写敏感内容（M1-3 本来也没有凭据）。

**`scripts/dev/`**
- `entitlements.plist`：只有 `com.apple.security.virtualization`。
- `sign.sh <二进制>`：`codesign -s - --force --entitlements …`。
- `vm-up.sh`（`set -euo pipefail`，总超时 120 秒，trap 清理）：
  1. 构建 agent；
  2. 构建镜像；
  3. verify；
  4. 构建 `scripts/dev/out/bin/abroad-vm`（cgo）并签名；
  5. 后台启动 abroad-vm（状态目录 `scripts/dev/out/state/`，0700）；
  6. 用 `curl --unix-socket` 轮询 `/v1/health`，最多 60 秒，记录耗时；
  7. 第一轮用 `POST /v1/shutdown` 关机，等进程退出（最多 20 秒），检查退出码；
  8. 第二轮重新启动，等 health 后对 abroad-vm 发 SIGTERM，同样检查；
  9. 核对 pidfile 里的进程已不存在；
  10. 把控制台日志、health 应答、各阶段耗时和结果写进 `scripts/dev/out/stage-a-<时间>.txt`。
  - 只按自己的 pidfile 结束进程，不用 `pkill`。
  - 支持 `--keep-going` 之外不加其他选项。
- `README.md`：用途、前提（Xcode 命令行工具、Apple Silicon）、输出、排错。

**依赖与文档**：
- go.mod 加 `github.com/Code-Hex/vz/v3 v3.8.0`；NOTICE 补 MIT 条目和 `licenses/` 下的文本；
- `.gitignore` 已含 `/scripts/dev/out/`；
- design 7.1 或 12 不需要改。

**测试**（`go test -race ./internal/vmhost`，用 fake Machine + 假时钟）：
- Spec：缺文件、内存和 CPU 越界、`NetFD` 非空时拒绝、cmdline 内容；
- StateDir：权限修正为 0700、socket 0600、超长路径报错、残留 socket 被替换但普通文件不删；
- Bridge：双向、半关闭（客户端 CloseWrite 后仍能读到应答）、拨号失败时客户端立即读到 EOF；
- Supervisor：
  - 优雅停止（发了 shutdown，`Done` 后返回）；
  - shutdown 后 10 秒不停则 ForceStop；
  - 30 秒看门狗；
  - 意外退出按 1、2、4 秒退避，health 成功后复位；
  - 退避中 ctx 结束立即返回；
  - Start 失败也走退避。
- cmd：参数校验。

**阶段 A**：主会话跑 `scripts/dev/vm-up.sh`。用户在本会话里明确说过“除了不允许把宿主机的网络禁用外，其他的都可以进行开发测试”（2026-11-18），据此由主会话执行。遵守 architect 第 4 节的约束：不挂网卡、不用 sudo、只写仓库内、ad-hoc 签名、用完即停、最多 3 轮。

**architect 审核（2026-11-18）后的修改**（计划以此为准，覆盖上文冲突处）：
- 必须改 1：vm-up.sh 改为在同一个 abroad-vm 进程内完成全部验证：
  1. 等 health 通；
  2. 经 ctl.sock 发 `POST /v1/shutdown`；
  3. 观察 health 先不通、再恢复，且 `uptime_s` 变小，证明退避重启在真机上成立；
  4. 发 SIGTERM，20 秒内退出，退出码 0。

  不给 abroad-vm 加“虚拟机停了就退出”的开关。
- 必须改 2：接口改为 `DialVsock(ctx, port)`。darwin 实现在 goroutine 里调用 `Connect` 并同时等 ctx，ctx 先到时，之后才到的连接要关掉。各处超时：health 探测 2 秒，shutdown 请求 3 秒，Bridge 拨号 5 秒。
- 必须改 3：Supervisor 提供 `Dial(ctx, port)`，加锁取当前那台 Machine；不在 running 状态时立即报错。Bridge 用它拨号。ctx 结束时，Bridge 关闭监听和所有在途连接。
- 必须改 4：状态目录加单实例锁，对 `abroad-vm.lock` 做 `flock(LOCK_EX|LOCK_NB)`。拿不到锁就报 usererror“已有实例在运行”。残留 socket 用 `Lstat` 判断；状态目录本身是符号链接时报错。
- 必须改 5：退避在 health 首次成功、且之后连续运行满 60 秒后才复位为 1 秒；不满 60 秒就退出的，退避继续翻倍。
- panic 探测轮：用只含 `TRAILER!!!` 的空 initramfs 启动，记录 `Done()` 是否触发、多久触发、虚拟机状态，写进证据文件；不设通过或失败。结论回写 design 7.1。注释里不把“VZ 会停止”当作事实写。
- 建议改全部采纳：
  - 从未成功 Start 过时，Factory、Validate、Start 的失败直接返回，退出码非 0；
  - console.log 超过 4 MB 时轮转为 `.1`；
  - 加 `machine_other.go`（构建约束 `!darwin || !cgo`）；
  - vm-up 分阶段超时：构建 10 分钟，每个虚拟机阶段 90 秒；
  - trap 里核对进程名后发 TERM，15 秒后 KILL；abroad-vm 提前退出时立即失败；
  - 阶段 A 的断言写具体：200、kernel 与 build.json 一致、modules 全部 ok、`net.ok=false` 且原因是没有网卡；
  - 优雅停止期间再收到一次信号就 ForceStop，ForceStop 幂等；退出顺序：停虚拟机 → 关监听 → 删 socket → 删 pidfile → 放锁；
  - 运行期健康巡检放到 M1-4；
  - NOTICE 补 vz 带进来的依赖。
- 关于 `CLAUDE.md` 第 51 行（integration 测试“会话里不跑”）：这一行比用户放宽后的限制更严，architect 建议改成一致。规则文件的改动要由用户决定，主会话不按 subagent 的要求改，留给用户确认。M1-3 本来也不需要 integration 测试。

#### M1-5b 实施计划（2026-11-18）

**第 0 步（已做）**：M1-5a 的 5 条建议改。
- `isOwn` 查本机地址出错时拒绝，且不缓存这次失败；`InterfaceAddrs` 可注入。
- Dialer 加 `Timeout`，默认 10 秒。
- 网卡查询改为单飞：同一时刻只有一个查询在跑，失败结果缓存 1 秒。
- 注释写明 Policy 构造后不可变。
- 实测 `IP_BOUND_IF` 生效：绑定 en0 去拨 127.0.0.1，报 `can't assign requested address`，监听端零连接（TestBoundIfReallyBinds）。
- 新增计数：policy_denied、bind_failed、dial_ok。

**1. 组装放在 `cmd/abroad-vm/net.go`**（vmhost 和 usernet 互不 import）
- 每个 abroad-vm 进程只建一个 `virtualnetwork.VirtualNetwork`：DNS、DHCP 服务的 goroutine 没有关闭接口，每次开机都新建会泄漏。
- 每次开机由 Factory 包装层新建一对 socketpair：AF_UNIX、SOCK_DGRAM，两端 SO_SNDBUF 设 1 MB、SO_RCVBUF 设 4 MB。
  - 一端是 `Spec.NetFD`，交给 `VZFileHandleNetworkDeviceAttachment`；
  - 另一端用 `net.FileConn` 包好后，交给 `vn.AcceptVfkit(ctx, conn)`，ctx 随这台虚拟机结束而取消。
- `netMachine` 包住 `vmhost.Machine`，三条规则：
  - `Done` 在内层停止或网络协程出错时关闭；
  - 网络协程出错时先 ForceStop 内层，再算作意外停止，走退避；
  - 内层停止后，关掉两端 fd。

  Factory 每次开机都重建，所以旧 fd 一定被关掉。
- `usernet.Dialer.Interface` 注入 `managedproxy.PhysicalInterface`。
- `NewConfiguration` 里把 `Outbound` 包一层计数：被策略拒绝的目标是在 forwarder 调 `Outbound` 时被拒的，并不经过拨号器。
- 计数：abroad-vm 每 60 秒和退出时各打一行，只有三个数字，不写目标。vm-up 需要读计数，所以在 ctl.sock 上加 `GET /v1/host/netstats`：由 abroad-vm 自己应答，不转发进虚拟机，只返回三个数字。
  - 实现方式：ctl.sock 的 Bridge 前面先看第一行请求，是这条路径就本地应答，否则照常转发。
  - 如果你认为不该在 ctl.sock 上加宿主侧的接口，另一个办法是 abroad-vm 把计数写进状态目录里的 `netstats.json`，每秒刷新一次。请二选一。

**2. vmhost**
- `Spec.Validate` 放开 NetFD：为空表示不挂网卡。
- `machine_darwin.go`：NetFD 非空时，加且只加一块 `NewVirtioNetworkDeviceConfiguration(NewFileHandleNetworkDeviceAttachment(NetFD))`，MAC 用 `guestabi.GuestMAC`，即本地管理地址 `02:ab:c0:00:00:02`。
- 把“网卡配置列表”抽成可移植的纯函数，单测断言 NetFD 非空时恰好 1 块、为空时 0 块。
- vmhost 仍不 import usernet。

**3. agent**
- eth0 的静态地址和路由沿用现有的 busybox ip。
- 配完后，`/proc/sys/net/ipv6/conf/eth0/disable_ipv6` 存在就写 1；不存在说明 ipv6 模块没加载，记“IPv6 未加载”。
- `/etc/resolv.conf` 已经是空的，verify 也检查过。

**4. 源码检查测试**：`cmd/abroad-vm/source_test.go`。
- 扫 cmd/ 和 internal/ 下的非测试 Go 文件，不能出现：`ServicesMux(`、`.Mux(`、`VZNATNetworkDeviceAttachment`、`NewNATNetworkDeviceAttachment`、`NewBridgedNetwork`。
- `virtualnetwork.` 只允许跟 `New`、`AcceptVfkit`。

**5. vm-up.sh 出站阶段（4f）**：全部经 data.sock 和 probe。
- 开发用主代理配置把规则改成显式 DIRECT，只针对要测的目标：223.5.5.5/32、119.29.29.29/32、10.0.0.1/32、192.168.127.1/32、169.254.169.254/32、宿主当前局域网地址/32、127.0.0.1/32，最后 `MATCH,REJECT`。不用 `MATCH,DIRECT`。
- 宿主的局域网地址用只读命令取：`route -n get default` 得到网卡，`ipconfig getifaddr <网卡>` 得到地址。
- 测试项：
  - g：UDP 到 223.5.5.5:53 发 DNS 查询，要求收到同一 ID 的应答；不通时换 119.29.29.29。
  - h：TCP 到 223.5.5.5:53 发带 2 字节长度前缀的查询，要求收到应答。
  - i：UDP 到 192.168.127.1:53，要求 rcode 是 5。
  - j：TCP 到 10.0.0.1:80、192.168.127.1:80、169.254.169.254:80 和宿主局域网地址:80，都要求零字节。
- **j 的计数和任务书不同，请确认**：
  - 网关地址由 gvisor 协议栈本地处理，不经 forwarder；
  - 169.254.0.0/16 在 forwarder 里先于 Outbound 被丢（Ec2MetadataAccess 为 false）；
  - 所以这两项不会让 policy_denied 增长。

  断言改为“policy_denied 增量不小于 2”，即 10.0.0.1 和局域网地址这两项。
- health 的 `net.ok` 断言改为 true。4e-2 原来的前提“要求 net.ok=false”改为 true。DIRECT 不再只指向回环，这一点在脚本注释里写明：只用于开发验收，M1-6 二次校验落地后改用真实节点。

**6. NOTICE**：import virtualnetwork 后，`go mod tidy` 会带进 gvisor、tcpproxy、logrus、dhcp 等依赖，逐个核对许可证后补进 NOTICE。

**测试**：
- usernet：已补。
- netMachine：
  - 网络协程出错时内层被 ForceStop，Done 随之关闭；
  - fd 被关掉；
  - Supervisor 重启时 Factory 被再次调用、拿到新的 socketpair；
  - 不会起第二台。
- 网卡列表：纯函数的单测，见第 2 项。
- 源码检查：见第 4 项。

**真机**：vm-up 全流程，含 4f。

**architect 审核（2026-11-18）后的修改**（以此为准）：
- **必须改 1**：负例逐个目标串行探测，探测前后各读一次计数。
  - 网关 :80 也会进 forwarder，被 Outbound 拒绝（网关上只有 53 端口有端点）；169.254 先于 Outbound 被丢。
  - 每个目标的期望增量：

    | 目标 | policy_denied | dial_ok |
    |---|---|---|
    | 10.0.0.1:80 | ≥1 | 0 |
    | 192.168.127.1:80 | ≥1 | 0 |
    | 局域网地址:80 | ≥1 | 0 |
    | 169.254.169.254:80 | 0 | 0 |
    | i（网关 :53，UDP） | 0 | 0 |
    | g、h | — | 各 ≥1，bind_failed 为 0 |

  - 四个负例的 dial_ok 增量都必须是 0。真机结果与上表不符时带着数字报告，不放宽断言。
- **必须改 2**：fd 生命周期。
  - FileConn 之后立刻关掉原来的 File。
  - 停止顺序：取消 ctx → 关 conn → 等 Accept 协程返回 → 关虚拟机端的 File。
  - 虚拟机端的 File 由 netMachine 持有，直到内层 Done。
  - Factory 失败、Start 失败两条路径也要按上面的顺序清理。
  - setsockopt 失败要报错。
- **必须改 3**：局域网地址取自拨号器当前绑定的网卡名（由 host.sock 返回），再用 `ipconfig getifaddr` 查；取不到就 fail。不用 `route get default`：开着 Clash TUN 时默认路由是 utun。
- **拍板**：
  - 计数走新开的 `host.sock`：0600，abroad-vm 自己用 net/http 应答 `GET /v1/netstats`，返回三个计数和网卡名，带超时。
  - Outbound 的计数包装放在 `usernet.NewConfiguration`，和 Dialer 共用同一个 policy_denied。
- **建议改全部采纳**：
  - 策略经 `atomic.Pointer[Policy]` 取；
  - 网络出错后 DialVsock 一律报错，作为兜底；
  - 关 IPv6 的顺序：eth0 up 之前写 all 和 default，up 之后写 eth0；
  - 源码检查改为：包限定符只允许 New、VirtualNetwork，禁止 `.Accept(Qemu|Bess|Stdio|VpnKit)(`，`types.Configuration{` 只能出现在 `usernet/config.go`；
  - 脚本注释写明 isOwn 只有单测覆盖；
  - 注释记下 switch 的 txBuf 在 ENOBUFS 时持锁空转这一已知限制。

#### M1-6a 实施计划（2026-11-18）：agent 二次校验 + `managedproxy.VMRuntime`

**第 0 步（已做）**：
- 新包 `internal/redact`，从 agent 的 ringlog 挪出脱敏函数；agent 保留 `Redact` 作别名。
- 新增 `redact.Writer`，按行脱敏。abroad-vm 启动时把 logrus 的输出接到它，有单测 TestThirdPartyLogsRedacted。
- 删掉 `started` 字段；`http.NotFound` 改成直接注册。
- TestBoundIfReallyBinds 跳过点对点网卡。
- 更新 vm-up.sh 的头注释。

**1. 新包 `internal/mihomocfg`**：agent 和 managedproxy 共用，只依赖 yaml。
- `Validate(data []byte) error`：按白名单校验。错误只说明哪一项、第几个，不回显名字和值。
- YAML 解析：`yaml.v3` 解到 `map[string]any`。重复键 yaml.v3 本身就报错；锚点和别名在解码时已展开，展开后照常校验。
- **顶层键白名单**：mixed-port、bind-address、allow-lan、mode、ipv6、log-level、find-process-mode、external-controller-unix、dns、geo-auto-update、profile、proxies、proxy-groups、rules、unified-delay。其他键一律拒绝，涵盖 tun、listeners、tunnels、各类 providers、sub-rules、sniffer、hosts、external-controller、external-ui、interface-name、routing-mark、geox-url、geodata-mode、authentication 等。
- **必须项**：
  - mode = rule；mixed-port = 7890；bind-address = 127.0.0.1；allow-lan = false；ipv6 = false；
  - geo-auto-update 只能是 false；
  - profile 只允许 store-selected、store-fake-ip，且都为 false；
  - external-controller-unix 只能在 `/run/claude-abroad/` 下，且不能含 `..`。
- **rules**：恰好一条 `MATCH,<名字>`。名字是已定义的节点或组，或者 REJECT。
- **proxies**：
  - 每项必须有 name，且不重复；
  - type 不能是 direct、dns、pass；
  - 不能带 interface-name、routing-mark；
  - dialer-proxy 只能指向已定义的名字。
- **proxy-groups**：
  - 成员只能是已定义的名字或 REJECT，不能有 DIRECT、PASS、COMPATIBLE、GLOBAL；
  - 不能带 use、include-all、include-all-proxies、include-all-providers、filter、exclude-filter、interface-name、routing-mark。
- **dns**：
  - 键只允许 enable、ipv6、use-hosts、use-system-hosts、default-nameserver、proxy-server-nameserver、nameserver；
  - use-system-hosts、use-hosts、ipv6 只能是 false；
  - 服务器必须是 `https://<IP 字面量>[:端口]/路径`，`#` 后面只能跟已定义的名字；
  - 明文、tls://、dhcp://、system 一律拒绝。

**2. `managedproxy.(Store).VMRuntime() ([]byte, error)`**：放在新文件 `vmruntime.go`，与 Runtime 共用节点和链式代理的处理。
- 抽出共用函数 `runtimeProxies()`。
- 输出：
  - mixed-port 7890，监听 127.0.0.1，没有 authentication；
  - external-controller-unix 为 `/run/claude-abroad/mihomo/controller.sock`；
  - dns 的三个列表都是 `https://1.1.1.1/dns-query` 和 `https://223.5.5.5/dns-query`；
  - 组 `CAGE` 的成员是 Selected，rules 是 `MATCH,CAGE`；
  - 不带 tun 键，这比 Runtime 更严。
- 生成后调用 `mihomocfg.Validate`，不通过就报错。
- golden 测试用单节点和链式两种 Store，逐项断言任务书列的内容，并且都通过校验器。

**3. agent `MainProxy.apply`**：最先调 `mihomocfg.Validate`，不通过回 422，消息是“主代理配置不符合要求：<哪一项>”。之后不写临时文件、不调 Check、不重启。

**4. vm-up.sh**：
- 4b 的“最小配置”改成 `MATCH,REJECT` 加空 proxies，能通过校验。
- 新增五种危险配置，各回 422，之后 mihomo 的状态和 configured 不变：`MATCH,DIRECT`、`mode: direct`、空规则、带 tun、带 IP-CIDR 规则。
- 4e-2：开发配置改为 `MATCH,REJECT`；d、g 两项注明在 M1-6c 的阶段 C 恢复；e、f 保留能跑的部分，都在 REJECT 下表现为 ok 后零字节。
- 4f：
  - g、h 改为把 223.5.5.5:53 当成 socks5 节点，断言 dial_ok 增量 ≥1、bind_failed 为 0、interface 非空；
  - i 去掉，注释写明由 usernet 的单测和 135116 那份证据覆盖；
  - j1–j4 改为依次把各目标当作节点下发，期望表不变；
  - 重启后的 g 同样改为节点形式。

**5. 文档**：design 7.2 和第 16 节、PROGRESS（含“需要用户”第 1 条的改写，以及 M2 的两条待办）、README。

**测试**：
- 校验器的表驱动单测：每条拒绝项各一个最小反例；正例至少三个：单节点、链式（dialer-proxy）、仅 `MATCH,REJECT`。
- VMRuntime 的 golden 测试与校验器串联。
- apply：校验不通过时不调 Check、不重启引擎、原文件字节不变、configured 不变；错误和日志里不含特征串。

**architect 审核（2026-11-18）后的修改**（以此为准）：
- **必须改 1**：dns 的 `nameserver` 必须带 `#<名字>`，名字是已定义的节点、组或 REJECT。`default-nameserver` 和 `proxy-server-nameserver` 只解析节点服务器的域名，可以直连，但禁止带 `#`。`#` 后面只能是单个名字。URL 不能带 userinfo 和查询串，主机只收 IPv4 字面量。`dns.enable` 必须为 true。VMRuntime 的 dns 与 Runtime 一致：`nameserver` 是 `https://1.1.1.1/dns-query#CAGE`。原因是 ss 的 UDP 遇到域名目标会经 `nameserver` 解析，不带 `#` 就会从本机真实 IP 发出查询。
- **必须改 2**：proxy-groups 只允许 name、type、proxies 三个精确键名；type 只能是 select；proxies 非空，成员是已定义的名字或 REJECT。原因：mihomo 读键名不区分大小写，`Proxies: [DIRECT]` 这种写法黑名单拦不住。
- **必须改 3**：
  - 节点 type 改用白名单，与 import.go 共用一份表：http、socks5、ss、ssr、vmess、vless、trojan、hysteria、hysteria2、tuic、anytls、mieru。
  - name 和 type 必须是精确键名的字符串。
  - 节点名和组名不能是 DIRECT、REJECT、REJECT-DROP、PASS、COMPATIBLE、GLOBAL，也不能含逗号。
  - 节点字段先按 mihomo 的规则归一化（转小写、`_` 换成 `-`）再比黑名单；dialer-proxy 只能出现一次，且必须用规范键名。
  - optionKey 移到 mihomocfg 共用。
- **必须改 4**：VMRuntime 只放 `Route()` 上的节点。`runtimeProxies()` 接受一个节点集合，Runtime 传全部节点，行为不变。
- **必须项**：要求显式存在，且类型严格；mixed-port 取 guestabi 的常量。
- **建议改全部采纳**：
  - log-level 只允许 silent、error、warning；find-process-mode 只允许 off；unified-delay 只允许布尔值；
  - 危险配置再加两条：`Proxies: [DIRECT]`、nameserver 不带 `#`；
  - 4f 加一项：用域名当节点服务器，断言 dial_ok 增量 ≥2，走一遍 proxy-server-nameserver 的直连 DoH；
  - mihomo 工作目录 0700（已经是）；
  - 加错误文本不含特征串的单测。
- 4f 改成只测 TCP 出站，UDP 出站由 M1-6c 的阶段 C 补回，写进注释和进度单。
