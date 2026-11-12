package node

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/CarterCole/bond/core/control"
)

func startTLSServer(t *testing.T, dir string, o ServerOptions) *Server {
	t.Helper()
	o.StateDir, o.Listen, o.Userspace, o.Admin, o.Log, o.Dial = dir, "127.0.0.1:0", true, "127.0.0.1:0", quiet, internet(t)
	if o.Public == "" {
		o.Public = "127.0.0.1"
	}
	s, err := StartServer(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// With no name to get a certificate for, the aggregator makes its own, and
// hubs take the fallback over TLS regardless.
func TestWebSocketOverTLS(t *testing.T) {
	dir := t.TempDir()
	s := startTLSServer(t, dir, ServerOptions{TLS: "127.0.0.1:0"})
	cfg, hdir := join(t, s, "hotel", control.KindHub)
	if len(cfg.Mesh.WS) != 1 || !strings.HasPrefix(cfg.Mesh.WS[0], "wss://127.0.0.1:") {
		t.Fatalf("devices were told %v", cfg.Mesh.WS)
	}
	h := startHub(t, HubOptions{Config: cfg, StateDir: hdir, Transport: "ws"})
	eventually(t, "fetch over websocket paths on TLS", 10*time.Second, wantPage(h))
	if up := pathsUp(h); !up["ws:a"] || !up["ws:b"] {
		t.Fatalf("paths: %v", up)
	}
	if _, err := os.Stat(filepath.Join(dir, "tls-self.pem")); err != nil {
		t.Fatal("self-signed certificate not kept:", err)
	}
}

// Devices are told what the flags say now, not what they said last time.
func TestWSURLFollowsTheFlags(t *testing.T) {
	dir := t.TempDir()
	s := startTLSServer(t, dir, ServerOptions{TLS: "127.0.0.1:0"})
	s.Close()
	s = startTLSServer(t, dir, ServerOptions{})
	cfg, _ := join(t, s, "hub", control.KindHub)
	if want := wsURL("ws", "127.0.0.1", s.Port()); len(cfg.Mesh.WS) != 1 || cfg.Mesh.WS[0] != want {
		t.Fatalf("devices were told %v, want %s", cfg.Mesh.WS, want)
	}
	s.Close()
	s = startTLSServer(t, dir, ServerOptions{TLS: "127.0.0.1:0", WS: "wss://bond.example.com/v1/ws"})
	cfg, _ = join(t, s, "hub2", control.KindHub)
	if len(cfg.Mesh.WS) != 1 || cfg.Mesh.WS[0] != "wss://bond.example.com/v1/ws" {
		t.Fatalf("--ws-url not honoured: %v", cfg.Mesh.WS)
	}
}

func TestTLSFlagsAreChecked(t *testing.T) {
	for _, o := range []ServerOptions{
		{TLS: ":0", TLSCert: "cert.pem"},
		{TLS: ":0", TLSKey: "key.pem"},
		{TLSCert: "cert.pem", TLSKey: "key.pem"},
	} {
		o.StateDir, o.Log = t.TempDir(), quiet
		if s, err := StartServer(o); err == nil {
			s.Close()
			t.Errorf("%+v: accepted", o)
		}
	}
}

func TestSelfSignedIsKept(t *testing.T) {
	file := filepath.Join(t.TempDir(), "self.pem")
	a, err := selfSigned(file, "203.0.113.5")
	if err != nil {
		t.Fatal(err)
	}
	b, err := selfSigned(file, "203.0.113.5")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Certificate[0], b.Certificate[0]) {
		t.Fatal("a new certificate was made although one was on disk")
	}
	if ip := a.Leaf.IPAddresses; len(ip) != 1 || ip[0].String() != "203.0.113.5" {
		t.Fatalf("certificate is for %v", ip)
	}
}

// certbot renews the files in place; the aggregator picks the new pair up and
// keeps the old one through a half-written renewal.
func TestCertFilesAreReloaded(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "cert.pem")
	first, err := selfSigned(file, "bond.example.com")
	if err != nil {
		t.Fatal(err)
	}
	f, err := loadCertFiles(file, file, quiet)
	if err != nil {
		t.Fatal(err)
	}
	serving := func() []byte {
		f.mu.Lock()
		f.checked = time.Time{} // as if a minute had passed
		f.mu.Unlock()
		c, _ := f.get(&tls.ClientHelloInfo{})
		return c.Certificate[0]
	}
	if !bytes.Equal(serving(), first.Certificate[0]) {
		t.Fatal("not serving the files' certificate")
	}

	second, err := selfSigned(filepath.Join(dir, "renewed.pem"), "bond.example.com")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "renewed.pem"))
	os.WriteFile(file, b, 0o600)
	later := time.Now().Add(time.Minute)
	os.Chtimes(file, later, later)
	if !bytes.Equal(serving(), second.Certificate[0]) {
		t.Fatal("renewal not picked up")
	}

	os.WriteFile(file, []byte("-----BEGIN CERTIFICATE-----\n"), 0o600)
	later = later.Add(time.Minute)
	os.Chtimes(file, later, later)
	if !bytes.Equal(serving(), second.Certificate[0]) {
		t.Fatal("a broken renewal replaced a working certificate")
	}
}

// Whenever Let's Encrypt cannot help, the self-signed certificate answers,
// and a failure is not retried on every handshake.
func TestACMEFallsBackToSelfSigned(t *testing.T) {
	self, err := selfSigned(filepath.Join(t.TempDir(), "self.pem"), "bond.example.com")
	if err != nil {
		t.Fatal(err)
	}
	a := &acmeCerts{
		m: &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist("bond.example.com"),
			Client:     &acme.Client{DirectoryURL: "http://127.0.0.1:1/directory"}, // nobody there
		},
		host: "bond.example.com", self: self, log: quiet,
	}
	for _, name := range []string{"", "other.example.com", "bond.example.com", "bond.example.com"} {
		c, err := a.get(&tls.ClientHelloInfo{ServerName: name})
		if err != nil || c != self {
			t.Errorf("server name %q: got %v, %v", name, c, err)
		}
	}
	if a.retryAt.IsZero() {
		t.Fatal("the failure was not remembered")
	}
}

// Getting a certificate for real, from a local Pebble (Let's Encrypt's test
// server) started with its own test/config/pebble-config.json, which checks
// tls-alpn-01 on port 5001. bond.localtest.me resolves to loopback.
//
//	PEBBLE_CA=<pebble>/test/certs/pebble.minica.pem go test ./core/node -run LetsEncrypt
func TestLetsEncryptCertificate(t *testing.T) {
	ca, err := os.ReadFile(os.Getenv("PEBBLE_CA"))
	if err != nil {
		t.Skip("set PEBBLE_CA to run against a local Pebble")
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	ac := &acme.Client{
		DirectoryURL: "https://127.0.0.1:14000/dir",
		HTTPClient:   &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}},
	}
	s := startTLSServer(t, t.TempDir(), ServerOptions{Public: "bond.localtest.me", TLS: ":5001", acme: ac})
	issuer := func(name string) (string, error) {
		c, err := tls.Dial("tcp", "127.0.0.1:5001", &tls.Config{ServerName: name, InsecureSkipVerify: true})
		if err != nil {
			return "", err
		}
		defer c.Close()
		return c.ConnectionState().PeerCertificates[0].Issuer.CommonName, nil
	}
	eventually(t, "a certificate from Pebble", 30*time.Second, func() error {
		if is, err := issuer("bond.localtest.me"); err != nil || !strings.HasPrefix(is, "Pebble") {
			return fmt.Errorf("issued by %q, %v", is, err)
		}
		return nil
	})
	if is, err := issuer(""); err != nil || is != "bond.localtest.me" {
		t.Fatalf("a dial by address got a certificate issued by %q, %v", is, err)
	}

	cfg, hdir := join(t, s, "hotel", control.KindHub)
	if len(cfg.Mesh.WS) != 1 || cfg.Mesh.WS[0] != "wss://bond.localtest.me:5001/v1/ws" {
		t.Fatalf("devices were told %v", cfg.Mesh.WS)
	}
	h := startHub(t, HubOptions{Config: cfg, StateDir: hdir, Transport: "ws"})
	eventually(t, "fetch over websocket paths on TLS", 10*time.Second, wantPage(h))
}

func TestWSURL(t *testing.T) {
	for _, c := range []struct {
		scheme, host string
		port         int
		want         string
	}{
		{"wss", "bond.example.com", 443, "wss://bond.example.com/v1/ws"},
		{"wss", "203.0.113.5", 8443, "wss://203.0.113.5:8443/v1/ws"},
		{"wss", "2001:db8::1", 443, "wss://[2001:db8::1]/v1/ws"},
		{"ws", "2001:db8::1", 7777, "ws://[2001:db8::1]:7777/v1/ws"},
		{"ws", "bond.example.com", 7777, "ws://bond.example.com:7777/v1/ws"},
	} {
		if got := wsURL(c.scheme, c.host, c.port); got != c.want {
			t.Errorf("wsURL(%s, %s, %d) = %s, want %s", c.scheme, c.host, c.port, got, c.want)
		}
	}
}

func TestIsDNSName(t *testing.T) {
	for host, want := range map[string]bool{
		"bond.example.com":  true,
		"bond.example.com.": true,
		"203.0.113.5":       false,
		"2001:db8::1":       false,
		"localhost":         false,
		"aggregator.local":  false,
	} {
		if got := isDNSName(host); got != want {
			t.Errorf("isDNSName(%q) = %v", host, got)
		}
	}
}
