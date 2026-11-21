//go:build windows

package wfp

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"

	"claude-abroad/internal/wfprules"
)

// 本文件是真正的系统调用（fwpuclnt.dll）。结构体布局按 64 位 Windows（amd64、arm64）的 fwpmtypes.h、fwptypes.h，
// 大小由 wfp_windows_test.go 核对；32 位 Windows 不是目标平台（Open 直接拒绝）。
// 每个调用的参数都来自 spec.go 算好的 FilterSpec，本文件不决定任何参数。

var (
	modFwpuclnt = windows.NewLazySystemDLL("fwpuclnt.dll")

	procEngineOpen0          = modFwpuclnt.NewProc("FwpmEngineOpen0")
	procEngineClose0         = modFwpuclnt.NewProc("FwpmEngineClose0")
	procTransactionBegin0    = modFwpuclnt.NewProc("FwpmTransactionBegin0")
	procTransactionCommit0   = modFwpuclnt.NewProc("FwpmTransactionCommit0")
	procTransactionAbort0    = modFwpuclnt.NewProc("FwpmTransactionAbort0")
	procProviderAdd0         = modFwpuclnt.NewProc("FwpmProviderAdd0")
	procProviderDeleteByKey0 = modFwpuclnt.NewProc("FwpmProviderDeleteByKey0")
	procSubLayerAdd0         = modFwpuclnt.NewProc("FwpmSubLayerAdd0")
	procSubLayerDeleteByKey0 = modFwpuclnt.NewProc("FwpmSubLayerDeleteByKey0")
	procFilterAdd0           = modFwpuclnt.NewProc("FwpmFilterAdd0")
	procFilterDeleteByKey0   = modFwpuclnt.NewProc("FwpmFilterDeleteByKey0")
	procFilterCreateEnum0    = modFwpuclnt.NewProc("FwpmFilterCreateEnumHandle0")
	procFilterEnum0          = modFwpuclnt.NewProc("FwpmFilterEnum0")
	procFilterDestroyEnum0   = modFwpuclnt.NewProc("FwpmFilterDestroyEnumHandle0")
	procProviderCreateEnum0  = modFwpuclnt.NewProc("FwpmProviderCreateEnumHandle0")
	procProviderEnum0        = modFwpuclnt.NewProc("FwpmProviderEnum0")
	procProviderDestroyEnum0 = modFwpuclnt.NewProc("FwpmProviderDestroyEnumHandle0")
	procSubLayerCreateEnum0  = modFwpuclnt.NewProc("FwpmSubLayerCreateEnumHandle0")
	procSubLayerEnum0        = modFwpuclnt.NewProc("FwpmSubLayerEnum0")
	procSubLayerDestroyEnum0 = modFwpuclnt.NewProc("FwpmSubLayerDestroyEnumHandle0")
	procFreeMemory0          = modFwpuclnt.NewProc("FwpmFreeMemory0")
)

var allProcs = []*windows.LazyProc{procEngineOpen0, procEngineClose0, procTransactionBegin0, procTransactionCommit0,
	procTransactionAbort0, procProviderAdd0, procProviderDeleteByKey0, procSubLayerAdd0, procSubLayerDeleteByKey0,
	procFilterAdd0, procFilterDeleteByKey0, procFilterCreateEnum0, procFilterEnum0, procFilterDestroyEnum0,
	procProviderCreateEnum0, procProviderEnum0, procProviderDestroyEnum0, procSubLayerCreateEnum0, procSubLayerEnum0,
	procSubLayerDestroyEnum0, procFreeMemory0}

const (
	rpcAuthnDefault     = 0xffffffff
	sessionFlagDynamic  = 0x1
	objFlagPersistent   = 0x1 // FWPM_PROVIDER_FLAG_PERSISTENT、FWPM_SUBLAYER_FLAG_PERSISTENT、FWPM_FILTER_FLAG_PERSISTENT
	errAlreadyExists    = 0x80320009
	errFilterNotFound   = 0x80320003
	errProviderNotFound = 0x80320005
	errSublayerNotFound = 0x80320007
)

type fwpmDisplayData0 struct {
	Name        *uint16
	Description *uint16
}

type fwpByteBlob struct {
	Size uint32
	Data *byte
}

type fwpValue0 struct {
	Type  uint32
	Value uintptr // 联合体：8 位到 32 位的整数直接放，其余是指针
}

type fwpV4AddrAndMask struct {
	Addr uint32
	Mask uint32
}

type fwpmFilterCondition0 struct {
	FieldKey       windows.GUID
	MatchType      uint32
	ConditionValue fwpValue0
}

type fwpmAction0 struct {
	Type       uint32
	FilterType windows.GUID
}

type fwpmFilter0 struct {
	FilterKey           windows.GUID
	DisplayData         fwpmDisplayData0
	Flags               uint32
	ProviderKey         *windows.GUID
	ProviderData        fwpByteBlob
	LayerKey            windows.GUID
	SubLayerKey         windows.GUID
	Weight              fwpValue0
	NumFilterConditions uint32
	FilterCondition     *fwpmFilterCondition0
	Action              fwpmAction0
	_                   uint32       // C 里下面的联合体含 UINT64，按 8 对齐
	ProviderContextKey  windows.GUID // 联合体 { UINT64 rawContext; GUID providerContextKey; }
	Reserved            *windows.GUID
	FilterID            uint64
	EffectiveWeight     fwpValue0
}

type fwpmSession0 struct {
	SessionKey           windows.GUID
	DisplayData          fwpmDisplayData0
	Flags                uint32
	TxnWaitTimeoutInMSec uint32
	ProcessID            uint32
	SID                  *windows.SID
	Username             *uint16
	KernelMode           int32
}

type fwpmProvider0 struct {
	ProviderKey  windows.GUID
	DisplayData  fwpmDisplayData0
	Flags        uint32
	ProviderData fwpByteBlob
	ServiceName  *uint16
}

type fwpmSublayer0 struct {
	SubLayerKey  windows.GUID
	DisplayData  fwpmDisplayData0
	Flags        uint32
	ProviderKey  *windows.GUID
	ProviderData fwpByteBlob
	Weight       uint16
}

func toWin(g wfprules.GUID) windows.GUID {
	return windows.GUID{
		Data1: uint32(g[0])<<24 | uint32(g[1])<<16 | uint32(g[2])<<8 | uint32(g[3]),
		Data2: uint16(g[4])<<8 | uint16(g[5]),
		Data3: uint16(g[6])<<8 | uint16(g[7]),
		Data4: [8]byte(g[8:16]),
	}
}

func fromWin(g windows.GUID) wfprules.GUID {
	var o wfprules.GUID
	o[0], o[1], o[2], o[3] = byte(g.Data1>>24), byte(g.Data1>>16), byte(g.Data1>>8), byte(g.Data1)
	o[4], o[5] = byte(g.Data2>>8), byte(g.Data2)
	o[6], o[7] = byte(g.Data3>>8), byte(g.Data3)
	copy(o[8:], g.Data4[:])
	return o
}

// res 把系统调用的返回值换成错误。写法是 res("操作")(procX.Call(...))：指针到 uintptr 的转换必须直接写在
// Call 的参数里（Go 只在这种写法下保证指针在调用期间有效），不能经过别的函数转手。
func res(op string) func(r, _ uintptr, _ error) error {
	return func(r, _ uintptr, _ error) error {
		if r != 0 {
			return &Error{Op: op, Code: uint32(r)}
		}
		return nil
	}
}

// Error 是 WFP 调用返回的错误码。
type Error struct {
	Op   string
	Code uint32
}

func (e *Error) Error() string { return fmt.Sprintf("%s 失败（0x%08X）", e.Op, e.Code) }

func isCode(err error, code uint32) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// Engine 是一个打开的 WFP 会话。动态会话关闭时，系统删除这个会话添加的全部对象。
type Engine struct {
	h       windows.Handle
	dynamic bool
}

// Open 打开 WFP 会话。dynamic 为 true 时是动态会话（第一级探查只用这个）。需要管理员权限。
func Open(dynamic bool) (*Engine, error) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		return nil, errors.New("只支持 64 位 Windows")
	}
	if err := modFwpuclnt.Load(); err != nil {
		return nil, fmt.Errorf("加载 fwpuclnt.dll：%w", err)
	}
	for _, p := range allProcs {
		if err := p.Find(); err != nil {
			return nil, fmt.Errorf("找不到系统函数：%w", err)
		}
	}
	name, _ := windows.UTF16PtrFromString(wfprules.ProviderName)
	s := fwpmSession0{DisplayData: fwpmDisplayData0{Name: name}, TxnWaitTimeoutInMSec: 5000}
	if dynamic {
		s.Flags = sessionFlagDynamic
	}
	e := &Engine{dynamic: dynamic}
	err := res("打开 WFP 会话")(procEngineOpen0.Call(0, rpcAuthnDefault, 0, uintptr(unsafe.Pointer(&s)), uintptr(unsafe.Pointer(&e.h))))
	runtime.KeepAlive(name)
	if err != nil {
		return nil, err
	}
	return e, nil
}

// Close 关闭会话（动态会话的对象随之删除）。
func (e *Engine) Close() error {
	if e == nil || e.h == 0 {
		return nil
	}
	err := res("关闭 WFP 会话")(procEngineClose0.Call(uintptr(e.h)))
	e.h = 0
	return err
}

// txn 在一个事务里执行 fn：fn 出错或提交失败时整体回滚。
func (e *Engine) txn(fn func() error) error {
	if err := res("开始事务")(procTransactionBegin0.Call(uintptr(e.h), 0)); err != nil {
		return err
	}
	if err := fn(); err != nil {
		if aerr := res("回滚事务")(procTransactionAbort0.Call(uintptr(e.h))); aerr != nil {
			return fmt.Errorf("%w；%v", err, aerr)
		}
		return err
	}
	return res("提交事务")(procTransactionCommit0.Call(uintptr(e.h)))
}

// Install 在一个事务里建立我们的提供者、子层，并添加全部过滤器。persistent 只能用在非动态会话上。
// 任何一步失败整体回滚，不会留下一半的规则。
func (e *Engine) Install(specs []FilterSpec, persistent bool) error {
	if persistent && e.dynamic {
		return errors.New("动态会话不能添加持久对象")
	}
	for _, s := range specs {
		if s.Provider != wfprules.ProviderKey || s.Sublayer != wfprules.SublayerKey {
			return fmt.Errorf("%s 不属于我们的提供者或子层，拒绝添加", s.Name)
		}
	}
	var flags uint32
	if persistent {
		flags = objFlagPersistent
	}
	return e.txn(func() error {
		pk := toWin(wfprules.ProviderKey)
		pname, _ := windows.UTF16PtrFromString(wfprules.ProviderName)
		prov := fwpmProvider0{ProviderKey: pk, DisplayData: fwpmDisplayData0{Name: pname}, Flags: flags}
		if err := res("添加提供者")(procProviderAdd0.Call(uintptr(e.h), uintptr(unsafe.Pointer(&prov)), 0)); err != nil {
			return err
		}
		sname, _ := windows.UTF16PtrFromString(wfprules.SublayerName)
		sub := fwpmSublayer0{SubLayerKey: toWin(wfprules.SublayerKey), DisplayData: fwpmDisplayData0{Name: sname}, Flags: flags, ProviderKey: &pk, Weight: wfprules.SublayerWeight}
		if err := res("添加子层")(procSubLayerAdd0.Call(uintptr(e.h), uintptr(unsafe.Pointer(&sub)), 0)); err != nil {
			return err
		}
		runtime.KeepAlive(pname)
		runtime.KeepAlive(sname)
		for _, s := range specs {
			if err := e.addFilter(s, flags); err != nil {
				return err
			}
		}
		return nil
	})
}

// condMem 持有条件值指向的内存，直到系统调用返回。
type condMem struct {
	u64  []uint64
	addr []fwpV4AddrAndMask
	blob []fwpByteBlob
	sd   []*windows.SECURITY_DESCRIPTOR
}

func (m *condMem) value(c CondSpec) (fwpValue0, error) {
	v := fwpValue0{Type: c.Type}
	switch c.Type {
	case TypeUint8, TypeUint16, TypeUint32:
		v.Value = uintptr(c.U64)
	case TypeUint64:
		m.u64 = append(m.u64, c.U64)
		v.Value = uintptr(unsafe.Pointer(&m.u64[len(m.u64)-1]))
	case TypeV4AddrMask:
		m.addr = append(m.addr, fwpV4AddrAndMask{Addr: c.Addr, Mask: c.Mask})
		v.Value = uintptr(unsafe.Pointer(&m.addr[len(m.addr)-1]))
	case TypeSecDesc:
		sd, err := windows.SecurityDescriptorFromString(c.SDDL)
		if err != nil {
			return v, fmt.Errorf("生成安全描述符：%w", err)
		}
		m.sd = append(m.sd, sd)
		m.blob = append(m.blob, fwpByteBlob{Size: sd.Length(), Data: (*byte)(unsafe.Pointer(sd))})
		v.Value = uintptr(unsafe.Pointer(&m.blob[len(m.blob)-1]))
	default:
		return v, fmt.Errorf("不支持的条件类型 %#x", c.Type)
	}
	return v, nil
}

func (e *Engine) addFilter(s FilterSpec, flags uint32) error {
	// 预先分配容量，append 不会搬家，取到的指针一直有效。
	n := len(s.Conds)
	m := &condMem{u64: make([]uint64, 0, n), addr: make([]fwpV4AddrAndMask, 0, n), blob: make([]fwpByteBlob, 0, n)}
	conds := make([]fwpmFilterCondition0, n)
	for i, c := range s.Conds {
		v, err := m.value(c)
		if err != nil {
			return fmt.Errorf("%s：%w", s.Name, err)
		}
		conds[i] = fwpmFilterCondition0{FieldKey: toWin(c.Key), MatchType: c.Match, ConditionValue: v}
	}
	name, _ := windows.UTF16PtrFromString(s.Name)
	pk := toWin(s.Provider)
	f := fwpmFilter0{
		FilterKey:   toWin(s.Key),
		DisplayData: fwpmDisplayData0{Name: name},
		Flags:       flags,
		ProviderKey: &pk,
		LayerKey:    toWin(s.Layer),
		SubLayerKey: toWin(s.Sublayer),
		Weight:      fwpValue0{Type: TypeUint8, Value: uintptr(s.Weight)},
		Action:      fwpmAction0{Type: s.Action},
	}
	if n > 0 {
		f.NumFilterConditions = uint32(n)
		f.FilterCondition = &conds[0]
	}
	var id uint64
	err := res("添加过滤器 " + s.Name)(procFilterAdd0.Call(uintptr(e.h), uintptr(unsafe.Pointer(&f)), 0, uintptr(unsafe.Pointer(&id))))
	runtime.KeepAlive(m)
	runtime.KeepAlive(conds)
	runtime.KeepAlive(name)
	return err
}

// Enumerate 枚举全部过滤器、提供者、子层。别家的过滤器只读层、子层、提供者、动作，不读条件。
func (e *Engine) Enumerate() (Snapshot, error) {
	var snap Snapshot
	var err error
	if snap.Providers, err = e.enumProviders(); err != nil {
		return snap, err
	}
	if snap.Sublayers, err = e.enumSublayers(); err != nil {
		return snap, err
	}
	for _, p := range snap.Providers {
		if p.Key == wfprules.ProviderKey {
			snap.OurProvider = true
		}
	}
	for _, s := range snap.Sublayers {
		if s.Key == wfprules.SublayerKey {
			snap.OurSublayer = true
		}
	}
	var h windows.Handle
	if err := res("建立过滤器的枚举")(procFilterCreateEnum0.Call(uintptr(e.h), 0, uintptr(unsafe.Pointer(&h)))); err != nil {
		return snap, err
	}
	defer procFilterDestroyEnum0.Call(uintptr(e.h), uintptr(h)) // defer 的参数当场求值，调用推迟到函数返回
	for {
		var entries **fwpmFilter0
		var got uint32
		if err := res("枚举过滤器")(procFilterEnum0.Call(uintptr(e.h), uintptr(h), 256, uintptr(unsafe.Pointer(&entries)), uintptr(unsafe.Pointer(&got)))); err != nil {
			return snap, err
		}
		if got == 0 {
			if entries != nil {
				freeMem(unsafe.Pointer(&entries))
			}
			break
		}
		list := unsafe.Slice(entries, got)
		for _, fp := range list {
			raw := RawFilter{Layer: fromWin(fp.LayerKey), Sublayer: fromWin(fp.SubLayerKey), Action: fp.Action.Type}
			if fp.ProviderKey != nil {
				raw.Provider, raw.HasProvider = fromWin(*fp.ProviderKey), true
			}
			snap.Others = append(snap.Others, raw)
			if (raw.HasProvider && raw.Provider == wfprules.ProviderKey) || raw.Sublayer == wfprules.SublayerKey {
				snap.Ours = append(snap.Ours, readFilter(fp, raw))
			}
		}
		freeMem(unsafe.Pointer(&entries))
		if got < 256 {
			break
		}
	}
	return snap, nil
}

func readFilter(fp *fwpmFilter0, raw RawFilter) FilterSpec {
	s := FilterSpec{Key: fromWin(fp.FilterKey), Layer: raw.Layer, Sublayer: raw.Sublayer, Provider: raw.Provider, Action: fp.Action.Type}
	if fp.DisplayData.Name != nil {
		s.Name = windows.UTF16PtrToString(fp.DisplayData.Name)
	}
	if fp.Weight.Type == TypeUint8 {
		s.Weight = uint8(fp.Weight.Value)
	} else {
		s.Weight = 0xff // 不是我们写的形式；Decode 会因权重越界报错
	}
	if fp.NumFilterConditions > 0 && fp.FilterCondition != nil {
		for _, c := range unsafe.Slice(fp.FilterCondition, fp.NumFilterConditions) {
			ptr := valuePtr(&c.ConditionValue)
			cs := CondSpec{Key: fromWin(c.FieldKey), Match: c.MatchType, Type: c.ConditionValue.Type}
			switch c.ConditionValue.Type {
			case TypeUint8, TypeUint16, TypeUint32:
				cs.U64 = uint64(c.ConditionValue.Value)
			case TypeUint64:
				cs.U64 = *(*uint64)(ptr)
			case TypeV4AddrMask:
				am := (*fwpV4AddrAndMask)(ptr)
				cs.Addr, cs.Mask = am.Addr, am.Mask
			case TypeSecDesc:
				b := (*fwpByteBlob)(ptr)
				if b != nil && b.Data != nil {
					sd := (*windows.SECURITY_DESCRIPTOR)(unsafe.Pointer(b.Data))
					cs.SDDL = sd.String()
				}
			}
			s.Conds = append(s.Conds, cs)
		}
	}
	return s
}

// valuePtr 把联合体按指针读出（枚举结果里的指针指向 WFP 分配的内存，不是 Go 的堆）。
func valuePtr(v *fwpValue0) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&v.Value)) }

func freeMem(p unsafe.Pointer) { procFreeMemory0.Call(uintptr(p)) }

func (e *Engine) enumProviders() ([]ObjInfo, error) {
	var h windows.Handle
	var tmpl struct{ Reserved uint64 } // FWPM_PROVIDER_ENUM_TEMPLATE0：不给模板时 Enum 报 FWP_E_NULL_POINTER
	if err := res("建立提供者的枚举")(procProviderCreateEnum0.Call(uintptr(e.h), uintptr(unsafe.Pointer(&tmpl)), uintptr(unsafe.Pointer(&h)))); err != nil {
		return nil, err
	}
	defer procProviderDestroyEnum0.Call(uintptr(e.h), uintptr(h)) // defer 的参数当场求值，调用推迟到函数返回
	var out []ObjInfo
	for {
		var entries **fwpmProvider0
		var got uint32
		if err := res("枚举提供者")(procProviderEnum0.Call(uintptr(e.h), uintptr(h), 256, uintptr(unsafe.Pointer(&entries)), uintptr(unsafe.Pointer(&got)))); err != nil {
			return nil, err
		}
		if got == 0 {
			if entries != nil {
				freeMem(unsafe.Pointer(&entries))
			}
			break
		}
		for _, p := range unsafe.Slice(entries, got) {
			o := ObjInfo{Key: fromWin(p.ProviderKey)}
			if p.DisplayData.Name != nil {
				o.Name = windows.UTF16PtrToString(p.DisplayData.Name)
			}
			out = append(out, o)
		}
		freeMem(unsafe.Pointer(&entries))
		if got < 256 {
			break
		}
	}
	return out, nil
}

func (e *Engine) enumSublayers() ([]ObjInfo, error) {
	var h windows.Handle
	var tmpl struct{ ProviderKey *windows.GUID } // FWPM_SUBLAYER_ENUM_TEMPLATE0，不限提供者
	if err := res("建立子层的枚举")(procSubLayerCreateEnum0.Call(uintptr(e.h), uintptr(unsafe.Pointer(&tmpl)), uintptr(unsafe.Pointer(&h)))); err != nil {
		return nil, err
	}
	defer procSubLayerDestroyEnum0.Call(uintptr(e.h), uintptr(h)) // defer 的参数当场求值，调用推迟到函数返回
	var out []ObjInfo
	for {
		var entries **fwpmSublayer0
		var got uint32
		if err := res("枚举子层")(procSubLayerEnum0.Call(uintptr(e.h), uintptr(h), 256, uintptr(unsafe.Pointer(&entries)), uintptr(unsafe.Pointer(&got)))); err != nil {
			return nil, err
		}
		if got == 0 {
			if entries != nil {
				freeMem(unsafe.Pointer(&entries))
			}
			break
		}
		for _, s := range unsafe.Slice(entries, got) {
			o := ObjInfo{Key: fromWin(s.SubLayerKey)}
			if s.DisplayData.Name != nil {
				o.Name = windows.UTF16PtrToString(s.DisplayData.Name)
			}
			out = append(out, o)
		}
		freeMem(unsafe.Pointer(&entries))
		if got < 256 {
			break
		}
	}
	return out, nil
}

// DeleteOurs 在一个事务里删除我们的全部过滤器（提供者或子层是我们的）、子层和提供者。只按我们固定的两个键找对象，
// 不碰别家的任何对象。对象已经不在不算错误。返回删除的过滤器数。
func (e *Engine) DeleteOurs() (int, error) {
	snap, err := e.Enumerate()
	if err != nil {
		return 0, err
	}
	n := 0
	err = e.txn(func() error {
		for _, f := range snap.Ours {
			k := toWin(f.Key)
			if err := res("删除过滤器")(procFilterDeleteByKey0.Call(uintptr(e.h), uintptr(unsafe.Pointer(&k)))); err != nil && !isCode(err, errFilterNotFound) {
				return err
			}
			n++
		}
		sk := toWin(wfprules.SublayerKey)
		if err := res("删除子层")(procSubLayerDeleteByKey0.Call(uintptr(e.h), uintptr(unsafe.Pointer(&sk)))); err != nil && !isCode(err, errSublayerNotFound) {
			return err
		}
		pk := toWin(wfprules.ProviderKey)
		if err := res("删除提供者")(procProviderDeleteByKey0.Call(uintptr(e.h), uintptr(unsafe.Pointer(&pk)))); err != nil && !isCode(err, errProviderNotFound) {
			return err
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// IsAlreadyExists 报告错误是不是“对象已存在”（例如上次留下的持久提供者）。
func IsAlreadyExists(err error) bool { return isCode(err, errAlreadyExists) }
