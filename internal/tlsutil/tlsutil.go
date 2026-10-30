// Package tlsutil provides the server's TLS material: a self-signed ECDSA P-256 certificate
// generated into the data dir, hot-reloading cert/key files, ACME via autocert, a hardened
// tls.Config, fingerprints and the optional HTTP->HTTPS redirect handler.
package tlsutil

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"ludworks/internal/config"
)

// Validity of generated self-signed certificates (the CA/Browser Forum historical maximum,
// accepted by Apple platforms for user-trusted certs).
const Validity = 825 * 24 * time.Hour

// renewBefore regenerates a self-signed cert this long before it expires.
const renewBefore = 30 * 24 * time.Hour

// announceBefore creates the successor of the self-signed cert this long before the current one
// expires. Its fingerprint and certificate are published (setup status), so clients that pinned
// the current cert can trust the next one before the switch instead of asking the user again.
const announceBefore = 60 * 24 * time.Hour

// virtualIfacePrefixes are container/VM bridges and tunnels whose addresses come and go with the
// containers and VMs; covering them would regenerate the cert (and break every pin) each time.
var virtualIfacePrefixes = []string{"docker", "br-", "veth", "virbr", "vnet", "cni", "flannel", "cali", "vxlan", "lxc", "lxd", "podman", "kube", "weave", "tun", "tap", "zt"}

// Fingerprint returns the SHA-256 of a DER certificate as uppercase colon-separated hex
// ("AB:CD:..."), the form browsers and openssl display. Clients should compare it
// case-insensitively ignoring colons.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	var b strings.Builder
	for i, c := range sum {
		if i > 0 {
			b.WriteByte(':')
		}
		fmt.Fprintf(&b, "%02X", c)
	}
	return b.String()
}

// DefaultHosts returns the SANs wanted for a self-signed cert: hostname, localhost, loopback
// addresses and the IPv4 addresses of the machine's real interfaces. Container/VM bridges and
// IPv6 addresses (privacy addresses rotate daily) are left out so the cert, and the fingerprint
// clients pinned, stays the same across restarts; a specific IPv6 listen address is still added
// by the caller.
func DefaultHosts() (dns []string, ips []net.IP) {
	dns = []string{"localhost"}
	if h, err := os.Hostname(); err == nil && h != "" && !strings.EqualFold(h, "localhost") {
		dns = append(dns, strings.ToLower(h))
	}
	ips = []net.IP{net.IPv4(127, 0, 0, 1).To4(), net.IPv6loopback}
	ifaces, err := net.Interfaces()
	if err != nil {
		return dns, ips
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || virtualIface(ifc.Name) {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if v4 := ipn.IP.To4(); v4 != nil && !v4.IsLoopback() && !v4.IsLinkLocalUnicast() {
				ips = append(ips, v4)
			}
		}
	}
	return dns, ips
}

func virtualIface(name string) bool {
	name = strings.ToLower(name)
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// SelfSigned describes the paths of the generated certificate and of its announced successor.
type SelfSigned struct {
	CertFile, KeyFile         string
	NextCertFile, NextKeyFile string
}

// SelfSignedPaths returns <dir>/self-signed.{crt,key} and <dir>/self-signed-next.{crt,key}.
func SelfSignedPaths(dir string) SelfSigned {
	return SelfSigned{
		CertFile: filepath.Join(dir, "self-signed.crt"), KeyFile: filepath.Join(dir, "self-signed.key"),
		NextCertFile: filepath.Join(dir, "self-signed-next.crt"), NextKeyFile: filepath.Join(dir, "self-signed-next.key"),
	}
}

// EnsureSelfSigned makes sure a valid self-signed certificate covering dns/ips exists in dir,
// generating a new one when it is missing, unreadable, expired (or expiring within 30 days),
// or lacks one of the wanted SANs. Extra SANs in the existing cert are tolerated so that a
// temporarily missing interface does not churn the certificate (and its pinned fingerprint).
// It returns the loaded certificate and whether it was (re)generated.
func EnsureSelfSigned(dir string, dns []string, ips []net.IP, now time.Time) (*tls.Certificate, bool, error) {
	p := SelfSignedPaths(dir)
	if c, err := tls.LoadX509KeyPair(p.CertFile, p.KeyFile); err == nil {
		leaf, err := x509.ParseCertificate(c.Certificate[0])
		if err == nil && now.Add(renewBefore).Before(leaf.NotAfter) && now.After(leaf.NotBefore) && coversAll(leaf, dns, ips) {
			c.Leaf = leaf
			return &c, false, nil
		}
	}
	// Renewal switches to the announced successor when there is a usable one (clients already
	// trust it); otherwise a new cert is generated.
	certPEM, keyPEM, ok := usableNext(p, dns, ips, now)
	if !ok {
		var err error
		if certPEM, keyPEM, err = GenerateSelfSigned(dns, ips, now); err != nil {
			return nil, false, err
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, err
	}
	// Key first: a crash between the writes leaves a mismatched pair, which simply triggers
	// regeneration on the next start.
	if err := config.WriteFileAtomic(p.KeyFile, keyPEM); err != nil {
		return nil, false, err
	}
	if err := config.WriteFileAtomic(p.CertFile, certPEM); err != nil {
		return nil, false, err
	}
	if ok {
		os.Remove(p.NextCertFile)
		os.Remove(p.NextKeyFile)
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, false, err
	}
	c.Leaf, _ = x509.ParseCertificate(c.Certificate[0])
	return &c, true, nil
}

// usableNext returns the announced successor when it is valid now and for longer than the renewal
// window and covers the wanted SANs.
func usableNext(p SelfSigned, dns []string, ips []net.IP, now time.Time) (certPEM, keyPEM []byte, ok bool) {
	certPEM, err1 := os.ReadFile(p.NextCertFile)
	keyPEM, err2 := os.ReadFile(p.NextKeyFile)
	if err1 != nil || err2 != nil {
		return nil, nil, false
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, nil, false
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil || !now.After(leaf.NotBefore) || !now.Add(renewBefore).Before(leaf.NotAfter) || !coversAll(leaf, dns, ips) {
		return nil, nil, false
	}
	return certPEM, keyPEM, true
}

// EnsureNextSelfSigned keeps the successor of the current self-signed cert: once the current one
// expires within announceBefore, a next cert is generated (or the existing one reused) so it can be
// announced; before that any leftover successor is removed. It returns the successor, or nil.
func EnsureNextSelfSigned(dir string, current *x509.Certificate, dns []string, ips []net.IP, now time.Time) (*x509.Certificate, error) {
	p := SelfSignedPaths(dir)
	if current == nil || now.Add(announceBefore).Before(current.NotAfter) {
		os.Remove(p.NextCertFile)
		os.Remove(p.NextKeyFile)
		return nil, nil
	}
	if certPEM, _, ok := usableNext(p, dns, ips, now); ok && now.Add(announceBefore).Before(notAfterOf(certPEM)) {
		return parsePEMCert(certPEM)
	}
	certPEM, keyPEM, err := GenerateSelfSigned(dns, ips, now)
	if err != nil {
		return nil, err
	}
	if err := config.WriteFileAtomic(p.NextKeyFile, keyPEM); err != nil {
		return nil, err
	}
	if err := config.WriteFileAtomic(p.NextCertFile, certPEM); err != nil {
		return nil, err
	}
	return parsePEMCert(certPEM)
}

func parsePEMCert(certPEM []byte) (*x509.Certificate, error) {
	b, _ := pem.Decode(certPEM)
	if b == nil {
		return nil, errors.New("tls: no PEM certificate")
	}
	return x509.ParseCertificate(b.Bytes)
}

func notAfterOf(certPEM []byte) time.Time {
	c, err := parsePEMCert(certPEM)
	if err != nil {
		return time.Time{}
	}
	return c.NotAfter
}

// CertificatePEM encodes a DER certificate as PEM.
func CertificatePEM(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func coversAll(leaf *x509.Certificate, dns []string, ips []net.IP) bool {
	for _, d := range dns {
		if !slices.ContainsFunc(leaf.DNSNames, func(s string) bool { return strings.EqualFold(s, d) }) {
			return false
		}
	}
	for _, ip := range ips {
		if !slices.ContainsFunc(leaf.IPAddresses, func(x net.IP) bool { return x.Equal(ip) }) {
			return false
		}
	}
	return true
}

// GenerateSelfSigned creates a new ECDSA P-256 self-signed server certificate. It is a leaf
// (not a CA): trusting it cannot be abused to mint certificates for other sites.
func GenerateSelfSigned(dns []string, ips []net.IP, now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	cn := "Ludworks"
	for _, d := range dns {
		if d != "localhost" {
			cn = d
			break
		}
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"Ludworks self-signed"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(Validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		DNSNames:              dns,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), nil
}

// FileReloader serves a cert/key pair from disk and picks up replacements (e.g. certbot
// renewals) without a restart. Files are re-checked at most every CheckInterval; a failed
// reload keeps serving the previous certificate.
type FileReloader struct {
	CertFile, KeyFile string
	CheckInterval     time.Duration

	mu        sync.Mutex
	cert      *tls.Certificate
	certMod   time.Time
	keyMod    time.Time
	lastCheck time.Time
	now       func() time.Time
	onError   func(error)
}

// NewFileReloader loads the pair once (failing fast on a bad configuration).
func NewFileReloader(certFile, keyFile string, onError func(error)) (*FileReloader, error) {
	r := &FileReloader{CertFile: certFile, KeyFile: keyFile, CheckInterval: time.Minute, now: time.Now, onError: onError}
	if err := r.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *FileReloader) reload() error {
	cs, err := os.Stat(r.CertFile)
	if err != nil {
		return err
	}
	ks, err := os.Stat(r.KeyFile)
	if err != nil {
		return err
	}
	c, err := tls.LoadX509KeyPair(r.CertFile, r.KeyFile)
	if err != nil {
		return fmt.Errorf("load %s / %s: %w", r.CertFile, r.KeyFile, err)
	}
	c.Leaf, err = x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return err
	}
	r.cert, r.certMod, r.keyMod = &c, cs.ModTime(), ks.ModTime()
	return nil
}

// Current returns the certificate, reloading it if the files changed.
func (r *FileReloader) Current() *tls.Certificate {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if now.Sub(r.lastCheck) >= r.CheckInterval {
		r.lastCheck = now
		cs, err1 := os.Stat(r.CertFile)
		ks, err2 := os.Stat(r.KeyFile)
		if err1 == nil && err2 == nil && (!cs.ModTime().Equal(r.certMod) || !ks.ModTime().Equal(r.keyMod)) {
			if err := r.reload(); err != nil && r.onError != nil {
				r.onError(err)
			}
		}
	}
	return r.cert
}

// GetCertificate implements tls.Config.GetCertificate.
func (r *FileReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.Current(), nil
}

// ServerConfig returns a hardened tls.Config: TLS 1.2+, AEAD-only ECDHE suites for 1.2,
// hybrid post-quantum X25519MLKEM768 then X25519 then P-256 key exchange, HTTP/2.
func ServerConfig(getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error)) *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: getCert,
		CipherSuites: []uint16{ // TLS 1.2 only; 1.3 suites are not configurable (all strong)
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		},
		CurvePreferences: []tls.CurveID{tls.X25519MLKEM768, tls.X25519, tls.CurveP256},
		NextProtos:       []string{"h2", "http/1.1"},
	}
}

// ACME returns an autocert manager restricted to domains, caching in cacheDir. The manager's
// HTTPHandler must be served on port 80 for http-01; tls-alpn-01 works on the TLS listener
// (ServerConfig + acme.ALPNProto in NextProtos, see ACMEConfig).
func ACME(domains []string, email, cacheDir string) *autocert.Manager {
	return &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache(cacheDir),
		HostPolicy: autocert.HostWhitelist(domains...),
		Email:      email,
	}
}

// ACMEConfig is ServerConfig wired to an autocert manager (with the ALPN challenge proto).
func ACMEConfig(m *autocert.Manager) *tls.Config {
	c := ServerConfig(m.GetCertificate)
	c.NextProtos = append(c.NextProtos, acme.ALPNProto)
	return c
}

// RedirectHandler answers plain-HTTP requests with a 308 to the HTTPS URL on httpsPort
// ("" or "443" omits the port). Hosts with invalid characters are rejected.
func RedirectHandler(httpsPort string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		if host == "" || strings.ContainsAny(host, "/\\@?#% \t\r\n") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if strings.Contains(host, ":") { // IPv6 literal
			host = "[" + host + "]"
		}
		if httpsPort != "" && httpsPort != "443" {
			host += ":" + httpsPort
		}
		w.Header().Set("Connection", "close")
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusPermanentRedirect)
	})
}

// CertInfo summarises a certificate for display.
func CertInfo(c *tls.Certificate) (fingerprint string, notAfter time.Time, dns []string, ips []string) {
	if c == nil || len(c.Certificate) == 0 {
		return "", time.Time{}, nil, nil
	}
	leaf := c.Leaf
	if leaf == nil {
		var err error
		if leaf, err = x509.ParseCertificate(c.Certificate[0]); err != nil {
			return Fingerprint(c.Certificate[0]), time.Time{}, nil, nil
		}
	}
	for _, ip := range leaf.IPAddresses {
		ips = append(ips, ip.String())
	}
	return Fingerprint(c.Certificate[0]), leaf.NotAfter, leaf.DNSNames, ips
}

// ACMECachedCert returns the certificate autocert has cached for domain, if any (used to show
// the fingerprint before the first handshake). The cache key is the domain name (ECDSA).
func ACMECachedCert(cacheDir, domain string) *tls.Certificate {
	b, err := os.ReadFile(filepath.Join(cacheDir, domain))
	if errors.Is(err, fs.ErrNotExist) || err != nil {
		return nil
	}
	// autocert stores the private key PEM followed by the certificate chain PEM.
	var certPEM, keyPEM bytes.Buffer
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		if strings.Contains(blk.Type, "PRIVATE KEY") {
			pem.Encode(&keyPEM, blk)
		} else {
			pem.Encode(&certPEM, blk)
		}
	}
	c, err := tls.X509KeyPair(certPEM.Bytes(), keyPEM.Bytes())
	if err != nil {
		return nil
	}
	return &c
}
