package imagebuild

// Alpine 软件包（.apk）的纯 Go 处理（m4-plan M4-1，风险 R19）：
//   - 解析 APKINDEX、做依赖闭包；
//   - 校验索引的签名（Alpine 的 RSA 公钥）和每个包控制段的校验和；
//   - 把包的数据段解进根文件系统的树。安装脚本只记录，不执行。
//
// 格式（apk-tools v2）：.apk 和 APKINDEX.tar.gz 都是几段首尾相接的 gzip，每段是一个不带结束块的 tar 片段：
//   签名段（.SIGN.RSA[256].<公钥名>：对下一段**压缩后的字节**的 RSA PKCS#1 v1.5 签名，RSA 用 SHA-1，RSA256 用 SHA-256）；
//   控制段（.PKGINFO 和安装脚本；索引文件里是 DESCRIPTION、APKINDEX）；
//   数据段（只有 .apk 有：包里的文件）。
// 索引里每个包的 C: 是 "Q1" + base64(控制段压缩字节的 SHA-1)；.PKGINFO 的 datahash 是数据段压缩字节的 SHA-256。

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

// APKPkg 是索引里的一个包。
type APKPkg struct {
	Name, Version, Arch string
	Depends             []string // D:（原样，含 so:、cmd:、版本约束、! 冲突）
	Provides            []string // p:
	Checksum            string   // C:（"Q1" + base64 SHA-1）
	Size                int64    // S:
	Priority            int      // k: provider_priority
	Repo                int      // 来自第几个仓库（调用方填）
}

// ParseAPKINDEX 解析 APKINDEX 文本（空行分隔的记录，每行 "X:值"）。
func ParseAPKINDEX(r io.Reader) ([]APKPkg, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var out []APKPkg
	var cur APKPkg
	have := false
	flush := func() error {
		if !have {
			return nil
		}
		if cur.Name == "" || cur.Version == "" {
			return fmt.Errorf("APKINDEX 里有缺少 P: 或 V: 的记录")
		}
		out = append(out, cur)
		cur, have = APKPkg{}, false
		return nil
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}
		if len(line) < 2 || line[1] != ':' {
			return nil, fmt.Errorf("APKINDEX 的行格式不对：%q", line)
		}
		have = true
		v := line[2:]
		switch line[0] {
		case 'P':
			cur.Name = v
		case 'V':
			cur.Version = v
		case 'A':
			cur.Arch = v
		case 'D':
			cur.Depends = strings.Fields(v)
		case 'p':
			cur.Provides = strings.Fields(v)
		case 'C':
			cur.Checksum = v
		case 'S':
			cur.Size, _ = strconv.ParseInt(v, 10, 64)
		case 'k':
			cur.Priority, _ = strconv.Atoi(v)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}

// depAtom 是一个依赖项：名字和可选的版本约束。
type depAtom struct {
	name, op, ver string
	conflict      bool
}

func parseDep(s string) depAtom {
	a := depAtom{}
	if strings.HasPrefix(s, "!") {
		a.conflict, s = true, s[1:]
	}
	for _, op := range []string{">=", "<=", "~", "=", ">", "<"} {
		if i := strings.Index(s, op); i > 0 {
			a.name, a.op, a.ver = s[:i], op, s[i+len(op):]
			return a
		}
	}
	a.name = s
	return a
}

// provideName 取 provides 项的名字和版本（"so:libfoo.so.1=1.2" → "so:libfoo.so.1", "1.2"）。
func provideName(p string) (string, string) {
	if i := strings.Index(p, "="); i > 0 {
		return p[:i], p[i+1:]
	}
	return p, ""
}

// CompareAPKVersion 比较两个 Alpine 版本号（够用的子集：数字段、字母后缀、_后缀、-r 修订号）。
func CompareAPKVersion(a, b string) int {
	ma, ra := splitRev(a)
	mb, rb := splitRev(b)
	if c := compareSegs(ma, mb); c != 0 {
		return c
	}
	return cmpInt(ra, rb)
}

func splitRev(v string) (string, int) {
	if i := strings.LastIndex(v, "-r"); i >= 0 {
		if n, err := strconv.Atoi(v[i+2:]); err == nil {
			return v[:i], n
		}
	}
	return v, 0
}

// compareSegs 按“数字串、非数字串”交替切开后逐段比较；数字按数值，其余按字典序。
func compareSegs(a, b string) int {
	ta, tb := tokens(a), tokens(b)
	for i := 0; i < len(ta) || i < len(tb); i++ {
		if i >= len(ta) {
			return -1
		}
		if i >= len(tb) {
			return 1
		}
		x, y := ta[i], tb[i]
		nx, ex := strconv.Atoi(x)
		ny, ey := strconv.Atoi(y)
		var c int
		if ex == nil && ey == nil {
			c = cmpInt(nx, ny)
		} else {
			c = strings.Compare(x, y)
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

func tokens(s string) []string {
	var out []string
	for len(s) > 0 {
		i := 1
		digit := s[0] >= '0' && s[0] <= '9'
		for i < len(s) && (s[i] >= '0' && s[i] <= '9') == digit {
			i++
		}
		if t := strings.Trim(s[:i], "."); t != "" {
			out = append(out, t)
		}
		s = s[i:]
	}
	return out
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func satisfies(ver, op, want string) bool {
	if op == "" {
		return true
	}
	if op == "~" {
		return ver == want || strings.HasPrefix(ver, want+".") || strings.HasPrefix(ver, want+"-")
	}
	c := CompareAPKVersion(ver, want)
	switch op {
	case "=":
		return c == 0
	case ">=":
		return c >= 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	case "<":
		return c < 0
	}
	return false
}

// ResolveAPK 从 want 出发做依赖闭包。installed 是根文件系统里已经有的包名和它们提供的名字
// （minirootfs 的 /lib/apk/db/installed），这些不再装。返回按名字排序的包（确定性）。
// 依赖找不到提供者、版本约束不满足时报错，指出是谁依赖了什么。
func ResolveAPK(index []APKPkg, want []string, installed map[string]string) ([]APKPkg, error) {
	byName := map[string][]int{}   // 包名 → 下标
	provider := map[string][]int{} // 提供的名字 → 下标
	for i, p := range index {
		byName[p.Name] = append(byName[p.Name], i)
		for _, pr := range p.Provides {
			n, _ := provideName(pr)
			provider[n] = append(provider[n], i)
		}
	}
	pick := func(a depAtom) (int, error) {
		var cands []int
		for _, i := range byName[a.name] {
			if satisfies(index[i].Version, a.op, a.ver) {
				cands = append(cands, i)
			}
		}
		if len(cands) == 0 {
			for _, i := range provider[a.name] {
				pv := ""
				for _, pr := range index[i].Provides {
					if n, v := provideName(pr); n == a.name {
						pv = v
					}
				}
				if pv == "" {
					pv = index[i].Version
				}
				if satisfies(pv, a.op, a.ver) {
					cands = append(cands, i)
				}
			}
		}
		if len(cands) == 0 {
			if len(byName[a.name])+len(provider[a.name]) > 0 {
				return -1, fmt.Errorf("没有满足版本约束 %s%s%s 的包", a.name, a.op, a.ver)
			}
			return -1, fmt.Errorf("没有包提供 %s", a.name)
		}
		// 选择顺序：名字正好相同的优先，其次 provider_priority 高的，再次版本高的，最后按名字（确定性）。
		sort.SliceStable(cands, func(x, y int) bool {
			px, py := index[cands[x]], index[cands[y]]
			if (px.Name == a.name) != (py.Name == a.name) {
				return px.Name == a.name
			}
			if px.Priority != py.Priority {
				return px.Priority > py.Priority
			}
			if c := CompareAPKVersion(px.Version, py.Version); c != 0 {
				return c > 0
			}
			return px.Name < py.Name
		})
		return cands[0], nil
	}
	chosen := map[string]int{}
	var queue []string
	type item struct{ dep, from string }
	work := []item{}
	for _, w := range want {
		work = append(work, item{w, "（要装的包）"})
	}
	for len(work) > 0 {
		it := work[0]
		work = work[1:]
		a := parseDep(it.dep)
		if a.conflict || strings.HasPrefix(a.name, "/") && installed[a.name] != "" {
			continue
		}
		if v, ok := installed[a.name]; ok && satisfies(v, a.op, a.ver) {
			continue // 根文件系统里已经有（minirootfs 的 busybox 提供 /bin/sh 等）
		}
		i, err := pick(a)
		if err != nil {
			return nil, fmt.Errorf("%s 依赖的 %s：%w", it.from, it.dep, err)
		}
		p := index[i]
		if prev, ok := chosen[p.Name]; ok {
			if prev != i {
				return nil, fmt.Errorf("%s 需要两个不同版本：%s 和 %s", p.Name, index[prev].Version, p.Version)
			}
			continue // 已经选过（循环依赖到这里终止）
		}
		chosen[p.Name] = i
		queue = append(queue, p.Name)
		for _, d := range p.Depends {
			work = append(work, item{d, p.Name})
		}
	}
	out := make([]APKPkg, 0, len(chosen))
	for _, i := range chosen {
		out = append(out, index[i])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	_ = queue
	return out, nil
}

// InstalledFromDB 解析 /lib/apk/db/installed（同 APKINDEX 格式）：包名和提供的名字 → 版本。
// 另外把根树里已有的 /bin/sh 这类路径依赖记为已满足。
func InstalledFromDB(db []byte, t *Tree) (map[string]string, error) {
	pkgs, err := ParseAPKINDEX(bytes.NewReader(db))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, p := range pkgs {
		out[p.Name] = p.Version
		for _, pr := range p.Provides {
			n, v := provideName(pr)
			if v == "" {
				v = p.Version
			}
			out[n] = v
		}
	}
	if t != nil {
		for _, e := range t.Entries() {
			if strings.HasPrefix(e.Path, "/bin/") || strings.HasPrefix(e.Path, "/usr/bin/") || strings.HasPrefix(e.Path, "/sbin/") {
				out[e.Path] = "0"
			}
		}
	}
	return out, nil
}

// ---- 分段与签名 ----

// splitGzipMembers 把首尾相接的 gzip 切成各段的原始（压缩）字节。
func splitGzipMembers(b []byte, max int) ([][]byte, error) {
	var out [][]byte
	off := 0
	for off < len(b) {
		if len(out) == max {
			return nil, fmt.Errorf("gzip 段数超过 %d", max)
		}
		br := bytes.NewReader(b[off:]) // bytes.Reader 实现 io.ByteReader：gzip 不会多读，读完一段后偏移正好在段尾
		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("第 %d 段不是 gzip：%w", len(out)+1, err)
		}
		zr.Multistream(false)
		if _, err := io.Copy(io.Discard, io.LimitReader(zr, maxTarFile+1)); err != nil {
			return nil, fmt.Errorf("第 %d 段解压失败：%w", len(out)+1, err)
		}
		n := len(b[off:]) - br.Len()
		out = append(out, b[off:off+n])
		off += n
	}
	return out, nil
}

// readSegment 解压一段并读出其中的 tar 项（tar 片段没有结束块）。
func readSegment(seg []byte, fn func(h *tar.Header, r io.Reader) error) error {
	zr, err := gzip.NewReader(bytes.NewReader(seg))
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(h, tr); err != nil {
			return err
		}
	}
}

// APKKeys 是信任的 Alpine 公钥：文件名（例如 alpine-devel@lists.alpinelinux.org-616ae350.rsa.pub）→ 公钥。
type APKKeys map[string]*rsa.PublicKey

// ParseAPKKey 解析 PEM 格式的 RSA 公钥。
func ParseAPKKey(b []byte) (*rsa.PublicKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("不是 PEM 公钥")
	}
	k, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("不是 RSA 公钥")
	}
	return rk, nil
}

// ErrSignature 表示签名不对或没有可信的公钥。
var ErrSignature = errors.New("签名校验不通过")

// verifySigned 校验第一段的签名是否覆盖第二段：签名文件名决定算法和公钥。
func verifySigned(segs [][]byte, keys APKKeys) (string, error) {
	if len(segs) < 2 {
		return "", fmt.Errorf("%w：缺少签名段或内容段", ErrSignature)
	}
	var keyName string
	var sig []byte
	var sha256Sig bool
	err := readSegment(segs[0], func(h *tar.Header, r io.Reader) error {
		n := path.Base(h.Name)
		switch {
		case strings.HasPrefix(n, ".SIGN.RSA256."):
			keyName, sha256Sig = strings.TrimPrefix(n, ".SIGN.RSA256."), true
		case strings.HasPrefix(n, ".SIGN.RSA."):
			keyName = strings.TrimPrefix(n, ".SIGN.RSA.")
		default:
			return nil
		}
		b, err := io.ReadAll(io.LimitReader(r, 4096))
		sig = b
		return err
	})
	if err != nil {
		return "", err
	}
	if keyName == "" {
		return "", fmt.Errorf("%w：第一段里没有 .SIGN.RSA 签名", ErrSignature)
	}
	k, ok := keys[keyName]
	if !ok {
		return keyName, fmt.Errorf("%w：签名用的公钥 %s 不在信任列表里", ErrSignature, keyName)
	}
	if sha256Sig {
		d := sha256.Sum256(segs[1])
		err = rsa.VerifyPKCS1v15(k, crypto.SHA256, d[:], sig)
	} else {
		d := sha1.Sum(segs[1])
		err = rsa.VerifyPKCS1v15(k, crypto.SHA1, d[:], sig)
	}
	if err != nil {
		return keyName, fmt.Errorf("%w：%s", ErrSignature, err)
	}
	return keyName, nil
}

// VerifyAPKINDEX 校验 APKINDEX.tar.gz 的签名，返回 APKINDEX 文本。
func VerifyAPKINDEX(b []byte, keys APKKeys) ([]byte, error) {
	segs, err := splitGzipMembers(b, 4)
	if err != nil {
		return nil, err
	}
	if _, err := verifySigned(segs, keys); err != nil {
		return nil, err
	}
	var idx []byte
	err = readSegment(segs[1], func(h *tar.Header, r io.Reader) error {
		if h.Name == "APKINDEX" {
			b, err := io.ReadAll(io.LimitReader(r, 64<<20))
			idx = b
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if idx == nil {
		return nil, fmt.Errorf("索引里没有 APKINDEX 文件")
	}
	return idx, nil
}

// VerifyAPK 校验 .apk 自带的签名（覆盖控制段）。返回签名用的公钥名。
func VerifyAPK(b []byte, keys APKKeys) (string, error) {
	segs, err := splitGzipMembers(b, 3)
	if err != nil {
		return "", err
	}
	return verifySigned(segs, keys)
}

// APKMeta 是一个包解进树后的信息。
type APKMeta struct {
	Name, Version string
	Scripts       []string // 安装脚本（.pre-install 等），只记录不执行
	Files         int
}

// apkControlChecksum 是索引里 C: 的格式："Q1" + base64(SHA-1(控制段压缩字节))。
func apkControlChecksum(control []byte) string {
	d := sha1.Sum(control)
	return "Q1" + base64.StdEncoding.EncodeToString(d[:])
}

// CheckAPKAgainstIndex 核对一个 .apk 的控制段校验和与索引里的 C: 一致（pin-packages 用，建立“索引签名 → 包”的信任链）。
func CheckAPKAgainstIndex(b []byte, p APKPkg) error {
	segs, err := splitGzipMembers(b, 3)
	if err != nil {
		return err
	}
	if len(segs) != 3 {
		return fmt.Errorf("%s：.apk 应有 3 段，实际 %d 段", p.Name, len(segs))
	}
	if got := apkControlChecksum(segs[1]); got != p.Checksum {
		return fmt.Errorf("%s：控制段的校验和与索引不符", p.Name)
	}
	return nil
}

// ExtractAPK 把 .apk 的数据段解进树：核对 .PKGINFO 里的 datahash；拒绝越界路径、绝对路径、
// 指向根外的符号链接；安装脚本只记录。
func ExtractAPK(b []byte, t *Tree) (APKMeta, error) {
	var m APKMeta
	segs, err := splitGzipMembers(b, 3)
	if err != nil {
		return m, err
	}
	if len(segs) != 3 {
		return m, fmt.Errorf(".apk 应有 3 段（签名、控制、数据），实际 %d 段", len(segs))
	}
	var dataHash string
	err = readSegment(segs[1], func(h *tar.Header, r io.Reader) error {
		switch n := h.Name; {
		case n == ".PKGINFO":
			b, err := io.ReadAll(io.LimitReader(r, 1<<20))
			if err != nil {
				return err
			}
			for _, l := range strings.Split(string(b), "\n") {
				k, v, ok := strings.Cut(l, " = ")
				if !ok {
					continue
				}
				switch k {
				case "pkgname":
					m.Name = v
				case "pkgver":
					m.Version = v
				case "datahash":
					dataHash = v
				}
			}
		case strings.HasPrefix(n, "."):
			m.Scripts = append(m.Scripts, n)
		}
		return nil
	})
	if err != nil {
		return m, fmt.Errorf("读控制段：%w", err)
	}
	if m.Name == "" {
		return m, fmt.Errorf("控制段里没有 .PKGINFO")
	}
	if dataHash != "" {
		d := sha256.Sum256(segs[2])
		if hex.EncodeToString(d[:]) != dataHash {
			return m, fmt.Errorf("%s：数据段的 SHA-256 与 .PKGINFO 不符", m.Name)
		}
	}
	sort.Strings(m.Scripts)
	err = readSegment(segs[2], func(h *tar.Header, r io.Reader) error {
		if err := checkAPKPath(h); err != nil {
			return fmt.Errorf("%s：%w", m.Name, err)
		}
		n := path.Clean("/" + h.Name)
		mode := fs.FileMode(h.Mode).Perm()
		if h.Mode&0o4000 != 0 {
			mode |= fs.ModeSetuid
		}
		if h.Mode&0o2000 != 0 {
			mode |= fs.ModeSetgid
		}
		if h.Mode&0o1000 != 0 {
			mode |= fs.ModeSticky
		}
		e := Entry{Path: n, UID: h.Uid, GID: h.Gid}
		switch h.Typeflag {
		case tar.TypeDir:
			if old, ok := t.Get(n); ok && old.IsDir() {
				return nil // 已有的目录保持原来的权限
			}
			e.Mode = fs.ModeDir | mode
		case tar.TypeReg:
			b, err := io.ReadAll(io.LimitReader(r, maxTarFile+1))
			if err != nil {
				return err
			}
			if len(b) > maxTarFile {
				return fmt.Errorf("%s 过大", n)
			}
			e.Mode, e.Data = mode, b
			m.Files++
		case tar.TypeSymlink:
			e.Mode, e.Link = fs.ModeSymlink|0o777, h.Linkname
			m.Files++
		case tar.TypeLink:
			src, ok := t.Get(path.Clean("/" + h.Linkname))
			if !ok || !src.IsRegular() {
				return fmt.Errorf("硬链接 %s 指向的 %s 不存在", n, h.Linkname)
			}
			e.Mode, e.Data = src.Mode, src.Data
			m.Files++
		default:
			return nil // PAX 头等其他类型跳过
		}
		t.Remove(n)
		return t.Add(e)
	})
	if err != nil {
		return m, err
	}
	return m, nil
}

// checkAPKPath 拒绝数据段里的越界路径、绝对路径和指向根外的符号链接。
func checkAPKPath(h *tar.Header) error {
	if h.Typeflag == tar.TypeXGlobalHeader || h.Typeflag == tar.TypeXHeader {
		return nil
	}
	if strings.HasPrefix(h.Name, "/") {
		return fmt.Errorf("包里有绝对路径 %q", h.Name)
	}
	for _, seg := range strings.Split(h.Name, "/") {
		if seg == ".." {
			return fmt.Errorf("包里有越界路径 %q", h.Name)
		}
	}
	if h.Typeflag == tar.TypeSymlink && !strings.HasPrefix(h.Linkname, "/") {
		// 相对目标：从链接所在目录出发，不能走到根之上。
		depth := strings.Count(path.Dir(path.Clean("/"+h.Name)), "/")
		if path.Dir(path.Clean("/"+h.Name)) == "/" {
			depth = 0
		}
		for _, seg := range strings.Split(h.Linkname, "/") {
			switch seg {
			case "..":
				depth--
				if depth < 0 {
					return fmt.Errorf("符号链接 %q 指向根外（%q）", h.Name, h.Linkname)
				}
			case ".", "":
			default:
				depth++
			}
		}
	}
	if h.Typeflag == tar.TypeLink {
		for _, seg := range strings.Split(h.Linkname, "/") {
			if seg == ".." {
				return fmt.Errorf("硬链接 %q 指向越界路径", h.Name)
			}
		}
	}
	return nil
}
