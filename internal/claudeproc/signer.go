package claudeproc

import (
	"errors"
	"regexp"
	"strings"
)

var teamRe = regexp.MustCompile(`^TeamIdentifier=([A-Z0-9]{10})$`)

// ParseCodesign 解析 codesign -dv 的输出（它写在标准错误里）。
//   - 没有签名（"not signed at all"）或 "TeamIdentifier=not set"：返回空串、nil；
//   - 有 Team ID：返回它；
//   - 其他（不认识的输出）：返回错误，不当成匹配。
func ParseCodesign(out string) (string, error) {
	if strings.Contains(out, "code object is not signed at all") {
		return "", nil
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "TeamIdentifier=not set" {
			return "", nil
		}
		if m := teamRe.FindStringSubmatch(line); m != nil {
			return m[1], nil
		}
	}
	return "", errors.New("不认识 codesign 的输出")
}
