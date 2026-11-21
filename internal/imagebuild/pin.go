package imagebuild

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"claude-abroad/internal/usererror"
)

// PinOptions 控制一次 pin-packages。
type PinOptions struct {
	ManifestPath string
	RepoRoot     string // Keys 目录相对它
	OutDir       string // 缓存在 <OutDir>/cache
	Client       *http.Client
	Log          func(format string, a ...any)
}

// LoadAPKKeys 读目录里的全部 .rsa.pub 公钥。
func LoadAPKKeys(dir string) (APKKeys, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	keys := APKKeys{}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".rsa.pub") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		k, err := ParseAPKKey(b)
		if err != nil {
			return nil, fmt.Errorf("%s：%w", e.Name(), err)
		}
		keys[e.Name()] = k
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%s 里没有公钥", dir)
	}
	return keys, nil
}

// PinPackages 按清单的 Want 重新固定软件包，写回清单，返回新旧列表的差异（每行一条）。
// 信任链：minirootfs（清单里固定了 SHA-256）→ 公钥目录（从它取出、随仓库保存）→ 索引的签名
// → 每个包控制段的校验和（索引的 C:）→ 包的 SHA-256（写进清单）。
func PinPackages(ctx context.Context, o PinOptions) ([]string, error) {
	logf := o.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	m, err := LoadManifest(o.ManifestPath)
	if err != nil {
		return nil, err
	}
	ps := m.Packages
	if ps == nil || len(ps.Repos) == 0 || len(ps.Want) == 0 || ps.Keys == "" {
		return nil, usererror.New("固定软件包", "清单里没有 packages（repos、keys、want）", "请先在 guest/manifest.json 里写好 packages。", nil)
	}
	keys, err := LoadAPKKeys(filepath.Join(o.RepoRoot, ps.Keys))
	if err != nil {
		return nil, usererror.Wrap("读取信任的公钥", err, "公钥目录随仓库保存（guest/keys），请检查。")
	}
	cache := filepath.Join(o.OutDir, CacheDirName)
	// 根文件系统里已经有的包：从固定好的 minirootfs 里读 /lib/apk/db/installed。
	mr, _ := m.Get("minirootfs")
	mrPath, err := Fetch(ctx, o.Client, mr, cache)
	if err != nil {
		return nil, err
	}
	db, err := extractFromFile(mrPath, []string{"lib/apk/db/installed"})
	if err != nil {
		return nil, usererror.New("读取 minirootfs 里已装的包", err.Error(), "请核对 guest/manifest.json 里的 minirootfs。", err)
	}
	root, err := buildBareRoot(mrPath)
	if err != nil {
		return nil, err
	}
	installed, err := InstalledFromDB(db["lib/apk/db/installed"], root)
	if err != nil {
		return nil, err
	}
	var index []APKPkg
	for ri, repo := range ps.Repos {
		b, err := httpGet(ctx, o.Client, strings.TrimSuffix(repo, "/")+"/APKINDEX.tar.gz", 64<<20)
		if err != nil {
			return nil, usererror.Wrap("下载软件包索引", err, "请检查网络后重试。")
		}
		txt, err := VerifyAPKINDEX(b, keys)
		if err != nil {
			return nil, usererror.New("校验软件包索引 "+repo, err.Error(), "索引的签名不对：可能被篡改，或 Alpine 换了签名公钥（核对 minirootfs 里的 /etc/apk/keys）。", err)
		}
		pk, err := ParseAPKINDEX(bytes.NewReader(txt))
		if err != nil {
			return nil, err
		}
		for i := range pk {
			pk[i].Repo = ri
		}
		index = append(index, pk...)
		logf("索引 %s：%d 个包，签名通过", repo, len(pk))
	}
	pkgs, err := ResolveAPK(index, ps.Want, installed)
	if err != nil {
		return nil, usererror.New("解软件包的依赖", err.Error(), "请检查 packages.want。", err)
	}
	var list []PinnedPackage
	for _, p := range pkgs {
		u := strings.TrimSuffix(ps.Repos[p.Repo], "/") + "/" + p.Name + "-" + p.Version + ".apk"
		b, err := httpGet(ctx, o.Client, u, maxTarFile)
		if err != nil {
			return nil, usererror.Wrap("下载软件包 "+p.Name, err, "请检查网络后重试。")
		}
		if err := CheckAPKAgainstIndex(b, p); err != nil {
			return nil, usererror.New("核对软件包 "+p.Name, err.Error(), "包与签过名的索引不一致，可能被篡改。", err)
		}
		meta, err := ExtractAPK(b, NewTree())
		if err != nil {
			return nil, usererror.New("解开软件包 "+p.Name, err.Error(), "", err)
		}
		d := sha256.Sum256(b)
		pp := PinnedPackage{Name: p.Name, Version: p.Version, URL: u, SHA256: hex.EncodeToString(d[:]), Scripts: meta.Scripts}
		// 存进缓存，构建时不必再下载。
		if err := os.MkdirAll(filepath.Join(cache, "apk"), 0o755); err == nil {
			_ = writeFileAtomic(filepath.Join(cache, "apk", pp.Artifact().FileName()), b)
		}
		list = append(list, pp)
	}
	diff := DiffPinned(ps.List, list)
	ps.List = list
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(o.ManifestPath, append(out, '\n')); err != nil {
		return nil, err
	}
	return diff, nil
}

// DiffPinned 列出新旧清单的差异（新增、删除、版本或哈希变化），供人工复核。
func DiffPinned(old, cur []PinnedPackage) []string {
	om := map[string]PinnedPackage{}
	for _, p := range old {
		om[p.Name] = p
	}
	nm := map[string]PinnedPackage{}
	for _, p := range cur {
		nm[p.Name] = p
	}
	var out []string
	for _, p := range cur {
		o, ok := om[p.Name]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("+ %s %s", p.Name, p.Version))
		case o.Version != p.Version:
			out = append(out, fmt.Sprintf("~ %s %s → %s", p.Name, o.Version, p.Version))
		case o.SHA256 != p.SHA256:
			out = append(out, fmt.Sprintf("! %s %s：版本没变但 SHA-256 变了（请核实）", p.Name, p.Version))
		case strings.Join(o.Scripts, ",") != strings.Join(p.Scripts, ","):
			out = append(out, fmt.Sprintf("~ %s 的安装脚本变了：%v → %v", p.Name, o.Scripts, p.Scripts))
		}
	}
	for _, p := range old {
		if _, ok := nm[p.Name]; !ok {
			out = append(out, fmt.Sprintf("- %s %s", p.Name, p.Version))
		}
	}
	sort.Strings(out)
	return out
}

func httpGet(ctx context.Context, c *http.Client, u string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s 返回 HTTP %d", u, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s 超过 %d 字节", u, max)
	}
	return b, nil
}

// buildBareRoot 只把 minirootfs 读成树（判断 /bin/sh 这类路径依赖是否已满足）。
func buildBareRoot(minirootfs string) (*Tree, error) {
	t := NewTree()
	f, err := os.Open(minirootfs)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := AddTarGz(t, f); err != nil {
		return nil, fmt.Errorf("读取 minirootfs：%w", err)
	}
	return t, nil
}
