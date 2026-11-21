package imagebuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"claude-abroad/internal/usererror"
)

// Artifact 是镜像的一个输入文件。
type Artifact struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	License string `json:"license"`
	Source  string `json:"source"`
}

// Manifest 是 guest/manifest.json。
type Manifest struct {
	Comment   string      `json:"comment,omitempty"`
	Alpine    string      `json:"alpine"`
	Artifacts []Artifact  `json:"artifacts"`
	Packages  *PackageSet `json:"packages,omitempty"` // M4-1：装进根文件系统的 Alpine 软件包
}

// PackageSet 是固定下来的 Alpine 软件包（m4-plan M4-1）。List 由 `abroad-imagebuild pin-packages` 生成：
// 按 Want 在 Repos 的索引里解依赖（索引的签名用 Keys 目录里的公钥校验），下载每个包、核对它与索引一致、
// 算出 SHA-256。正式构建只按 List 下载并校验，不在构建时解依赖，这样可以复现。
type PackageSet struct {
	Comment string          `json:"comment,omitempty"`
	Repos   []string        `json:"repos"` // 例如 https://dl-cdn.alpinelinux.org/alpine/v3.22/main/aarch64
	Keys    string          `json:"keys"`  // 信任的公钥目录（相对仓库根），例如 guest/keys
	Want    []string        `json:"want"`
	List    []PinnedPackage `json:"list"`
}

// PinnedPackage 是固定下来的一个包。Scripts 是包里的安装脚本（只记录，构建时不执行；需要的动作由 imagebuild 等价完成）。
type PinnedPackage struct {
	Name    string   `json:"name"`
	Version string   `json:"version"`
	URL     string   `json:"url"`
	SHA256  string   `json:"sha256"`
	Scripts []string `json:"scripts,omitempty"`
}

// Artifact 把固定的包当成一个下载项（复用 Fetch 的缓存和校验）。
func (p PinnedPackage) Artifact() Artifact {
	return Artifact{Name: p.Name, Version: p.Version, URL: p.URL, SHA256: p.SHA256}
}

var (
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	namePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)
)

// RequiredArtifacts 是构建必须有的输入。
var RequiredArtifacts = []string{"netboot", "minirootfs", "sing-box", "mihomo"}

// LoadManifest 读取并校验清单：每项必须有 https 地址和 64 位小写十六进制的 SHA-256。
func LoadManifest(p string) (*Manifest, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, usererror.Wrap("读取镜像清单", err, "请确认 guest/manifest.json 存在。")
	}
	return ParseManifest(b)
}

// ParseManifest 解析并校验清单内容。
func ParseManifest(b []byte) (*Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytesReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, usererror.New("读取镜像清单", "清单不是有效的 JSON 或含未知字段", "请检查 guest/manifest.json。", err)
	}
	seen := map[string]bool{}
	for _, a := range m.Artifacts {
		if !namePattern.MatchString(a.Name) || seen[a.Name] {
			return nil, usererror.New("读取镜像清单", fmt.Sprintf("条目名 %q 不合法或重复", a.Name), "请检查 guest/manifest.json。", nil)
		}
		seen[a.Name] = true
		u, err := url.Parse(a.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, usererror.New("读取镜像清单", fmt.Sprintf("%s 的地址必须是 https", a.Name), "请检查 guest/manifest.json。", err)
		}
		if !sha256Pattern.MatchString(a.SHA256) {
			return nil, usererror.New("读取镜像清单", fmt.Sprintf("%s 缺少 SHA-256 或格式不对（需要 64 位小写十六进制）", a.Name), "请从官方发布页取得校验值后填入 guest/manifest.json。", nil)
		}
	}
	if ps := m.Packages; ps != nil {
		for _, r := range ps.Repos {
			if u, err := url.Parse(r); err != nil || u.Scheme != "https" || u.Host == "" {
				return nil, usererror.New("读取镜像清单", "软件包仓库的地址必须是 https："+r, "请检查 guest/manifest.json。", err)
			}
		}
		pseen := map[string]bool{}
		for _, p := range ps.List {
			u, err := url.Parse(p.URL)
			if !namePattern.MatchString(p.Name) || pseen[p.Name] || err != nil || u.Scheme != "https" || !sha256Pattern.MatchString(p.SHA256) {
				return nil, usererror.New("读取镜像清单", fmt.Sprintf("软件包 %q 的名字、地址或 SHA-256 不合法（或重复）", p.Name),
					"请重新运行 abroad-imagebuild pin-packages。", err)
			}
			pseen[p.Name] = true
		}
	}
	for _, n := range RequiredArtifacts {
		if !seen[n] {
			return nil, usererror.New("读取镜像清单", "缺少必需的条目 "+n, "请检查 guest/manifest.json。", nil)
		}
	}
	return &m, nil
}

// Get 按名字取条目。
func (m *Manifest) Get(name string) (Artifact, bool) {
	for _, a := range m.Artifacts {
		if a.Name == name {
			return a, true
		}
	}
	return Artifact{}, false
}

// FileName 是下载后在缓存目录里的文件名。
func (a Artifact) FileName() string { return urlBase(a.URL) }

const maxDownload = 1 << 30

// IdleTimeout 是下载时允许连续收不到数据的最长时间。不设总超时：
// 慢速网络下 327 MB 的 netboot 包可能要很久，只要还在收数据就不打断。
var IdleTimeout = 60 * time.Second

// idleReader 在连续 timeout 没有读到数据时调用 cancel。
type idleReader struct {
	r     io.Reader
	timer *time.Timer
	d     time.Duration
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if n > 0 {
		i.timer.Reset(i.d)
	}
	return n, err
}

// ErrChecksum 表示下载内容的 SHA-256 与清单不符。
var ErrChecksum = errors.New("SHA-256 与清单不符")

// Fetch 把条目下载到 dir（已存在且校验通过就直接用）。先写临时文件，
// 校验通过才改名；任何失败都不留下目标文件。
func Fetch(ctx context.Context, client *http.Client, a Artifact, dir string) (string, error) {
	dst := filepath.Join(dir, a.FileName())
	if ok, _ := fileHasSHA256(dst, a.SHA256); ok {
		return dst, nil
	}
	_ = os.Remove(dst) // 校验不过的旧文件
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", usererror.Wrap("创建下载目录", err, "请检查仓库目录的写权限。")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	idle := time.AfterFunc(IdleTimeout, cancel)
	defer idle.Stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", usererror.Wrap("下载 "+a.Name, err, "请检查网络后重试。")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", usererror.New("下载 "+a.Name, fmt.Sprintf("服务器返回 HTTP %d", resp.StatusCode), "地址可能已失效，请更新 guest/manifest.json。", nil)
	}
	tmp, err := os.CreateTemp(dir, "."+a.FileName()+".part-*")
	if err != nil {
		return "", usererror.Wrap("创建临时文件", err, "请检查仓库目录的写权限。")
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // 成功改名后这里是空操作
	h := sha256.New()
	body := &idleReader{r: resp.Body, timer: idle, d: IdleTimeout}
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(body, maxDownload+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", usererror.Wrap("下载 "+a.Name, err, "请检查网络后重试。")
	}
	if n > maxDownload {
		return "", usererror.New("下载 "+a.Name, "文件超过 1 GiB", "请检查 guest/manifest.json 里的地址。", nil)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != a.SHA256 {
		return "", usererror.New("下载 "+a.Name, "SHA-256 与清单不符", "文件可能被篡改或清单写错了，请从官方发布页核对 guest/manifest.json。", ErrChecksum)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return "", usererror.Wrap("保存 "+a.Name, err, "请检查仓库目录的写权限。")
	}
	return dst, nil
}

func fileHasSHA256(p, want string) (bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == want, nil
}
