package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-abroad/internal/exitconf"
)

func TestLinkFlagsUp(t *testing.T) {
	for out, want := range map[string]bool{
		"5: wg1: <POINTOPOINT,NOARP,UP,LOWER_UP> mtu 1420 qdisc noqueue state UNKNOWN": true,
		"5: wg1: <POINTOPOINT,NOARP> mtu 1420 state DOWN":                              false,
		"5: tun1: <NO-CARRIER,POINTOPOINT,MULTICAST,NOARP,UP> mtu 1500":                true,
		"Device \"wg1\" does not exist.":                                               false,
		"":                                                                             false,
	} {
		if linkFlagsUp(out) != want {
			t.Errorf("%q：应为 %v", out, want)
		}
	}
}

func TestParseLatestHandshakes(t *testing.T) {
	if !parseLatestHandshakes("abc=\t0\n").IsZero() || !parseLatestHandshakes("").IsZero() {
		t.Fatal("没有握手应为零值")
	}
	if got := parseLatestHandshakes("a=\t1700000000\nb=\t1700000100\nbad line here\n"); !got.Equal(time.Unix(1700000100, 0)) {
		t.Fatal(got)
	}
}

// 真实 ops 的文件部分：目录 0700、文件按给定权限、路径都在 Root 下、RemoveAll 删干净。
func TestExecExitOpsFiles(t *testing.T) {
	root := t.TempDir()
	o := execExitOps{Root: root}
	if err := o.MkdirPrivate(ExitsDir + "/hk"); err != nil {
		t.Fatal(err)
	}
	if err := o.WriteFile(ExitsDir+"/hk/wg.conf", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(root, ExitsDir, "hk"))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("%v %v", fi, err)
	}
	fi, err = os.Stat(filepath.Join(root, ExitsDir, "hk", "wg.conf"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi, err)
	}
	if err := o.RemoveAll(ExitsDir + "/hk"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ExitsDir, "hk")); !os.IsNotExist(err) {
		t.Fatal("应删掉")
	}
}

// health 带出口状态；API 注册 /v1/exits；出口表要求 sing-box 在运行。
func TestExitsHealthAndAPI(t *testing.T) {
	h := newExitsHarness(t)
	if err := h.x.Apply(context.Background(), []ExitSpec{{Tag: "sub", Kind: exitconf.Clash, Conf: exClash}}); err != nil {
		t.Fatal(err)
	}
	waitExitUp(t, h, "sub")
	st := &State{now: time.Now}
	st.SetExits(h.x.Status)
	if hs := st.Snapshot(); len(hs.Exits) != 1 || !hs.Exits[0].Up {
		t.Fatalf("%+v", hs.Exits)
	}
	api := (&API{State: st, Exits: h.x}).Handler()
	w := httptest.NewRecorder()
	api.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/exits", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"sub"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	stopped := &Engine{} // 从没启动：状态不是 running
	if _, up := (exitsTable{base: staticTable{}, ex: h.x, singBox: stopped}).Lookup("sub"); up {
		t.Fatal("sing-box 没在运行时分流出口不可用")
	}
}

// v2.9：推送或配置里的 DNS 只收可用的 IPv4，最多 2 个；一个都没有时退到固定解析器。
func TestExitDNSValidation(t *testing.T) {
	for a, want := range map[string]bool{
		"10.78.0.1": true, "192.168.1.1": true, "8.8.8.8": true, "172.16.0.53": true,
		"127.0.0.1": false, "224.0.0.251": false, "0.0.0.0": false, "255.255.255.255": false, "169.254.1.1": false,
		"198.18.3.4": false, "198.19.0.1": false, "10.200.1.1": false, "fd00::1": false, "::ffff:127.0.0.1": false,
	} {
		if ValidExitDNS(netip.MustParseAddr(a)) != want {
			t.Errorf("%s 应为 %v", a, want)
		}
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "dns")
	_ = os.WriteFile(f, []byte("127.0.0.1\n10.78.0.1\nfd00::1\n10.78.0.1\n198.18.0.9\n10.78.0.2\n10.78.0.3\n"), 0o600)
	e := &ExitSocks{DNSFile: f}
	if got := e.dnsServers(); len(got) != 2 || got[0].String() != "10.78.0.1" || got[1].String() != "10.78.0.2" {
		t.Fatalf("%v", got)
	}
	_ = os.WriteFile(f, []byte("127.0.0.1\n198.18.0.9\n"), 0o600)
	if got := e.dnsServers(); len(got) != 1 || got[0] != DefaultExitDNS[0] {
		t.Fatalf("全不可用时应退到固定解析器：%v", got)
	}
}
