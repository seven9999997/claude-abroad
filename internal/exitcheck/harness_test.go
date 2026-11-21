package exitcheck

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"claude-abroad/internal/exitpolicy"
	"claude-abroad/internal/wire"
)

// world 模拟“经出口看到的外网”：出口 IP、各查询源和探测地址的行为、UDP 53 的应答。
// 所有 HTTPS 都由进程内的 TLS 服务器应答（测试 CA 签发，覆盖用到的全部域名）。
type world struct {
	t *testing.T

	mu       sync.Mutex
	exitIP   string
	exitLoc  string            // Cloudflare trace 的 loc（空表示不给）
	geo      map[string]geoRec // IP → 位置
	mode     map[string]string // 域名 → ok、timeout、connect、channel、badcert、429、html、nofield、hang、200
	udpMode  string            // ok、silent、badid、badq、connect
	geoHits  int
	ipHits   map[string]int
	httpsHit int

	srv, bad *httptest.Server
	ca       *x509.CertPool
}

type geoRec struct{ country, region, city, tz string }

var testHosts = []string{"www.gstatic.com", "cloudflare-dns.com", "www.cloudflare.com", "ipwho.is", "api.ip.sb"}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, exitIP: "203.0.113.7", exitLoc: "US", mode: map[string]string{}, ipHits: map[string]int{},
		geo: map[string]geoRec{
			"203.0.113.7":  {"US", "Example State", "Exampleville", "America/Los_Angeles"},
			"203.0.113.8":  {"US", "Example State", "Exampleville", "America/Los_Angeles"}, // 同城换 IP
			"203.0.113.9":  {"US", "Other State", "Othertown", "America/New_York"},         // 同国换城市
			"198.51.100.5": {"JP", "Tokyo", "Tokyo", "Asia/Tokyo"},                         // 换国家
			"1.0.1.1":      {"CN", "Fujian", "Fuzhou", "Asia/Shanghai"},                    // 大陆段（内置列表的第一段）
		}}
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	w.ca = x509.NewCertPool()
	w.ca.AddCert(caCert)
	leaf := func(signer *x509.Certificate, signKey *ecdsa.PrivateKey) tls.Certificate {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "test"}, DNSNames: testHosts,
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, _ := x509.CreateCertificate(rand.Reader, tpl, signer, &k.PublicKey, signKey)
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
	}
	start := func(cert tls.Certificate) *httptest.Server {
		s := httptest.NewUnstartedServer(http.HandlerFunc(w.serve))
		s.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
		s.Config.ErrorLog = log.New(io.Discard, "", 0)
		s.StartTLS()
		t.Cleanup(s.Close)
		return s
	}
	w.srv = start(leaf(caCert, caKey))
	selfKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	selfTpl := &x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "evil"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	selfDER, _ := x509.CreateCertificate(rand.Reader, selfTpl, selfTpl, &selfKey.PublicKey, selfKey)
	selfCert, _ := x509.ParseCertificate(selfDER)
	w.bad = start(leaf(selfCert, selfKey)) // 证书链不到测试 CA：模拟被劫持
	return w
}

func (w *world) set(f func(w *world)) { w.mu.Lock(); f(w); w.mu.Unlock() }

func (w *world) hostMode(h string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.mode[h]
}

func (w *world) serve(rw http.ResponseWriter, r *http.Request) {
	host := r.Host
	m := w.hostMode(host)
	switch m {
	case "429":
		rw.WriteHeader(http.StatusTooManyRequests)
		return
	case "html":
		rw.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(rw, "<html><body>captive portal</body></html>")
		return
	case "hang":
		<-r.Context().Done()
		return
	}
	w.mu.Lock()
	ip, loc := w.exitIP, w.exitLoc
	w.mu.Unlock()
	switch {
	case host == "www.gstatic.com":
		w.mu.Lock()
		w.httpsHit++
		w.mu.Unlock()
		if m == "200" {
			_, _ = io.WriteString(rw, "<html>login</html>")
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	case host == "cloudflare-dns.com":
		b, _ := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
		q := new(dns.Msg)
		if q.Unpack(b) != nil {
			rw.WriteHeader(400)
			return
		}
		a := new(dns.Msg)
		a.SetReply(q)
		out, _ := a.Pack()
		rw.Header().Set("Content-Type", "application/dns-message")
		_, _ = rw.Write(out)
	case host == "www.cloudflare.com":
		w.hit("cf")
		if m == "nofield" {
			fmt.Fprintf(rw, "fl=1\nh=www.cloudflare.com\n")
			return
		}
		fmt.Fprintf(rw, "fl=1\nh=www.cloudflare.com\nip=%s\nts=1\n", ip)
		if loc != "" {
			fmt.Fprintf(rw, "loc=%s\n", loc)
		}
	case host == "ipwho.is" && r.URL.Path == "/":
		w.hit("ipwho-self")
		g := w.geoOf(ip)
		fmt.Fprintf(rw, `{"success":true,"ip":%q,"country_code":%q}`, ip, g.country)
	case host == "ipwho.is":
		w.countGeo()
		qip := strings.TrimPrefix(r.URL.Path, "/")
		g := w.geoOf(qip)
		if m == "nofield" {
			fmt.Fprintf(rw, `{"success":true,"ip":%q,"country_code":%q}`, qip, g.country)
			return
		}
		fmt.Fprintf(rw, `{"success":true,"ip":%q,"country":"X","country_code":%q,"region":%q,"city":%q,"timezone":{"id":%q},"connection":{"asn":64500,"org":"Example Hosting"}}`,
			qip, g.country, g.region, g.city, g.tz)
	case host == "api.ip.sb" && r.URL.Path == "/geoip":
		w.hit("ipsb-self")
		g := w.geoOf(ip)
		fmt.Fprintf(rw, `{"ip":%q,"country_code":%q}`, ip, g.country)
	case host == "api.ip.sb":
		w.countGeo()
		qip := strings.TrimPrefix(r.URL.Path, "/geoip/")
		g := w.geoOf(qip)
		fmt.Fprintf(rw, `{"ip":%q,"country_code":%q,"region":%q,"city":%q,"timezone":%q,"asn":64500,"asn_organization":"Example Hosting"}`,
			qip, g.country, g.region, g.city, g.tz)
	default:
		rw.WriteHeader(404)
	}
}

func (w *world) hit(k string) { w.mu.Lock(); w.ipHits[k]++; w.mu.Unlock() }
func (w *world) countGeo()    { w.mu.Lock(); w.geoHits++; w.mu.Unlock() }
func (w *world) geoOf(ip string) geoRec {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.geo[ip]
}
func (w *world) geoCount() int { w.mu.Lock(); defer w.mu.Unlock(); return w.geoHits }

// Open 是“按出口开流”的假实现。
func (w *world) Open(ctx context.Context, h wire.Header) (net.Conn, error) {
	if h.Net == wire.UDP {
		return w.openUDP(ctx)
	}
	host := h.Host
	if host == "" {
		host = h.IP
	}
	switch w.hostMode(host) {
	case "timeout":
		<-ctx.Done()
		return nil, ctx.Err()
	case "connect":
		return nil, &wire.ReplyError{Code: wire.CodeDialFailed}
	case "channel":
		return nil, errors.New("会话已断开")
	case "badcert":
		var d net.Dialer
		return d.DialContext(ctx, "tcp", w.bad.Listener.Addr().String())
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", w.srv.Listener.Addr().String())
}

func (w *world) openUDP(ctx context.Context) (net.Conn, error) {
	w.mu.Lock()
	m := w.udpMode
	w.mu.Unlock()
	if m == "connect" {
		return nil, &wire.ReplyError{Code: wire.CodeUDPUnsupported}
	}
	a, b := net.Pipe()
	go func() {
		defer b.Close()
		dc := wire.NewDatagramConn(b)
		buf := make([]byte, 4096)
		n, err := dc.ReadDatagram(buf)
		if err != nil {
			return
		}
		q := new(dns.Msg)
		if q.Unpack(buf[:n]) != nil {
			return
		}
		switch m {
		case "silent":
			<-ctx.Done() // 回应帧 ok 了，但没有任何应答
			return
		}
		r := new(dns.Msg)
		r.SetReply(q)
		if m == "badid" {
			r.Id++
		}
		if m == "badq" {
			r.Question[0].Name = "other.example."
		}
		out, _ := r.Pack()
		_ = dc.WriteDatagram(out)
		_, _ = io.Copy(io.Discard, b)
	}()
	return a, nil
}

// checker 返回接到 world 的 Checker（时限调小，测试快）。
func (w *world) checker() *Checker {
	// 时限放宽：全量 -race 并行跑时 TLS 握手可能超过 1 秒（测超时的用例自己设小时限）。
	return &Checker{Opener: w, RootCAs: w.ca, Total: 15 * time.Second, HTTPS: 5 * time.Second, IP: 3 * time.Second,
		Geo: 3 * time.Second, UDP: time.Second, DoH: 5 * time.Second}
}

func base(rule exitpolicy.Rule, ip string) *Baseline {
	g := map[string]geoRec{"203.0.113.7": {"US", "Example State", "Exampleville", "America/Los_Angeles"}}[ip]
	return &Baseline{V: 1, Rule: rule, IP: ip, Country: g.country, Region: g.region, City: g.city, Timezone: g.tz, At: time.Now()}
}

func mustAddr(s string) netip.Addr { return netip.MustParseAddr(s) }

var _ = os.Getpid

var ipRe = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
