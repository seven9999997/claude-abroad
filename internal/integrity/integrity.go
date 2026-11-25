// Package integrity 是程序完整性校验（M6-7，替代没有付费开发者账号时用不了的 SecStaticCodeCheckValidity，推迟项 P6）：
// 构建时给 Helpers 和虚拟机镜像生成 SHA-256 清单；abroadd、guard 启动时按清单核对文件。清单本身的摘要在构建时经
// -ldflags 写进校验方的程序（ManifestDigest），清单被改了也能发现。
//
// 边界（写进设计 15）：
//   - 清单不含做校验的那个程序自己（它的摘要要写进自己，会循环）；
//   - 没有代码签名，防的是文件损坏、被替换，不防有意连同程序一起换掉的人（第 2 节：不防有意绕过的管理员）；
//   - 开发构建没有写入摘要时不校验，状态里如实写“未启用（开发构建）”。
package integrity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ManifestDigest 是清单文件的 SHA-256（十六进制），构建时经 -ldflags "-X claude-abroad/internal/integrity.ManifestDigest=…" 写入。
// 为空表示开发构建：不校验。
var ManifestDigest = ""

// Manifest 是完整性清单：相对路径 → SHA-256（十六进制）。
type Manifest struct {
	Version int               `json:"version"`
	Files   map[string]string `json:"files"`
}

// Generate 生成清单（按文件名排序后的 JSON，同样的输入逐字节相同）。names 是相对 base 的路径。
func Generate(base string, names []string) ([]byte, error) {
	m := Manifest{Version: 1, Files: map[string]string{}}
	for _, n := range names {
		if !validName(n) {
			return nil, fmt.Errorf("路径不合法：%q", n)
		}
		sum, err := fileSHA256(filepath.Join(base, n))
		if err != nil {
			return nil, fmt.Errorf("读 %s：%w", n, err)
		}
		m.Files[n] = sum
	}
	b, err := json.MarshalIndent(m, "", "  ") // map 的键按字典序输出
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func validName(n string) bool {
	return n != "" && !filepath.IsAbs(n) && !strings.Contains(n, "..") && filepath.Clean(n) == n
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Digest 是一段清单内容的 SHA-256（十六进制）。
func Digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Problem 是一个校验不过的地方（不含完整路径）。
type Problem struct {
	File string `json:"file"`
	Kind string `json:"kind"` // missing、mismatch、manifest（清单本身被改或读不出）
}

// Result 是一次校验的结果。
type Result struct {
	Checked  bool      `json:"checked"`            // 做了校验（开发构建、没配置清单时为 false）
	OK       bool      `json:"ok"`                 // 校验通过（没做校验时也为 true：不拦）
	Note     string    `json:"note,omitempty"`     // 没做校验的原因
	Problems []Problem `json:"problems,omitempty"` // 不通过的地方
}

// ErrNotConfigured 是没有配置清单。
var ErrNotConfigured = errors.New("没有配置完整性清单")

// Checker 按清单核对文件。
type Checker struct {
	Manifest string // 清单文件的路径
	Base     string // 清单里相对路径的根
	Digest   string // 清单应有的摘要（一般是 ManifestDigest）；为空表示开发构建，不校验
}

// Check 校验一次（只读）。
func (c *Checker) Check() Result {
	if c == nil || c.Manifest == "" {
		return Result{OK: true, Note: "没有配置完整性清单"}
	}
	if c.Digest == "" {
		return Result{OK: true, Note: "未启用（开发构建）"}
	}
	res := Result{Checked: true}
	b, err := os.ReadFile(c.Manifest)
	if err != nil || Digest(b) != c.Digest {
		res.Problems = append(res.Problems, Problem{File: filepath.Base(c.Manifest), Kind: "manifest"})
		return res
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if dec.Decode(&m) != nil || m.Version != 1 || len(m.Files) == 0 {
		res.Problems = append(res.Problems, Problem{File: filepath.Base(c.Manifest), Kind: "manifest"})
		return res
	}
	names := make([]string, 0, len(m.Files))
	for n := range m.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !validName(n) {
			res.Problems = append(res.Problems, Problem{File: filepath.Base(c.Manifest), Kind: "manifest"})
			continue
		}
		sum, err := fileSHA256(filepath.Join(c.Base, n))
		switch {
		case err != nil:
			res.Problems = append(res.Problems, Problem{File: n, Kind: "missing"})
		case sum != m.Files[n]:
			res.Problems = append(res.Problems, Problem{File: n, Kind: "mismatch"})
		}
	}
	res.OK = len(res.Problems) == 0
	return res
}

// Summary 是给日志、冲突检查用的一句话。
func (r Result) Summary() string {
	if r.OK {
		if !r.Checked {
			return "完整性校验：" + r.Note
		}
		return "完整性校验通过"
	}
	var parts []string
	for _, p := range r.Problems {
		switch p.Kind {
		case "missing":
			parts = append(parts, p.File+" 不见了")
		case "mismatch":
			parts = append(parts, p.File+" 内容不对")
		default:
			parts = append(parts, "完整性清单被改过或读不出")
		}
	}
	return "完整性校验不通过：" + strings.Join(parts, "；")
}
