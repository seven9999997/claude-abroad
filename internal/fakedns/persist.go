package fakedns

import (
	"bufio"
	"bytes"
	"claude-abroad/internal/platform"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// 映射文件格式（设计 4.3：每 30 秒落盘一次，abroadd 重启后读回）：
//
//	魔数 "ABRDFKIP"（8 字节）、版本 uint16、条数 uint32，
//	每条：IPv4（4 字节）、最近使用的 Unix 纳秒 int64、域名长度 uint8、域名，（版本 2 起）标志 uint8（bit0：本地域名），
//	末尾：前面全部内容的 sha256（32 字节）。整数都是大端。版本 1 的文件仍可读入，标志都按 0。
//
// 文件里有访问过的域名：0600，错误信息里不带域名。
const (
	fileMagic   = "ABRDFKIP"
	fileVersion = 2
	// MaxFileSize 是映射文件的大小上限（默认池满载约 3500 万字节以内）。
	MaxFileSize = 48 << 20
)

// ErrNoFile 是映射文件不存在（调用方当作空表，不算故障）。
var ErrNoFile = errors.New("fake-IP 映射文件不存在")

// ErrCorrupt 是映射文件损坏或不合法（整份丢弃，池保持为空）。
var ErrCorrupt = errors.New("fake-IP 映射文件损坏")

type record struct {
	addr  netip.Addr
	last  time.Time
	name  string
	local bool
}

// Save 把当前映射写到 w。
func (p *Pool) Save(w io.Writer) error {
	p.mu.Lock()
	recs := make([]record, 0, len(p.byName))
	for _, e := range p.byName {
		recs = append(recs, record{p.addr(e.idx), e.last, e.name, e.local})
	}
	p.mu.Unlock()
	sort.Slice(recs, func(i, j int) bool { return recs[i].addr.Less(recs[j].addr) })
	var buf bytes.Buffer
	buf.WriteString(fileMagic)
	_ = binary.Write(&buf, binary.BigEndian, uint16(fileVersion))
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(recs)))
	for _, r := range recs {
		a := r.addr.As4()
		buf.Write(a[:])
		_ = binary.Write(&buf, binary.BigEndian, r.last.UnixNano())
		buf.WriteByte(byte(len(r.name)))
		buf.WriteString(r.name)
		var flags byte
		if r.local {
			flags |= 1
		}
		buf.WriteByte(flags)
	}
	sum := sha256.Sum256(buf.Bytes())
	buf.Write(sum[:])
	_, err := w.Write(buf.Bytes())
	return err
}

// Load 用 r 的内容替换当前映射，全有或全无：任何一条不合法就整份丢弃，返回 ErrCorrupt（包装），池变成空的。
// 加载后引用数全为 0。
func (p *Pool) Load(r io.Reader) error {
	data, err := io.ReadAll(io.LimitReader(r, MaxFileSize+1))
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reset()
	if err != nil {
		return fmt.Errorf("读 fake-IP 映射文件：%w", err)
	}
	recs, err := p.parse(data)
	if err != nil {
		return err
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].last.Before(recs[j].last) }) // LRU 顺序：最久没用的在前
	for _, rc := range recs {
		i, _ := p.index(rc.addr)
		local := rc.local
		if p.localMatch != nil { // 以当前名单为准
			local = p.isLocalLocked(rc.name)
		}
		e := &entry{name: rc.name, idx: i, last: rc.last, local: local}
		e.elem = p.lru.PushBack(e)
		p.byName[rc.name], p.byIdx[i] = e, e
		p.occupy(i)
	}
	return nil
}

func (p *Pool) parse(data []byte) ([]record, error) {
	bad := func(why string) ([]record, error) { return nil, fmt.Errorf("%w：%s", ErrCorrupt, why) }
	if len(data) > MaxFileSize {
		return bad("超过大小上限")
	}
	const head = len(fileMagic) + 2 + 4
	if len(data) < head+sha256.Size {
		return bad("太短")
	}
	body, sum := data[:len(data)-sha256.Size], data[len(data)-sha256.Size:]
	if got := sha256.Sum256(body); !bytes.Equal(got[:], sum) {
		return bad("校验和不对")
	}
	if string(body[:len(fileMagic)]) != fileMagic {
		return bad("魔数不对")
	}
	v := binary.BigEndian.Uint16(body[len(fileMagic):])
	if v != 1 && v != fileVersion {
		return bad(fmt.Sprintf("版本 %d 不支持", v))
	}
	n := binary.BigEndian.Uint32(body[len(fileMagic)+2:])
	if n > p.size {
		return bad("条数超过地址池容量")
	}
	rd := bytes.NewReader(body[head:])
	recs := make([]record, 0, n)
	seenAddr := make(map[netip.Addr]bool, n)
	seenName := make(map[string]bool, n)
	for k := uint32(0); k < n; k++ {
		var a [4]byte
		var nanos int64
		var l byte
		if _, err := io.ReadFull(rd, a[:]); err != nil {
			return bad("截断")
		}
		if err := binary.Read(rd, binary.BigEndian, &nanos); err != nil {
			return bad("截断")
		}
		if l, _ = rd.ReadByte(); l == 0 {
			return bad("域名为空")
		}
		nb := make([]byte, l)
		if _, err := io.ReadFull(rd, nb); err != nil {
			return bad("截断")
		}
		var flags byte
		if v >= 2 {
			f, err := rd.ReadByte()
			if err != nil {
				return bad("截断")
			}
			if f&^1 != 0 {
				return bad("标志不认识")
			}
			flags = f
		}
		addr := netip.AddrFrom4(a)
		name, err := Normalize(string(nb))
		if err != nil || name != string(nb) {
			return bad("域名不合法")
		}
		if !p.Contains(addr) {
			return bad("地址不在可分配范围")
		}
		if seenAddr[addr] || seenName[name] {
			return bad("地址或域名重复")
		}
		seenAddr[addr], seenName[name] = true, true
		recs = append(recs, record{addr, time.Unix(0, nanos), name, flags&1 != 0})
	}
	if rd.Len() != 0 {
		return bad("末尾有多余内容")
	}
	return recs, nil
}

// SaveFile 原子地把映射写到 path：同目录临时文件 → fsync → rename（0600），再 fsync 目录。失败不留临时文件。
func (p *Pool) SaveFile(path string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".fakeip-*.tmp")
	if err != nil {
		return fmt.Errorf("写 fake-IP 映射文件：%w", err)
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("写 fake-IP 映射文件：%w", err)
	}
	w := bufio.NewWriter(tmp)
	if err := p.Save(w); err != nil {
		return fmt.Errorf("写 fake-IP 映射文件：%w", err)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("写 fake-IP 映射文件：%w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("写 fake-IP 映射文件：%w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("写 fake-IP 映射文件：%w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("写 fake-IP 映射文件：%w", err)
	}
	ok = true
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// LoadFile 读 path（O_NOFOLLOW，只接受普通文件）。文件不存在返回 ErrNoFile（包装）；
// 其他错误时池为空。
func (p *Pool) LoadFile(path string) error {
	cleanTemp(filepath.Dir(path))
	f, err := platform.OpenNoFollow(path, os.O_RDONLY, 0)
	if err != nil {
		p.mu.Lock()
		p.reset()
		p.mu.Unlock()
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNoFile
		}
		return fmt.Errorf("打开 fake-IP 映射文件（不接受符号链接）：%w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		p.mu.Lock()
		p.reset()
		p.mu.Unlock()
		return fmt.Errorf("%w：不是普通文件", ErrCorrupt)
	}
	return p.Load(f)
}

// cleanTemp 删掉 SaveFile 被打断（SIGKILL、断电）后残留的临时文件（里面有域名），只删普通文件。
func cleanTemp(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, ".fakeip-*.tmp"))
	for _, m := range matches {
		if st, err := os.Lstat(m); err == nil && st.Mode().IsRegular() {
			_ = os.Remove(m)
		}
	}
}

// Run 每 every 检查一次，有改动才落盘到 path；ctx 结束时有改动就最后落一次。
// 启动时和之后每 PruneInterval（默认 24 小时）清掉超过 MaxAge 没用的映射。
// 落盘失败交给 onErr（可为 nil），不中断。返回时已完成最后一次落盘。
func (p *Pool) Run(ctx context.Context, path string, every time.Duration, onErr func(error)) {
	pruneEvery := p.PruneInterval
	if pruneEvery <= 0 {
		pruneEvery = PruneEvery
	}
	pt := time.NewTicker(pruneEvery)
	defer pt.Stop()
	var saved uint64
	p.mu.Lock()
	saved = p.dirty
	p.mu.Unlock()
	p.Prune(p.clock(), MaxAge) // 在记下 saved 之后清理，清掉的结果会在下一个周期落盘
	flush := func() {
		p.mu.Lock()
		d := p.dirty
		p.mu.Unlock()
		if d == saved {
			return
		}
		if err := p.SaveFile(path); err != nil {
			if onErr != nil {
				onErr(err)
			}
			return
		}
		saved = d
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case <-t.C:
			flush()
		case <-pt.C:
			p.Prune(p.clock(), MaxAge)
		}
	}
}
