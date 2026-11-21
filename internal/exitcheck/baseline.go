package exitcheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"claude-abroad/internal/exitpolicy"
)

// BaselineVersion 是基准文件的版本。
const BaselineVersion = 1

// Baseline 是“应用主代理”那次检查得到的出口基准（design 8），也是地区锁定的依据（9.1）。
// 文件在状态目录里，0600，原子写；内容有出口 IP 和位置，属于敏感数据。
type Baseline struct {
	V        int             `json:"v"`
	Rule     exitpolicy.Rule `json:"rule"`
	IP       string          `json:"ip"`
	Country  string          `json:"country"`
	Region   string          `json:"region,omitempty"`
	City     string          `json:"city,omitempty"`
	Timezone string          `json:"timezone,omitempty"`
	At       time.Time       `json:"at"`
}

// ErrNoBaseline 表示没有可用的基准（不存在、损坏、被换成符号链接、权限不对、字段不合法）：
// 这时启用要求重新“应用主代理”，不能跳过比对（m4-plan v2.1 决定 6）。
var ErrNoBaseline = errors.New("没有可用的出口基准")

// BaselineFromReport 用一轮检查（WantGeo）的结果建立基准。出口不在大陆、取到了 IP 和国家才可以。
func BaselineFromReport(r Report, rule exitpolicy.Rule) (Baseline, error) {
	if !rule.Valid() {
		return Baseline{}, fmt.Errorf("出口变化规则不认识：%q", rule)
	}
	if it, ok := r.Item("mainland"); !ok || it.Result != Pass || !r.IP.IsValid() || r.Country == "" {
		return Baseline{}, errors.New("这一轮检查没有明确的出口 IP 和国家，或出口在大陆，不能作为基准")
	}
	b := Baseline{V: BaselineVersion, Rule: rule, IP: r.IP.String(), Country: r.Country, At: r.At}
	if r.Geo != nil {
		b.Region, b.City, b.Timezone = r.Geo.Region, r.Geo.City, r.Geo.Timezone
	}
	if rule.NeedsGeo() && (b.Region == "" || b.City == "") {
		return Baseline{}, errors.New("规则是同一城市，但这一轮没有查到州省和城市")
	}
	return b, b.validate()
}

func (b Baseline) geo() exitpolicy.Geo {
	ip, _ := netip.ParseAddr(b.IP)
	return exitpolicy.Geo{IP: ip, Country: b.Country, Region: b.Region, City: b.City, Timezone: b.Timezone}
}

var countryRe = regexp.MustCompile(`^[A-Z]{2}$`)

func (b Baseline) validate() error {
	ip, err := netip.ParseAddr(b.IP)
	switch {
	case b.V != BaselineVersion:
		return fmt.Errorf("版本 %d 不认识", b.V)
	case !b.Rule.Valid():
		return fmt.Errorf("规则 %q 不认识", b.Rule)
	case err != nil || !ip.Is4() || !ip.IsGlobalUnicast() || ip.IsPrivate():
		return errors.New("出口 IP 不合法")
	case !countryRe.MatchString(b.Country):
		return errors.New("国家代码不合法")
	case len(b.Region) > 120 || len(b.City) > 120:
		return errors.New("地区名称过长")
	case b.Timezone != "":
		if _, err := time.LoadLocation(b.Timezone); err != nil {
			return errors.New("时区不合法")
		}
	}
	if b.Rule.NeedsGeo() && (b.Region == "" || b.City == "") {
		return errors.New("规则是同一城市，但缺少州省或城市")
	}
	if b.At.IsZero() {
		return errors.New("缺少时间")
	}
	return nil
}

// SaveBaseline 原子地写基准：同目录临时文件 → 0600 → fsync → rename → fsync 目录。
func SaveBaseline(path string, b Baseline) error {
	if err := b.validate(); err != nil {
		return fmt.Errorf("基准不合法：%w", err)
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".baseline-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if df, err := os.Open(dir); err == nil {
		_ = df.Sync()
		df.Close()
	}
	return nil
}

// LoadBaseline 读基准。不存在、不是普通文件（符号链接等）、别人可读写、格式或字段不合法，一律返回包着 ErrNoBaseline 的错误。
func LoadBaseline(path string) (Baseline, error) {
	noBase := func(why string) (Baseline, error) {
		return Baseline{}, fmt.Errorf("%w：%s", ErrNoBaseline, why)
	}
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return noBase("文件不存在")
	}
	if err != nil {
		return noBase(err.Error())
	}
	if !fi.Mode().IsRegular() {
		return noBase("不是普通文件（可能被换成了符号链接）")
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return noBase(fmt.Sprintf("权限是 %o，应只有属主能读写", fi.Mode().Perm()))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return noBase(err.Error())
	}
	var b Baseline
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return noBase("内容损坏")
	}
	if err := b.validate(); err != nil {
		return noBase(err.Error())
	}
	return b, nil
}
