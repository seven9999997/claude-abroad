package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"claude-abroad/internal/platform"
	"claude-abroad/internal/region"
	"claude-abroad/internal/sysnet"
)

// 待还原的地区（m4-plan v2.5 必须改 1）：恢复时时区或语言有几项没能写回原值，不能卡住恢复（不能拿网络当人质），
// 把这几项的原值和所属用户另存一份，照常写 RESTORED；之后重试，下一次从 RESTORED 启用时以它为准。
// 文件在状态目录里，0600，原子写，不随写入 RESTORED 清掉。

// PendingFileName 是待还原文件。
const PendingFileName = "region-pending.json"

// 待还原的项（日志和 status 里只出现这些名字，不出现值）。
const (
	ItemTimezone = "timezone"
	ItemAutoTZ   = "auto_timezone"
	ItemUser     = "user_language"
	ItemSystem   = "system_language"
)

// ItemLabel 是项目的中文名。
func ItemLabel(item string) string {
	switch item {
	case ItemTimezone:
		return "时区"
	case ItemAutoTZ:
		return "自动设置时区"
	case ItemUser:
		return "用户的语言和地区格式"
	case ItemSystem:
		return "系统级的语言"
	}
	return item
}

// RegionPending 是还没还原的原值：只有失败的项和所属用户。
type RegionPending struct {
	V          int             `json:"v"`
	Items      []string        `json:"items"`
	Timezone   string          `json:"timezone,omitempty"`
	AutoTZ     *sysnet.OptStr  `json:"auto_tz,omitempty"`
	User       string          `json:"user,omitempty"`
	UserLangs  *sysnet.OptList `json:"user_languages,omitempty"`
	UserLocale *sysnet.OptStr  `json:"user_locale,omitempty"`
	SysLangs   *sysnet.OptList `json:"sys_languages,omitempty"`
}

func (p *RegionPending) has(item string) bool { return slices.Contains(p.Items, item) }

func (p RegionPending) validate() error {
	if p.V != 1 || len(p.Items) == 0 {
		return errors.New("版本或项目不对")
	}
	for _, it := range p.Items {
		switch it {
		case ItemTimezone:
			if region.ValidTimezone(p.Timezone) != nil {
				return errors.New("时区不对")
			}
		case ItemAutoTZ:
			if p.AutoTZ == nil {
				return errors.New("缺自动时区")
			}
		case ItemUser:
			if p.User == "" || p.UserLangs == nil || p.UserLocale == nil {
				return errors.New("缺用户级的值")
			}
		case ItemSystem:
			if p.SysLangs == nil {
				return errors.New("缺系统级的值")
			}
		default:
			return errors.New("不认识的项")
		}
	}
	return nil
}

// Labels 是给人看的项目名（顿号连接）。
func (p RegionPending) Labels() string {
	var out []string
	for _, it := range p.Items {
		out = append(out, ItemLabel(it))
	}
	return strings.Join(out, "、")
}

// overlay 把待还原的值盖到 o 上（下一次启用时原值以待还原文件为准）。
// Apply 把待还原的项盖到 o 上（o 一般是当前读到的地区），得到要写回的目标（M6-5b 的应急程序用）。
func (p RegionPending) Apply(o sysnet.MacLocale) sysnet.MacLocale { return p.overlay(o) }

func (p RegionPending) overlay(o sysnet.MacLocale) sysnet.MacLocale {
	if p.has(ItemTimezone) {
		o.Timezone = p.Timezone
	}
	if p.has(ItemAutoTZ) {
		o.AutoTZ = *p.AutoTZ
	}
	if p.has(ItemUser) && (o.User == "" || o.User == p.User) {
		o.User, o.UserLangs, o.UserLocale = p.User, *p.UserLangs, *p.UserLocale
	}
	if p.has(ItemSystem) {
		o.SysLangs = *p.SysLangs
	}
	return o
}

func (s *Store) pendingPath() string { return filepath.Join(s.Dir, PendingFileName) }

// ErrPendingCorrupt 是待还原文件读不出来（忽略它，发事件）。
var ErrPendingCorrupt = errors.New("待还原文件损坏")

// LoadPending 读待还原文件。不存在时 ok 为 false；损坏、不是普通文件、权限不对时返回 ErrPendingCorrupt。
func (s *Store) LoadPending() (RegionPending, bool, error) {
	info, err := os.Lstat(s.pendingPath())
	if errors.Is(err, os.ErrNotExist) {
		return RegionPending{}, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 64<<10 {
		return RegionPending{}, false, ErrPendingCorrupt
	}
	b, err := os.ReadFile(s.pendingPath())
	if err != nil {
		return RegionPending{}, false, ErrPendingCorrupt
	}
	var p RegionPending
	if json.Unmarshal(b, &p) != nil || p.validate() != nil {
		return RegionPending{}, false, ErrPendingCorrupt
	}
	return p, true, nil
}

// SavePending 原子地写待还原文件（0600）。
func (s *Store) SavePending(p RegionPending) error {
	p.V = 1
	if err := p.validate(); err != nil {
		return fmt.Errorf("待还原文件内容不对：%w", err)
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := s.ensureDir(); err != nil {
		return err
	}
	tmp := filepath.Join(s.Dir, fmt.Sprintf(".%s.%d.%d.tmp", PendingFileName, os.Getpid(), time.Now().UnixNano()))
	fd, err := platform.OpenNoFollow(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := fd.Write(data)
	if werr == nil {
		werr = fd.Sync()
	}
	if cerr := fd.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, s.pendingPath())
	}
	if werr != nil {
		os.Remove(tmp)
		return werr
	}
	_ = fsyncDir(s.Dir)
	if s.afterPending != nil {
		s.afterPending("save " + strings.Join(p.Items, ","))
	}
	return nil
}

// DeletePending 删掉待还原文件（不存在时也算成功）。
func (s *Store) DeletePending() error {
	err := os.Remove(s.pendingPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err == nil {
		_ = fsyncDir(s.Dir)
		if s.afterPending != nil {
			s.afterPending("delete")
		}
	}
	return err
}
