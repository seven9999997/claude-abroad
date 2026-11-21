package daemon

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"claude-abroad/internal/envcheck"
	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/exitpolicy"
	"claude-abroad/internal/wire"
)

// envWorld 是环境检测 6b 在 daemon 测试里用的“外网”：全部是本地的假服务（不对外发任何请求）。
//   - 一个 TLS 服务器（测试 CA）应答取 IP 的源、位置源、HTTPS 探测、Claude 的两个域名；
//   - 两个假 STUN：一个回报出口的 UDP 地址，一个给“本机真实 IP”（经直连）。
type envWorld struct {
	ca   *x509.CertPool
	tls  string
	stun string
	real string

	realDials atomic.Int32

	mu      sync.Mutex
	exitIP  string
	country map[string]string // IP → 国家
}

var envHosts = []string{"ip.test", "geo.test", "www.gstatic.com", "cloudflare-dns.com", "claude.ai", "api.anthropic.com"}

func newEnvWorld(t *testing.T) *envWorld {
	t.Helper()
	w := &envWorld{exitIP: "203.0.113.7", country: map[string]string{"203.0.113.7": "US", "203.0.113.9": "US"}}
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "env-ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	w.ca = x509.NewCertPool()
	w.ca.AddCert(caCert)
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: envHosts, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, caCert, &k.PublicKey, caKey)
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: k}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ErrorLog: log.New(io.Discard, "", 0), Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		ip := w.exitIP
		w.mu.Unlock()
		switch {
		case r.Host == "ip.test":
			io.WriteString(rw, ip+" "+w.countryOf(ip))
		case r.Host == "geo.test":
			q := strings.TrimPrefix(r.URL.Path, "/")
			io.WriteString(rw, q+"|"+w.countryOf(q)+"|America/Los_Angeles|Example ISP")
		case r.Host == "www.gstatic.com":
			rw.WriteHeader(http.StatusNoContent)
		default:
			rw.WriteHeader(http.StatusForbidden) // Claude：任意状态码都算到达
		}
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	w.tls = l.Addr().String()
	w.stun = envStun(t, "203.0.113.9:4500")
	w.real = envStun(t, "198.51.100.77:5000")
	return w
}

func (w *envWorld) countryOf(ip string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.country[ip]
}

// envStun 是本地的假 STUN 服务器，回报 addr。
func envStun(t *testing.T, addr string) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	ap := netip.MustParseAddrPort(addr)
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < 20 {
				continue
			}
			attr := make([]byte, 12)
			binary.BigEndian.PutUint16(attr[0:], 0x0020)
			binary.BigEndian.PutUint16(attr[2:], 8)
			attr[5] = 1
			binary.BigEndian.PutUint16(attr[6:], ap.Port()^0x2112)
			a := ap.Addr().As4()
			binary.BigEndian.PutUint32(attr[8:], binary.BigEndian.Uint32(a[:])^0x2112A442)
			resp := make([]byte, 20)
			binary.BigEndian.PutUint16(resp[0:], 0x0101)
			binary.BigEndian.PutUint16(resp[2:], 12)
			copy(resp[4:20], buf[4:20])
			_, _ = pc.WriteTo(append(resp, attr...), from)
		}
	}()
	return pc.LocalAddr().String()
}

// dial 把所有地址映射到本地假服务：TCP → TLS 服务器，UDP → 假 STUN。测试里没有对真实地址的拨号。
func (w *envWorld) dial(ctx context.Context, network, _ string) (net.Conn, error) {
	var d net.Dialer
	if strings.HasPrefix(network, "udp") {
		return d.DialContext(ctx, "udp", w.stun)
	}
	return d.DialContext(ctx, "tcp", w.tls)
}

type testIPSource struct{}

func (testIPSource) Name() string { return "test-ip" }
func (testIPSource) URL() string  { return "https://ip.test/" }
func (testIPSource) Parse(b []byte) (exitpolicy.IPInfo, error) {
	ip, c, ok := strings.Cut(strings.TrimSpace(string(b)), " ")
	a, err := netip.ParseAddr(ip)
	if !ok || err != nil {
		return exitpolicy.IPInfo{}, errors.New("格式不对")
	}
	return exitpolicy.IPInfo{IP: a, Country: c}, nil
}

type testGeoSource struct{}

func (testGeoSource) Name() string             { return "test-geo" }
func (testGeoSource) URL(ip netip.Addr) string { return "https://geo.test/" + ip.String() }
func (testGeoSource) Parse(b []byte, ip netip.Addr) (exitpolicy.Geo, error) {
	f := strings.Split(strings.TrimSpace(string(b)), "|")
	if len(f) != 4 || f[1] == "" {
		return exitpolicy.Geo{}, errors.New("格式不对")
	}
	return exitpolicy.Geo{IP: ip, Country: f[1], Timezone: f[2], Org: f[3]}, nil
}

// deps6b 把 6b 的依赖全部指向本地假服务。
func (w *envWorld) deps6b(e *EnvDeps) {
	e.Probe = w.dial
	e.RootCAs = w.ca
	e.IPSources = []exitcheck.IPSource{testIPSource{}}
	e.GeoSources = []exitcheck.GeoSource{testGeoSource{}}
	e.Echo = func(context.Context) (netip.Addr, error) { return netip.MustParseAddr("203.0.113.53"), nil }
	e.StunServers = []string{"stun-a.test:3478"}
	e.RealDial = func(ctx context.Context) (net.Conn, error) {
		w.realDials.Add(1)
		var d net.Dialer
		return d.DialContext(ctx, "udp", w.real)
	}
	e.ProcEnvs = func(context.Context) ([]envcheck.ProcEnv, error) { return nil, nil }
	e.Home = func() (string, error) { return "/nonexistent-home-for-test", nil }
	e.ReachOpen = func(ctx context.Context, _ wire.Header) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", w.tls)
	}
	e.StunTimeout, e.ReachTimeout = 2*time.Second, 3*time.Second
}
