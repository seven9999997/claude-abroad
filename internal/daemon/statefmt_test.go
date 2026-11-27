package daemon

import (
	"encoding"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"claude-abroad/internal/buildinfo"
	"claude-abroad/internal/claudecfg"
	"claude-abroad/internal/claudedomains"
	"claude-abroad/internal/envcheck"
	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/guard"
	"claude-abroad/internal/managedproxy"
	"claude-abroad/internal/region"
	"claude-abroad/internal/resolverlock"
	"claude-abroad/internal/rules"
	"claude-abroad/internal/state"
)

var updateStateFormat = flag.Bool("update-state-format", false, "重写 testdata/state-format.golden（落盘结构变了时；要求 buildinfo.StateFormat 已经加一）")

const stateFormatGolden = "testdata/state-format.golden"

// persistedEntry 是状态目录里的一种落盘文件：位置、编码（json、yaml）、Go 类型的零值；文本文件 v 为 nil、note 写内容。
type persistedEntry struct {
	path string // 相对安装目录的 state/
	enc  string
	v    any
	note string
}

// persistedFiles 是状态目录下的全部落盘文件（v0.71 原地升级：逐个列出、和 Go 类型对上）。
// state/abroadd 是 abroadd 的 --state-dir；state/store 是 --main-proxy-store；state/vm 只有运行时的 socket、pid、
// 控制台日志，不算落盘格式。新增一种落盘文件时加在这里。
func persistedFiles() []persistedEntry {
	rl, cc, cd, ec, rs := resolverlock.PersistedFormats(), claudecfg.PersistedFormats(), claudedomains.PersistedFormats(),
		envcheck.PersistedFormats(), rules.PersistedFormats()
	return []persistedEntry{
		{"abroadd/" + state.FileName, "json", state.File{}, ""},
		{"abroadd/" + state.PendingFileName, "json", state.RegionPending{}, ""},
		{"abroadd/" + BaselineFile, "json", exitcheck.Baseline{}, ""},
		{"abroadd/" + RegionFileName, "json", region.Target{}, ""},
		{"abroadd/" + RulesFile, "yaml", rs["rules.yaml"], ""},
		{"abroadd/" + SetupFile, "json", setupFile{}, ""},
		{"abroadd/" + LANSettingFile, "json", LANSetting{}, ""},
		{"abroadd/" + ClaudeProxySwitchFile, "json", claudeSwitchFile{}, ""},
		{"abroadd/" + guard.ParamsFile, "json", guard.Params{}, ""},
		{"abroadd/" + resolverlock.RecordFile, "json", rl[resolverlock.RecordFile], ""},
		{"abroadd/" + claudecfg.RecordFile, "json", cc[claudecfg.RecordFile], ""},
		{"abroadd/" + claudedomains.FileName, "json", cd[claudedomains.FileName], ""},
		{"abroadd/" + envcheck.SettingsFileName, "json", ec[envcheck.SettingsFileName], ""},
		{"abroadd/" + envcheck.RunHistoryFileName, "json", ec[envcheck.RunHistoryFileName], ""},
		{"abroadd/" + envcheck.HistoryFileName, "json", ec[envcheck.HistoryFileName], ""},
		{"abroadd/" + SecretsDir + "/exit-<标签>.json", "json", StoredExit{}, ""},
		{"store/proxy/sources.json", "json", managedproxy.Store{}, ""},
		{"store/proxy/applied.json", "json", appliedRecord{}, ""},
		{"store/proxy/lan-confirm.json", "json", []lanConfirm{}, ""},
		{"store/proxy/pending.json", "json", map[string]string{}, ""},
		{"abroadd/" + PreparedFile, "text", nil, "一行 RFC3339 时间（首次准备完成的标记）"},
		{"abroadd/" + UninstalledFile, "text", nil, "一行 RFC3339 时间（已经一键卸载的标记）"},
		{"abroadd/" + state.ReturnBlockSetFile, "text", nil, "一行回国拦截地址段的选择（pf.ReturnBlockSet）"},
		{"abroadd/" + envcheck.HistoryKeyName, "binary", nil, "出口 IP 历史的密钥"},
		{"abroadd/" + state.LockName, "lock", nil, "进程锁（没有内容）"},
	}
}

var (
	jsonMarshaler = reflect.TypeFor[json.Marshaler]()
	textMarshaler = reflect.TypeFor[encoding.TextMarshaler]()
)

// describeFormat 用反射把一种落盘结构的字段清单列成固定的文字：每个结构体（按类型名去重）一段，每个导出字段一行：
// Go 字段名、标签里的名字和选项、类型和底层种类。自带序列化（MarshalJSON、MarshalText）的类型只写名字，不展开。
func describeFormat(v any, tagKey string) []string {
	var out []string
	seen := map[reflect.Type]bool{}
	var walk func(t reflect.Type)
	typ := func(t reflect.Type) string { return t.String() + "(" + t.Kind().String() + ")" }
	walk = func(t reflect.Type) {
		for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
			if t.Kind() == reflect.Map {
				walk(t.Key())
			}
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct || seen[t] || t.Implements(jsonMarshaler) || t.Implements(textMarshaler) ||
			reflect.PointerTo(t).Implements(jsonMarshaler) || reflect.PointerTo(t).Implements(textMarshaler) {
			return
		}
		seen[t] = true
		out = append(out, "  type "+t.String())
		var next []reflect.Type
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() && !f.Anonymous {
				continue
			}
			tag, ok := f.Tag.Lookup(tagKey)
			if !ok {
				tag = "（没有标签）"
			}
			out = append(out, fmt.Sprintf("    %s %s=%q %s", f.Name, tagKey, tag, typ(f.Type)))
			next = append(next, f.Type)
		}
		for _, n := range next {
			walk(n)
		}
	}
	t := reflect.TypeOf(v)
	out = append(out, "  root "+typ(t))
	walk(t)
	return out
}

// stateFormatListing 是全部落盘文件的清单（不含第一行的格式版本号）。
func stateFormatListing(files []persistedEntry) []string {
	var out []string
	for _, f := range files {
		out = append(out, fmt.Sprintf("file %s %s", f.path, f.enc))
		if f.v == nil {
			out = append(out, "  "+f.note)
			continue
		}
		out = append(out, describeFormat(f.v, f.enc)...)
	}
	return out
}

func stateFormatHeader(format int) string { return "StateFormat " + strconv.Itoa(format) }

// checkStateFormat 比较入库的清单和现在的：字段有任何增删、改名、改类型都报错，提示同时改 StateFormat。
func checkStateFormat(golden string, cur []string, format int) error {
	lines := strings.Split(strings.TrimRight(golden, "\n"), "\n")
	if len(lines) == 0 || lines[0] != stateFormatHeader(format) {
		return fmt.Errorf("清单第一行是 %q，buildinfo.StateFormat 是 %d：改了 StateFormat 要用 -update-state-format 重写清单", lines[0], format)
	}
	if !slices.Equal(lines[1:], cur) {
		var diff []string
		for _, l := range cur {
			if !slices.Contains(lines[1:], l) {
				diff = append(diff, "+ "+l)
			}
		}
		for _, l := range lines[1:] {
			if !slices.Contains(cur, l) {
				diff = append(diff, "- "+l)
			}
		}
		return fmt.Errorf("状态目录的落盘结构变了（字段增删、改名或改类型）：请同时把 buildinfo.StateFormat 加一（原地升级会拒绝、"+
			"改用卸载再重装），再用 go test ./internal/daemon -run TestStateFormatPinned -update-state-format 重写清单。差异：\n%s", strings.Join(diff, "\n"))
	}
	return nil
}

// updateStateFormatGolden 生成新的清单；结构变了而 StateFormat 没有加一时拒绝重写（不能只改清单不改版本号）。
func updateStateFormatGolden(golden string, cur []string, format int) (string, error) {
	lines := strings.Split(strings.TrimRight(golden, "\n"), "\n")
	if golden != "" && !slices.Equal(lines[1:], cur) && lines[0] == stateFormatHeader(format) {
		return "", fmt.Errorf("落盘结构变了但 buildinfo.StateFormat 还是 %d：先把它加一再重写清单", format)
	}
	return stateFormatHeader(format) + "\n" + strings.Join(cur, "\n") + "\n", nil
}

// v0.71（顾问 2026-11-27 必须改）：状态目录下每一种落盘结构的字段名和类型列成固定清单；任何变化都让测试失败，
// 提示同时改 buildinfo.StateFormat。
func TestStateFormatPinned(t *testing.T) {
	cur := stateFormatListing(persistedFiles())
	old, err := os.ReadFile(stateFormatGolden)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if *updateStateFormat {
		s, err := updateStateFormatGolden(string(old), cur, buildinfo.StateFormat)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(stateFormatGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(stateFormatGolden, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err != nil {
		t.Fatalf("没有 %s：用 -update-state-format 生成", stateFormatGolden)
	}
	if err := checkStateFormat(string(old), cur, buildinfo.StateFormat); err != nil {
		t.Fatal(err)
	}
	// 状态文件自己的版本号也钉住（它变了同样是格式变化）。
	if state.Version != 1 {
		t.Fatalf("state.Version 变成了 %d：请同时把 buildinfo.StateFormat 加一并更新本测试", state.Version)
	}
	// 每个落盘文件都列了（清单里没有重复的位置）。
	seen := map[string]bool{}
	for _, f := range persistedFiles() {
		if seen[f.path] {
			t.Fatalf("%s 列了两次", f.path)
		}
		seen[f.path] = true
	}
}

// 反例：改一个字段名、改一个标签、改一个类型、加一个字段，清单都会变，比较报错；结构变了而 StateFormat 没加一时
// 不许重写清单；加一之后可以。
func TestStateFormatPinnedCounterexamples(t *testing.T) {
	type inner struct {
		N int `json:"n"`
	}
	type base struct {
		A string  `json:"a"`
		B []inner `json:"b,omitempty"`
	}
	type renamed struct {
		A2 string  `json:"a"`
		B  []inner `json:"b,omitempty"`
	}
	type retagged struct {
		A string  `json:"aa"`
		B []inner `json:"b,omitempty"`
	}
	type retyped struct {
		A int     `json:"a"`
		B []inner `json:"b,omitempty"`
	}
	type innerRenamed struct {
		M int `json:"m"`
	}
	type nested struct {
		A string         `json:"a"`
		B []innerRenamed `json:"b,omitempty"`
	}
	type added struct {
		A string  `json:"a"`
		B []inner `json:"b,omitempty"`
		C bool    `json:"c"`
	}
	list := func(v any) []string { return stateFormatListing([]persistedEntry{{"abroadd/x.json", "json", v, ""}}) }
	golden, err := updateStateFormatGolden("", list(base{}), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkStateFormat(golden, list(base{}), 1); err != nil {
		t.Fatalf("没变时不应报错：%v", err)
	}
	for name, v := range map[string]any{"改字段名": renamed{}, "改标签": retagged{}, "改类型": retyped{}, "改内层字段": nested{}, "加字段": added{}} {
		err := checkStateFormat(golden, list(v), 1)
		if err == nil || !strings.Contains(err.Error(), "StateFormat 加一") {
			t.Errorf("%s：应报错并提示改 StateFormat：%v", name, err)
		}
		if _, err := updateStateFormatGolden(golden, list(v), 1); err == nil {
			t.Errorf("%s：StateFormat 没加一时不应允许重写清单", name)
		}
		g2, err := updateStateFormatGolden(golden, list(v), 2)
		if err != nil {
			t.Errorf("%s：加一之后应允许重写：%v", name, err)
		}
		if err := checkStateFormat(g2, list(v), 2); err != nil {
			t.Errorf("%s：重写后应通过：%v", name, err)
		}
	}
	if err := checkStateFormat(golden, list(base{}), 2); err == nil {
		t.Error("StateFormat 改了而清单没重写时应报错")
	}
}
