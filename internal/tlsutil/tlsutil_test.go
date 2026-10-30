package tlsutil

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
	"time"
)

func TestEnsureSelfSignedLifecycle(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	dns := []string{"localhost", "nas"}
	ips := []net.IP{net.ParseIP("127.0.0.1").To4(), net.ParseIP("::1"), net.ParseIP("192.168.1.10").To4()}

	c1, gen, err := EnsureSelfSigned(dir, dns, ips, now)
	if err != nil || !gen {
		t.Fatalf("first: gen=%v err=%v", gen, err)
	}
	leaf := c1.Leaf
	if leaf.IsCA || leaf.NotAfter.Sub(now) != Validity || leaf.Subject.CommonName != "nas" {
		t.Fatalf("leaf: ca=%v notAfter=%v cn=%s", leaf.IsCA, leaf.NotAfter, leaf.Subject.CommonName)
	}
	if _, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok {
		t.Fatal("not ECDSA")
	}
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Fatal("EKU")
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(SelfSignedPaths(dir).KeyFile)
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("key mode %v", st.Mode().Perm())
		}
	}

	// Same inputs (or a subset) → reused, same fingerprint.
	c2, gen, _ := EnsureSelfSigned(dir, dns, ips[:2], now.Add(24*time.Hour))
	if gen || Fingerprint(c2.Certificate[0]) != Fingerprint(c1.Certificate[0]) {
		t.Fatal("cert regenerated without reason")
	}
	// New interface IP → regenerated.
	c3, gen, _ := EnsureSelfSigned(dir, dns, append(ips, net.ParseIP("10.0.0.5").To4()), now)
	if !gen || Fingerprint(c3.Certificate[0]) == Fingerprint(c1.Certificate[0]) {
		t.Fatal("SAN change did not regenerate")
	}
	// Close to expiry → regenerated.
	_, gen, _ = EnsureSelfSigned(dir, dns, ips, now.Add(Validity-10*24*time.Hour))
	if !gen {
		t.Fatal("expiring cert not regenerated")
	}
	// Corrupt files → regenerated.
	os.WriteFile(SelfSignedPaths(dir).CertFile, []byte("garbage"), 0o600)
	if _, gen, err = EnsureSelfSigned(dir, dns, ips, now); !gen || err != nil {
		t.Fatalf("corrupt: gen=%v err=%v", gen, err)
	}
}

func TestFingerprintFormat(t *testing.T) {
	fp := Fingerprint([]byte("x"))
	if !regexp.MustCompile(`^([0-9A-F]{2}:){31}[0-9A-F]{2}$`).MatchString(fp) {
		t.Fatal(fp)
	}
}

func TestFileReloader(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	cert1, key1, _ := GenerateSelfSigned([]string{"a.example"}, nil, now)
	cf, kf := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	os.WriteFile(cf, cert1, 0o600)
	os.WriteFile(kf, key1, 0o600)
	var reloadErr error
	r, err := NewFileReloader(cf, kf, func(e error) { reloadErr = e })
	if err != nil {
		t.Fatal(err)
	}
	clock := now
	r.now = func() time.Time { return clock }
	fp1 := Fingerprint(r.Current().Certificate[0])

	cert2, key2, _ := GenerateSelfSigned([]string{"b.example"}, nil, now)
	os.WriteFile(cf, cert2, 0o600)
	os.WriteFile(kf, key2, 0o600)
	later := time.Now().Add(2 * time.Second)
	os.Chtimes(cf, later, later)
	os.Chtimes(kf, later, later)
	if Fingerprint(r.Current().Certificate[0]) != fp1 {
		t.Fatal("reloaded before CheckInterval")
	}
	clock = clock.Add(2 * time.Minute)
	c, _ := r.GetCertificate(nil)
	if Fingerprint(c.Certificate[0]) == fp1 || c.Leaf.DNSNames[0] != "b.example" {
		t.Fatal("not reloaded")
	}
	// A broken replacement keeps the previous cert.
	os.WriteFile(kf, key1, 0o600) // mismatched pair
	later = later.Add(5 * time.Second)
	os.Chtimes(kf, later, later)
	clock = clock.Add(2 * time.Minute)
	if c2 := r.Current(); Fingerprint(c2.Certificate[0]) != Fingerprint(c.Certificate[0]) || reloadErr == nil {
		t.Fatal("bad reload replaced cert or error not reported")
	}
	if _, err := NewFileReloader(filepath.Join(dir, "missing"), kf, nil); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestHandshakeWithPinnedCert(t *testing.T) {
	cert, _, err := EnsureSelfSigned(t.TempDir(), []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1").To4()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// A plain TLS listener: httptest.StartTLS would inject its own certificate.
	ln, err := tls.Listen("tcp", "127.0.0.1:0", ServerConfig(func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return cert, nil }))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })}
	go srv.Serve(ln)
	defer srv.Close()
	url := "https://" + ln.Addr().String() + "/"

	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	for _, v := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MaxVersion: v}}}
		resp, err := client.Get(url)
		if err != nil {
			t.Fatalf("TLS %x handshake: %v", v, err)
		}
		resp.Body.Close()
	}
	old := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MaxVersion: tls.VersionTLS11}}}
	if _, err := old.Get(url); err == nil {
		t.Fatal("TLS 1.1 accepted")
	}
}

func TestRedirectHandler(t *testing.T) {
	h := RedirectHandler("8443")
	cases := map[string]string{
		"nas.local:8080": "https://nas.local:8443/x?y=1",
		"192.168.1.2":    "https://192.168.1.2:8443/x?y=1",
		"[fd00::1]:8080": "https://[fd00::1]:8443/x?y=1",
	}
	for host, want := range cases {
		r := httptest.NewRequest("GET", "http://example/x?y=1", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 308 || w.Header().Get("Location") != want {
			t.Errorf("%s: %d %s", host, w.Code, w.Header().Get("Location"))
		}
	}
	r := httptest.NewRequest("GET", "http://example/", nil)
	r.Host = "evil.com/@x"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("bad host: %d", w.Code)
	}
	r = httptest.NewRequest("GET", "http://example/", nil)
	r.Host = "nas"
	w = httptest.NewRecorder()
	RedirectHandler("443").ServeHTTP(w, r)
	if w.Header().Get("Location") != "https://nas/" {
		t.Fatal(w.Header().Get("Location"))
	}
}

func TestDefaultHosts(t *testing.T) {
	dns, ips := DefaultHosts()
	if dns[0] != "localhost" || len(ips) < 2 || !ips[0].Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("%v %v", dns, ips)
	}
	// Rotating IPv6 privacy addresses would churn the cert: only ::1 is included.
	for _, ip := range ips[2:] {
		if ip.To4() == nil {
			t.Fatalf("IPv6 address %v in the default SANs", ip)
		}
	}
	for _, n := range []string{"docker0", "br-3f2a", "veth12ab", "virbr0", "cni0", "lxcbr0"} {
		if !virtualIface(n) {
			t.Fatalf("%s not treated as virtual", n)
		}
	}
	for _, n := range []string{"eth0", "enp3s0", "wlan0", "wg0", "tailscale0"} {
		if virtualIface(n) {
			t.Fatalf("%s treated as virtual", n)
		}
	}
}

func TestNextSelfSignedAnnouncedThenUsed(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	dns := []string{"localhost", "nas"}
	ips := []net.IP{net.ParseIP("127.0.0.1").To4(), net.ParseIP("192.168.1.10").To4()}
	p := SelfSignedPaths(dir)

	cur, _, err := EnsureSelfSigned(dir, dns, ips, now)
	if err != nil {
		t.Fatal(err)
	}
	// Long before expiry: nothing announced, stale leftovers removed.
	os.WriteFile(p.NextCertFile, []byte("old"), 0o600)
	if next, err := EnsureNextSelfSigned(dir, cur.Leaf, dns, ips, now.Add(24*time.Hour)); next != nil || err != nil {
		t.Fatalf("announced too early: %v %v", next, err)
	}
	if _, err := os.Stat(p.NextCertFile); !os.IsNotExist(err) {
		t.Fatal("stale next cert kept")
	}

	// 50 days before expiry: a successor is announced, and the same one on the next start.
	t1 := cur.Leaf.NotAfter.Add(-50 * 24 * time.Hour)
	if c, gen, _ := EnsureSelfSigned(dir, dns, ips, t1); gen || Fingerprint(c.Certificate[0]) != Fingerprint(cur.Certificate[0]) {
		t.Fatal("current cert replaced before the renewal window")
	}
	next, err := EnsureNextSelfSigned(dir, cur.Leaf, dns, ips, t1)
	if err != nil || next == nil {
		t.Fatalf("no successor: %v", err)
	}
	again, _ := EnsureNextSelfSigned(dir, cur.Leaf, dns, ips, t1.Add(24*time.Hour))
	if again == nil || Fingerprint(again.Raw) != Fingerprint(next.Raw) {
		t.Fatal("successor not reused")
	}

	// Inside the renewal window: the announced successor becomes the cert.
	renewed, gen, err := EnsureSelfSigned(dir, dns, ips, cur.Leaf.NotAfter.Add(-20*24*time.Hour))
	if err != nil || !gen || Fingerprint(renewed.Certificate[0]) != Fingerprint(next.Raw) {
		t.Fatalf("renewal did not switch to the announced cert: gen=%v err=%v", gen, err)
	}
	if _, err := os.Stat(p.NextCertFile); !os.IsNotExist(err) {
		t.Fatal("successor left behind after the switch")
	}
}
