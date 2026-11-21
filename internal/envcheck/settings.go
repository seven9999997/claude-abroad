package envcheck

import (
	"claude-abroad/internal/platform"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"
)

// SettingsFileName 是警告项开关的文件（状态目录里，0600）。
const SettingsFileName = "envcheck.json"

// Settings 是用户关闭了的警告项（m4-plan v2.6 决定 5）。硬性项不能关。
type Settings struct {
	Path string
	mu   sync.Mutex
}

type settingsFile struct {
	Disabled []string `json:"disabled"`
}

// ErrHardItem 是试图关闭硬性项或不认识的项。
var ErrHardItem = errors.New("只能关闭警告项")

// Disabled 返回关闭了的项。文件不存在、读不出来、权限不对时按“没有关闭任何项”处理。
func (s *Settings) Disabled() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]bool{}
	info, err := os.Lstat(s.Path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return out
	}
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return out
	}
	var f settingsFile
	if json.Unmarshal(b, &f) != nil {
		return out
	}
	for _, id := range f.Disabled {
		out[id] = true
	}
	return out
}

// SetDisabled 设置关闭的警告项（整份替换）。有硬性项或不认识的项时返回 ErrHardItem，不改文件。
func (s *Settings) SetDisabled(ids []string, items []Item) error {
	level := map[string]Level{}
	for _, it := range items {
		level[it.ID] = it.Level
	}
	for _, id := range ids {
		if level[id] != Warn {
			return fmt.Errorf("%w：%q", ErrHardItem, id)
		}
	}
	ids = slices.Clone(ids)
	sort.Strings(ids)
	ids = slices.Compact(ids)
	b, _ := json.Marshal(settingsFile{Disabled: ids})
	s.mu.Lock()
	defer s.mu.Unlock()
	tmp := filepath.Join(filepath.Dir(s.Path), fmt.Sprintf(".%s.%d.tmp", SettingsFileName, time.Now().UnixNano()))
	fd, err := platform.OpenNoFollow(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := fd.Write(b)
	if werr == nil {
		werr = fd.Sync()
	}
	if cerr := fd.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, s.Path)
	}
	if werr != nil {
		os.Remove(tmp)
	}
	return werr
}
