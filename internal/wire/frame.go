// Package wire 定义本机（abroadd / abroad-dev）与虚拟机内 abroad-agent 之间
// 数据面的帧格式（设计文档 4.4）。
//
// 每条 yamux 数据流的开头：
//
//	本机 → agent：帧头   2 字节大端长度 + JSON（Header），上限 4 KiB
//	agent → 本机：回应帧 2 字节大端长度 + JSON（Reply），上限 4 KiB
//
// 回应帧之后：TCP 是原始字节流；UDP 是"2 字节长度 + 数据报"组成的帧（见 udp.go），
// 一条流对应一个五元组。
//
// 解析是严格的：不认识的版本、未知字段、非法取值、多余数据一律拒绝，
// 这样两端版本不一致时会明确失败，而不是猜测对方的意思。
package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"strings"
)

// Version 是当前帧格式版本。
const Version = 1

// MaxFrame 是帧头和回应帧 JSON 部分的最大字节数。
const MaxFrame = 4096

// Network 取值。
const (
	TCP = "tcp"
	UDP = "udp"
)

// Header 是每条数据流开头的请求帧。
type Header struct {
	V    int    `json:"v"`
	Exit string `json:"exit"`           // 出口标签，例如 main、wg-us
	Net  string `json:"net"`            // tcp 或 udp
	Host string `json:"host,omitempty"` // 目标域名；有域名时优先由出口解析
	IP   string `json:"ip,omitempty"`   // 目标 IP；host 为空时必填
	Port int    `json:"port"`
	App  string `json:"app,omitempty"` // 应用 ID，只用于调试级日志
}

// Code 是回应帧里的错误码。
type Code string

const (
	CodeBadRequest     Code = "bad_request"     // 帧头不合法
	CodeUnknownExit    Code = "unknown_exit"    // 出口标签不认识（不会换出口）
	CodeExitDown       Code = "exit_down"       // 出口存在但没在运行
	CodeDialFailed     Code = "dial_failed"     // 经出口连目标失败
	CodeUDPUnsupported Code = "udp_unsupported" // 出口不支持 UDP
)

var knownCodes = map[Code]bool{
	CodeBadRequest: true, CodeUnknownExit: true, CodeExitDown: true,
	CodeDialFailed: true, CodeUDPUnsupported: true,
}

// Reply 是 agent 对帧头的回应。
type Reply struct {
	OK   bool `json:"ok"`
	Code Code `json:"code,omitempty"`
}

// ReplyError 表示 agent 回了 ok=false。
type ReplyError struct{ Code Code }

func (e *ReplyError) Error() string { return "虚拟机拒绝了数据流：" + string(e.Code) }

// ErrBadFrame 包装所有帧格式错误，可用 errors.Is 判断。
var ErrBadFrame = errors.New("数据面帧格式错误")

func badFrame(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrBadFrame, fmt.Sprintf(format, a...))
}

var (
	exitPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	hostPattern = regexp.MustCompile(`^[A-Za-z0-9_]([A-Za-z0-9_.-]*[A-Za-z0-9_])?$`)
)

const maxAppLen = 512

// Validate 检查帧头的每个字段。
func (h Header) Validate() error {
	if h.V != Version {
		return badFrame("不支持的版本 %d", h.V)
	}
	if !exitPattern.MatchString(h.Exit) {
		return badFrame("出口标签不合法")
	}
	if h.Net != TCP && h.Net != UDP {
		return badFrame("net 只能是 tcp 或 udp")
	}
	if h.Port < 1 || h.Port > 65535 {
		return badFrame("端口超出范围")
	}
	if h.Host == "" && h.IP == "" {
		return badFrame("host 和 ip 至少要有一个")
	}
	if h.Host != "" {
		if err := validHost(h.Host); err != nil {
			return err
		}
	}
	if h.IP != "" {
		a, err := netip.ParseAddr(h.IP)
		if err != nil || a.Zone() != "" {
			return badFrame("IP 不合法")
		}
	}
	if len(h.App) > maxAppLen {
		return badFrame("应用 ID 过长")
	}
	for i := 0; i < len(h.App); i++ {
		if c := h.App[i]; c < 0x20 || c == 0x7f {
			return badFrame("应用 ID 含控制字符")
		}
	}
	return nil
}

// validHost 检查域名。IP 字面量（包括 "2130706433" 这种最后一段全是数字的写法）
// 必须放在 ip 字段，不能放在 host 里，否则按 ip 做的策略看不到它。
func validHost(host string) error {
	if len(host) > 253 || !hostPattern.MatchString(host) || strings.Contains(host, "..") {
		return badFrame("域名不合法")
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return badFrame("IP 地址要放在 ip 字段")
	}
	labels := strings.Split(host, ".")
	for _, l := range labels {
		if len(l) > 63 {
			return badFrame("域名的某一段超过 63 字节")
		}
	}
	last := labels[len(labels)-1]
	if strings.Trim(last, "0123456789") == "" {
		return badFrame("域名最后一段不能全是数字（IP 地址要放在 ip 字段）")
	}
	return nil
}

// Validate 检查回应帧：ok=true 时不能带错误码，ok=false 时错误码必须认识。
func (r Reply) Validate() error {
	if r.OK && r.Code != "" {
		return badFrame("成功的回应不应带错误码")
	}
	if !r.OK && !knownCodes[r.Code] {
		return badFrame("未知的错误码 %q", r.Code)
	}
	return nil
}

// Err 把回应转成错误：成功时为 nil。
func (r Reply) Err() error {
	if r.OK {
		return nil
	}
	return &ReplyError{Code: r.Code}
}

// WriteHeader 校验并写出帧头。
func WriteHeader(w io.Writer, h Header) error {
	if err := h.Validate(); err != nil {
		return err
	}
	return writeJSON(w, h)
}

// ReadHeader 读取并校验帧头。
func ReadHeader(r io.Reader) (Header, error) {
	var h Header
	body, err := readFrame(r)
	if err != nil {
		return h, err
	}
	// 先只看版本：版本不认识时报"版本"而不是"未知字段"。
	var probe struct {
		V *int `json:"v"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return h, badFrame("JSON 无效")
	}
	if probe.V == nil {
		return h, badFrame("缺少版本号")
	}
	if *probe.V != Version {
		return h, badFrame("不支持的版本 %d", *probe.V)
	}
	if err := strictDecode(body, &h); err != nil {
		return h, err
	}
	return h, h.Validate()
}

// WriteReply 校验并写出回应帧。
func WriteReply(w io.Writer, rep Reply) error {
	if err := rep.Validate(); err != nil {
		return err
	}
	return writeJSON(w, rep)
}

// ReadReply 读取并校验回应帧。
func ReadReply(r io.Reader) (Reply, error) {
	var rep Reply
	body, err := readFrame(r)
	if err != nil {
		return rep, err
	}
	if err := strictDecode(body, &rep); err != nil {
		return rep, err
	}
	return rep, rep.Validate()
}

func writeJSON(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(body) > MaxFrame {
		return badFrame("帧长 %d 超过上限 %d", len(body), MaxFrame)
	}
	buf := make([]byte, 2+len(body))
	binary.BigEndian.PutUint16(buf, uint16(len(body)))
	copy(buf[2:], body)
	_, err = w.Write(buf) // 一次写出，避免与其他写入交错
	return err
}

func readFrame(r io.Reader) ([]byte, error) {
	var lb [2]byte
	if _, err := io.ReadFull(r, lb[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(lb[:]))
	if n == 0 || n > MaxFrame {
		return nil, badFrame("帧长 %d 不在 1–%d 之间", n, MaxFrame)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return body, nil
}

func strictDecode(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badFrame("JSON 无效或含未知字段")
	}
	if dec.More() {
		return badFrame("帧尾有多余数据")
	}
	if _, err := dec.Token(); err != io.EOF {
		return badFrame("帧尾有多余数据")
	}
	return nil
}
