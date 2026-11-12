package node

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// The aggregator can also serve the join exchange and the WebSocket fallback
// over TLS on a port of its own, 443 normally, so that a hub on a network
// that lets only HTTPS out still gets a path without a Cloudflare Tunnel in
// front.
//
// TLS here is camouflage, not protection. Every bond datagram is sealed with
// its session's key and carries WireGuard inside, and the join exchange is
// sealed with the join code, so hubs do not check the certificate. That lets
// the fallback through proxies that inspect TLS and re-sign it, and lets an
// aggregator with no DNS name use a certificate it made itself. It still gets
// a real one when it can: some of those proxies refuse to connect onward to
// a server whose certificate they cannot verify.

const (
	acmeRetry    = time.Hour   // after Let's Encrypt fails; well inside its rate limits
	certRecheck  = time.Minute // how often --tls-cert files are looked at for a renewal
	selfValidFor = 10 * 365 * 24 * time.Hour
)

// serverTLS builds the TLS listener's config. host is the aggregator's public
// name or address. It returns a description of where the certificate comes
// from, for the log.
func serverTLS(o ServerOptions, host string, log *slog.Logger) (*tls.Config, string, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}
	if o.TLSCert != "" {
		f, err := loadCertFiles(o.TLSCert, o.TLSKey, log)
		if err != nil {
			return nil, "", err
		}
		cfg.GetCertificate = f.get
		return cfg, "files", nil
	}
	self, err := selfSigned(filepath.Join(o.StateDir, "tls-self.pem"), host)
	if err != nil {
		return nil, "", err
	}
	if !isDNSName(host) {
		cfg.Certificates = []tls.Certificate{*self}
		return cfg, "self-signed", nil
	}
	a := &acmeCerts{
		m: &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Cache:      autocert.DirCache(filepath.Join(o.StateDir, "acme")),
			HostPolicy: autocert.HostWhitelist(host),
			Client:     o.acme,
		},
		host: host,
		self: self,
		log:  log,
	}
	cfg.GetCertificate = a.get
	cfg.NextProtos = append(cfg.NextProtos, acme.ALPNProto) // Let's Encrypt's tls-alpn-01 challenge
	return cfg, "letsencrypt", nil
}

// warmCert gets the certificate a hub dialling host would be served, so that
// one from Let's Encrypt is fetched now rather than in the first hub's
// handshake, and a failure shows up in the log while someone is watching.
func warmCert(cfg *tls.Config, host string) {
	if cfg.GetCertificate != nil {
		go cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: host, CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256}})
	}
}

// isDNSName reports whether host is a name Let's Encrypt could issue for.
func isDNSName(host string) bool {
	if _, err := netip.ParseAddr(host); err == nil {
		return false
	}
	return strings.Contains(strings.Trim(host, "."), ".") && !strings.HasSuffix(host, ".local")
}

// acmeCerts gets the certificate from Let's Encrypt and falls back to the
// self-signed one whenever that is not possible: a hub that dialled an IP
// address and sent no name, or a port 443 Let's Encrypt cannot reach.
type acmeCerts struct {
	m    *autocert.Manager
	host string
	self *tls.Certificate
	log  *slog.Logger

	mu      sync.Mutex
	retryAt time.Time
}

func (a *acmeCerts) get(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if slices.Contains(hello.SupportedProtos, acme.ALPNProto) {
		return a.m.GetCertificate(hello) // Let's Encrypt checking we hold the name
	}
	if !strings.EqualFold(strings.TrimSuffix(hello.ServerName, "."), a.host) {
		return a.self, nil
	}
	a.mu.Lock()
	wait := time.Now().Before(a.retryAt)
	a.mu.Unlock()
	if wait {
		return a.self, nil
	}
	c, err := a.m.GetCertificate(hello)
	if err != nil {
		a.mu.Lock()
		a.retryAt = time.Now().Add(acmeRetry)
		a.mu.Unlock()
		a.log.Warn("could not get a certificate from Let's Encrypt; using a self-signed one for now", "host", a.host, "retry_in", acmeRetry, "err", err)
		return a.self, nil
	}
	return c, nil
}

// certFiles serves a certificate kept up to date by something else, such as
// certbot, and picks up its renewals.
type certFiles struct {
	certFile, keyFile string
	log               *slog.Logger

	mu      sync.Mutex
	cert    *tls.Certificate
	stamp   string // modification times the loaded pair had
	checked time.Time
}

func loadCertFiles(certFile, keyFile string, log *slog.Logger) (*certFiles, error) {
	f := &certFiles{certFile: certFile, keyFile: keyFile, log: log}
	if err := f.reload(); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *certFiles) modTimes() string {
	var out []string
	for _, name := range []string{f.certFile, f.keyFile} {
		if st, err := os.Stat(name); err == nil {
			out = append(out, st.ModTime().String())
		}
	}
	return strings.Join(out, ",")
}

// reload loads the pair if it has changed since it was last loaded.
func (f *certFiles) reload() error {
	stamp := f.modTimes()
	if f.cert != nil && stamp == f.stamp {
		return nil
	}
	c, err := tls.LoadX509KeyPair(f.certFile, f.keyFile)
	if err != nil {
		return fmt.Errorf("loading TLS certificate: %w", err)
	}
	f.cert, f.stamp = &c, stamp
	return nil
}

func (f *certFiles) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if time.Since(f.checked) >= certRecheck {
		f.checked = time.Now()
		if err := f.reload(); err != nil {
			// Half-written during a renewal, most likely: keep serving the old one.
			f.log.Warn("keeping the previous TLS certificate", "err", err)
		}
	}
	return f.cert, nil
}

// selfSigned loads the aggregator's own certificate from file, making it on
// first use. It is kept so that the aggregator looks the same across restarts.
func selfSigned(file, host string) (*tls.Certificate, error) {
	if b, err := os.ReadFile(file); err == nil {
		if c, err := tls.X509KeyPair(b, b); err == nil {
			return &c, nil
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(selfValidFor),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		tmpl.IPAddresses = []net.IP{ip.AsSlice()}
	} else if host != "" {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	b := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb})...)
	if err := os.WriteFile(file, b, 0o600); err != nil {
		return nil, err
	}
	c, err := tls.X509KeyPair(b, b)
	return &c, err
}

// tlsListenAddr accepts a bare port for --tls as well as an address.
func tlsListenAddr(s string) string {
	if _, err := strconv.Atoi(s); err == nil {
		return ":" + s
	}
	return s
}

// wsURL is the WebSocket fallback URL for an aggregator at host, leaving the
// port out where it is the scheme's own.
func wsURL(scheme, host string, port int) string {
	hp := net.JoinHostPort(host, strconv.Itoa(port))
	if (scheme == "wss" && port == 443) || (scheme == "ws" && port == 80) {
		hp = host
		if strings.Contains(host, ":") {
			hp = "[" + host + "]"
		}
	}
	return (&url.URL{Scheme: scheme, Host: hp, Path: "/v1/ws"}).String()
}
