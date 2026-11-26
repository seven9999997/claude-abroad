package envcheck

import (
	"io/fs"
	"path"
	"sort"
	"strings"
)

// 设备体检（设计 8.1 末尾，M6-6a）：按需手动运行，不参与放行、不关闸门。扫描系统和当前用户的字体目录，列出随中文
// 办公软件安装的字体（宋体、黑体、楷体、仿宋、微软雅黑、等线、方正等），由用户自己在“字体册”里停用。只读。

// FontDirs 是要扫描的字体目录（相对各自的根：系统的根是 /，用户的根是主目录）。
var FontDirs = []string{"Library/Fonts"}

// chineseOfficeFonts 是文件名特征（小写比较，去掉扩展名之后按前缀或整名）：Windows 和 Office 带的中文字体。
var chineseOfficeFonts = []struct{ prefix, name string }{
	{"simsun", "宋体（SimSun）"},
	{"nsimsun", "新宋体（NSimSun）"},
	{"simhei", "黑体（SimHei）"},
	{"simkai", "楷体（KaiTi）"},
	{"kaiti", "楷体（KaiTi）"},
	{"simfang", "仿宋（FangSong）"},
	{"fangsong", "仿宋（FangSong）"},
	{"msyh", "微软雅黑（Microsoft YaHei）"},
	{"microsoft yahei", "微软雅黑（Microsoft YaHei）"},
	{"deng", "等线（DengXian）"},
	{"fz", "方正字体"},
	{"stxihei", "华文细黑"},
	{"stsong", "华文宋体"},
	{"stkaiti", "华文楷体"},
	{"stfangsong", "华文仿宋"},
	{"stzhongsong", "华文中宋"},
}

var fontExt = map[string]bool{".ttf": true, ".ttc": true, ".otf": true, ".otc": true, ".dfont": true}

// DeviceFinding 是一个可疑的字体文件（不含完整路径：只有位置和文件名）。
type DeviceFinding struct {
	Font     string `json:"font"`     // 字体的名字
	File     string `json:"file"`     // 文件名
	Location string `json:"location"` // 系统字体、用户字体
}

// DeviceReport 是设备体检的结果。
type DeviceReport struct {
	Findings []DeviceFinding `json:"findings"`
	Note     string          `json:"note"`
}

// DeviceNote 是体检结果的说明。
const DeviceNote = "随中文办公软件安装的字体可能让网页能看出这台电脑装过中文办公软件。体检按字体文件名判断，可能不准；只提示、不影响放行；不再需要时可以在“字体册”里停用。"

func matchFont(file string) (string, bool) {
	ext := strings.ToLower(path.Ext(file))
	if !fontExt[ext] {
		return "", false
	}
	base := strings.ToLower(strings.TrimSuffix(file, path.Ext(file)))
	for _, f := range chineseOfficeFonts {
		if strings.HasPrefix(base, f.prefix) {
			// “fz”“deng”这种短前缀：后面紧跟字母数字下划线以外的才算词首（fzlt…、dengb 都算；dengue 不是字体名，不会遇到）
			return f.name, true
		}
	}
	return "", false
}

// RunDevice 扫描字体目录。system、home 为 nil 时跳过那一处；目录不存在、读不了都跳过（只读，不报错）。
func RunDevice(system, home fs.FS) DeviceReport {
	rep := DeviceReport{Findings: []DeviceFinding{}, Note: DeviceNote}
	scan := func(fsys fs.FS, where string) {
		if fsys == nil {
			return
		}
		for _, dir := range FontDirs {
			_ = fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					if p == dir {
						return fs.SkipDir
					}
					return nil
				}
				if d.IsDir() {
					if strings.Count(p, "/")-strings.Count(dir, "/") > 2 { // 只看两层
						return fs.SkipDir
					}
					return nil
				}
				if name, ok := matchFont(d.Name()); ok {
					rep.Findings = append(rep.Findings, DeviceFinding{Font: name, File: d.Name(), Location: where})
				}
				return nil
			})
		}
	}
	scan(system, "系统字体")
	scan(home, "用户字体")
	sort.Slice(rep.Findings, func(i, j int) bool {
		a, b := rep.Findings[i], rep.Findings[j]
		if a.Location != b.Location {
			return a.Location < b.Location
		}
		return a.File < b.File
	})
	return rep
}
