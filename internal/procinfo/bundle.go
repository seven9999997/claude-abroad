package procinfo

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileBundles 从 <app>/Contents/Info.plist 读 Bundle ID，按 .app 路径缓存，Info.plist 的修改时间变了就重读。
// 只读文件，不写。
type FileBundles struct {
	mu    sync.Mutex
	cache map[string]bundleEntry
}

type bundleEntry struct {
	mtime time.Time
	size  int64
	id    string
	err   error
}

// Read 实现 BundleReader。
func (f *FileBundles) Read(app string) (string, error) {
	p := filepath.Join(app, "Contents", "Info.plist")
	st, err := os.Stat(p)
	if err != nil {
		return "", errors.New("读不到 Info.plist")
	}
	if !st.Mode().IsRegular() || st.Size() > MaxPlistSize {
		return "", errors.New("Info.plist 不是普通文件或超过大小上限")
	}
	f.mu.Lock()
	if e, ok := f.cache[app]; ok && e.mtime.Equal(st.ModTime()) && e.size == st.Size() {
		f.mu.Unlock()
		return e.id, e.err
	}
	f.mu.Unlock()
	id, err := readPlistFile(p)
	f.mu.Lock()
	if f.cache == nil {
		f.cache = map[string]bundleEntry{}
	}
	if len(f.cache) > 4096 {
		f.cache = map[string]bundleEntry{}
	}
	f.cache[app] = bundleEntry{mtime: st.ModTime(), size: st.Size(), id: id, err: err}
	f.mu.Unlock()
	return id, err
}

func readPlistFile(p string) (string, error) {
	fh, err := os.Open(p)
	if err != nil {
		return "", errors.New("读不到 Info.plist")
	}
	defer fh.Close()
	b, err := io.ReadAll(io.LimitReader(fh, MaxPlistSize+1))
	if err != nil {
		return "", errors.New("读不到 Info.plist")
	}
	return BundleIDFromPlist(b)
}
