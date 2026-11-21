package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// 虚拟机里的地区（design 9.2，m4-plan M4-5）：PUT /v1/region 写时区和语言，只碰这三个文件；
// 有变化时重启引擎一次，同样的值重复下发时什么都不做。时区数据来自镜像里的 tzdata（只读）。
const (
	RegionLocaltime = "/etc/localtime"
	RegionTZ        = "/etc/TZ"
	RegionLocaleSh  = "/etc/profile.d/locale.sh"
	ZoneinfoDir     = "/usr/share/zoneinfo"
)

// RegionFS 是地区要用的文件操作：读（三个文件和时区数据）和原子替换（三个文件）。
type RegionFS interface {
	ReadFile(path string) ([]byte, error)
	// Replace 原子地把 path 换成 data（path 是符号链接时换掉链接本身，不写穿到链接目标）。
	Replace(path string, data []byte, perm fs.FileMode) error
}

// Region 处理 PUT /v1/region。
type Region struct {
	FS      RegionFS
	Restart func(ctx context.Context) // 重启引擎（sing-box；主代理已下发时还有 mihomo）
	Base    context.Context
	Logf    func(format string, a ...any)

	mu sync.Mutex
}

// RegionRequest 是请求体。
type RegionRequest struct {
	Timezone string `json:"timezone"`
	Lang     string `json:"lang"`
}

var (
	agentTZRe   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+\-]*(/[A-Za-z0-9][A-Za-z0-9_+\-]*){0,2}$`)
	agentLangRe = regexp.MustCompile(`^[a-z]{2,3}(-[A-Z][a-z]{3})?(-([A-Z]{2}|[0-9]{3}))?$`)
)

// posixLocale 由语言标签得出 LANG 的值：en-US → en_US.UTF-8，zh-Hant-TW → zh_TW.UTF-8（musl-locales 不带文字代码）。
func posixLocale(lang string) string {
	parts := strings.Split(lang, "-")
	out := parts[0]
	if last := parts[len(parts)-1]; len(parts) > 1 && (len(last) == 2 || len(last) == 3 && last[0] >= '0' && last[0] <= '9') {
		out += "_" + last
	}
	return out + ".UTF-8"
}

// RegionFiles 是要写的三个文件的内容（纯函数）。读时区数据失败时报错。
func (r *Region) files(req RegionRequest) (map[string][]byte, error) {
	if len(req.Timezone) > 64 || !agentTZRe.MatchString(req.Timezone) || strings.Contains(req.Timezone, "..") || req.Timezone == "Local" {
		return nil, fmt.Errorf("时区不对：%q", req.Timezone)
	}
	if !agentLangRe.MatchString(req.Lang) {
		return nil, fmt.Errorf("语言不对：%q", req.Lang)
	}
	zone, err := r.FS.ReadFile(filepath.Join(ZoneinfoDir, req.Timezone))
	if err != nil || !bytes.HasPrefix(zone, []byte("TZif")) {
		return nil, fmt.Errorf("时区 %q 不在虚拟机的时区数据里", req.Timezone)
	}
	loc := posixLocale(req.Lang)
	return map[string][]byte{
		RegionLocaltime: zone,
		RegionTZ:        []byte(req.Timezone + "\n"),
		RegionLocaleSh:  []byte("# 由 abroad-agent 写入（地区同步），不要手改\nexport LANG=" + loc + "\nexport LC_ALL=" + loc + "\n"),
	}, nil
}

// Apply 写入并在有变化时重启引擎。返回是否有变化。
func (r *Region) Apply(ctx context.Context, req RegionRequest) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	want, err := r.files(req)
	if err != nil {
		return false, err
	}
	changed := false
	for _, p := range []string{RegionLocaltime, RegionTZ, RegionLocaleSh} {
		if cur, err := r.FS.ReadFile(p); err == nil && bytes.Equal(cur, want[p]) {
			continue
		}
		if err := r.FS.Replace(p, want[p], 0o644); err != nil {
			return changed, fmt.Errorf("写 %s：%w", p, err)
		}
		changed = true
	}
	if changed && r.Restart != nil {
		r.Restart(ctx)
	}
	return changed, nil
}

func (r *Region) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPut {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 PUT"})
		return
	}
	var body RegionRequest
	dec := json.NewDecoder(io.LimitReader(req.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不对"})
		return
	}
	base := r.Base
	if base == nil {
		base = context.Background()
	}
	changed, err := r.Apply(base, body)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	if r.Logf != nil {
		r.Logf("地区：已同步（有变化：%v）", changed)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"changed": changed})
}

// rootFS 是真实的 RegionFS（Root 是测试用的根目录前缀；虚拟机里是空）。
type rootFS struct{ root string }

func (f rootFS) ReadFile(p string) ([]byte, error) { return os.ReadFile(filepath.Join(f.root, p)) }

func (f rootFS) Replace(p string, data []byte, perm fs.FileMode) error {
	full := filepath.Join(f.root, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), ".abroad-region-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), full)
}
