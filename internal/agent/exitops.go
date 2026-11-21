package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// execExitOps 是分流出口的真实系统操作（虚拟机里，agent 以 root 运行）。文件路径都加上 Root（测试用临时目录）。
type execExitOps struct {
	Root string
}

func (o execExitOps) path(p string) string { return filepath.Join(o.Root, p) }

func (o execExitOps) Run(ctx context.Context, argv ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var b bytes.Buffer
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = &b, &b
	cmd.Env = EngineEnv
	err := cmd.Run()
	out := b.String()
	if len(out) > 4096 {
		out = out[:4096]
	}
	return out, err
}

func (o execExitOps) MkdirPrivate(p string) error {
	if err := os.MkdirAll(o.path(p), 0o700); err != nil {
		return err
	}
	return os.Chmod(o.path(p), 0o700)
}

// WriteFile 写到临时文件再改名（/proc 下的直接写）。
func (o execExitOps) WriteFile(p string, data []byte, perm fs.FileMode) error {
	if strings.HasPrefix(p, "/proc/") {
		return os.WriteFile(o.path(p), data, perm)
	}
	full := o.path(p)
	tmp, err := os.CreateTemp(filepath.Dir(full), ".exit-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), full)
}

func (o execExitOps) RemoveAll(p string) error { return os.RemoveAll(o.path(p)) }

// ResolveAll 经虚拟机的解析器（网关）查 IPv4：网关只代解析登记过的服务器域名，答的同时记进宿主的放行名单。
func (o execExitOps) ResolveAll(ctx context.Context, host string) ([]netip.Addr, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	as, err := net.DefaultResolver.LookupNetIP(cctx, "ip4", host)
	if err != nil {
		return nil, err
	}
	if len(as) == 0 {
		return nil, errors.New("没有 IPv4 地址")
	}
	out := make([]netip.Addr, len(as))
	for i, a := range as {
		out[i] = a.Unmap()
	}
	return out, nil
}

func (o execExitOps) LinkUp(ns, dev string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := o.Run(ctx, "/sbin/ip", "-n", ns, "-o", "link", "show", "dev", dev)
	return err == nil && linkFlagsUp(out)
}

// linkFlagsUp 看 `ip -o link show` 输出里尖括号中的标志有没有 UP（隧道设备的 operstate 常是 UNKNOWN，不能看 state）。
func linkFlagsUp(out string) bool {
	i, j := strings.IndexByte(out, '<'), strings.IndexByte(out, '>')
	if i < 0 || j < i {
		return false
	}
	for _, f := range strings.Split(out[i+1:j], ",") {
		if f == "UP" {
			return true
		}
	}
	return false
}

func (o execExitOps) WGHandshake(ctx context.Context, ns, dev string) (time.Time, error) {
	out, err := o.Run(ctx, "/sbin/ip", "netns", "exec", ns, "/usr/bin/wg", "show", dev, "latest-handshakes")
	if err != nil {
		return time.Time{}, errors.New("wg show 失败")
	}
	return parseLatestHandshakes(out), nil
}

// parseLatestHandshakes 解析 `wg show <dev> latest-handshakes`（每行“公钥<TAB>秒”），取最近的一次；0 表示没有握手。
func parseLatestHandshakes(out string) time.Time {
	var best int64
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			continue
		}
		if v, err := strconv.ParseInt(f[1], 10, 64); err == nil && v > best {
			best = v
		}
	}
	if best == 0 {
		return time.Time{}
	}
	return time.Unix(best, 0)
}
