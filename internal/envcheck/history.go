package envcheck

import (
	"claude-abroad/internal/platform"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// 出口变化记录（第 13 项，m4-plan v2.7 决定 8）：保留 48 小时；落盘的不是 IP，而是用本机随机密钥算出的摘要，只为数变化次数。
const (
	HistoryFileName = "ip-history.json"
	HistoryKeyName  = "ip-history.key"
	HistoryWindow   = 48 * time.Hour
	HistoryWarnAt   = 3 // 48 小时内变化这么多次给警告
)

// History 是出口变化记录。文件和密钥都在状态目录里，0600。
type History struct {
	Dir     string
	OnEvent func(kind, detail string)

	mu  sync.Mutex
	key []byte
}

type histEntry struct {
	At     time.Time `json:"at"`
	Digest string    `json:"digest"`
}

type histFile struct {
	V       int         `json:"v"`
	Entries []histEntry `json:"entries"`
}

func writePrivate(path string, data []byte) error {
	tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s.%d.tmp", filepath.Base(path), time.Now().UnixNano()))
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
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		os.Remove(tmp)
	}
	return werr
}

func readPrivate(path string, max int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > max {
		return nil, errors.New("文件不安全或太大")
	}
	return os.ReadFile(path)
}

func (h *History) event(kind, detail string) {
	if h.OnEvent != nil {
		h.OnEvent(kind, detail)
	}
}

func (h *History) loadKey() ([]byte, error) {
	if h.key != nil {
		return h.key, nil
	}
	p := filepath.Join(h.Dir, HistoryKeyName)
	if b, err := readPrivate(p, 1024); err == nil && len(b) == 64 {
		if k, err := hex.DecodeString(string(b)); err == nil {
			h.key = k
			return k, nil
		}
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if err := writePrivate(p, []byte(hex.EncodeToString(k))); err != nil {
		return nil, err
	}
	h.key = k
	return k, nil
}

func (h *History) load() histFile {
	b, err := readPrivate(filepath.Join(h.Dir, HistoryFileName), 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return histFile{V: 1}
	}
	var f histFile
	if err != nil || json.Unmarshal(b, &f) != nil || f.V != 1 {
		h.event("ip_history_corrupt", "出口变化记录读不出来，从空记录开始")
		return histFile{V: 1}
	}
	return f
}

// Record 记下这一轮的出口 IP（只存摘要），返回 48 小时内的变化次数。
func (h *History) Record(ip netip.Addr, now time.Time) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key, err := h.loadKey()
	if err != nil {
		return 0, err
	}
	m := hmac.New(sha256.New, key)
	m.Write(ip.Unmap().AsSlice())
	d := hex.EncodeToString(m.Sum(nil))[:32]
	f := h.load()
	kept := f.Entries[:0]
	for _, e := range f.Entries {
		if now.Sub(e.At) < HistoryWindow {
			kept = append(kept, e)
		}
	}
	f.Entries = kept
	if n := len(f.Entries); n == 0 || f.Entries[n-1].Digest != d {
		f.Entries = append(f.Entries, histEntry{At: now, Digest: d})
	}
	b, _ := json.Marshal(f)
	if err := writePrivate(filepath.Join(h.Dir, HistoryFileName), b); err != nil {
		return 0, err
	}
	return max(0, len(f.Entries)-1), nil
}
