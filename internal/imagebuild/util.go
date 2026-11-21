package imagebuild

import (
	"bytes"
	"net/url"
	"path"
)

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// urlBase 取 URL 路径的最后一段作为文件名。
func urlBase(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return path.Base(raw)
	}
	return path.Base(u.Path)
}
