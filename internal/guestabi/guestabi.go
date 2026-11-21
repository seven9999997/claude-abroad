// Package guestabi 是宿主（abroad-vm、abroad-imagebuild）和虚拟机里的 abroad-agent 共用的约定：
// vsock 端口、内核命令行参数、镜像内路径。改这里任何一项都要同时重建镜像。
package guestabi

const (
	// DataPort 是数据面（wire/yamux）的 vsock 端口，CtlPort 是控制面（HTTP）的。设计文档 4.4。
	DataPort = 1080
	CtlPort  = 1081

	// 内核命令行参数：虚拟机网卡的静态地址和网关。vmhost 总是显式传；agent 的默认值只是兜底。
	CmdlineIP = "abroad.ip"
	CmdlineGW = "abroad.gw"
	// CmdlineDev 是开发标记（abroad.dev=1）：只有带它时 agent 才注册开发用的多出口接口 /v1/dev-exits
	// （m3-plan v2.8 决定 7；只有带 abroaddev 构建标签的 abroad-vm 能加上它，发布构建去掉）。
	CmdlineDev = "abroad.dev"
	// DefaultIP、DefaultGW 是 gvisor-tap-vsock 的默认网段。
	DefaultIP = "192.168.127.2/24"
	DefaultGW = "192.168.127.1"
	// NetIface 是虚拟机里唯一的网卡。
	NetIface = "eth0"
	// HostLoopbackIP 是虚拟机里代表“本机 127.0.0.1”的地址（网关上的虚拟地址，经 NAT 映射）。
	// 只在 abroad-vm 的出站白名单里有回环项时存在（设计 4.5：本机其他代理软件的端口当主代理）。
	HostLoopbackIP = "192.168.127.254"
	// GuestMAC 是虚拟机网卡的 MAC（本地管理地址，固定，便于用户态网络识别）。
	GuestMAC = "02:ab:c0:00:00:02"

	// AgentPath 是根镜像里的 abroad-agent；initramfs 里同一个程序放在 /init。
	AgentPath = "/sbin/abroad-agent"
	// BootModulesList 在 initramfs 里，按加载顺序列出启动模块的绝对路径。
	BootModulesList = "/etc/abroad/boot-modules"
	// ModloopDir 是 modloop 在最终根目录下的挂载点，模块在 <ModloopDir>/modules/<内核版本>/。
	ModloopDir = "/modloop"
	// NewRoot 是 boot 阶段组装最终根目录（overlay）的位置。
	NewRoot = "/newroot"
	// OverlaySize 是 overlay 可写层（tmpfs）的上限。设计文档 7.1。
	OverlaySize = "32m"

	// 引擎（设计文档 7.2、7.3）。版本只在 guest/manifest.json 里维护。
	SingBoxPath = "/usr/bin/sing-box"
	MihomoPath  = "/usr/bin/mihomo"

	// RunDir 是虚拟机里的运行目录（tmpfs 上，0700）：凭据和配置只放这里，不落盘（设计文档 7.4）。
	RunDir          = "/run/claude-abroad"
	MainProxyConfig = RunDir + "/main.yaml"     // 主代理 mihomo 的配置（0600）
	MihomoDir       = RunDir + "/mihomo"        // mihomo 的工作目录
	SingBoxConfig   = RunDir + "/sing-box.json" // sing-box 的配置（0600）
	SingBoxDir      = RunDir + "/sing-box"      // sing-box 的工作目录

	// SingBoxSocksPort 是 sing-box 的 socks 入站（只听 127.0.0.1），数据面以 auth_user=<出口> 连它。
	SingBoxSocksPort = 1090
	// MainProxyPort 是主代理 mihomo 的 mixed 端口（只听 127.0.0.1），sing-box 的 main 出站连它。
	MainProxyPort = 7890
	// ExitMain 是主代理的出口标签（设计文档 7.2）。
	ExitMain = "main"
)
