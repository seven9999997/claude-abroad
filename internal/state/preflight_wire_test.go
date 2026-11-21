package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claude-abroad/internal/preflight"
)

// sampleRunner 用 preflight 的 testdata 应答；路由按 via() 当前给出的接口生成。
type sampleRunner struct {
	t     *testing.T
	proxy string        // scutil --proxy 用哪个样本
	via   func() string // 1.1.1.1、129.0.0.1、198.18.0.2 经哪个接口（"" 表示经 en0）
}

func (r *sampleRunner) read(name string) []byte {
	b, err := os.ReadFile(filepath.Join("..", "preflight", "testdata", name))
	if err != nil {
		r.t.Fatal(err)
	}
	return b
}

func (r *sampleRunner) Run(_ context.Context, p string, args []string, stdin []byte) ([]byte, []byte, error) {
	switch key := filepath.Base(p) + " " + strings.Join(args, " "); key {
	case "scutil --proxy":
		return r.read(r.proxy), nil, nil
	case "scutil --nwi":
		return r.read("nwi-v4.txt"), nil, nil
	case "ps -axo comm":
		return r.read("ps-clean.constructed.txt"), nil, nil
	case "netstat -rn -f inet":
		out := string(r.read("netstat-clean.constructed.txt"))
		if v := r.via(); v != "" {
			out += "0/1                198.18.0.1         UGSc                 " + v + "\n128.0/1            198.18.0.1         UGSc                 " + v + "\n"
		}
		return []byte(out), nil, nil
	case "route -n get default":
		return r.read("route-default-en0.txt"), nil, nil
	case "route -n get 1.1.1.1", "route -n get 129.0.0.1", "route -n get 198.18.0.2":
		out := string(r.read("route-en0.constructed.txt"))
		if v := r.via(); v != "" {
			out = strings.Replace(out, "interface: en0", "interface: "+v, 1)
		}
		return []byte(out), nil, nil
	default:
		r.t.Fatalf("没有准备应答：%q", key)
		return nil, nil, nil
	}
}

// e. 真实检查器接进状态机：阻断时没有任何改动、状态文件未写入。
func TestPreflightBlocksEnable(t *testing.T) {
	b := newBed(t)
	chk := &preflight.Checker{R: &sampleRunner{t: t, proxy: "proxy-pac.constructed.txt", via: func() string { return "" }}, OurUtun: b.tun.Name}
	b.m.D.Preflight = chk
	err := b.m.Enable(bg)
	if !errors.Is(err, preflight.ErrBlocked) || !isUserError(err) {
		t.Fatalf("%v", err)
	}
	if tl := b.tl(); len(tl) != 0 {
		t.Fatalf("阻断后有改动：%v", tl)
	}
	if b.writes != 0 {
		t.Fatal("写过状态文件")
	}
	if _, err := os.Stat(filepath.Join(b.dir, FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("状态文件不应存在")
	}
	b.noViolations()
}

// e. 从 DISABLED 启用：自己的 utun 和路由不算冲突，启用成功。
func TestPreflightOwnTakeoverFromDisabled(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	mustOK(t, b.m.Disable(bg, false))
	chk := &preflight.Checker{R: &sampleRunner{t: t, proxy: "proxy-off.txt", via: b.tun.Name}, OurUtun: b.tun.Name}
	b.m.D.Preflight = chk
	mustOK(t, b.m.Enable(bg))
	b.assertEnabled()
	if f := chk.Last().Findings; len(f) != 0 {
		t.Fatalf("%+v", f)
	}
	// 同样的路由，不告诉检查器哪个是自己的：判冲突。
	other := &preflight.Checker{R: &sampleRunner{t: t, proxy: "proxy-off.txt", via: b.tun.Name}}
	if err := other.Check(bg); !errors.Is(err, preflight.ErrBlocked) {
		t.Fatalf("%v", err)
	}
	b.noViolations()
}
