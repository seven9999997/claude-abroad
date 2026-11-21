package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ErrDatagramTooLarge 表示缓冲区放不下这个数据报：该数据报已被丢弃，流仍然可用。
var ErrDatagramTooLarge = errors.New("数据报超过缓冲区大小")

// MaxDatagram 是一个 UDP 数据报的最大长度（2 字节长度字段能表示的上限）。
const MaxDatagram = 65535

// DatagramConn 在一条字节流上收发"2 字节大端长度 + 数据"格式的 UDP 数据报。
// 写入并发安全；读取应只在一个 goroutine 里进行。
type DatagramConn struct {
	rw  io.ReadWriter
	wmu sync.Mutex
}

// NewDatagramConn 包装一条已经完成帧头和回应帧交换的数据流。
func NewDatagramConn(rw io.ReadWriter) *DatagramConn { return &DatagramConn{rw: rw} }

// WriteDatagram 写出一个数据报（允许 0 字节）。长度和数据一次写出，不会与并发写交错。
func (c *DatagramConn) WriteDatagram(p []byte) error {
	if len(p) > MaxDatagram {
		return badFrame("数据报长 %d 超过上限 %d", len(p), MaxDatagram)
	}
	buf := make([]byte, 2+len(p))
	binary.BigEndian.PutUint16(buf, uint16(len(p)))
	copy(buf[2:], p)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.rw.Write(buf)
	return err
}

// ReadDatagram 读入一个完整的数据报到 p，返回长度。
// p 放不下时整个数据报被读掉并返回 ErrDatagramTooLarge，流仍然对齐，可以继续读下一个。
// 流在两个数据报之间正常结束时返回 io.EOF；在数据报中间结束时返回 io.ErrUnexpectedEOF。
func (c *DatagramConn) ReadDatagram(p []byte) (int, error) {
	var lb [2]byte
	if _, err := io.ReadFull(c.rw, lb[:]); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint16(lb[:]))
	if n > len(p) {
		if _, err := io.CopyN(io.Discard, c.rw, int64(n)); err != nil {
			return 0, unexpected(err)
		}
		return 0, fmt.Errorf("%w：缓冲区 %d 字节，数据报 %d 字节", ErrDatagramTooLarge, len(p), n)
	}
	if _, err := io.ReadFull(c.rw, p[:n]); err != nil {
		return 0, unexpected(err)
	}
	return n, nil
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}
