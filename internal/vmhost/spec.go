// Package vmhost 运行 Claude 出国的虚拟机（设计文档 3、4.4、7.1）：
// 虚拟机配置、状态目录、ctl.sock → vsock 桥、生命周期（看门狗、退避重启、优雅停止）。
//
// 除 machine_darwin.go（Virtualization.framework，经 Code-Hex/vz）外都可移植，
// 测试用假的 Machine。
//
// 底线 6（docs/m1-plan.md）：虚拟机宿主和用户态网络只通过 Spec.NetFD 耦合，不共享其他状态。
package vmhost

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"claude-abroad/internal/guestabi"
	"claude-abroad/internal/imagebuild"
)

// Spec 是一台虚拟机的配置。类型上没有 NAT 或桥接网卡的选项：
// 网卡只能是用户态网络给的 fd（VZFileHandleNetworkDeviceAttachment，设计文档 4.5）。
type Spec struct {
	Kernel     string // 未压缩的 arm64 Image
	Initrd     string
	RootDisk   string // 只读 squashfs
	Modloop    string // 只读 squashfs
	CPUs       uint
	MemoryMiB  uint64
	ConsoleLog string // virtio 控制台输出写到这里（0600）
	Cmdline    string
	// NetFD 是用户态网络（gvisor-tap-vsock）一端的 fd（SOCK_DGRAM 的 socketpair）。
	// 为 nil 表示不挂网卡。只能接 VZFileHandleNetworkDeviceAttachment，没有 NAT 或桥接选项。
	NetFD *os.File
}

// DefaultCmdline 是内核命令行：控制台 hvc0；panic=0 让内核 panic 后停住不复位——
// VZ 把客户机复位当作原地重启（阶段 A 实测，design 7.1），panic=-1 会造成复位循环且
// Supervisor 不知情；停住后由看门狗或运行期巡检强停，走唯一的重启路径。
// 网卡地址总是显式传，agent 里的默认值只是兜底。
func DefaultCmdline() string {
	return fmt.Sprintf("console=hvc0 panic=0 %s=%s %s=%s",
		guestabi.CmdlineIP, guestabi.DefaultIP, guestabi.CmdlineGW, guestabi.DefaultGW)
}

// SpecFromImageDir 用 abroad-imagebuild 的输出目录填路径。
func SpecFromImageDir(imageDir, consoleLog string) Spec {
	return Spec{
		Kernel:     filepath.Join(imageDir, imagebuild.OutKernel),
		Initrd:     filepath.Join(imageDir, imagebuild.OutInitramfs),
		RootDisk:   filepath.Join(imageDir, imagebuild.OutRootFS),
		Modloop:    filepath.Join(imageDir, imagebuild.OutModloop),
		CPUs:       1,
		MemoryMiB:  256,
		ConsoleLog: consoleLog,
		Cmdline:    DefaultCmdline(),
	}
}

// Validate 检查配置。确定性的错误，调用方应直接退出而不是重试。
func (s Spec) Validate() error {
	for _, f := range []struct{ name, path string }{
		{"内核", s.Kernel}, {"initramfs", s.Initrd}, {"根盘", s.RootDisk}, {"modloop", s.Modloop},
	} {
		fi, err := os.Stat(f.path)
		if err != nil {
			return fmt.Errorf("找不到%s（%s）：%w；请先构建镜像（abroad-imagebuild build）", f.name, f.path, err)
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s（%s）不是普通文件", f.name, f.path)
		}
	}
	if s.CPUs < 1 || s.CPUs > 4 {
		return fmt.Errorf("CPU 数 %d 不在 1–4 之间", s.CPUs)
	}
	if s.MemoryMiB < 128 || s.MemoryMiB > 2048 {
		return fmt.Errorf("内存 %d MiB 不在 128–2048 之间", s.MemoryMiB)
	}
	if s.ConsoleLog == "" {
		return errors.New("没有指定控制台日志路径")
	}
	if s.Cmdline == "" {
		return errors.New("内核命令行为空")
	}
	return nil
}

// NetDevice 描述要挂的一块网卡（可移植，machine_darwin.go 照此创建 VZ 设备）。
type NetDevice struct {
	FD  *os.File
	MAC string
}

// NetDevices 返回要挂的网卡：NetFD 非空时恰好一块（文件句柄网卡 + 固定 MAC），为空时零块。
func (s Spec) NetDevices() []NetDevice {
	if s.NetFD == nil {
		return nil
	}
	return []NetDevice{{FD: s.NetFD, MAC: guestabi.GuestMAC}}
}
