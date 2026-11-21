//go:build darwin

package tun

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// macOS 内核头文件里的常量（x/sys/unix 没有导出）。
const (
	sysprotoControl  = 2 // SYSPROTO_CONTROL（<sys/sys_domain.h>）
	utunOptIfname    = 2 // UTUN_OPT_IFNAME（<net/if_utun.h>）
	utunControlName  = "com.apple.net.utun_control"
	utunHeaderLength = 4 // 每个包前面的地址族（网络字节序）
)

// utun 是 macOS 的 utun 设备。fd 设成非阻塞后包成 *os.File，读写走 Go 运行时的轮询器，
// 所以 Close 能唤醒阻塞中的 Read（直接对原始 fd 做阻塞 read，close 不会让它返回）。
type utun struct {
	f    *os.File
	name string
	mtu  int

	rmu  sync.Mutex
	rbuf []byte // 读缓冲（含 4 字节头），复用，不每包分配
	wmu  sync.Mutex
	wbuf []byte
}

// OpenUtun 打开一个新的 utun（需要 root）。unit 为 -1 时由系统分配编号；否则打开 utun<unit>。
// 只创建设备，不配地址和路由（M2-7、M2-9）。
func OpenUtun(unit, mtu int) (Device, error) {
	fd, err := unix.Socket(unix.AF_SYSTEM, unix.SOCK_DGRAM, sysprotoControl)
	if err != nil {
		return nil, fmt.Errorf("创建 utun 控制 socket：%w", err)
	}
	ok := false
	defer func() {
		if !ok {
			unix.Close(fd)
		}
	}()
	info := &unix.CtlInfo{}
	copy(info.Name[:], utunControlName)
	if err := unix.IoctlCtlInfo(fd, info); err != nil {
		return nil, fmt.Errorf("查询 utun 控制 id：%w", err)
	}
	sc := &unix.SockaddrCtl{ID: info.Id, Unit: uint32(unit + 1)} // sc_unit = 编号 + 1；0 表示由系统分配
	if err := unix.Connect(fd, sc); err != nil {
		return nil, fmt.Errorf("打开 utun：%w", err)
	}
	name, err := unix.GetsockoptString(fd, sysprotoControl, utunOptIfname)
	if err != nil {
		return nil, fmt.Errorf("读取 utun 名称：%w", err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		return nil, fmt.Errorf("设置 utun 非阻塞：%w", err)
	}
	unix.CloseOnExec(fd)
	f := os.NewFile(uintptr(fd), name)
	ok = true
	return &utun{f: f, name: name, mtu: mtu, rbuf: make([]byte, utunHeaderLength+mtu+1), wbuf: make([]byte, utunHeaderLength+mtu)}, nil
}

func mapClosed(err error) error {
	if errors.Is(err, os.ErrClosed) || errors.Is(err, fs.ErrClosed) {
		return ErrClosed
	}
	return err
}

// Read 读一个 IP 包（去掉 4 字节地址族头）。缓冲区比包小时返回 ErrShortBuffer。
// 读缓冲比 4+MTU 多留 1 字节：读满说明包比 MTU 大、被数据报 socket 截断了（网卡实际 MTU 比 Device.MTU() 大），
// 这个包丢掉并返回 ErrTooBig（栈计入 dropped_other 后继续读），绝不把截断的包当完整包交上去。
func (u *utun) Read(p []byte) (int, error) {
	u.rmu.Lock()
	defer u.rmu.Unlock()
	for {
		n, err := u.f.Read(u.rbuf)
		if err != nil {
			return 0, mapClosed(err)
		}
		if n <= utunHeaderLength {
			continue // 只有头没有包：丢掉
		}
		if n > utunHeaderLength+u.mtu {
			return 0, ErrTooBig
		}
		pkt := u.rbuf[utunHeaderLength:n]
		if len(pkt) > len(p) {
			return 0, ErrShortBuffer
		}
		return copy(p, pkt), nil
	}
}

// Write 写一个 IP 包，按版本号填 4 字节地址族头。
func (u *utun) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, errors.New("空包")
	}
	if len(p) > u.mtu {
		return 0, ErrTooBig
	}
	var af uint32
	switch p[0] >> 4 {
	case 4:
		af = unix.AF_INET
	case 6:
		af = unix.AF_INET6
	default:
		return 0, errors.New("不是 IPv4 或 IPv6 包")
	}
	u.wmu.Lock()
	defer u.wmu.Unlock()
	binary.BigEndian.PutUint32(u.wbuf, af)
	n := copy(u.wbuf[utunHeaderLength:], p)
	if _, err := u.f.Write(u.wbuf[:utunHeaderLength+n]); err != nil {
		return 0, mapClosed(err)
	}
	return len(p), nil
}

func (u *utun) MTU() int     { return u.mtu }
func (u *utun) Name() string { return u.name }

// Close 幂等；会唤醒阻塞中的 Read、Write。
func (u *utun) Close() error {
	if err := u.f.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	return nil
}
