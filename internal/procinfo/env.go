package procinfo

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
)

// EnvInfo 是一个进程环境变量里环境检测关心的部分（m4-plan v2.7 决定 7）：
//   - ProxyVars：出现了哪些代理变量（只有变量名，原样大小写；值在解析时就丢掉，不离开 ParseProcEnv）；
//   - Lang、LcAll：语言变量的值（不敏感）。
type EnvInfo struct {
	ProxyVars []string
	Lang      string
	LcAll     string
}

// proxyVarNames 是要看的代理变量（含小写）。
var proxyVarNames = map[string]bool{
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true,
	"http_proxy": true, "https_proxy": true, "all_proxy": true,
}

// ParseProcEnv 解析 kern.procargs2 里参数之后的环境变量，只取白名单里的名字。
func ParseProcEnv(b []byte) (EnvInfo, error) {
	var out EnvInfo
	if len(b) < 4 {
		return out, errors.New("procargs2 太短")
	}
	argc := int(int32(binary.LittleEndian.Uint32(b)))
	if argc < 0 || argc > 4096 {
		return out, errors.New("procargs2 的参数个数不对")
	}
	rest := b[4:]
	i := bytes.IndexByte(rest, 0)
	if i <= 0 {
		return out, errors.New("procargs2 没有可执行文件路径")
	}
	rest = rest[i:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	next := func() (string, bool) {
		if len(rest) == 0 {
			return "", false
		}
		j := bytes.IndexByte(rest, 0)
		if j < 0 {
			j = len(rest)
		}
		s := string(rest[:j])
		if j < len(rest) {
			rest = rest[j+1:]
		} else {
			rest = nil
		}
		return s, true
	}
	for n := 0; n < argc; n++ {
		if _, ok := next(); !ok {
			return out, nil
		}
	}
	for {
		kv, ok := next()
		if !ok || kv == "" {
			return out, nil
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch {
		case proxyVarNames[k]:
			if strings.TrimSpace(v) != "" { // 只记“有没有”；值不保存
				out.ProxyVars = append(out.ProxyVars, k)
			}
		case k == "LANG":
			out.Lang = v
		case k == "LC_ALL":
			out.LcAll = v
		}
	}
}
