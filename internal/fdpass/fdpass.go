//go:build unix

package fdpass

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

var errTooLong = errors.New("消息超过上限")

// Send 发一个消息，f 不为 nil 时把它的 fd 附在消息的第一个字节上。调用方仍负责关闭 f。
func Send(c *net.UnixConn, msg []byte, f *os.File) error {
	if len(msg) > MaxLimit-1 {
		return errTooLong
	}
	buf := binary.BigEndian.AppendUint16(nil, uint16(len(msg)))
	buf = append(buf, msg...)
	var oob []byte
	if f != nil {
		// 不用 f.Fd()：它会把文件改成阻塞模式，而阻塞标志属于底层的打开文件，
		// 会连带影响已经（或即将）交给对方的那份连接（M3-4 测试中实测：对方偶发读到 EOF）。
		rc, err := f.SyscallConn()
		if err != nil {
			return err
		}
		var fd int
		if err := rc.Control(func(p uintptr) { fd = int(p) }); err != nil {
			return err
		}
		oob = unix.UnixRights(fd)
		defer runtimeKeepAlive(f)
	}
	n, oobn, err := c.WriteMsgUnix(buf, oob, nil)
	if err != nil {
		return err
	}
	if oobn != len(oob) {
		return errors.New("fd 没有完整发送")
	}
	if n < len(buf) {
		// 剩下的部分按普通数据写完（fd 已经随第一个字节送出）。
		if _, err := c.Write(buf[n:]); err != nil {
			return err
		}
	}
	return nil
}

// Recv 收一个消息（不超过 limit，limit<=0 时用 MaxMsg）和至多一个 fd。没有 fd 时 f 为 nil。
// 收到的 fd 设了 close-on-exec。
func Recv(c *net.UnixConn, limit int) (msg []byte, f *os.File, err error) {
	if limit <= 0 {
		limit = MaxMsg
	}
	if limit > MaxLimit-1 {
		limit = MaxLimit - 1
	}
	var fds []int
	defer func() {
		if err != nil {
			for _, fd := range fds {
				_ = unix.Close(fd)
			}
			if f != nil {
				f.Close()
				f = nil
			}
		}
	}()
	var got []byte
	need := 2
	buf := make([]byte, 2+limit)
	oob := make([]byte, unix.CmsgSpace(4*4)) // 留出多个 fd 的空间，才能发现“多了”
	for len(got) < need {
		n, oobn, flags, _, rerr := c.ReadMsgUnix(buf[:need-len(got)], oob)
		n, oobn = max(n, 0), max(oobn, 0) // 出错（例如读期限到了）时可能是 -1
		if oobn > 0 {
			if len(got) > 0 {
				// fd 出现在消息中间：协议不对。先收下来再关掉。
				more, perr := parseRights(oob[:oobn])
				fds = append(fds, more...)
				if perr == nil {
					perr = errors.New("fd 出现在消息中间")
				}
				return nil, nil, perr
			}
			more, perr := parseRights(oob[:oobn])
			fds = append(fds, more...)
			if perr != nil {
				return nil, nil, perr
			}
		}
		if flags&unix.MSG_CTRUNC != 0 {
			return nil, nil, errors.New("控制消息被截断")
		}
		got = append(got, buf[:n]...)
		if len(got) >= 2 && need == 2 {
			l := int(binary.BigEndian.Uint16(got))
			if l > limit {
				return nil, nil, errTooLong
			}
			need = 2 + l
		}
		if rerr != nil {
			if len(got) < need {
				return nil, nil, fmt.Errorf("消息没收完：%w", rerr)
			}
			break
		}
		if n == 0 && len(got) < need {
			return nil, nil, errors.New("连接已关闭，消息没收完")
		}
	}
	if len(fds) > 1 {
		return nil, nil, fmt.Errorf("期望至多 1 个 fd，收到 %d 个", len(fds))
	}
	if len(fds) == 1 {
		unix.CloseOnExec(fds[0])
		f = os.NewFile(uintptr(fds[0]), "fdpass")
		fds = nil
	}
	return got[2:need], f, nil
}

func parseRights(oob []byte) ([]int, error) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	var fds []int
	var bad error
	for i := range msgs {
		got, err := unix.ParseUnixRights(&msgs[i])
		if err != nil {
			bad = errors.New("不认识的控制消息")
			continue
		}
		fds = append(fds, got...)
	}
	return fds, bad
}

func runtimeKeepAlive(f *os.File) { runtime.KeepAlive(f) }
