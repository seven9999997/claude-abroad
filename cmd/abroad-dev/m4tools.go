package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"claude-abroad/internal/claudeproc"
	"claude-abroad/internal/procinfo"
)

// claude-facts 是 M4-9 真机脚本用的只读核对（m4-plan v2.14）：
//
//	abroad-dev claude-facts --facts              假定的 Team ID、桌面版 Bundle ID、Claude Code 的安装位置特征
//	abroad-dev claude-facts --path P [--path Q]  每个路径按扫描时同样的规则算不算 Claude（R17、假 Claude 进程）
//	abroad-dev claude-facts --count              现在能看到的 Claude 进程数（要结束的、没能确认的；不发任何信号）
//	abroad-dev claude-facts --team-of P          P 的代码签名 Team ID（codesign -dv，只读）
//
// 输出是一行 JSON；路径里的主目录换成 ~。不发信号、不改任何东西。

// ClaudeFacts 是 --facts 的输出。
type ClaudeFacts struct {
	TeamID         string   `json:"team_id"`
	DesktopBundle  string   `json:"desktop_bundle_id"`
	InstallMarkers []string `json:"install_markers"`
}

// PathFact 是 --path 的一项。
type PathFact struct {
	Path string `json:"path"`
	claudeproc.PathVerdict
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// homeTilde 把路径里的主目录换成 ~（输出给用户贴回的文件，不带用户名）。
func homeTilde(p, home string) string {
	if home != "" && home != "/" && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

// claudeFactsRun 是可测的主体：scanner 和 home 由调用方给。
func claudeFactsRun(args []string, sc *claudeproc.Scanner, home string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("claude-facts", flag.ContinueOnError)
	fs.SetOutput(stderr)
	facts := fs.Bool("facts", false, "假定的 Team ID、Bundle ID、安装位置特征")
	count := fs.Bool("count", false, "现在能看到的 Claude 进程数")
	teamOf := fs.String("team-of", "", "查这个路径的代码签名 Team ID")
	var paths multiFlag
	fs.Var(&paths, "path", "判断这个路径算不算 Claude（可重复）")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}
	n := 0
	for _, b := range []bool{*facts, *count, *teamOf != "", len(paths) > 0} {
		if b {
			n++
		}
	}
	if n != 1 {
		fmt.Fprintln(stderr, "用法：abroad-dev claude-facts --facts | --count | --team-of 路径 | --path 路径 [--path 路径 …]（只选一种）")
		return 2
	}
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	switch {
	case *facts:
		_ = enc.Encode(ClaudeFacts{TeamID: claudeproc.AnthropicTeamID, DesktopBundle: claudeproc.DesktopBundleID, InstallMarkers: claudeproc.InstallMarkers()})
	case *count:
		rep, err := sc.Scan()
		if err != nil {
			fmt.Fprintf(stderr, "扫描进程失败：%v\n", err)
			return 1
		}
		_ = enc.Encode(map[string]int{"targets": len(rep.Targets), "unverified": len(rep.Unverified)})
	case *teamOf != "":
		id, err := sc.Signer.TeamID(*teamOf)
		out := map[string]string{"path": homeTilde(*teamOf, home), "team_id": id}
		if err != nil {
			out["error"] = "查不了签名"
		}
		_ = enc.Encode(out)
	default:
		var out []PathFact
		for _, p := range paths {
			out = append(out, PathFact{Path: homeTilde(p, home), PathVerdict: sc.ClassifyPath(p)})
		}
		_ = enc.Encode(out)
	}
	return 0
}

func claudeFacts(args []string, stdout, stderr io.Writer) int {
	home, _ := os.UserHomeDir()
	if h := os.Getenv("M4_USER_HOME"); h != "" { // 以 root 运行时由脚本给出登录用户的主目录（只用于把路径换成 ~）
		home = h
	}
	sc := &claudeproc.Scanner{Procs: procinfo.SysProcs{}, Signer: claudeproc.CodesignSigner{}, Bundle: (&procinfo.FileBundles{}).Read, Self: os.Getpid()}
	return claudeFactsRun(args, sc, home, stdout, stderr)
}
