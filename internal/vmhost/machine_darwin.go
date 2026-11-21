//go:build darwin && cgo

package vmhost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/Code-Hex/vz/v3"
)

// vzMachine 用 Virtualization.framework（经 Code-Hex/vz）运行虚拟机。
// 设备：Linux 引导（Image + initramfs + cmdline）、两块只读 virtio 盘、传统 balloon、
// entropy、写文件的 virtio 控制台、vsock；Spec.NetFD 非空时再挂一块文件句柄网卡。永远不用 NAT 网卡
// （VZNATNetworkDeviceAttachment 会在宿主建网桥并加 NAT 规则，设计文档 4.5）。
//
// 需要 com.apple.security.virtualization 授权（ad-hoc 签名即可，见 scripts/dev/sign.sh）。
type vzMachine struct {
	vm *vz.VirtualMachine

	once sync.Once
	done chan struct{}
	mu   sync.Mutex
	err  error
}

// NewVZ 按配置创建虚拟机（还没启动）。
func NewVZ(s Spec) (Machine, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if err := PrepareConsoleLog(s.ConsoleLog); err != nil {
		return nil, fmt.Errorf("准备控制台日志：%w", err)
	}
	bl, err := vz.NewLinuxBootLoader(s.Kernel, vz.WithCommandLine(s.Cmdline), vz.WithInitrd(s.Initrd))
	if err != nil {
		return nil, fmt.Errorf("配置引导：%w", err)
	}
	cfg, err := vz.NewVirtualMachineConfiguration(bl, s.CPUs, s.MemoryMiB<<20)
	if err != nil {
		return nil, fmt.Errorf("创建虚拟机配置：%w", err)
	}

	var disks []vz.StorageDeviceConfiguration
	for _, p := range []string{s.RootDisk, s.Modloop} {
		att, err := vz.NewDiskImageStorageDeviceAttachment(p, true)
		if err != nil {
			return nil, fmt.Errorf("挂载磁盘 %s：%w", p, err)
		}
		dev, err := vz.NewVirtioBlockDeviceConfiguration(att)
		if err != nil {
			return nil, fmt.Errorf("创建磁盘设备：%w", err)
		}
		disks = append(disks, dev)
	}
	cfg.SetStorageDevicesVirtualMachineConfiguration(disks)

	balloon, err := vz.NewVirtioTraditionalMemoryBalloonDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("创建 balloon：%w", err)
	}
	cfg.SetMemoryBalloonDevicesVirtualMachineConfiguration([]vz.MemoryBalloonDeviceConfiguration{balloon})

	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("创建 entropy：%w", err)
	}
	cfg.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})

	console, err := vz.NewFileSerialPortAttachment(s.ConsoleLog, true)
	if err != nil {
		return nil, fmt.Errorf("控制台日志：%w", err)
	}
	serial, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(console)
	if err != nil {
		return nil, fmt.Errorf("创建控制台：%w", err)
	}
	cfg.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{serial})

	// 网卡：只用 VZFileHandleNetworkDeviceAttachment（用户态网络的 socketpair 一端），设计 4.5。
	var nics []*vz.VirtioNetworkDeviceConfiguration
	for _, nd := range s.NetDevices() {
		att, err := vz.NewFileHandleNetworkDeviceAttachment(nd.FD)
		if err != nil {
			return nil, fmt.Errorf("创建网卡：%w", err)
		}
		nic, err := vz.NewVirtioNetworkDeviceConfiguration(att)
		if err != nil {
			return nil, fmt.Errorf("创建网卡：%w", err)
		}
		hw, err := net.ParseMAC(nd.MAC)
		if err != nil {
			return nil, err
		}
		mac, err := vz.NewMACAddress(hw)
		if err != nil {
			return nil, fmt.Errorf("网卡 MAC：%w", err)
		}
		nic.SetMACAddress(mac)
		nics = append(nics, nic)
	}
	cfg.SetNetworkDevicesVirtualMachineConfiguration(nics)

	sock, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("创建 vsock：%w", err)
	}
	cfg.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{sock})

	if ok, err := cfg.Validate(); !ok || err != nil {
		return nil, fmt.Errorf("虚拟机配置无效：%v", err)
	}
	vm, err := vz.NewVirtualMachine(cfg)
	if err != nil {
		return nil, fmt.Errorf("创建虚拟机：%w（如果提示缺少授权，请用 scripts/dev/sign.sh 签名）", err)
	}
	return &vzMachine{vm: vm, done: make(chan struct{})}, nil
}

func (m *vzMachine) Start() error {
	states := m.vm.StateChangedNotify()
	if err := m.vm.Start(); err != nil {
		return fmt.Errorf("启动虚拟机：%w", err)
	}
	go func() {
		for st := range states {
			switch st {
			case vz.VirtualMachineStateStopped:
				m.finish(errors.New("虚拟机已停止"))
				return
			case vz.VirtualMachineStateError:
				m.finish(errors.New("虚拟机出错停止"))
				return
			}
		}
	}()
	return nil
}

func (m *vzMachine) finish(err error) {
	m.once.Do(func() {
		m.mu.Lock()
		m.err = err
		m.mu.Unlock()
		close(m.done)
	})
}

func (m *vzMachine) Done() <-chan struct{} { return m.done }

func (m *vzMachine) Err() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.err
}

// ForceStop 强停（不给客户机清理的机会）。幂等；Stop 本身也没有超时，这里限 10 秒。
func (m *vzMachine) ForceStop() error {
	select {
	case <-m.done:
		return nil
	default:
	}
	if !m.vm.CanStop() {
		st := m.vm.State()
		if st == vz.VirtualMachineStateStopped || st == vz.VirtualMachineStateError {
			return nil
		}
		return fmt.Errorf("虚拟机当前状态（%v）不能强停", st)
	}
	errc := make(chan error, 1)
	go func() { errc <- m.vm.Stop() }()
	select {
	case err := <-errc:
		if err != nil {
			return fmt.Errorf("强停虚拟机：%w", err)
		}
		m.finish(errors.New("虚拟机被强停"))
		return nil
	case <-time.After(10 * time.Second):
		return errors.New("强停虚拟机超时")
	}
}

// DialVsock 连接虚拟机里的 vsock 端口。vz 的 Connect 没有超时，这里放在 goroutine 里，
// ctx 先结束就返回错误，之后才到的连接直接关掉，不泄漏 fd。
//
// 返回的连接不支持 CloseWrite（vz v3.8.0 没有暴露），桥接时会退化为全关闭。
func (m *vzMachine) DialVsock(ctx context.Context, port uint32) (net.Conn, error) {
	devs := m.vm.SocketDevices()
	if len(devs) == 0 {
		return nil, errors.New("虚拟机没有 vsock 设备")
	}
	type result struct {
		c   net.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := devs[0].Connect(port)
		if err != nil {
			ch <- result{nil, err}
			return
		}
		ch <- result{c, nil}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("连接 vsock %d：%w", port, r.err)
		}
		return r.c, nil
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.c != nil {
				r.c.Close()
			}
		}()
		return nil, fmt.Errorf("连接 vsock %d：%w", port, ctx.Err())
	}
}
